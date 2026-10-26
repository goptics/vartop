package tui

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
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

// rated is a healthy unit labelled label, at key, whose worker "orders"
// completes rate a second and has pending waiting, as of at.
func rated(at time.Time, label, key string, rate float64, pending int) fleet.PodSnapshot {
	s := smp("Running", 4, 1, 3, pending)
	s.At, s.RateOK, s.CompletedRate = at, true, rate
	return unit(label, key, map[string]*fleet.Sample{"orders": s})
}

// trendFixture stores five minutes of history on the server "multi": 21
// snapshots 15s apart, the newest at end, of two units that run "orders".
// instance-1 completes j a second in the j-th (0 to 20) with 7 pending;
// instance-2 a steady 5, with 3 pending.
func trendFixture(t *testing.T, end time.Time) *fixture {
	t.Helper()
	f := newFixture(t, multiConfig("http://a.invalid", "http://b.invalid"))
	f.claim()
	end = end.Round(time.Millisecond)
	for j := range 21 {
		at := end.Add(-time.Duration(20-j) * 15 * time.Second)
		f.write("multi", snapAt(at,
			rated(at, "instance-1", "http://a.invalid", float64(j), 7),
			rated(at, "instance-2", "http://b.invalid", 5, 3)))
	}
	return f
}

// trendCells is the Trend cell of each row of the dashboard, untrimmed.
func trendCells(t *testing.T, m *Model) []string {
	t.Helper()
	d := m.dashboardScreen()
	cols := d.table.Columns()
	if len(cols) == 0 || cols[len(cols)-1].Title != "Trend" {
		t.Fatalf("the table has no Trend column: %v", cols)
	}
	var cells []string
	for _, r := range d.table.Rows() {
		cells = append(cells, r[len(r)-1])
	}
	return cells
}

// ansi matches the escape sequences that colour a view.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// kpiLineOf is the KPI line of m's dashboard view, plain; "" when there is none.
func kpiLineOf(m *Model) string {
	for _, line := range strings.Split(ansi.ReplaceAllString(m.View(), ""), "\n") {
		if strings.Contains(line, "completed/s") {
			return strings.TrimRight(line, " ")
		}
	}
	return ""
}

// TestTrendFromFeed: each row's Trend is the completed/s of its worker on
// its unit over the last 10 minutes of the feed, one column per slice of
// that time; where the feed has no snapshots the column is blank.
func TestTrendFromFeed(t *testing.T) {
	f := trendFixture(t, time.Now())
	m := f.model("multi")
	deliver(m, tea.WindowSizeMsg{Width: 160, Height: 40}) // 20 columns of Trend
	syncNow(t, m)

	// Five minutes of history: the left half of the 10-minute window is blank.
	// instance-1 climbs 0 to 20 a second: pairs of snapshots 30s apart.
	want := []string{
		strings.Repeat(" ", 10) + "▁▂▃▃▄▅▆▆▇█",
		strings.Repeat(" ", 10) + strings.Repeat("█", 10),
	}
	if got := trendCells(t, m); !slices.Equal(got, want) {
		t.Fatalf("Trend cells = %q, want %q", got, want)
	}

	// Only the unit's own series in the view of one unit.
	m.selectedLabel, m.showNodeColumn = "instance-2", false
	m.dashboardScreen().rebuildTable(m)
	syncNow(t, m)
	if got := trendCells(t, m); len(got) != 1 || got[0] != want[1] {
		t.Errorf("instance-2's Trend cells = %q, want %q", got, want[1:])
	}

	// A narrower Trend draws the same time in fewer columns.
	deliver(m, tea.WindowSizeMsg{Width: 92, Height: 40}) // 12 columns
	if got := trendCells(t, m); len(got) != 1 || got[0] != strings.Repeat(" ", 6)+strings.Repeat("█", 6) {
		t.Errorf("instance-2's Trend at 12 columns = %q", got)
	}
}

// TestTrendHasNoRates: a snapshot that has no rate yet (the first one, a
// restarted counter) is a gap, which the Trend leaves blank, and the row is
// no shorter for it.
func TestTrendHasNoRates(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid"))
	f.claim()
	f.write("multi", fresh(unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 7)})))
	m := f.model("multi")
	if got, want := trendCells(t, m), []string{strings.Repeat(" ", 20)}; !slices.Equal(got, want) {
		t.Errorf("Trend cells = %q, want %q", got, want)
	}
	if got, want := rowsOf(m), []string{"orders|instance-1|Running|4|1|7|3"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	// Nor has the KPI line a rate to give: — as on the web pages, not 0.0.
	if line := kpiLineOf(m); !regexp.MustCompile(`^  completed/s {7}— .*  failed/min {7}— .*  pending {7}7 `).MatchString(line) {
		t.Errorf("KPI line = %q, want — for the rates it has none of, and pending 7", line)
	}
}

// TestKPILineOfDownUnit: the view of a unit that answered nothing in the
// newest snapshot has no current values — not zeros — and its history from
// the snapshots it answered is drawn.
func TestKPILineOfDownUnit(t *testing.T) {
	f := newFixture(t, multiConfig("http://a.invalid", "http://b.invalid"))
	f.claim()
	end := time.Now().Round(time.Millisecond)
	for j := range 5 {
		at := end.Add(-time.Duration(5-j) * 15 * time.Second)
		f.write("multi", snapAt(at, rated(at, "instance-1", "http://a.invalid", 3, 1), rated(at, "instance-2", "http://b.invalid", float64(j+1), 2)))
	}
	f.write("multi", snapAt(end, rated(end, "instance-1", "http://a.invalid", 3, 1), downUnit("instance-2", "http://b.invalid", "connection refused")))

	m := f.model("multi")
	m.selectedLabel, m.showNodeColumn = "instance-2", false
	m.dashboardScreen().rebuildTable(m)
	syncNow(t, m)
	if got := rowsOf(m); len(got) != 0 {
		t.Fatalf("rows of a unit that answered nothing: %v", got)
	}
	line := kpiLineOf(m)
	if !regexp.MustCompile(`^  completed/s {7}— [▁-█ ]{12}  failed/min {7}— [▁-█ ]{12}  pending {7}— [▁-█ ]{12}$`).MatchString(line) {
		t.Fatalf("KPI line = %q, want — for each value, then 12 columns each", line)
	}
	if !strings.ContainsAny(line, "▁▂▃▄▅▆▇█") {
		t.Errorf("KPI line = %q, want the history from before the unit went down drawn", line)
	}

	// The merged view has the unit that answered: its values are current.
	m.selectedLabel, m.showNodeColumn = "", true
	m.dashboardScreen().rebuildTable(m)
	syncNow(t, m)
	if line := kpiLineOf(m); !strings.HasPrefix(line, "  completed/s ") || regexp.MustCompile(`^  completed/s {7}—`).MatchString(line) {
		t.Errorf("merged KPI line = %q, want a current completed/s", line)
	}
}

// TestTrendOfLongInterval: where a poll is a minute a slice of the window is
// two, so one poll each leaves no hole: the Trend is 5 slices, at the cell's
// right.
func TestTrendOfLongInterval(t *testing.T) {
	cfg := multiConfig("http://a.invalid")
	cfg.Servers["multi"].Interval = 60
	f := newFixture(t, cfg)
	f.claim()
	end := time.Now().Round(time.Millisecond)
	for j := range 11 { // 10 minutes of polls, each up to 20% early or late
		at := end.Add(-time.Duration(10-j) * time.Minute)
		if j < 10 {
			at = at.Add(time.Duration(j%3-1) * 12 * time.Second)
		}
		f.write("multi", snapAt(at, rated(at, "instance-1", "http://a.invalid", 4, 1)))
	}
	m := f.model("multi")
	if got, want := trendCells(t, m), []string{strings.Repeat(" ", 15) + "█████"}; !slices.Equal(got, want) {
		t.Errorf("Trend = %q, want %q", got, want)
	}
}

// TestTrendColumnThresholds: the Trend column follows the first fixed
// columns: it takes the width left over once the name has its 12 and the
// column its padding — at most 20 — and is left out when that is under 8.
func TestTrendColumnThresholds(t *testing.T) {
	f := trendFixture(t, time.Now())
	for _, tc := range []struct {
		width int
		node  bool // merged view, with the Node column; else one unit's
		want  int  // the Trend column's width; 0: none
	}{
		{92, true, 0}, {101, true, 0}, {102, true, 8}, {106, true, 12}, {113, true, 19}, {114, true, 20}, {200, true, 20},
		{80, false, 0}, {87, false, 0}, {88, false, 8}, {96, false, 16}, {100, false, 20}, {200, false, 20},
	} {
		name := fmt.Sprintf("%d columns", tc.width)
		if !tc.node {
			name += ", one unit"
		}
		t.Run(name, func(t *testing.T) {
			m := f.modelFor(t, "multi")
			if !tc.node {
				m.selectedLabel, m.showNodeColumn = "instance-1", false
			}
			deliver(m, tea.WindowSizeMsg{Width: tc.width, Height: 40})
			syncNow(t, m)

			d := m.dashboardScreen()
			cols := d.table.Columns()
			fixed := 6 // Name, Status, Concurrency, Processing, Pending, Idle
			if tc.node {
				fixed++
			}
			if tc.want == 0 && len(cols) != fixed || tc.want > 0 && len(cols) != fixed+1 {
				t.Fatalf("%d columns, want %d + Trend %d: %v", len(cols), fixed, tc.want, cols)
			}
			var sum int
			for _, c := range cols {
				sum += c.Width + 2
			}
			if sum > tc.width {
				t.Errorf("the columns take %d of %d columns: %v", sum, tc.width, cols)
			}
			if cols[0].Width < 12 {
				t.Errorf("the Name column is %d wide, want at least 12", cols[0].Width)
			}
			rows := d.table.Rows()
			if len(rows) == 0 {
				t.Fatal("no rows")
			}
			for _, r := range rows {
				if len(r) != len(cols) {
					t.Fatalf("a row has %d cells for %d columns: %q", len(r), len(cols), r)
				}
			}
			view := m.View()
			if tc.want == 0 {
				if strings.Contains(view, "Trend") {
					t.Errorf("the view has a Trend column:\n%s", view)
				}
				return
			}
			if last := cols[len(cols)-1]; last.Title != "Trend" || last.Width != tc.want {
				t.Fatalf("last column = %+v, want Trend, %d wide", last, tc.want)
			}
			if !strings.Contains(view, "Trend") {
				t.Errorf("the view has no Trend column:\n%s", view)
			}
			for _, cell := range trendCells(t, m) {
				if n := utf8.RuneCountInString(cell); n != tc.want {
					t.Errorf("Trend cell %q is %d columns, want %d", cell, n, tc.want)
				}
				if strings.Trim(cell, " ▁▂▃▄▅▆▇█") != "" {
					t.Errorf("Trend cell %q is not plain sparkline", cell)
				}
			}
		})
	}
}

// TestTrendASCIIFallback: where block runes are wide the Trend is ASCII.
func TestTrendASCIIFallback(t *testing.T) {
	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	f := trendFixture(t, time.Now())
	m := f.model("multi")
	for _, cell := range trendCells(t, m) {
		if strings.Trim(cell, " _.-=#") != "" || strings.TrimSpace(cell) == "" {
			t.Errorf("Trend cell %q is not an ASCII sparkline", cell)
		}
	}
	if line := kpiLineOf(m); strings.ContainsAny(line, "▁▂▃▄▅▆▇█") || !strings.HasSuffix(line, "#") {
		t.Errorf("the KPI line is not ASCII, or draws no sparkline: %q", line)
	}
}

// TestTrendStaleKeepsHistory: a stale feed's values are —, its history is
// still drawn.
func TestTrendStaleKeepsHistory(t *testing.T) {
	f := trendFixture(t, time.Now().Add(-time.Minute))
	m := f.model("multi")
	if got, want := rowsOf(m), []string{"orders|instance-1|—|—|—|—|—", "orders|instance-2|—|—|—|—|—"}; !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	cells := trendCells(t, m)
	if len(cells) != 2 || !strings.HasSuffix(cells[0], "▇█") || !strings.HasSuffix(cells[1], "██") {
		t.Errorf("Trend cells = %q, want the history drawn", cells)
	}
}

// TestKPILine: under the header, the fleet's completed/s, failed/min and
// pending, each its current value and a sparkline of the last 10 minutes,
// as wide as the line has room for — at most 12 columns, none below 4.
func TestKPILine(t *testing.T) {
	f := trendFixture(t, time.Now())
	m := f.model("multi")
	deliver(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	syncNow(t, m)

	// 12 columns of 50s: the 5 minutes of history fill the right 6.
	const blank = "      "
	re := regexp.MustCompile(`^  completed/s {4}25\.0 ([▁-█ ]{12})  failed/min {5}0\.0 ([▁-█ ]{12})  pending {7}7 ([▁-█ ]{12})$`)
	line := kpiLineOf(m)
	sub := re.FindStringSubmatch(line)
	if sub == nil {
		t.Fatalf("KPI line = %q, want completed/s, failed/min and pending, each a value and 12 columns", line)
	}
	if !strings.HasPrefix(sub[1], blank) || !strings.HasSuffix(sub[1], "█") || strings.Contains(sub[1][len(blank):], " ") {
		t.Errorf("completed/s sparkline = %q, want its right half drawn, rising", sub[1])
	}
	if sub[2] != blank+"▁▁▁▁▁▁" {
		t.Errorf("failed/min sparkline = %q, want its right half at the lowest level", sub[2])
	}
	if sub[3] != blank+"██████" {
		t.Errorf("pending sparkline = %q, want its right half at the top level", sub[3])
	}

	// It sits between the header and the table, which is a line shorter.
	lines := strings.Split(ansi.ReplaceAllString(m.View(), ""), "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "completed/s") })
	if head := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "vartop [multi]") }); head < 0 || at <= head {
		t.Errorf("the KPI line is at %d, the header at %d", at, head)
	}
	if hdr := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "Status") }); hdr <= at {
		t.Errorf("the table header is at %d, the KPI line at %d", hdr, at)
	}
	if got, want := m.dashboardScreen().table.Height(), 40-14; got != want {
		t.Errorf("table height = %d, want %d: the screen's less the header's 12, the KPI line and the table's own header", got, want)
	}

	// Its sparklines take a third each of what the text leaves, to 12 and
	// from 4: the text of this line, with the values' fields 7 wide, is 61
	// columns with the sparklines' gaps, 58 without them.
	for _, tc := range []struct{ width, spark int }{{60, 0}, {72, 0}, {73, 4}, {76, 5}, {97, 12}, {200, 12}} {
		deliver(m, tea.WindowSizeMsg{Width: tc.width, Height: 40})
		line := kpiLineOf(m)
		want := 58
		if tc.spark > 0 {
			want = 61 + 3*tc.spark
		}
		if got := utf8.RuneCountInString(line); got != want {
			t.Errorf("at %d columns the KPI line is %d wide, want %d: %q", tc.width, got, want, line)
		}
		if drawn := strings.ContainsAny(line, "▁▂▃▄▅▆▇█"); drawn != (tc.spark > 0) {
			t.Errorf("at %d columns the KPI line draws sparklines = %v, want %v: %q", tc.width, drawn, tc.spark > 0, line)
		}
	}
}

// TestKPILineOfOneUnit: the view of one unit has that unit's KPIs.
func TestKPILineOfOneUnit(t *testing.T) {
	f := trendFixture(t, time.Now())
	m := f.model("multi")
	m.selectedLabel, m.showNodeColumn = "instance-2", false
	m.dashboardScreen().rebuildTable(m)
	syncNow(t, m)
	if line := kpiLineOf(m); !regexp.MustCompile(`^  completed/s {5}5\.0 .*  pending {7}3 `).MatchString(line) {
		t.Errorf("instance-2's KPI line = %q, want 5.0 completed/s and 3 pending", line)
	}
}

// TestKPILineStale: while the newest snapshot is stale its values are —; its
// history stays drawn.
func TestKPILineStale(t *testing.T) {
	f := trendFixture(t, time.Now().Add(-time.Minute))
	m := f.model("multi")
	line := kpiLineOf(m)
	if !regexp.MustCompile(`^  completed/s {7}— [▁-█ ]{12}  failed/min {7}— [▁-█ ]{12}  pending {7}— [▁-█ ]{12}$`).MatchString(line) {
		t.Errorf("stale KPI line = %q, want — for each value and the sparklines drawn", line)
	}
	if !strings.HasSuffix(line, "██████") {
		t.Errorf("stale KPI line = %q, want its history drawn", line)
	}
}

// TestKPILineOnlyWithData: no KPI line where there is nothing to chart: no
// snapshot yet, a server not configured, a unit no longer reported.
func TestKPILineOnlyWithData(t *testing.T) {
	empty := newFixture(t, multiConfig("http://a.invalid"))
	if line := kpiLineOf(empty.model("multi")); line != "" {
		t.Errorf("KPI line before the first snapshot: %q", line)
	}

	f := trendFixture(t, time.Now())
	m := f.model("multi")
	m.selectedLabel, m.showNodeColumn = "instance-9", false
	m.dashboardScreen().rebuildTable(m)
	syncNow(t, m)
	if line := kpiLineOf(m); line != "" {
		t.Errorf("KPI line of a unit that is gone: %q", line)
	}

	m = f.model("multi")
	deliver(m, configMsg{cfg: &config.Config{Servers: map[string]*config.Server{}}})
	if line := kpiLineOf(m); line != "" {
		t.Errorf("KPI line of a server not configured: %q", line)
	}
}

// TestResizeKeepsSelectionAcrossTrend: a resize that brings the Trend column
// in or out rebuilds the rows with it, and the cursor stays on its row.
func TestResizeKeepsSelectionAcrossTrend(t *testing.T) {
	f := trendFixture(t, time.Now())
	m := f.model("multi")
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	d := m.dashboardScreen()
	if d.selectedLabel() != "instance-2" {
		t.Fatalf("selected %q, want instance-2", d.selectedLabel())
	}
	for _, width := range []int{92, 106, 92, 200, 20, 120} {
		deliver(m, tea.WindowSizeMsg{Width: width, Height: 40})
		rows := d.table.Rows()
		if len(rows) != 2 {
			t.Fatalf("%d columns: %d rows, want 2", width, len(rows))
		}
		for _, r := range rows {
			if len(r) != len(d.table.Columns()) {
				t.Fatalf("%d columns: a row has %d cells for %d columns", width, len(r), len(d.table.Columns()))
			}
		}
		if d.table.Cursor() != 1 || d.selectedLabel() != "instance-2" || d.selectedWorkerName() != "orders" {
			t.Errorf("%d columns: cursor %d on %q %q, want 1 on instance-2 orders", width, d.table.Cursor(), d.selectedLabel(), d.selectedWorkerName())
		}
	}
}

// countTrends makes m build its series through a counter, and returns it.
func countTrends(m *Model) *atomic.Int64 {
	var n atomic.Int64
	build := m.buildTrends
	m.buildTrends = func(snaps []*fleet.Snapshot, label string, interval time.Duration, noun fleet.Noun) trendSet {
		n.Add(1)
		return build(snaps, label, interval, noun)
	}
	return &n
}

// TestTrendsBuiltInTheCommand: the sparklines' series are built by the sync
// command, off the UI goroutine, once for each snapshot and view — not by
// Update, nor by a View, a resize or a sync that brings no snapshot.
func TestTrendsBuiltInTheCommand(t *testing.T) {
	end := time.Now()
	f := trendFixture(t, end)
	m := f.model("multi")
	built := countTrends(m)
	d := m.dashboardScreen()

	// Nothing new: the command finds the series it has, and Update, View and
	// a resize build none.
	for range 3 {
		syncNow(t, m)
		_ = m.View()
	}
	for _, width := range []int{92, 106, 160} {
		deliver(m, tea.WindowSizeMsg{Width: width, Height: 40})
		_ = m.View()
	}
	if n := built.Load(); n != 0 {
		t.Fatalf("series built %d times with no new snapshot, want none", n)
	}

	// A new snapshot: making the command builds nothing, running it does,
	// and Update takes the series in without building.
	at := end.Add(15 * time.Second).Round(time.Millisecond)
	f.write("multi", snapAt(at, rated(at, "instance-1", "http://a.invalid", 40, 7), rated(at, "instance-2", "http://b.invalid", 5, 3)))
	cmd := m.syncCmd(m.feed, m.gen)
	if n := built.Load(); n != 0 {
		t.Fatalf("making the sync command built the series (%d)", n)
	}
	msg := cmd()
	if n := built.Load(); n != 1 {
		t.Fatalf("running the sync command built the series %d times, want 1", n)
	}
	deliver(m, msg)
	_ = m.View()
	if n := built.Load(); n != 1 {
		t.Errorf("series built %d times by Update and View, want still 1", n)
	}
	if line := kpiLineOf(m); !regexp.MustCompile(`^  completed/s {4}45\.0 `).MatchString(line) {
		t.Errorf("KPI line = %q, want the new snapshot's 45.0 completed/s", line)
	}
	if d.trends.key.latest != m.latest() {
		t.Error("the series installed are not of the newest snapshot")
	}

	// Another view of the server is another build, by the command: the rows
	// show at once, the Trend and KPI sparklines blank until it comes.
	m.selectedLabel, m.showNodeColumn = "instance-1", false
	cmd = m.show("multi")
	if n := built.Load(); n != 1 {
		t.Errorf("show built the series (%d builds)", n)
	}
	if cells := trendCells(t, m); len(cells) != 1 || strings.TrimSpace(cells[0]) != "" {
		t.Errorf("Trend cells before the series arrive = %q, want blank", cells)
	}
	if line := kpiLineOf(m); strings.ContainsAny(line, "▁▂▃▄▅▆▇█") {
		t.Errorf("KPI line before the series arrive = %q, want its sparklines blank", line)
	}
	deliver(m, cmd())
	if n := built.Load(); n != 2 {
		t.Errorf("series built %d times, want 2", n)
	}
	if cells := trendCells(t, m); len(cells) != 1 || strings.TrimSpace(cells[0]) == "" {
		t.Errorf("Trend cells after = %q, want the unit's", cells)
	}
}

// TestTrendsOfAnotherViewAreIgnored: series that are not of what the model
// shows now — another feed, another view, an older snapshot — are not taken
// in, though their sync is: they would draw another server's history.
func TestTrendsOfAnotherViewAreIgnored(t *testing.T) {
	end := time.Now()
	f := trendFixture(t, end)
	m := f.model("multi")
	d := m.dashboardScreen()
	want := trendCells(t, m)
	installed := d.trends.key

	at := end.Add(15 * time.Second).Round(time.Millisecond)
	older := m.latest()
	f.write("multi", snapAt(at, rated(at, "instance-1", "http://a.invalid", 40, 7), rated(at, "instance-2", "http://b.invalid", 0, 3)))
	good := m.syncCmd(m.feed, m.gen)().(syncedMsg)
	otherFeed := store.NewFeed(f.dir, "multi", 5*time.Second)
	t.Cleanup(func() { _ = otherFeed.Close() })

	for _, tc := range []struct {
		name   string
		change func(*trendsKey)
	}{
		{"another feed", func(k *trendsKey) { k.feed = otherFeed }},
		{"another view", func(k *trendsKey) { k.label = "instance-1" }},
		{"an older snapshot", func(k *trendsKey) { k.latest = older }},
		{"no feed", func(k *trendsKey) { k.feed = nil }},
	} {
		msg := good
		tc.change(&msg.trends.key)
		deliver(m, msg)
		if d.trends.key != installed {
			t.Errorf("%s: the series were taken in", tc.name)
		}
		if got := trendCells(t, m); !slices.Equal(got, want) {
			t.Errorf("%s: Trend cells changed to %q", tc.name, got)
		}
	}

	deliver(m, good)
	if d.trends.key == installed || d.trends.key.latest != m.latest() {
		t.Error("the series of the newest snapshot were not taken in")
	}
	if got := trendCells(t, m); slices.Equal(got, want) {
		t.Error("Trend cells did not change with the newest snapshot")
	}
}

// TestSwitchingServerNeverShowsOldTrends: after a switch the new server's
// rows show with a blank Trend until its series come — the old server's are
// not drawn beside the new one's rows — and then its own.
func TestSwitchingServerNeverShowsOldTrends(t *testing.T) {
	f := newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{"http://a.invalid"}, Interval: 5},
		"b": {Endpoints: []string{"http://b.invalid"}, Interval: 5},
	}})
	f.claim()
	end := time.Now().Round(time.Millisecond)
	for j := range 21 {
		at := end.Add(-time.Duration(20-j) * 15 * time.Second)
		f.write("a", snapAt(at, rated(at, "instance-1", "http://a.invalid", float64(j), 1)))
		f.write("b", snapAt(at, rated(at, "instance-1", "http://b.invalid", 5, 1)))
	}
	aTrend := strings.Repeat(" ", 10) + "▁▂▃▃▄▅▆▆▇█"
	bTrend := strings.Repeat(" ", 10) + strings.Repeat("█", 10)
	m := f.model("a")
	if got := trendCells(t, m); !slices.Equal(got, []string{aTrend}) {
		t.Fatalf("a's Trend = %q, want %q", got, aTrend)
	}
	deliver(m, m.show("b")()) // b is shown once: its feed holds its snapshots from here on
	if got := trendCells(t, m); !slices.Equal(got, []string{bTrend}) {
		t.Fatalf("b's Trend = %q, want %q", got, bTrend)
	}

	// Back to a, whose rows are there at once: the series held are b's.
	late := m.syncCmd(m.feed, m.gen)() // a sync of b under way when the switch comes
	cmd := m.show("a")
	if got := rowsOf(m); len(got) != 1 {
		t.Fatalf("a's rows at once = %v, want its one", got)
	}
	if got := trendCells(t, m); strings.TrimSpace(got[0]) != "" {
		t.Fatalf("right after the switch, Trend = %q, want blank: b's is not a's", got)
	}
	if line := kpiLineOf(m); strings.ContainsAny(line, "▁▂▃▄▅▆▇█") {
		t.Fatalf("right after the switch the KPI line has b's sparklines: %q", line)
	}
	deliver(m, late)
	if got := trendCells(t, m); strings.TrimSpace(got[0]) != "" {
		t.Fatalf("a late sync of b drew its Trend on a: %q", got)
	}

	deliver(m, cmd())
	if got := trendCells(t, m); !slices.Equal(got, []string{aTrend}) {
		t.Errorf("a's Trend = %q, want %q", got, aTrend)
	}
}

// TestSwitchClosesActionMenu: the action menu is for the row it was opened
// on; a switch of server closes it, so that p, r and s do not act on a unit
// of another server that has the same label and worker.
func TestSwitchClosesActionMenu(t *testing.T) {
	podA, podB := newFakePod(t), newFakePod(t)
	f := newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{podA.url()}, Interval: 5, Token: "tok"},
		"b": {Endpoints: []string{podB.url()}, Interval: 5, Token: "tok"},
	}})
	f.claim()
	orders := func() map[string]*fleet.Sample { return map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)} }
	f.write("a", fresh(unit("instance-1", podA.url(), orders())))
	f.write("b", fresh(unit("instance-1", podB.url(), orders())))

	m := f.model("a")
	deliver(m, m.show("b")()) // b's feed is loaded: its rows are there at once when it is shown again
	deliver(m, m.show("a")())
	press(m, runeKey('a'))
	d := m.dashboardScreen()
	if !d.inActionSelection || d.selectedLabel() != "instance-1" || d.selectedWorkerName() != "orders" {
		t.Fatalf("menu open %v on %q %q, want open on instance-1 orders", d.inActionSelection, d.selectedLabel(), d.selectedWorkerName())
	}

	// Switch to b, as the switcher does, with the menu open.
	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("b"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.key != "b" {
		t.Fatalf("server = %q, want b", m.key)
	}
	if got := rowsOf(m); len(got) != 1 || !strings.HasPrefix(got[0], "orders|instance-1|") {
		t.Fatalf("b's rows at once = %v, want instance-1 orders", got)
	}
	if d.inActionSelection {
		t.Fatal("the action menu is still open on b")
	}
	if cmd := press(m, runeKey('p')); cmd != nil {
		runAll(m, cmd)
	}
	if podA.total() != 0 || podB.total() != 0 {
		t.Errorf("requests after p: a %v, b %v, want none", podA.seen(), podB.seen())
	}
}

// BenchmarkNewTrends is the build of the sparklines' series, which the sync
// command runs off the UI goroutine, for a fleet of 50 units with 10 workers
// each, an hour of history.
func BenchmarkNewTrends(b *testing.B) {
	for _, interval := range []time.Duration{5 * time.Second, time.Second} {
		b.Run(interval.String(), func(b *testing.B) {
			end := time.Now()
			var snaps []*fleet.Snapshot
			for j := range int(time.Hour / interval) {
				at := end.Add(-time.Duration(int(time.Hour/interval)-1-j) * interval)
				pods := make([]fleet.PodSnapshot, 50)
				for u := range pods {
					workers := make(map[string]*fleet.Sample)
					for w := range 10 {
						s := smp("Running", 4, 1, 3, 2)
						s.At, s.RateOK, s.CompletedRate = at, true, float64(j%7+w)
						workers[fmt.Sprintf("worker-%d", w)] = s
					}
					pods[u] = unit(fmt.Sprintf("pod-%d", u), fmt.Sprintf("http://pod-%d", u), workers)
				}
				snaps = append(snaps, snapAt(at, pods...))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				newTrends(snaps, "", interval, fleet.Noun{One: "pod", Many: "pods"})
			}
		})
	}
}
