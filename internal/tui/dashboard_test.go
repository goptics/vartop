package tui

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// multiConfig is one server, "multi", over the given endpoints, polled every
// 5s with token "tok".
func multiConfig(endpoints ...string) *config.Config {
	return &config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"multi": {Endpoints: endpoints, Interval: 5, Token: "tok"},
	}}
}

// TestRowsFromFeed: the dashboard lists one row per unit × worker of the
// newest stored snapshot — units in its order, workers by name, the Node
// cell the unit's label — leaves errored units to the header, shows a
// worker its unit did not answer for as not reached, and filters by label.
func TestRowsFromFeed(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid", "http://b.invalid", "http://c.invalid"))
	f.claim()
	older := snapAt(time.Now().Add(-5*time.Second), unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{
		"orders": smp("Paused", 1, 0, 1, 0),
	}))
	f.write("multi", older)
	latest := fresh(
		unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{
			"orders": smp("Running", 4, 1, 3, 7),
			"emails": smp("Paused", 2, 0, 2, 0),
		}),
		downUnit("instance-2", "http://b.invalid", "connection refused"),
		fleet.PodSnapshot{Key: "http://c.invalid", Via: "http://c.invalid", Label: "instance-3",
			Workers: map[string]*fleet.Sample{"orders": smp("Running", 4, 2, 2, 1)}, Unreached: []string{"emails"}},
	)
	latest.Behind = map[string]int{"http://lb.invalid:8080": 2}
	f.write("multi", latest)

	cfg, _ := f.config()
	m := NewModel(cfg, "multi", f.dir, f.ctl, f.start)
	t.Cleanup(func() { _ = m.Close() })
	if m.Init() == nil {
		t.Fatal("Init returned no command")
	}
	if rows := rowsOf(&m); len(rows) != 0 {
		t.Fatalf("rows before the first sync: %v", rows)
	}
	deliver(&m, tea.WindowSizeMsg{Width: 160, Height: 40})
	syncNow(t, &m)

	want := []string{
		"emails|instance-1|Paused|2|0|0|2",
		"orders|instance-1|Running|4|1|7|3",
		"emails|instance-3|not reached|—|—|—|—",
		"orders|instance-3|Running|4|2|1|2",
	}
	if got := rowsOf(&m); !slices.Equal(got, want) {
		t.Fatalf("rows =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	view := m.View()
	for _, s := range []string{
		"vartop [multi]", "2/3 instances", "2 instances behind lb.invalid:8080",
		"updated " + fleet.PointTime(latest), "instance-2: connection refused",
	} {
		if !strings.Contains(view, s) {
			t.Errorf("view lacks %q:\n%s", s, view)
		}
	}
	if strings.Contains(view, "No new data") {
		t.Errorf("a fresh snapshot is called stale:\n%s", view)
	}

	// One unit: its rows only, with no Node column.
	m.selectedLabel, m.showNodeColumn = "instance-3", false
	m.dashboardScreen().rebuildTable(&m)
	syncNow(t, &m)
	want = []string{"emails|not reached|—|—|—|—", "orders|Running|4|2|1|2"}
	if got := rowsOf(&m); !slices.Equal(got, want) {
		t.Fatalf("instance-3's rows = %v, want %v", got, want)
	}
	if view := m.View(); !strings.Contains(view, "→ instance-3") {
		t.Errorf("the header does not name the unit shown:\n%s", view)
	}
}

// TestStaleHeader: a newest snapshot older than 2·interval + 2s makes the
// header say how old, and why — the daemon is not running, the server's
// collection error, or neither known — the way the web pages do, and the
// current values turn to —.
func TestStaleHeader(t *testing.T) {
	cfg := multiConfig("http://a.invalid")
	cfg.Servers["multi"].Interval = 1
	f := newFixture(t, cfg)
	f.claim()
	old := snapAt(time.Now().Add(-30*time.Second), unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{
		"orders": smp("Running", 4, 1, 3, 7),
	}))
	f.write("multi", old)
	m := f.model("multi")

	age := `No new data for \d+s`
	check := func(what, suffix string) {
		t.Helper()
		syncNow(t, m)
		view := m.View()
		if !regexp.MustCompile(age + regexp.QuoteMeta(suffix)).MatchString(view) {
			t.Errorf("%s: view lacks %q:\n%s", what, "No new data for {age}"+suffix, view)
		}
		if !strings.Contains(view, "updated "+fleet.PointTime(old)) {
			t.Errorf("%s: view lacks the stale snapshot's time:\n%s", what, view)
		}
		if got, want := rowsOf(m), []string{"orders|instance-1|—|—|—|—|—"}; !slices.Equal(got, want) {
			t.Errorf("%s: rows = %v, want %v", what, got, want)
		}
	}
	check("daemon running, no error", ".")
	if err := f.ctl.SetServerErr("multi", "discovery failed: boom"); err != nil {
		t.Fatal(err)
	}
	check("server error", " — multi: discovery failed: boom")
	f.release()
	check("daemon down", " — the vartop daemon is not running.")

	// The detail screen shows the stale values as — too.
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.isDetailActive() {
		t.Fatal("enter did not open the detail screen")
	}
	if view := m.View(); strings.Contains(view, "4 workers") || !strings.Contains(view, "—") {
		t.Errorf("the detail screen shows stale values:\n%s", view)
	}
	for _, width := range []int{120, 60} { // wide and compact header
		deliver(m, tea.WindowSizeMsg{Width: width, Height: 40})
		if view := m.View(); !regexp.MustCompile(`No new data for \d+s`).MatchString(view) {
			t.Errorf("the %d-column detail screen does not say it is stale:\n%s", width, view)
		}
	}
	deliver(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	press(m, tea.KeyMsg{Type: tea.KeyEsc})

	f.claim()
	f.write("multi", fresh(unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 7)})))
	syncNow(t, m)
	if view := m.View(); strings.Contains(view, "No new data") {
		t.Errorf("a fresh snapshot is called stale:\n%s", view)
	}
	if got, want := rowsOf(m), []string{"orders|instance-1|Running|4|1|7|3"}; !slices.Equal(got, want) {
		t.Errorf("fresh rows = %v, want %v", got, want)
	}
}

// TestBalancedActionRefused: an action on a unit behind a load balancer is
// refused with fleet.OneBalancedMsg, from the dashboard and the detail
// screen alike, and nothing is sent or poked.
func TestBalancedActionRefused(t *testing.T) {
	lb := newFakePod(t)
	f := newFixture(t, multiConfig(lb.url()))
	f.claim()
	s := fresh(
		fleet.PodSnapshot{Key: "#a", Via: lb.url(), Instance: "a", Label: "instance-1", Balanced: true,
			Workers: map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}},
		fleet.PodSnapshot{Key: "#b", Via: lb.url(), Instance: "b", Label: "instance-2", Balanced: true,
			Workers: map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}},
	)
	s.Behind = map[string]int{lb.url(): 2}
	f.write("multi", s)
	m := f.model("multi")

	refused := func(where string) {
		t.Helper()
		press(m, runeKey('a'))
		cmd := press(m, runeKey('p'))
		if cmd == nil {
			t.Fatalf("%s: pause returned no command", where)
		}
		deliver(m, cmd())
		if m.err == nil || m.err.Error() != fleet.OneBalancedMsg {
			t.Fatalf("%s: error = %v, want %q", where, m.err, fleet.OneBalancedMsg)
		}
		if view := m.View(); !strings.Contains(view, "A load balancer picks which instance answers") {
			t.Errorf("%s: the refusal is not shown:\n%s", where, view)
		}
	}
	refused("dashboard")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.isDetailActive() {
		t.Fatal("enter did not open the detail screen")
	}
	m.err = nil
	refused("detail")

	if n := lb.total(); n != 0 {
		t.Errorf("the balancer got %d requests: %v", n, lb.seen())
	}
	if n := f.pokes("multi"); n != 0 {
		t.Errorf("pokes = %d, want 0: nothing was sent", n)
	}
}

// TestDirectActionPokes: an action on a direct unit is sent to that unit
// alone, with the server's token; the server is poked, and synced again
// shortly after.
func TestDirectActionPokes(t *testing.T) {
	pod := newFakePod(t)
	other := newFakePod(t)
	f := newFixture(t, multiConfig(pod.url(), other.url()))
	f.claim()
	f.write("multi", fresh(
		unit("instance-1", pod.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		unit("instance-2", other.url(), map[string]*fleet.Sample{"emails": smp("Running", 4, 1, 3, 0)}),
	))
	m := f.model("multi")

	press(m, runeKey('a'))
	if !m.dashboardScreen().inActionSelection {
		t.Fatal("a did not enter action mode")
	}
	cmd := press(m, runeKey('p'))
	if m.dashboardScreen().inActionSelection {
		t.Fatal("the action key did not leave action mode")
	}
	if cmd == nil {
		t.Fatal("pause returned no command")
	}
	msg := cmd()
	const want = "PATCH /workers/orders/actions/pause"
	if pod.count(want) != 1 || other.total() != 0 {
		t.Fatalf("pause routed wrong: pod=%v other=%v", pod.seen(), other.seen())
	}
	if got := pod.auths(); len(got) != 1 || got[0] != "Bearer tok" {
		t.Fatalf("Authorization = %q, want the server's token", got)
	}
	if n := f.pokes("multi"); n != 1 {
		t.Fatalf("pokes = %d, want 1", n)
	}
	if deliver(m, msg) == nil {
		t.Error("no sync scheduled after the action")
	}
	if m.err != nil {
		t.Errorf("error after a good action: %v", m.err)
	}

	// A refused action is shown, and still pokes: varmq got the request.
	pod.rejectActions()
	press(m, runeKey('a'))
	deliver(m, press(m, runeKey('s'))())
	if m.err == nil || !strings.Contains(m.err.Error(), "action rejected") {
		t.Fatalf("error = %v, want varmq's refusal", m.err)
	}
	if view := m.View(); !strings.Contains(view, "action rejected") {
		t.Errorf("the refusal is not shown:\n%s", view)
	}
	if n := f.pokes("multi"); n != 2 {
		t.Errorf("pokes = %d, want 2", n)
	}
}

// TestDashboardActionRoutesToRowPod drives the dashboard's own action keys
// (a, then p) on the second row of a merged view where both units run the
// same worker: the PATCH must reach only that row's unit.
func TestDashboardActionRoutesToRowPod(t *testing.T) {
	pod1 := newFakePod(t)
	pod2 := newFakePod(t)
	f := newFixture(t, multiConfig(pod1.url(), pod2.url()))
	f.claim()
	f.write("multi", fresh(
		unit("instance-1", pod1.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		unit("instance-2", pod2.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
	))
	m := f.model("multi")
	m.dashboardScreen().table.SetCursor(1)

	press(m, runeKey('a'))
	if !m.dashboardScreen().inActionSelection {
		t.Fatal("a did not enter action mode")
	}
	cmd := press(m, runeKey('p'))
	if cmd == nil {
		t.Fatal("pause returned no command")
	}
	cmd()

	want := "PATCH /workers/orders/actions/pause"
	if pod2.count(want) != 1 || pod1.count(want) != 0 {
		t.Fatalf("pause routed wrong: pod1=%v pod2=%v", pod1.seen(), pod2.seen())
	}
}

// TestDashboardActionModeNeedsRows: with no workers listed there is nothing to
// act on, so a must not enter action mode.
func TestDashboardActionModeNeedsRows(t *testing.T) {
	m := testModel(t)
	press(m, runeKey('a'))
	if m.dashboardScreen().inActionSelection {
		t.Fatal("entered action mode with an empty table")
	}
}

// TestRefreshStartsStoppedDaemon: r pokes the server, and starts the daemon
// when none runs; a start that fails says why on the error line.
func TestRefreshStartsStoppedDaemon(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid"))
	m := f.model("multi")

	refresh := func() {
		t.Helper()
		cmd := press(m, runeKey('r'))
		if cmd == nil {
			t.Fatal("r returned no command")
		}
		deliver(m, cmd())
	}
	refresh()
	if f.started() != 1 || f.pokes("multi") != 1 {
		t.Fatalf("with no daemon: starts = %d, pokes = %d; want 1, 1", f.started(), f.pokes("multi"))
	}
	if m.err != nil {
		t.Errorf("error after a good start: %v", m.err)
	}

	errStart := errors.New("vartop daemon did not start; see /data/daemon.log")
	f.failStart(errStart)
	refresh()
	if !errors.Is(m.err, errStart) {
		t.Fatalf("error = %v, want %v", m.err, errStart)
	}
	if view := m.View(); !strings.Contains(view, errStart.Error()) {
		t.Errorf("the start error is not shown:\n%s", view)
	}

	f.claim() // a daemon runs
	refresh()
	if f.started() != 2 || f.pokes("multi") != 3 {
		t.Fatalf("with a daemon: starts = %d, pokes = %d; want 2, 3", f.started(), f.pokes("multi"))
	}
	if m.err != nil {
		t.Errorf("r kept the old error: %v", m.err)
	}
}

// TestWaitingForFirstSnapshot: a server with nothing stored yet says so,
// and says when no daemon runs to collect it.
func TestWaitingForFirstSnapshot(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid"))
	m := f.model("multi")
	view := m.View()
	for _, want := range []string{"Waiting for the first snapshot of multi…", "the vartop daemon is not running"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}

	m.pushScreen(newDetailScreen("orders", "instance-1")) // left open from before
	view = m.View()
	for _, want := range []string{"Waiting for the first snapshot of multi…", "the vartop daemon is not running"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail view lacks %q:\n%s", want, view)
		}
	}
	m.popScreen()

	f.claim()
	if err := f.ctl.SetServerErr("multi", "connecting…"); err != nil {
		t.Fatal(err)
	}
	syncNow(t, m)
	view = m.View()
	if !strings.Contains(view, "multi: connecting…") || strings.Contains(view, "not running") {
		t.Errorf("view with a daemon connecting:\n%s", view)
	}
}

// TestStaleActionSettles: an action on a server whose newest snapshot is
// stale pokes it and waits for a newer snapshot — unless no daemon runs to
// take one — and is refused, nothing sent, when none comes in time; a
// label the newest snapshot lacks is refused too.
func TestStaleActionSettles(t *testing.T) {
	for _, c := range []struct {
		name   string
		daemon bool // a daemon holds the lease
		fresh  bool // the test writes a fresh snapshot once poked
		label  string
		want   string // the refusal; "" = sent
		pokes  int64
	}{
		{name: "no daemon", want: `No new data for \d+m\d+s — the vartop daemon is not running\.`},
		{name: "nothing new", daemon: true, want: `No new data for \d+m\d+s\.`, pokes: 1},
		{name: "fresh in time", daemon: true, fresh: true, pokes: 2},
		{name: "gone label", daemon: true, label: "instance-9", want: `Instance instance-9 is no longer reported`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pod := newFakePod(t)
			f := newFixture(t, multiConfig(pod.url()))
			f.claim()
			orders := func() map[string]*fleet.Sample { return map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)} }
			at := time.Now().Add(-time.Minute)
			if c.label != "" {
				at = time.Now() // fresh: only the label is wrong
			}
			f.write("multi", snapAt(at, unit("instance-1", pod.url(), orders())))
			if !c.daemon {
				f.release()
			}
			m := f.model("multi")
			m.settleWait = 300 * time.Millisecond
			if c.fresh {
				m.settleWait = 10 * time.Second
			}

			label := "instance-1"
			if c.label != "" {
				label = c.label
			}
			cmd := m.actCmd("orders", label, "pause")
			if cmd == nil {
				t.Fatal("no action command")
			}
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			if c.fresh {
				fleettest.WaitUntil(t, 5*time.Second, "the settle poke", func() bool { return f.pokes("multi") == 1 })
				f.write("multi", fresh(unit("instance-1", pod.url(), orders())))
			}
			var msg tea.Msg
			select {
			case msg = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("the action never ended")
			}
			deliver(m, msg)

			sent := pod.count("PATCH /workers/orders/actions/pause")
			switch {
			case c.want == "" && (m.err != nil || sent != 1):
				t.Errorf("error %v, %d PATCH sent; want it sent once", m.err, sent)
			case c.want != "" && (m.err == nil || !regexp.MustCompile("^"+c.want+"$").MatchString(m.err.Error())):
				t.Errorf("error = %v, want %s", m.err, c.want)
			case c.want != "" && pod.total() != 0:
				t.Errorf("refused, yet varmq got %v", pod.seen())
			}
			if n := f.pokes("multi"); n != c.pokes {
				t.Errorf("pokes = %d, want %d", n, c.pokes)
			}
		})
	}
}

// TestSelectionFollowsRow: the highlighted row stays on its unit and worker
// when rows before it come and go between syncs; when that row is gone the
// action menu closes, so the next key sends nothing to another unit.
func TestSelectionFollowsRow(t *testing.T) {
	pod1, pod2 := newFakePod(t), newFakePod(t)
	f := newFixture(t, multiConfig(pod1.url(), pod2.url()))
	f.claim()
	one := unit("instance-1", pod1.url(), map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})
	two := unit("instance-2", pod2.url(), map[string]*fleet.Sample{
		"emails": smp("Running", 2, 0, 2, 0),
		"orders": smp("Running", 4, 1, 3, 0),
	})
	f.write("multi", fresh(one, two))
	m := f.model("multi")
	d := m.dashboardScreen()
	d.table.SetCursor(1)
	if d.selectedLabel() != "instance-2" || d.selectedWorkerName() != "emails" {
		t.Fatalf("precondition: selected %s/%s", d.selectedLabel(), d.selectedWorkerName())
	}

	// instance-1 goes down: its row leaves, the selection stays on emails.
	f.write("multi", fresh(downUnit("instance-1", pod1.url(), "connection refused"), two))
	syncNow(t, m)
	if d.selectedLabel() != "instance-2" || d.selectedWorkerName() != "emails" {
		t.Fatalf("after instance-1 left: selected %s/%s, want instance-2/emails", d.selectedLabel(), d.selectedWorkerName())
	}
	// It comes back: the selection still follows.
	f.write("multi", fresh(one, two))
	syncNow(t, m)
	if d.selectedLabel() != "instance-2" || d.selectedWorkerName() != "emails" {
		t.Fatalf("after instance-1 came back: selected %s/%s, want instance-2/emails", d.selectedLabel(), d.selectedWorkerName())
	}

	// The selected row goes while the action menu is open: it closes.
	press(m, runeKey('a'))
	if !d.inActionSelection {
		t.Fatal("a did not open the action menu")
	}
	f.write("multi", fresh(one))
	syncNow(t, m)
	if d.inActionSelection {
		t.Fatal("the action menu stayed open on a row that is gone")
	}
	if cmd := press(m, runeKey('p')); cmd != nil {
		if msg := cmd(); msg != nil {
			deliver(m, msg)
		}
	}
	if pod1.total() != 0 || pod2.total() != 0 {
		t.Fatalf("a key after the row left sent: pod1=%v pod2=%v", pod1.seen(), pod2.seen())
	}
	if got := rowsOf(m); len(got) != 1 || d.table.Cursor() != 0 {
		t.Fatalf("rows %v, cursor %d; want the one row, selected", got, d.table.Cursor())
	}
}
