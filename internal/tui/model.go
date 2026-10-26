package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// Model is the root TUI state: shared state plus a stack of screens. The
// dashboard is always at the base of the stack; overlays (switcher, worker
// detail) are pushed on top of it. Each screen owns its own state and key
// handling and is given the root model to read and mutate shared state.
//
// The TUI only reads what the vartop daemon collects into the store: the
// feed of the server it shows, synced every second. It reaches varmq only
// to send a worker action.
type Model struct {
	cfg     *config.Config
	key     string // config key of the server shown
	dataDir string
	ctl     *store.Control
	start   func() error                   // starts the daemon; nil: nothing to start
	load    func() (*config.Config, error) // reads the config again, every tick

	// feeds holds the feed of every server shown so far, kept for the
	// model's life (Close closes them): showing a server again is instant.
	// A map, so every copy of the model shares it.
	feeds map[feedKey]*store.Feed
	feed  *store.Feed // the shown server's; nil when it is not configured
	// gen numbers what the model showed: every switch, even back to the
	// same server, starts a new one, and a sync of an older one is dropped.
	gen int

	// What the last sync of this generation said.
	syncErr error  // reading the feed failed
	down    bool   // no daemon holds a fresh lease
	srvErr  string // the server's collection error

	err error // the error line: what the last refresh or action said

	switchLatest map[string]*fleet.Snapshot // the switcher's read of the store, by server key

	settleWait time.Duration           // how long an action on a stale server waits for a newer snapshot
	closeFeed  func(*store.Feed) error // closes a feed no longer wanted; (*store.Feed).Close but in tests
	// buildTrends builds the series of the sparklines (see trendsFor); newTrends,
	// but counted in tests.
	buildTrends func([]*fleet.Snapshot, string, time.Duration, fleet.Noun) trendSet
	// buildDetail builds the series of the detail screen's charts (see
	// detailFor); newDetailSeries, but counted in tests.
	buildDetail func([]*fleet.Snapshot, string, string) detailSeries

	width  int
	height int

	selectedLabel  string // empty = merged view; non-empty = the unit with that label only
	showNodeColumn bool   // false when viewing a single unit (Node column hidden)

	screens  []screen
	switcher *switcherScreen
}

// feedKey names a feed: the server, and the interval it is polled at.
type feedKey struct {
	server   string
	interval time.Duration
}

// loadConfig reads the config. The TUI reads it again every tick, as the
// daemon and vartop web do, so that a server removed meanwhile shows as
// not configured, and an action takes the token configured now. Tests
// replace it.
var loadConfig = config.Load

// NewModel is the TUI over the store in dataDir, showing the server keyed
// serverKey in cfg. ctl is the store's control file: the daemon's lease,
// the servers' errors and pokes. start starts the daemon (ensureDaemon),
// for r when none runs; nil starts nothing. The first sync of the server's
// feed comes with Init.
func NewModel(cfg *config.Config, serverKey string, dataDir string, ctl *store.Control, start func() error) Model {
	m := Model{
		cfg:            cfg,
		dataDir:        dataDir,
		ctl:            ctl,
		start:          start,
		load:           loadConfig,
		feeds:          make(map[feedKey]*store.Feed),
		settleWait:     fleet.SettleWait,
		closeFeed:      (*store.Feed).Close,
		buildTrends:    newTrends,
		buildDetail:    newDetailSeries,
		showNodeColumn: true,
		width:          80, // Default width, will be updated by WindowSizeMsg
		height:         24, // Default height, will be updated by WindowSizeMsg
	}

	// The dashboard is the base of the stack; the switcher is a singleton so
	// its expanded-server state persists across open/close.
	m.screens = []screen{newDashboardScreen()}
	m.switcher = newSwitcherScreen()
	m.show(serverKey) // its sync is Init's
	m.switcher.rebuild(&m)

	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.syncCmd(m.feed, m.gen), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Key messages are handled entirely by the active screen, which also
	// forwards navigation keys to its embedded component (table/list).
	if key, ok := msg.(tea.KeyMsg); ok {
		return m, m.activeScreen().Update(&m, key)
	}

	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.handleWindowSize(msg)

	case tickMsg:
		cmds = append(cmds, tickCmd(), m.configCmd(), m.syncCmd(m.feed, m.gen))
		if m.isSwitcherActive() {
			cmds = append(cmds, m.switcherCmd())
		}

	case syncedMsg:
		m.handleSynced(msg)

	case configMsg:
		cmds = append(cmds, m.handleConfig(msg))

	case switcherMsg:
		m.switchLatest = msg.latest
		m.switcher.rebuild(&m)

	case actedMsg:
		cmds = append(cmds, m.handleActed(msg))

	case resyncMsg:
		if msg.server == m.key && msg.gen == m.gen {
			cmds = append(cmds, m.syncCmd(m.feed, m.gen))
		}
	}

	// Non-key messages are forwarded to the active screen's embedded
	// component, matching the old per-view update block.
	cmds = append(cmds, m.activeScreen().Update(&m, msg))

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	return m.activeScreen().View(&m)
}

// show makes the server keyed key the one shown, in a new generation: its
// feed (opened once, kept after) shows at once what it held, and the
// command returned syncs it; nil when key is not configured.
func (m *Model) show(key string) tea.Cmd {
	m.key = key
	m.gen++
	// The action menu is for the row it was opened on, which another server
	// may have a namesake of.
	m.dashboardScreen().inActionSelection = false
	m.syncErr, m.down, m.srvErr = nil, false, ""
	m.feed = nil
	if srv := m.server(); srv != nil {
		k := feedKey{key, srv.PollInterval()}
		if m.feeds[k] == nil {
			m.feeds[k] = store.NewFeed(m.dataDir, key, k.interval)
		}
		m.feed = m.feeds[k]
	}
	m.dashboardScreen().refresh(m)
	return m.syncCmd(m.feed, m.gen)
}

// server is the config of the server shown, nil when it is not configured.
func (m *Model) server() *config.Server { return m.cfg.Servers[m.key] }

// latest is the newest snapshot of the server shown, nil when there is none
// (or the server is not configured).
func (m *Model) latest() *fleet.Snapshot {
	if m.feed == nil {
		return nil
	}
	return m.feed.Latest()
}

// interval is the poll interval of the server shown.
func (m *Model) interval() time.Duration {
	if srv := m.server(); srv != nil {
		return srv.PollInterval()
	}
	return (&config.Server{}).PollInterval()
}

// noun is what the server shown calls its units.
func (m *Model) noun() fleet.Noun { return fleet.NounFor(m.server()) }

// stale reports whether the newest snapshot of the server shown is stale
// at now (see fleet.Stale).
func (m *Model) stale(now time.Time) bool { return fleet.Stale(m.latest(), m.interval(), now) }

// resetPodView returns to the merged view of every unit of a server; the
// table is rebuilt for it by the show that comes with it.
func (m *Model) resetPodView() {
	m.err = nil
	m.selectedLabel = ""
	m.showNodeColumn = true
}

// Close closes the files of every feed the model opened.
func (m *Model) Close() error {
	for _, f := range m.feeds {
		_ = f.Close()
	}
	return nil
}
