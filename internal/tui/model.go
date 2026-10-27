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
	sameEnv func(store.Lease) bool         // whether a daemon's lease is of this vartop's environment (R31); nil: every one is
	load    func() (*config.Config, error) // reads the config again, every tick

	// feeds holds the feed of every server shown so far (Close closes
	// them): showing a server again is instant. A feed hidden — its server
	// not shown — is not synced; one hidden for hiddenFeedTTL is closed and
	// forgotten, and one whose newest snapshot is stale when its server is
	// shown again is read afresh (see show). hidden says since when each
	// was. Maps, so every copy of the model shares them.
	feeds  map[feedKey]*store.Feed
	hidden map[feedKey]time.Time
	feed   *store.Feed // the shown server's; nil when it is not configured
	// gen numbers what the model showed: every switch, even back to the
	// same server, starts a new one, and a sync of an older one is dropped.
	gen int
	// syncing is the generation of a sync of the feed shown under way — one
	// a tick, a show, an action or the detail screen started — until its
	// syncedMsg comes; 0 when none is. A tick starts none meanwhile (see
	// tickSync). The generation names the feed too: a feed is shown in a
	// new one.
	syncing int

	// What the last sync of this generation said.
	syncErr error  // reading the feed failed
	down    bool   // no daemon holds a fresh lease
	srvErr  string // the server's collection error

	// daemon is what the last sync read of the daemon (store.ReadDaemon):
	// its lease, by which a snapshot is current (R44), and whether that is
	// another configuration's than this vartop's (R31), whose snapshots are
	// of that one's servers. Not the server's: a switch keeps it.
	daemon store.DaemonState

	err error // the error line: what the last refresh or action said

	switchLatest map[string]*fleet.Snapshot // the switcher's read of the store, by server key

	settleWait time.Duration           // how long an action on a stale server waits for a newer snapshot
	tick       func() tea.Cmd          // the next tick: tickCmd, but in tests
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

// Daemon is what the TUI is told of the vartop daemon by the vartop that
// runs it.
type Daemon struct {
	// Start starts the daemon (ensureDaemon) in this vartop's environment,
	// for r when none runs, or when one collects with another configuration;
	// nil starts nothing.
	Start func() error
	// SameEnv reports whether a daemon's lease — fresh, released or stale
	// — is of this vartop's environment: its config file and KUBECONFIG
	// (R31, R44). nil takes every daemon for this environment's.
	SameEnv func(store.Lease) bool
	// StartErr is what making sure the daemon runs said as the TUI opened —
	// it did not start, say — shown on the error line: printed, it would
	// be hidden behind the TUI's screen. nil when there is nothing to say.
	StartErr error
}

// NewModel is the TUI over the store in dataDir, showing the server keyed
// serverKey in cfg. ctl is the store's control file: the daemon's lease,
// the servers' errors and pokes. d is what the TUI knows of the daemon. The
// first sync of the server's feed comes with Init.
func NewModel(cfg *config.Config, serverKey string, dataDir string, ctl *store.Control, d Daemon) Model {
	m := Model{
		cfg:            cfg,
		dataDir:        dataDir,
		ctl:            ctl,
		start:          d.Start,
		sameEnv:        d.SameEnv,
		err:            d.StartErr,
		load:           loadConfig,
		feeds:          make(map[feedKey]*store.Feed),
		hidden:         make(map[feedKey]time.Time),
		settleWait:     fleet.SettleWait,
		closeFeed:      (*store.Feed).Close,
		tick:           tickCmd,
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
	return tea.Batch(m.syncCmd(m.feed, m.gen), m.tick())
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
		cmds = append(cmds, m.tick(), m.configCmd(), m.tickSync(), m.evictHidden(time.Time(msg)))
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
// command returned syncs it; nil when key is not configured. The feed shown
// before is kept, hidden (see Model.feeds).
//
// A kept feed whose newest snapshot is stale is not shown: hidden, it was
// not synced, though the daemon may have collected all along, and it would
// show its server hours old ("No new data for 3h…"). A new feed reads the
// last hour afresh instead, and the old one is closed by the command.
func (m *Model) show(key string) tea.Cmd {
	now := time.Now()
	var was feedKey // the feed shown before; none if zero
	if m.feed != nil {
		was = feedKey{m.key, m.feed.Interval()}
	}
	m.key = key
	m.gen++
	// The action menu is for the row it was opened on, which another server
	// may have a namesake of.
	m.dashboardScreen().inActionSelection = false
	m.syncErr, m.down, m.srvErr = nil, false, ""
	m.feed = nil
	var stale []*store.Feed
	if srv := m.server(); srv != nil {
		k := feedKey{key, srv.PollInterval()}
		if f := m.feeds[k]; f != nil && k != was && fleet.Stale(f.Latest(), k.interval, now) {
			stale = append(stale, f)
			delete(m.feeds, k)
		}
		if m.feeds[k] == nil {
			m.feeds[k] = store.NewFeed(m.dataDir, key, k.interval)
		}
		m.feed = m.feeds[k]
		delete(m.hidden, k)
	}
	if f := m.feeds[was]; f != nil && f != m.feed {
		m.hidden[was] = now
	}
	m.dashboardScreen().refresh(m)
	return tea.Batch(m.startSync(), m.closeCmd(stale))
}

// startSync is the sync of the feed shown (syncCmd), noted as under way (see
// Model.syncing); nil when there is no feed.
func (m *Model) startSync() tea.Cmd {
	cmd := m.syncCmd(m.feed, m.gen)
	if cmd != nil {
		m.syncing = m.gen
	}
	return cmd
}

// tickSync is a tick's sync of the feed shown: none while one of this
// generation is under way — one slower than the tick (the trends of a large
// fleet, say) would be started again, for the same snapshot, on every tick.
func (m *Model) tickSync() tea.Cmd {
	if m.syncing == m.gen {
		return nil
	}
	return m.startSync()
}

// evictHidden forgets the feeds hidden for hiddenFeedTTL at now, and closes
// them with the command it returns, as vartop web drops the feeds it no
// longer reads: not synced, what they hold is old by then, and an open file
// would keep the daemon from removing or setting it aside (Windows).
func (m *Model) evictHidden(now time.Time) tea.Cmd {
	var gone []*store.Feed
	for k, since := range m.hidden {
		if now.Sub(since) < hiddenFeedTTL {
			continue
		}
		if f := m.feeds[k]; f != nil && f != m.feed {
			gone = append(gone, f)
			delete(m.feeds, k)
		}
		delete(m.hidden, k)
	}
	return m.closeCmd(gone)
}

// closeCmd closes feeds the model no longer holds, off the UI goroutine:
// Close waits for a sync of the feed under way. nil for none.
func (m *Model) closeCmd(feeds []*store.Feed) tea.Cmd {
	if len(feeds) == 0 {
		return nil
	}
	closeFeed := m.closeFeed
	return func() tea.Msg {
		for _, f := range feeds {
			_ = closeFeed(f)
		}
		return nil
	}
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

// stale reports whether what the server shown has is not current at now
// (see notCurrent). Its values show —.
func (m *Model) stale(now time.Time) bool { return m.notCurrent(m.latest(), m.interval(), now) }

// notCurrent reports whether latest, the newest snapshot of a server polled
// every interval, is not current at now, by what the last sync read of the
// daemon (store.DaemonState.Current, R44): stale, a previous daemon's, or
// another configuration's. With none, only the lease of another
// configuration makes it so: its daemon's servers are not this one's.
func (m *Model) notCurrent(latest *fleet.Snapshot, interval time.Duration, now time.Time) bool {
	return m.daemon.Other || latest != nil && !m.daemon.Current(latest, interval, now)
}

// otherConfigMsg is the notice of a daemon collecting with another
// configuration (R31): the header's, and every action's refusal.
var otherConfigMsg = fleet.OtherConfigMsg("press r")

// staleMsg is the header's warning, under the status bar: why what the
// server shown has is not current (see stale) — the lease is another
// configuration's, or how old its newest snapshot is and why (stale, or a
// previous daemon's); "" when it is current, or there is none (the body
// says so then).
func (m *Model) staleMsg(now time.Time) string {
	latest := m.latest()
	switch {
	case latest == nil:
		return ""
	case m.daemon.Other:
		return otherConfigMsg
	case m.stale(now):
		return fleet.StaleMsg(m.key, latest, now, m.down, m.srvErr)
	}
	return ""
}

// resetPodView returns to the merged view of every unit of a server; the
// table is rebuilt for it by the show that comes with it.
func (m *Model) resetPodView() {
	m.err = nil
	m.selectedLabel = ""
	m.showNodeColumn = true
}

// Close closes the files of every feed the model holds: those it let go
// were closed when it did.
func (m *Model) Close() error {
	for _, f := range m.feeds {
		_ = f.Close()
	}
	return nil
}
