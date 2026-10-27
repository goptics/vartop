package tui

import (
	"errors"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
)

// TestDropsOtherGenSync: a sync that ends after the model moved on — to
// another server, or to the same one shown again — is dropped; only the
// current server and generation's applies.
func TestDropsOtherGenSync(t *testing.T) {
	f := newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{"http://a.invalid"}, Interval: 5},
		"b": {Endpoints: []string{"http://b.invalid"}, Interval: 5},
	}})
	f.claim()
	f.write("a", fresh(unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})))
	f.write("b", fresh(unit("instance-1", "http://b.invalid", map[string]*fleet.Sample{"emails": smp("Paused", 2, 0, 2, 0)})))
	m := f.model("a")
	late := m.syncCmd(m.feed, m.gen) // a's sync, still running when the switch comes
	aGen := m.gen

	deliver(m, m.show("b")())
	bRows := []string{"emails|instance-1|Paused|2|0|0|2"}
	if got := rowsOf(m); !slices.Equal(got, bRows) {
		t.Fatalf("b's rows = %v, want %v", got, bRows)
	}

	boom := errors.New("boom")
	for _, msg := range []syncedMsg{
		late().(syncedMsg),
		{server: "a", gen: aGen, err: boom},
		{server: "a", gen: m.gen, err: boom},
		{server: "b", gen: aGen, err: boom},
	} {
		deliver(m, msg)
		if got := rowsOf(m); !slices.Equal(got, bRows) {
			t.Fatalf("after %+v: rows = %v, want b's", msg, got)
		}
		if view := m.View(); strings.Contains(view, "boom") {
			t.Fatalf("after %+v: another sync's error is shown:\n%s", msg, view)
		}
	}

	deliver(m, syncedMsg{server: "b", gen: m.gen, err: boom})
	if view := m.View(); !strings.Contains(view, "cannot read the history of b: boom") {
		t.Fatalf("the current sync's error is not shown:\n%s", view)
	}
}

// TestTUIRemovedServer: a TUI showing a server that is then removed from
// the config says it is not configured, shows none of its data, and sends
// nothing: no request to varmq, no poke — from the dashboard or a detail
// screen left open.
func TestTUIRemovedServer(t *testing.T) {
	fa, aSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 4, Busy: 1}})
	fb, bSrv := fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 4, Busy: 2}})
	f := newFixture(t, &config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"multi": {Interval: 1, Endpoints: []string{aSrv.URL, bSrv.URL}},
		"other": {Interval: 1, Endpoints: []string{"http://other.invalid"}},
	}})
	f.startDaemon()
	m := f.model("multi")
	waitRows(t, m, 2)
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // a detail screen, left open
	if !m.isDetailActive() {
		t.Fatal("enter did not open the detail screen")
	}

	f.stopDaemon() // so that every call the fakes get from now is the TUI's
	calls, pokes := len(fa.Calls())+len(fb.Calls()), f.pokes("multi")
	f.setConfig(&config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"other": {Interval: 1, Endpoints: []string{"http://other.invalid"}},
	}})
	reloadConfig(t, m)

	const copy = "Server multi is not configured"
	check := func(where string) {
		t.Helper()
		view := m.View()
		if !strings.Contains(view, copy) {
			t.Errorf("%s: view lacks %q:\n%s", where, copy, view)
		}
		if strings.Contains(view, "Running") || strings.Contains(view, "instance-1") {
			t.Errorf("%s: view shows the removed server's data:\n%s", where, view)
		}
		for _, k := range []tea.KeyMsg{runeKey('a'), runeKey('p'), runeKey('r'), runeKey('a'), runeKey('s'), {Type: tea.KeyEnter}} {
			if cmd := press(m, k); cmd != nil {
				if msg := cmd(); msg != nil {
					deliver(m, msg)
				}
			}
		}
		if cmd := m.syncCmd(m.feed, m.gen); cmd != nil {
			t.Errorf("%s: a tick would sync a server that is not configured", where)
		}
		deliver(m, tea.WindowSizeMsg{Width: 30, Height: 8})
		_ = m.View()
		deliver(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	check("detail")
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("esc left %T active", m.activeScreen())
	}
	check("dashboard")
	if rows := rowsOf(m); len(rows) != 0 {
		t.Errorf("rows of a removed server: %v", rows)
	}

	if now := len(fa.Calls()) + len(fb.Calls()); now != calls {
		t.Errorf("varmq got %d requests after the server was removed", now-calls)
	}
	if now := f.pokes("multi"); now != pokes {
		t.Errorf("the removed server was poked %d times", now-pokes)
	}

	// The switcher lists what is configured now.
	openSwitcher(t, m)
	var names []string
	for _, it := range m.switcher.switcher.Items() {
		names = append(names, it.(switchServerItem).name)
	}
	if !slices.Equal(names, []string{"other"}) {
		t.Errorf("switcher lists %v, want [other]", names)
	}
}

// TestActedAppliesAcrossGenerations: the end of an action is shown for the
// server on screen even when the model moved to a new generation of it
// meanwhile (the action may well have been sent); one for another server is
// dropped.
func TestActedAppliesAcrossGenerations(t *testing.T) {
	pod := newFakePod(t)
	pod.rejectActions()
	f := newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{pod.url()}, Interval: 5},
		"b": {Endpoints: []string{"http://b.invalid"}, Interval: 5},
	}})
	f.claim()
	f.write("a", fresh(unit("instance-1", pod.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})))
	m := f.model("a")
	cmd := m.actCmd("orders", "instance-1", "pause")
	runAll(m, m.show("a")) // shown again while the action runs: a new generation
	if deliver(m, cmd()) == nil {
		t.Error("no sync scheduled after the action")
	}
	if m.err == nil || !strings.Contains(m.err.Error(), "action rejected") {
		t.Fatalf("error = %v, want the action's, begun a generation earlier", m.err)
	}
	if pod.count("PATCH /workers/orders/actions/pause") != 1 {
		t.Fatalf("varmq got %v", pod.seen())
	}

	shown := m.err
	deliver(m, actedMsg{server: "b", err: errors.New("b's")})
	if m.err != shown {
		t.Fatalf("error = %v: another server's action was applied", m.err)
	}
}

// TestRemovedServerFeedClosed: the feed of a server removed from the config,
// or of an interval it no longer has, is closed and forgotten: an open file
// would keep the daemon from removing it.
func TestRemovedServerFeedClosed(t *testing.T) {
	f := newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{"http://a.invalid"}, Interval: 5},
		"b": {Endpoints: []string{"http://b.invalid"}, Interval: 5},
		"c": {Endpoints: []string{"http://c.invalid"}, Interval: 5},
	}})
	m := f.model("a")
	var closed []feedKey
	var before map[feedKey]*store.Feed // the model's feeds before the config changed
	m.closeFeed = func(fd *store.Feed) error {
		for k, v := range m.feeds {
			if v == fd {
				t.Errorf("feed %v closed while the model still holds it", k)
			}
		}
		for _, k := range []feedKey{{"a", 5 * time.Second}, {"b", 5 * time.Second}, {"c", 5 * time.Second}} {
			if before[k] == fd {
				closed = append(closed, k)
			}
		}
		return fd.Close()
	}
	runAll(m, m.show("b"))
	runAll(m, m.show("c"))
	runAll(m, m.show("a"))
	before = maps.Clone(m.feeds)

	f.setConfig(&config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{"http://a.invalid"}, Interval: 5},
		"c": {Endpoints: []string{"http://c.invalid"}, Interval: 2},
	}})
	reloadConfig(t, m)
	slices.SortFunc(closed, func(x, y feedKey) int { return strings.Compare(x.server, y.server) })
	if want := []feedKey{{"b", 5 * time.Second}, {"c", 5 * time.Second}}; !slices.Equal(closed, want) {
		t.Fatalf("closed %v, want %v", closed, want)
	}
	if _, ok := m.feeds[feedKey{"a", 5 * time.Second}]; !ok || len(m.feeds) != 1 {
		t.Fatalf("feeds kept: %v, want a's only", slices.Collect(maps.Keys(m.feeds)))
	}
}

// otherConfigCopy is the TUI's notice of a daemon collecting with another
// configuration (R31).
const otherConfigCopy = "The vartop daemon is collecting with another configuration — press r to use this one."

// TestOtherConfigDaemon (R31): a daemon collecting with another
// configuration — its config file or KUBECONFIG not this vartop's — collects
// servers of that one: the TUI says so, shows no current value, and refuses
// every action with the notice, nothing sent and nothing poked. r starts
// this vartop's daemon, which replaces it.
func TestOtherConfigDaemon(t *testing.T) {
	pod := newFakePod(t)
	f := newFixture(t, multiConfig(pod.url()))
	f.claimAs(store.Lease{PID: os.Getpid(), Build: "test", ConfigPath: "/b/vartop/config.json"})
	f.write("multi", fresh(unit("instance-1", pod.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})))
	f.setEnvCheck(func(l store.Lease) bool { return l.ConfigPath == "/a/vartop/config.json" })
	m := f.model("multi")

	view := m.View()
	if !strings.Contains(view, otherConfigCopy) {
		t.Errorf("dashboard lacks the notice %q:\n%s", otherConfigCopy, view)
	}
	if got, want := rowsOf(m), []string{"orders|instance-1|—|—|—|—|—"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v: no current value", got, want)
	}

	refused := func(where string, keys ...tea.KeyMsg) {
		t.Helper()
		for _, action := range []rune{'p', 'r', 's', 'R'} { // pause, resume, stop, restart
			m.err = nil
			for _, k := range keys {
				press(m, k)
			}
			press(m, runeKey('a'))
			cmd := press(m, runeKey(action))
			if cmd == nil {
				t.Fatalf("%s: %c returned no command", where, action)
			}
			deliver(m, cmd())
			if m.err == nil || m.err.Error() != otherConfigCopy {
				t.Errorf("%s: %c: error = %v, want %q", where, action, m.err, otherConfigCopy)
			}
		}
	}
	refused("dashboard")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.isDetailActive() {
		t.Fatal("enter did not open the detail screen")
	}
	view = m.View()
	if !strings.Contains(view, otherConfigCopy) || strings.Contains(view, "Running") || strings.Contains(view, "7 workers") {
		t.Errorf("detail: want the notice and no current value:\n%s", view)
	}
	refused("detail")
	if n := pod.total(); n != 0 {
		t.Errorf("varmq got %d requests: %v", n, pod.seen())
	}
	if n := f.pokes("multi"); n != 0 {
		t.Errorf("the other configuration's daemon was poked %d times", n)
	}

	// The switcher shows no current health either.
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	openSwitcher(t, m)
	for _, it := range m.switcher.switcher.Items() {
		if it, ok := it.(switchServerItem); ok && !it.stale {
			t.Errorf("switcher shows %s's health: %+v", it.name, it)
		}
	}
	press(m, tea.KeyMsg{Type: tea.KeyEsc})

	// r starts this vartop's daemon, and pokes the other one not.
	m.err = nil
	cmd := press(m, runeKey('r'))
	if cmd == nil {
		t.Fatal("r returned no command")
	}
	deliver(m, cmd())
	if f.started() != 1 || m.err != nil {
		t.Errorf("r: starts = %d, error %v; want this vartop's daemon started", f.started(), m.err)
	}

	// Once the daemon collects in this environment, all is as before.
	f.setEnvCheck(nil)
	syncNow(t, m)
	if view := m.View(); strings.Contains(view, otherConfigCopy) {
		t.Errorf("the notice stays once the environment is this one's:\n%s", view)
	}
	if got, want := rowsOf(m), []string{"orders|instance-1|Running|4|1|0|3"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// TestOtherConfigWaiting (R31): a server with no snapshot under a daemon of
// another configuration waits with the notice, not "not running". Its
// lease let go is still that configuration's (R44): what the store holds
// came from its daemon.
func TestOtherConfigWaiting(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid"))
	f.claimAs(store.Lease{PID: os.Getpid(), Build: "test", ConfigPath: "/b/vartop/config.json"})
	f.setEnvCheck(func(store.Lease) bool { return false })
	m := f.model("multi")
	view := m.View()
	for _, want := range []string{"Waiting for the first snapshot of multi…", otherConfigCopy} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if cmd := m.actCmd("orders", "instance-1", "pause"); cmd != nil {
		if deliver(m, cmd()); m.err == nil || m.err.Error() != otherConfigCopy {
			t.Errorf("action error = %v, want %q", m.err, otherConfigCopy)
		}
	}
	if n := f.pokes("multi"); n != 0 {
		t.Errorf("pokes = %d, want none", n)
	}

	f.release()
	checks := f.envChecked()
	m.err = nil // the refusal the action left on the error line
	syncNow(t, m)
	view = m.View()
	if !strings.Contains(view, otherConfigCopy) {
		t.Errorf("after the lease is let go: want the notice still:\n%s", view)
	}
	if f.envChecked() == checks {
		t.Error("a lease let go was not checked for its environment")
	}
}

// handOffUnits is a snapshot taken at at of a unit at url, and of one at
// demo-host that answered nothing, behind a balancer: what the header says
// of them is the configuration's that collected it.
func handOffUnits(at time.Time, url string) *fleet.Snapshot {
	s := snapAt(at, unit("instance-1", url, map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		downUnit("instance-2", "http://demo-host:8080", "dial tcp demo-host:8080: connection refused"))
	s.Behind = map[string]int{"http://lb.invalid:8080": 2}
	return s
}

// TestOtherConfigHandOff (R44): while the daemons change hands — the other
// configuration's stopped (r stops it, or `vartop daemon stop`), and this
// vartop's not claimed yet, or claimed but not written yet — the newest
// snapshot is still the other one's, and fresh. It is not current: no
// value, none of its units' errors, balancers or addresses, every action
// refused, nothing sent. Once this vartop's daemon writes, all is as ever.
func TestOtherConfigHandOff(t *testing.T) {
	pod := newFakePod(t)
	f := newFixture(t, multiConfig(pod.url()))
	f.setEnvCheck(func(l store.Lease) bool { return l.ConfigPath == "/a/vartop/config.json" })
	f.claimAs(store.Lease{PID: os.Getpid(), Build: "test", ConfigPath: "/b/vartop/config.json"})
	f.write("multi", handOffUnits(time.Now().Add(-time.Second), pod.url()))
	f.release()
	m := f.model("multi")

	theirs := []string{"connection refused", "demo-host", "behind lb.invalid", "(" + pod.url() + ")"}
	notCurrent := func(what string) {
		t.Helper()
		if got, want := rowsOf(m), []string{"orders|instance-1|—|—|—|—|—"}; !slices.Equal(got, want) {
			t.Errorf("%s: rows = %v, want %v", what, got, want)
		}
		views := []string{m.View()}
		press(m, tea.KeyMsg{Type: tea.KeyEnter})
		if !m.isDetailActive() {
			t.Fatalf("%s: enter did not open the detail screen", what)
		}
		views = append(views, m.View())
		press(m, tea.KeyMsg{Type: tea.KeyEsc})
		for _, view := range views {
			for _, s := range theirs {
				if strings.Contains(view, s) {
					t.Errorf("%s: the view shows %q of the units of the snapshot:\n%s", what, s, view)
				}
			}
		}
	}
	act := func() error {
		m.err = nil
		deliver(m, m.actCmd("orders", "instance-1", "pause")())
		return m.err
	}

	if view := m.View(); !strings.Contains(view, otherConfigCopy) {
		t.Errorf("the other configuration's daemon stopped, its snapshot fresh: view lacks %q:\n%s", otherConfigCopy, view)
	}
	notCurrent("released")
	if err := act(); err == nil || err.Error() != otherConfigCopy {
		t.Errorf("action = %v, want %q", err, otherConfigCopy)
	}
	if n := pod.total(); n != 0 || f.pokes("multi") != 0 {
		t.Errorf("varmq got %v, %d pokes; want nothing sent, nothing poked", pod.seen(), f.pokes("multi"))
	}

	// This vartop's daemon claims: the newest snapshot, older than the
	// claim, is still the other's.
	f.claimAs(store.Lease{PID: os.Getpid(), Build: "test", ConfigPath: "/a/vartop/config.json"})
	m.err = nil // the refusal the action left on the error line
	syncNow(t, m)
	if view := m.View(); strings.Contains(view, otherConfigCopy) || !regexp.MustCompile(`No new data for \d+s\.`).MatchString(view) {
		t.Errorf("claimed, not written yet: want the stale notice:\n%s", view)
	}
	notCurrent("claimed, not written yet")
	if err := act(); err == nil || !strings.HasPrefix(err.Error(), "No new data for ") {
		t.Errorf("action before this daemon's first snapshot = %v, want the stale notice", err)
	}
	if n := pod.total(); n != 0 || f.pokes("multi") != 1 {
		t.Errorf("varmq got %v, %d pokes; want nothing sent, this vartop's daemon poked once", pod.seen(), f.pokes("multi"))
	}

	// Its first snapshot: current, and acted on.
	f.write("multi", handOffUnits(time.Now(), pod.url()))
	syncNow(t, m)
	if got, want := rowsOf(m), []string{"orders|instance-1|Running|4|1|0|3"}; !slices.Equal(got, want) {
		t.Errorf("this daemon's first snapshot: rows = %v, want %v", got, want)
	}
	if view := m.View(); !strings.Contains(view, "instance-2: dial tcp demo-host:8080: connection refused") || !strings.Contains(view, "2 instances behind lb.invalid:8080") {
		t.Errorf("this daemon's first snapshot: the header lacks its units' errors and balancers:\n%s", view)
	}
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if view := m.View(); !strings.Contains(view, "("+pod.url()+")") {
		t.Errorf("this daemon's first snapshot: the detail header lacks the unit's address:\n%s", view)
	}
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if err := act(); err != nil {
		t.Errorf("action after this daemon's first snapshot = %v, want sent", err)
	}
	if n := pod.count("PATCH /workers/orders/actions/pause"); n != 1 {
		t.Errorf("pause sent %d times, want 1", n)
	}
}

// TestActionRefusedWhenLeaseUnreadable (R44): with the daemon's lease
// unreadable, whose units the newest snapshot names is not known: an
// action is refused, nothing sent. The rows keep showing the snapshot, by
// the stale rule.
func TestActionRefusedWhenLeaseUnreadable(t *testing.T) {
	pod := newFakePod(t)
	f := newFixture(t, multiConfig(pod.url()))
	f.claim()
	f.write("multi", fresh(unit("instance-1", pod.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})))
	m := f.model("multi")
	if err := f.ctl.Close(); err != nil { // control.db, as the TUI reads it, fails
		t.Fatal(err)
	}
	deliver(m, m.actCmd("orders", "instance-1", "pause")())
	if m.err == nil || !strings.HasPrefix(m.err.Error(), "cannot read the vartop daemon's lease: ") {
		t.Errorf("action = %v, want refused: the lease cannot be read", m.err)
	}
	if n := pod.total(); n != 0 {
		t.Errorf("varmq got %v, want nothing", pod.seen())
	}
	syncNow(t, m)
	if got, want := rowsOf(m), []string{"orders|instance-1|Running|4|1|0|3"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v: shown by the stale rule", got, want)
	}
}

// TestStartErrorShown: what making sure the daemon runs said as the TUI
// opened (Daemon.StartErr) is on its error line from the start — printed,
// it would be hidden behind the TUI's screen — until r tries again.
func TestStartErrorShown(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid"))
	cfg, _ := f.config()
	startErr := errors.New("vartop daemon did not start; see /data/daemon.log")
	m := NewModel(cfg, "multi", f.dir, f.ctl, Daemon{Start: f.start, StartErr: startErr})
	t.Cleanup(func() { _ = m.Close() })
	deliver(&m, tea.WindowSizeMsg{Width: 120, Height: 40})
	syncNow(t, &m)
	if view := m.View(); !strings.Contains(view, startErr.Error()) {
		t.Errorf("the start error is not shown:\n%s", view)
	}
	deliver(&m, press(&m, runeKey('r'))())
	if view := m.View(); strings.Contains(view, startErr.Error()) {
		t.Errorf("the start error stays after r started the daemon:\n%s", view)
	}
}

// TestTickSyncsOnceAtATime (D6): a tick starts no sync of the feed shown
// while one of its generation is under way — a build of the trends slower
// than the tick would be started again, for the same snapshot, on every
// tick — and starts one again once it has ended.
func TestTickSyncsOnceAtATime(t *testing.T) {
	end := time.Now()
	f := trendFixture(t, end)
	m := f.model("multi")
	m.tick = func() tea.Cmd { return nil } // no timer: the test delivers the ticks
	built := countTrends(m)
	write := func(at time.Time) {
		at = at.Round(time.Millisecond)
		f.write("multi", snapAt(at, rated(at, "instance-1", "http://a.invalid", 40, 7), rated(at, "instance-2", "http://b.invalid", 5, 3)))
	}

	write(end.Add(15 * time.Second))
	first := deliver(m, tickMsg(time.Now()))
	second := deliver(m, tickMsg(time.Now())) // the first tick's sync has not ended
	runAll(m, first)
	runAll(m, second)
	if n := built.Load(); n != 1 {
		t.Errorf("trends built %d times for one new snapshot, want 1", n)
	}
	if m.dashboardScreen().trends.key.latest != m.latest() {
		t.Error("the trends of the newest snapshot were not taken in")
	}

	write(end.Add(30 * time.Second))
	runAll(m, deliver(m, tickMsg(time.Now())))
	if n := built.Load(); n != 2 {
		t.Errorf("trends built %d times, want 2: a tick after the sync ended syncs again", n)
	}
}

// TestActionShowsItsEffect (D7): after an action the TUI waits, as vartop
// web does, for the snapshot its poke has the daemon take — up to
// fleet.SettleWait — and shows it at once, rather than syncing once a moment
// later, maybe before the daemon answered. With no snapshot in time it shows
// what it has; with no daemon to answer it does not wait, and refuses
// (R44: an action needs one).
func TestActionShowsItsEffect(t *testing.T) {
	pod := newFakePod(t)
	f := newFixture(t, multiConfig(pod.url()))
	f.claim()
	f.write("multi", fresh(unit("instance-1", pod.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})))
	m := f.model("multi")
	m.settleWait = 10 * time.Second

	cmd := m.actCmd("orders", "instance-1", "pause")
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	fleettest.WaitUntil(t, 5*time.Second, "the poke", func() bool { return f.pokes("multi") == 1 })
	// The daemon answers the poke: a snapshot after it, the worker paused.
	f.write("multi", snapAt(time.Now().Add(time.Millisecond), unit("instance-1", pod.url(), map[string]*fleet.Sample{"orders": smp("Paused", 4, 0, 4, 0)})))
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the action never ended")
	}
	deliver(m, msg)
	if m.err != nil {
		t.Fatalf("error after a good action: %v", m.err)
	}
	if got, want := rowsOf(m), []string{"orders|instance-1|Paused|4|0|0|4"}; !slices.Equal(got, want) {
		t.Errorf("rows right after the action = %v, want %v", got, want)
	}

	// No snapshot comes: the action ends once the wait is over, unrefused.
	m.settleWait = 300 * time.Millisecond
	began := time.Now()
	deliver(m, m.actCmd("orders", "instance-1", "resume")())
	if took := time.Since(began); took < m.settleWait || m.err != nil {
		t.Errorf("with no snapshot coming: took %v, error %v; want the wait of %v, no error", took, m.err, m.settleWait)
	}

	// No daemon runs: an action needs one (R44), and none answers a poke —
	// refused at once, nothing sent.
	f.release()
	m.settleWait = time.Minute
	began = time.Now()
	deliver(m, m.actCmd("orders", "instance-1", "stop")())
	if took := time.Since(began); took > 30*time.Second || m.err == nil || !strings.HasSuffix(m.err.Error(), " — the vartop daemon is not running.") {
		t.Errorf("with no daemon: took %v, error %v; want refused at once, the daemon not running", took, m.err)
	}
	if n := pod.count("PATCH /workers/orders/actions/stop"); n != 0 {
		t.Errorf("stop sent %d times, want none", n)
	}
}

// twoServers is a store of servers a and b, each one unit running orders,
// polled every 5s.
func twoServers(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{"http://a.invalid"}, Interval: 5},
		"b": {Endpoints: []string{"http://b.invalid"}, Interval: 5},
	}})
}

// countCloses makes m close feeds through a counter of each, and returns it.
func countCloses(m *Model) func(*store.Feed) int {
	var mu sync.Mutex
	closed := map[*store.Feed]int{}
	m.closeFeed = func(fd *store.Feed) error {
		mu.Lock()
		closed[fd]++
		mu.Unlock()
		return fd.Close()
	}
	return func(fd *store.Feed) int {
		mu.Lock()
		defer mu.Unlock()
		return closed[fd]
	}
}

// TestHiddenFeedReloaded (D4): a server shown again whose feed, kept while
// hidden — not synced meanwhile — holds a stale newest snapshot is read
// afresh: the hours-old snapshot is not shown as stale while the daemon has
// been collecting all along, and the old feed is closed.
func TestHiddenFeedReloaded(t *testing.T) {
	f := twoServers(t)
	f.claim()
	orders := func(status string) map[string]*fleet.Sample {
		return map[string]*fleet.Sample{"orders": smp(status, 4, 1, 3, 0)}
	}
	f.write("a", fresh(unit("instance-1", "http://a.invalid", orders("Running"))))
	f.write("b", snapAt(time.Now().Add(-time.Minute), unit("instance-1", "http://b.invalid", orders("Running"))))
	m := f.model("b")
	closes := countCloses(m)
	old := m.feed
	runAll(m, m.show("a")) // b is hidden, its feed kept

	// The daemon collects b meanwhile.
	f.write("b", fresh(unit("instance-1", "http://b.invalid", orders("Paused"))))
	cmd := m.show("b")
	if m.feed == old {
		t.Error("b's stale feed was shown again")
	}
	if view := m.View(); strings.Contains(view, "No new data for") {
		t.Errorf("b shows its old snapshot as stale:\n%s", view)
	}
	runAll(m, cmd)
	if got, want := rowsOf(m), []string{"orders|instance-1|Paused|4|1|0|3"}; !slices.Equal(got, want) {
		t.Errorf("b's rows = %v, want %v", got, want)
	}
	if closes(old) != 1 {
		t.Errorf("b's old feed closed %d times, want once", closes(old))
	}

	// A kept feed that is not stale is shown as it is: switching is instant.
	held := m.feed
	runAll(m, m.show("a"))
	runAll(m, m.show("b"))
	if m.feed != held || closes(held) != 0 {
		t.Error("a fresh kept feed was not kept")
	}
}

// TestHiddenFeedClosed (D4): the feed of a server hidden for 10 minutes is
// closed and forgotten, as vartop web's unused feeds are: its file is let go
// (Windows: the daemon could not remove it). The one shown stays, however
// long.
func TestHiddenFeedClosed(t *testing.T) {
	f := twoServers(t)
	m := f.model("b")
	m.tick = func() tea.Cmd { return nil }
	closes := countCloses(m)
	b := m.feed
	runAll(m, m.show("a"))
	a := m.feed

	runAll(m, deliver(m, tickMsg(time.Now().Add(hiddenFeedTTL-time.Minute))))
	if closes(b) != 0 || len(m.feeds) != 2 {
		t.Fatalf("hidden for 9 minutes: closed %d, %d feeds kept; want it kept", closes(b), len(m.feeds))
	}
	runAll(m, deliver(m, tickMsg(time.Now().Add(hiddenFeedTTL+time.Second))))
	if closes(b) != 1 {
		t.Errorf("b's feed closed %d times after 10 minutes hidden, want once", closes(b))
	}
	if _, kept := m.feeds[feedKey{"b", 5 * time.Second}]; kept {
		t.Error("b's feed is still kept")
	}
	if closes(a) != 0 || m.feeds[feedKey{"a", 5 * time.Second}] != a {
		t.Error("the feed shown was closed")
	}

	// Shown again, b has a feed anew.
	runAll(m, m.show("b"))
	if m.feed == nil || m.feed == b {
		t.Error("b shown again has no new feed")
	}
}
