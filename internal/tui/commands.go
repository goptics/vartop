package tui

import (
	"errors"
	"fmt"
	"maps"
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
	// resyncAfter is how long after an action the TUI syncs again: the poke
	// has the daemon poll at once, so its effect shows within a second.
	resyncAfter = 300 * time.Millisecond
	// settlePoll is how often an action on a stale server syncs, waiting for
	// the snapshot its poke asked for.
	settlePoll = 100 * time.Millisecond
)

// tickCmd sends the next tickMsg in syncEvery.
func tickCmd() tea.Cmd {
	return tea.Tick(syncEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// syncCmd syncs feed, the feed of the server shown in generation gen, and
// reads what control.db says of it. nil for no feed: nothing to sync.
func (m Model) syncCmd(feed *store.Feed, gen int) tea.Cmd {
	if feed == nil {
		return nil
	}
	key, ctl := m.key, m.ctl
	return func() tea.Msg {
		msg := syncedMsg{server: key, gen: gen, err: feed.Sync()}
		msg.down, msg.srvErr = controlState(ctl, key)
		return msg
	}
}

// controlState is what control.db says of the server keyed key: whether no
// daemon holds a fresh lease, and the server's collection error. A lease
// that cannot be read counts as held: nothing says the daemon is down.
func controlState(ctl *store.Control, key string) (down bool, srvErr string) {
	if _, fresh, err := ctl.Lease(); err == nil && !fresh {
		down = true
	}
	if errs, err := ctl.ServerErrs(); err == nil {
		srvErr = errs[key]
	}
	return down, srvErr
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
// starts the daemon when none holds a fresh lease. nil when the server is
// not configured: there is nothing to poke.
func (m *Model) refreshCmd() tea.Cmd {
	m.err = nil
	if m.server() == nil {
		return nil
	}
	key, ctl, start := m.key, m.ctl, m.start
	return func() tea.Msg {
		_, err := ctl.Poke(key)
		if _, fresh, lerr := ctl.Lease(); lerr == nil && !fresh && start != nil {
			err = errors.Join(err, start())
		}
		return actedMsg{server: key, err: err}
	}
}

// actCmd sends action to worker on the unit labelled label of the server
// shown, as its newest snapshot has it: a direct unit is sent it, with the
// server's token, and the server is poked; a unit behind a load balancer
// is refused (fleet.OneBalancedMsg), as is a snapshot still stale after a
// poke. nil when there is nothing to act on.
func (m *Model) actCmd(worker, label, action string) tea.Cmd {
	srv := m.server()
	if srv == nil || m.feed == nil || worker == "" || label == "" {
		return nil
	}
	m.err = nil
	a := actor{feed: m.feed, ctl: m.ctl, key: m.key, interval: srv.PollInterval(), token: srv.Token, noun: m.noun(), wait: m.settleWait}
	return func() tea.Msg {
		return actedMsg{server: a.key, err: a.act(worker, label, action)}
	}
}

// actor sends an action on one server's units.
type actor struct {
	feed     *store.Feed
	ctl      *store.Control
	key      string
	interval time.Duration
	token    string
	noun     fleet.Noun
	wait     time.Duration // how long a stale server is given to take a newer snapshot (fleet.SettleWait)
}

func (a actor) act(worker, label, action string) error {
	latest := a.feed.Latest()
	if latest == nil || fleet.Stale(latest, a.interval, time.Now()) {
		a.settle()
		latest = a.feed.Latest()
	}
	switch now := time.Now(); {
	case latest == nil:
		return errors.New(fleet.WaitingMsg(a.key))
	case fleet.Stale(latest, a.interval, now):
		down, srvErr := controlState(a.ctl, a.key)
		return errors.New(fleet.StaleMsg(a.key, latest, now, down, srvErr))
	}
	units := fleet.PodsOf(latest, label)
	switch {
	case len(units) == 0:
		return errors.New(fleet.GoneLabelMsg(label, a.noun))
	case units[0].Balanced:
		return errors.New(fleet.OneBalancedMsg)
	}
	err := api.NewClient(units[0].Key, a.token).Action(worker, action)
	_, _ = a.ctl.Poke(a.key) // sent, or maybe sent: the daemon polls now, to show what it did
	if err != nil {
		return fmt.Errorf("failed to %s worker %s on %s: %w", action, worker, label, err)
	}
	return nil
}

// settle pokes the server and syncs its feed until it holds a snapshot
// taken after the poke, for up to a.wait. With no daemon running, which
// would answer the poke, it returns at once.
func (a actor) settle() {
	if down, _ := controlState(a.ctl, a.key); down {
		return
	}
	at, err := a.ctl.Poke(a.key)
	if err != nil {
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
