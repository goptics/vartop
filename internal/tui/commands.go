package tui

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// Command functions return tea.Cmd for Bubble Tea. Everything that reads a
// file runs in one, off the UI goroutine: the first sync of a feed loads an
// hour of snapshots.

const (
	// syncEvery is how often the TUI syncs the feed it shows: the daemon
	// polls at the server's interval, the TUI only reads what it stored.
	syncEvery = time.Second
	// settlePoll is how often an action or a refresh syncs, waiting for the
	// snapshot its poke asked for.
	settlePoll = 100 * time.Millisecond
	// hiddenFeedTTL is how long the feed of a server not shown is kept: as
	// long as vartop web keeps a feed no page reads.
	hiddenFeedTTL = 10 * time.Minute
)

// tickCmd sends the next tickMsg in syncEvery.
func tickCmd() tea.Cmd {
	return tea.Tick(syncEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// syncCmd syncs feed, the feed of the server shown in generation gen, and
// reads what control.db says of it; and builds the series of the sparklines
// of the view shown, unless the model has them (see trendsFor), and those of
// the charts of the detail screen, when it is the one shown (see detailFor).
// nil for no feed: nothing to sync.
func (m Model) syncCmd(feed *store.Feed, gen int) tea.Cmd {
	if feed == nil {
		return nil
	}
	key, ctl, sameEnv := m.key, m.ctl, m.sameEnv
	label, interval, noun, build := m.selectedLabel, m.interval(), m.noun(), m.buildTrends
	have := m.dashboardScreen().trends.key
	var open unitWorker // what the detail screen shows; none if it is not
	var haveDetail detailKey
	buildDetail := m.buildDetail
	if d, ok := m.activeScreen().(*detailScreen); ok && d.workerName != "" {
		open, haveDetail = unitWorker{d.label, d.workerName}, d.series.key
	}
	return func() tea.Msg {
		msg := syncedMsg{server: key, gen: gen, err: feed.Sync()}
		msg.down, msg.otherEnv, msg.srvErr = controlState(ctl, key, sameEnv)
		msg.trends = trendsFor(feed, label, interval, noun, build, have)
		if open.worker != "" {
			msg.detail = detailFor(feed, open.label, open.worker, buildDetail, haveDetail)
		}
		return msg
	}
}

// trendsFor builds, with build, the series of the sparklines of feed's
// snapshots for the view of the unit labelled label ("" is every unit). They
// come keyed by what they are built from, and none are built, and none sent,
// when have, the key of those the model holds, is that already: a sync of the
// snapshots it has costs nothing. Update only takes them in, it never builds
// them: at a fleet's size that takes tens of milliseconds, and hundreds with
// a short interval.
func trendsFor(feed *store.Feed, label string, interval time.Duration, noun fleet.Noun, build func([]*fleet.Snapshot, string, time.Duration, fleet.Noun) trendSet, have trendsKey) trendSet {
	snaps := feed.Snapshots() // read once: the key's newest snapshot is the series'
	key := trendsKey{feed: feed, label: label}
	if len(snaps) > 0 {
		key.latest = snaps[len(snaps)-1]
	}
	if key == have {
		return trendSet{}
	}
	ts := build(snaps, label, interval, noun)
	ts.key = key
	return ts
}

// detailFor builds, with build, the series of the charts of feed's snapshots
// for the worker on the unit labelled label. They come keyed by what they are
// built from, and none are built, and none sent, when have, the key of those
// the screen holds, is that already: a sync of the snapshots it has costs
// nothing. As with trendsFor, Update only takes them in, it never builds
// them.
func detailFor(feed *store.Feed, label, worker string, build func([]*fleet.Snapshot, string, string) detailSeries, have detailKey) detailSeries {
	snaps := feed.Snapshots() // read once: the key's newest snapshot is the series'
	key := detailKey{feed: feed, label: label, worker: worker}
	if len(snaps) > 0 {
		key.latest = snaps[len(snaps)-1]
	}
	if key == have {
		return detailSeries{}
	}
	s := build(snaps, label, worker)
	s.key = key
	return s
}

// controlState is what control.db says of the server keyed key: what it
// says of the daemon (see daemonState), and the server's collection error.
func controlState(ctl *store.Control, key string, sameEnv func(store.Lease) bool) (down, other bool, srvErr string) {
	down, other = daemonState(ctl, sameEnv)
	if errs, err := ctl.ServerErrs(); err == nil {
		srvErr = errs[key]
	}
	return down, other, srvErr
}

// daemonState is what control.db says of the daemon: whether none holds a
// fresh lease (down), and whether the one that does collects with another
// configuration than this vartop's (other; R31) — sameEnv says, nil taking
// every one for this one's. Only a fresh lease is a daemon's that collects:
// a stale one is not running, whatever its environment. A lease that cannot
// be read counts as held, in this environment: nothing says otherwise.
func daemonState(ctl *store.Control, sameEnv func(store.Lease) bool) (down, other bool) {
	l, fresh, err := ctl.Lease()
	switch {
	case err != nil:
		return false, false
	case !fresh:
		return true, false
	}
	return false, sameEnv != nil && !sameEnv(l)
}

// configCmd reads the config again. One that cannot be read (half written,
// say) is no message: the model keeps the one it has.
func (m Model) configCmd() tea.Cmd {
	load := m.load
	return func() tea.Msg {
		cfg, err := load()
		if err != nil || cfg == nil {
			return nil
		}
		return configMsg{cfg: cfg}
	}
}

// switcherCmd reads the newest stored snapshot of every configured server,
// for the switcher: each file opened, read and closed again.
func (m Model) switcherCmd() tea.Cmd {
	keys, dataDir := slices.Sorted(maps.Keys(m.cfg.Servers)), m.dataDir
	return func() tea.Msg {
		latest := make(map[string]*fleet.Snapshot, len(keys))
		for _, key := range keys {
			latest[key] = readLatest(dataDir, key)
		}
		return switcherMsg{latest: latest}
	}
}

// readLatest is the newest snapshot stored for the server keyed key; nil
// when there is none, or it cannot be read: the switcher shows no count.
func readLatest(dataDir, key string) *fleet.Snapshot {
	db, err := store.OpenServer(dataDir, key, false)
	if err != nil {
		return nil
	}
	defer db.Close()
	s, err := db.Latest()
	if err != nil {
		return nil
	}
	return s
}

// refreshCmd pokes the server shown, so that the daemon polls it now, and
// starts the daemon when none holds a fresh lease; then waits, as an action
// does, for the snapshot the poke asked for (see actor.await). One
// collecting with another configuration (R31) is not poked: this vartop's
// is started first, which replaces it (ensureDaemon), as vartop web's Start
// collector does. nil when the server is not configured: there is nothing
// to poke.
func (m *Model) refreshCmd() tea.Cmd {
	m.err = nil
	if m.server() == nil || m.feed == nil {
		return nil
	}
	start := m.start
	a := actor{feed: m.feed, ctl: m.ctl, sameEnv: m.sameEnv, key: m.key, wait: m.settleWait}
	return func() tea.Msg {
		down, other := daemonState(a.ctl, a.sameEnv)
		if other {
			if start == nil {
				return actedMsg{server: a.key, err: errors.New(otherConfigMsg)}
			}
			if err := start(); err != nil {
				return actedMsg{server: a.key, err: err}
			}
		}
		at, err := a.ctl.Poke(a.key)
		if down && start != nil {
			err = errors.Join(err, start())
		}
		if err == nil {
			a.await(at)
		}
		return actedMsg{server: a.key, err: err}
	}
}

// actCmd sends action to worker on the unit labelled label of the server
// shown, as its newest snapshot has it: a direct unit is sent it, with the
// server's token, and the server is poked, the command ending with the
// snapshot that shows what it did (up to fleet.SettleWait); a unit behind a
// load balancer is refused (fleet.OneBalancedMsg), as is a snapshot still
// stale after a poke, and any under a daemon collecting with another
// configuration (R31). nil when there is nothing to act on.
func (m *Model) actCmd(worker, label, action string) tea.Cmd {
	srv := m.server()
	if srv == nil || m.feed == nil || worker == "" || label == "" {
		return nil
	}
	m.err = nil
	a := actor{feed: m.feed, ctl: m.ctl, sameEnv: m.sameEnv, key: m.key, interval: srv.PollInterval(), token: srv.Token, noun: m.noun(), wait: m.settleWait}
	return func() tea.Msg {
		return actedMsg{server: a.key, err: a.act(worker, label, action)}
	}
}

// actor sends an action on one server's units.
type actor struct {
	feed     *store.Feed
	ctl      *store.Control
	sameEnv  func(store.Lease) bool // the model's (see daemonState)
	key      string
	interval time.Duration
	token    string
	noun     fleet.Noun
	wait     time.Duration // how long a poke is given to bring a snapshot (fleet.SettleWait)
}

func (a actor) act(worker, label, action string) error {
	if _, other := daemonState(a.ctl, a.sameEnv); other {
		// Its snapshots name units of another configuration's servers: this
		// one's token must not reach them. Nor is that daemon poked.
		return errors.New(otherConfigMsg)
	}
	latest := a.feed.Latest()
	if latest == nil || fleet.Stale(latest, a.interval, time.Now()) {
		a.settle()
		latest = a.feed.Latest()
	}
	switch now := time.Now(); {
	case latest == nil:
		return errors.New(fleet.WaitingMsg(a.key))
	case fleet.Stale(latest, a.interval, now):
		down, _, srvErr := controlState(a.ctl, a.key, a.sameEnv)
		return errors.New(fleet.StaleMsg(a.key, latest, now, down, srvErr))
	}
	units := fleet.PodsOf(latest, label)
	switch {
	case len(units) == 0:
		return errors.New(fleet.GoneLabelMsg(label, a.noun))
	case units[0].Balanced:
		return errors.New(fleet.OneBalancedMsg)
	}
	// Escaped, as fleet.Act sends it: the name is one segment of the path.
	err := api.NewClient(units[0].Key, a.token).Action(url.PathEscape(worker), action)
	// Sent, or maybe sent: the daemon polls now, and the action ends with
	// the snapshot that shows what it did, as vartop web's does.
	if at, perr := a.ctl.Poke(a.key); perr == nil {
		a.await(at)
	}
	if err != nil {
		return fmt.Errorf("failed to %s worker %s on %s: %w", action, worker, label, err)
	}
	return nil
}

// settle pokes the server and waits for the snapshot the poke asks for
// (see await). With no daemon running, which would answer the poke, it
// pokes nothing and returns at once.
func (a actor) settle() {
	if down, _ := daemonState(a.ctl, a.sameEnv); down {
		return
	}
	if at, err := a.ctl.Poke(a.key); err == nil {
		a.await(at)
	}
}

// await syncs the feed until it holds a snapshot taken at or after at, when
// the server was poked — the one the poke had the daemon take — for up to
// a.wait (fleet.SettleWait). With no daemon running to answer the poke, it
// returns at once.
func (a actor) await(at time.Time) {
	if down, _ := daemonState(a.ctl, a.sameEnv); down {
		return
	}
	for deadline := time.Now().Add(a.wait); time.Now().Before(deadline); time.Sleep(settlePoll) {
		if a.feed.Sync() == nil {
			if l := a.feed.Latest(); l != nil && !l.At.Before(at) {
				return
			}
		}
	}
}
