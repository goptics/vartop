package tui

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

const (
	detailTestWorker = "email-sender"
	detailTestLabel  = "instance-1"
)

// detailTestSample is what the store holds for email-sender on instance-1.
func detailTestSample() *fleet.Sample {
	s := smp("Running", 7, 3, 4, 11)
	s.Metrics.Submitted = 1234
	s.Metrics.Completed = 1200
	s.Metrics.Successful = 1100
	s.Metrics.Failed = 100
	return s
}

// detailFixture stores a fresh snapshot of "local": instance-1 runs
// email-sender, instance-2 is down, and instance-3 lists email-sender but
// did not answer for it.
func detailFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, localConfig())
	f.claim()
	f.write("local", fresh(
		unit(detailTestLabel, "http://localhost:9000", map[string]*fleet.Sample{detailTestWorker: detailTestSample()}),
		downUnit("instance-2", "http://localhost:9001", "connection refused"),
		fleet.PodSnapshot{Key: "http://localhost:9002", Via: "http://localhost:9002", Label: "instance-3",
			Workers: map[string]*fleet.Sample{}, Unreached: []string{detailTestWorker}},
	))
	return f
}

// detailModel opens the detail screen of email-sender on label, sized.
func detailModel(t *testing.T, label string, width, height int) *Model {
	t.Helper()
	m := detailFixture(t).model("local")
	deliver(m, tea.WindowSizeMsg{Width: width, Height: height})
	m.pushScreen(newDetailScreen(detailTestWorker, label))
	return m
}

// loadedDetailModel opens the detail screen of email-sender on instance-1.
func loadedDetailModel(t *testing.T, width, height int) *Model {
	t.Helper()
	return detailModel(t, detailTestLabel, width, height)
}

// TestDetailMissingSample: a unit that is down, did not answer for the
// worker, or is no longer reported shows "{label}: {why}" in place of the
// worker's values.
func TestDetailMissingSample(t *testing.T) {
	for label, want := range map[string]string{
		"instance-2": "instance-2: connection refused",
		"instance-3": "instance-3: not reached",
		"instance-9": "instance-9: no longer reported",
	} {
		m := detailModel(t, label, 80, 24)
		view := m.View()
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
		if strings.Contains(view, "Performance Metrics") {
			t.Errorf("%s: values shown for a missing sample:\n%s", label, view)
		}
	}
}

func TestDetailViewShowsWorkerAndMetrics(t *testing.T) {
	m := loadedDetailModel(t, 120, 60)
	view := m.View()
	for _, want := range []string{
		detailTestWorker, "Worker Details", "Submitted", "1234", "Completed", "1200",
		"Successful", "1100", "Failed", "100", "Success Rate", "91.7%",
		"Concurrency", "7 workers", "11 jobs", "Server: local", detailTestLabel, "http://localhost:9000",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewNarrowLayoutShowsWorker(t *testing.T) {
	m := loadedDetailModel(t, 60, 80)
	view := m.View()
	for _, want := range []string{detailTestWorker, "Server: local", "→ " + detailTestLabel, "Submitted", "1234"} {
		if !strings.Contains(view, want) {
			t.Fatalf("narrow view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailScrollChangesView(t *testing.T) {
	m := loadedDetailModel(t, 80, 15)
	top := m.View()
	if !strings.Contains(top, "Worker Details") {
		t.Fatalf("top view missing header:\n%s", top)
	}

	for i := 0; i < 100; i++ {
		press(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	scrolled := m.View()
	if scrolled == top {
		t.Fatal("view unchanged after scrolling down 100 times")
	}
	if strings.Contains(scrolled, "Worker Details") {
		t.Fatalf("header still visible after scrolling to the bottom:\n%s", scrolled)
	}

	// Scrolling beyond the bottom is stable.
	for i := 0; i < 50; i++ {
		press(m, runeKey('j'))
	}
	if again := m.View(); again != scrolled {
		t.Fatal("view changed when scrolling past the bottom")
	}

	press(m, runeKey('g'))
	if got := m.View(); got != top {
		t.Fatalf("g did not return to the original view:\n%s", got)
	}
}

func TestDetailScrollUpAtTopIsNoop(t *testing.T) {
	m := loadedDetailModel(t, 80, 15)
	top := m.View()
	for i := 0; i < 5; i++ {
		press(m, tea.KeyMsg{Type: tea.KeyUp})
		press(m, runeKey('k'))
	}
	if got := m.View(); got != top {
		t.Fatal("scrolling up at the top changed the view")
	}
}

func TestDetailScrollJKRoundTrip(t *testing.T) {
	m := loadedDetailModel(t, 80, 15)
	top := m.View()
	for i := 0; i < 3; i++ {
		press(m, runeKey('j'))
	}
	if m.View() == top {
		t.Fatal("j did not scroll")
	}
	for i := 0; i < 3; i++ {
		press(m, runeKey('k'))
	}
	if got := m.View(); got != top {
		t.Fatal("k x3 after j x3 did not return to the original view")
	}
}

func TestDetailBottomTopRoundTrip(t *testing.T) {
	m := loadedDetailModel(t, 80, 15)
	top := m.View()

	press(m, runeKey('G'))
	bottom := m.View()
	if bottom == top {
		t.Fatal("G did not move the view")
	}
	press(m, runeKey('g'))
	if got := m.View(); got != top {
		t.Fatal("g after G did not return to the original view")
	}
	press(m, tea.KeyMsg{Type: tea.KeyEnd})
	if got := m.View(); got != bottom {
		t.Fatal("end key does not match G")
	}
	press(m, tea.KeyMsg{Type: tea.KeyHome})
	if got := m.View(); got != top {
		t.Fatal("home key does not match g")
	}
}

func TestDetailPageKeysDoNotPanic(t *testing.T) {
	m := loadedDetailModel(t, 80, 15)
	top := m.View()
	for i := 0; i < 20; i++ {
		press(m, tea.KeyMsg{Type: tea.KeyPgDown})
		_ = m.View()
	}
	if m.View() == top {
		t.Fatal("pgdown never moved the view")
	}
	for i := 0; i < 20; i++ {
		press(m, tea.KeyMsg{Type: tea.KeyPgUp})
		_ = m.View()
	}
	if got := m.View(); got != top {
		t.Fatal("pgup x20 did not return to the top")
	}
}

func TestDetailScrollNoPanicWhenContentFits(t *testing.T) {
	m := loadedDetailModel(t, 200, 80)
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyDown}, {Type: tea.KeyPgDown}, runeKey('G'), {Type: tea.KeyPgUp}, {Type: tea.KeyUp}, runeKey('g'),
	} {
		press(m, k)
		_ = m.View()
	}
}

func TestDetailScrollKeysOnMissingSample(t *testing.T) {
	m := detailModel(t, "instance-2", 80, 15)
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyDown}, {Type: tea.KeyPgDown}, runeKey('G'), {Type: tea.KeyPgUp}, {Type: tea.KeyUp}, runeKey('g'),
	} {
		press(m, k)
		if view := m.View(); !strings.Contains(view, "instance-2: connection refused") {
			t.Fatalf("missing-sample view lost after %q:\n%s", k.String(), view)
		}
	}
}

func TestDetailActionModeToggle(t *testing.T) {
	m := loadedDetailModel(t, 120, 40)
	normal := m.View()
	if strings.Contains(normal, "pause") || strings.Contains(normal, "stop") {
		t.Fatalf("normal footer already lists actions:\n%s", normal)
	}

	press(m, runeKey('a'))
	action := m.View()
	for _, want := range []string{"pause", "resume", "stop", "restart"} {
		if !strings.Contains(action, want) {
			t.Fatalf("action footer missing %q:\n%s", want, action)
		}
	}
	if action == normal {
		t.Fatal("view did not change in action mode")
	}

	// a is not bound in action mode; only esc (or an action) leaves it.
	press(m, runeKey('a'))
	if got := m.View(); got != action {
		t.Fatal("a changed the view while already in action mode")
	}
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.View(); got != normal {
		t.Fatal("esc did not restore the normal footer")
	}
}

func TestDetailActionKeysReturnCmdAndExitActionMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"pause", runeKey('p')},
		{"resume", runeKey('r')},
		{"stop", runeKey('s')},
		{"restart", runeKey('R')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := loadedDetailModel(t, 120, 40)
			normal := m.View()
			press(m, runeKey('a'))
			cmd := press(m, tc.key)
			if cmd == nil {
				t.Fatalf("%s returned nil command", tc.name)
			}
			if !m.isDetailActive() {
				t.Fatalf("%s left the detail screen", tc.name)
			}
			if got := m.View(); got != normal {
				t.Fatalf("still in action mode after %s", tc.name)
			}
		})
	}
}

func TestDetailActionKeysIgnoredOutsideActionMode(t *testing.T) {
	m := loadedDetailModel(t, 120, 40)
	if cmd := press(m, runeKey('p')); cmd != nil {
		t.Fatal("p outside action mode returned a command")
	}
	if cmd := press(m, runeKey('s')); cmd != nil {
		t.Fatal("s outside action mode returned a command")
	}
}

func TestDetailEscExitsActionMode(t *testing.T) {
	m := loadedDetailModel(t, 120, 40)
	normal := m.View()
	press(m, runeKey('a'))
	if cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatal("esc in action mode returned a command")
	}
	if !m.isDetailActive() {
		t.Fatal("esc in action mode left the detail screen, want it to only leave action mode")
	}
	if got := m.View(); got != normal {
		t.Fatal("esc did not leave action mode")
	}
}

func TestDetailEscPopsToDashboard(t *testing.T) {
	m := loadedDetailModel(t, 80, 24)
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen = %T, want *dashboardScreen", m.activeScreen())
	}
	if len(m.screens) != 1 {
		t.Fatalf("stack length = %d, want 1", len(m.screens))
	}
}

func TestDetailQuit(t *testing.T) {
	m := loadedDetailModel(t, 80, 24)
	if cmd := press(m, runeKey('q')); !isQuit(cmd) {
		t.Fatal("q in detail did not return tea.Quit")
	}

	m = loadedDetailModel(t, 80, 24)
	press(m, runeKey('a'))
	if cmd := press(m, runeKey('q')); !isQuit(cmd) {
		t.Fatal("q in action mode did not return tea.Quit")
	}
}

// TestDetailRefreshPokes: r on the detail screen pokes its server, as on
// the dashboard, and keeps the worker's values shown.
func TestDetailRefreshPokes(t *testing.T) {
	f := detailFixture(t)
	m := f.model("local")
	m.pushScreen(newDetailScreen(detailTestWorker, detailTestLabel))
	cmd := press(m, runeKey('r'))
	if cmd == nil {
		t.Fatal("r returned no command, want a poke")
	}
	deliver(m, cmd())
	if n := f.pokes("local"); n != 1 {
		t.Fatalf("pokes = %d, want 1", n)
	}
	if view := m.View(); !strings.Contains(view, "1234") {
		t.Fatalf("values gone after refresh:\n%s", view)
	}
}

func TestResizeAllScreensDoesNotPanic(t *testing.T) {
	widths := []int{20, 80, 92, 106, 200}
	heights := []int{5, 24, 60}

	// The stores, made once; each subtest gets a model of its own.
	detail, switcher, empty := detailFixture(t), switcherFixture(t), newFixture(t, localConfig())
	trend := trendFixture(t, time.Now())
	staleTrend := trendFixture(t, time.Now().Add(-time.Minute))
	stale := newFixture(t, localConfig())
	stale.claim()
	stale.write("local", snapAt(time.Now().Add(-time.Minute),
		unit(detailTestLabel, "http://localhost:9000", map[string]*fleet.Sample{detailTestWorker: detailTestSample()}),
		downUnit("instance-2", "http://localhost:9001", "a long error that does not fit on a narrow terminal at all")))
	stale.release()

	screens := []struct {
		name  string
		setup func(t *testing.T) *Model
	}{
		{"dashboard", func(t *testing.T) *Model {
			return detail.modelFor(t, "local")
		}},
		{"dashboard-unit", func(t *testing.T) *Model {
			m := detail.modelFor(t, "local")
			m.selectedLabel, m.showNodeColumn = "instance-3", false
			m.dashboardScreen().rebuildTable(m)
			syncNow(t, m)
			return m
		}},
		{"dashboard-trend", func(t *testing.T) *Model {
			return trend.modelFor(t, "multi")
		}},
		{"dashboard-trend-unit", func(t *testing.T) *Model {
			m := trend.modelFor(t, "multi")
			m.selectedLabel, m.showNodeColumn = "instance-1", false
			m.dashboardScreen().rebuildTable(m)
			syncNow(t, m)
			return m
		}},
		{"dashboard-stale", func(t *testing.T) *Model {
			return stale.modelFor(t, "local")
		}},
		{"waiting", func(t *testing.T) *Model {
			return empty.modelFor(t, "local")
		}},
		{"not-configured", func(t *testing.T) *Model {
			m := detail.modelFor(t, "local")
			deliver(m, configMsg{cfg: &config.Config{Servers: map[string]*config.Server{}}})
			return m
		}},
		{"switcher", func(t *testing.T) *Model {
			m := switcher.modelFor(t, "local")
			openSwitcher(t, m)
			selectSwitcherItem(t, m, serverItem("multi"))
			press(m, tea.KeyMsg{Type: tea.KeyEnter})
			return m
		}},
		{"detail-missing", func(t *testing.T) *Model {
			m := detail.modelFor(t, "local")
			m.pushScreen(newDetailScreen(detailTestWorker, "instance-2"))
			return m
		}},
		{"detail", func(t *testing.T) *Model {
			m := detail.modelFor(t, "local")
			m.pushScreen(newDetailScreen(detailTestWorker, detailTestLabel))
			return m
		}},
		{"detail-stale", func(t *testing.T) *Model {
			m := stale.modelFor(t, "local")
			m.pushScreen(newDetailScreen(detailTestWorker, detailTestLabel))
			return m
		}},
		{"detail-not-configured", func(t *testing.T) *Model {
			m := detail.modelFor(t, "local")
			m.pushScreen(newDetailScreen(detailTestWorker, detailTestLabel))
			deliver(m, configMsg{cfg: &config.Config{Servers: map[string]*config.Server{}}})
			return m
		}},
		{"detail-charts", func(t *testing.T) *Model {
			m := trend.modelFor(t, "multi")
			openDetail(t, m)
			return m
		}},
		{"detail-charts-stale", func(t *testing.T) *Model {
			m := staleTrend.modelFor(t, "multi")
			openDetail(t, m)
			return m
		}},
		{"detail-charts-action", func(t *testing.T) *Model {
			m := trend.modelFor(t, "multi")
			openDetail(t, m)
			press(m, runeKey('a'))
			return m
		}},
		{"detail-action", func(t *testing.T) *Model {
			m := detail.modelFor(t, "local")
			m.pushScreen(newDetailScreen(detailTestWorker, detailTestLabel))
			press(m, runeKey('a'))
			return m
		}},
	}

	for _, sc := range screens {
		for _, w := range widths {
			for _, h := range heights {
				t.Run(fmt.Sprintf("%s/%dx%d", sc.name, w, h), func(t *testing.T) {
					m := sc.setup(t)
					deliver(m, tea.WindowSizeMsg{Width: w, Height: h})
					_ = m.View()

					// Navigation after a resize must also be safe.
					press(m, tea.KeyMsg{Type: tea.KeyDown})
					press(m, tea.KeyMsg{Type: tea.KeyUp})
					_ = m.View()
				})
			}
		}
	}
}

// setServer replaces the config of the server m shows.
func setServer(m *Model, srv *config.Server) {
	srv.Name = m.key
	m.cfg.Servers[m.key] = srv
}

func TestConnectionLabelDNS(t *testing.T) {
	m := testModel(t)
	setServer(m, &config.Server{DNS: &config.DNSRef{Host: "varmq", Port: 8080}})
	if got := m.connectionLabel(); got != "dns:varmq:8080" {
		t.Fatalf("connectionLabel = %q, want dns:varmq:8080", got)
	}
	if view := m.View(); !strings.Contains(view, "dns:varmq:8080") {
		t.Fatalf("dashboard view missing dns label:\n%s", view)
	}
}

func TestConnectionLabelKubernetes(t *testing.T) {
	m := testModel(t)
	setServer(m, &config.Server{Kubernetes: &config.KubernetesRef{Context: "ctx", Namespace: "ns", Service: "app"}})
	if got := m.connectionLabel(); got != "ctx/ns" {
		t.Fatalf("connectionLabel = %q, want ctx/ns", got)
	}
	if view := m.View(); !strings.Contains(view, "ctx/ns") {
		t.Fatalf("dashboard view missing kubernetes label:\n%s", view)
	}
}

func TestConnectionLabelStaticAndSelectedPod(t *testing.T) {
	m := testModel(t)
	if got := m.connectionLabel(); got != "http://localhost:9000" {
		t.Fatalf("static single endpoint label = %q", got)
	}
	setServer(m, &config.Server{Endpoints: []string{"http://a:1", "http://b:1"}})
	if got := m.connectionLabel(); got != "2 pods" {
		t.Fatalf("static multi endpoint label = %q, want 2 pods", got)
	}
	m.selectedLabel = "instance-2"
	if got := m.connectionLabel(); got != "instance-2" {
		t.Fatalf("selected unit label = %q, want instance-2", got)
	}
}

// Charts.

// failing is a healthy unit labelled label, at key, whose worker "orders"
// completes completed a second and fails failed a second, as of at.
func failing(at time.Time, label, key string, completed, failed float64) fleet.PodSnapshot {
	s := smp("Running", 4, 1, 3, 0)
	s.At, s.RateOK, s.CompletedRate, s.FailedRate = at, true, completed, failed
	return unit(label, key, map[string]*fleet.Sample{"orders": s})
}

// historyFixture stores five minutes of history on the server "multi": 21
// snapshots 15s apart, the newest at end, of two units that run "orders":
// instance-1 steadily completes 12.5 a second and fails 2; instance-2
// completes 4 and fails none.
func historyFixture(t *testing.T, end time.Time) *fixture {
	t.Helper()
	f := newFixture(t, multiConfig("http://a.invalid", "http://b.invalid"))
	f.claim()
	end = end.Round(time.Millisecond)
	for j := range 21 {
		at := end.Add(-time.Duration(20-j) * 15 * time.Second)
		f.write("multi", snapAt(at,
			failing(at, "instance-1", "http://a.invalid", 12.5, 2),
			failing(at, "instance-2", "http://b.invalid", 4, 0)))
	}
	return f
}

// openDetail opens the detail screen of the dashboard's selected row, as
// enter does, and runs what that returns: the sync that builds its charts.
func openDetail(t *testing.T, m *Model) {
	t.Helper()
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.isDetailActive() {
		t.Fatal("enter did not open the detail screen")
	}
	if cmd == nil {
		t.Fatal("opening the detail screen returned no command: its charts would wait for the next tick")
	}
	deliver(m, cmd())
}

// chartOf is the chart the detail view shows under title: its 5 rows and the
// axis, plain, without the body's padding, a line's trailing blanks trimmed;
// nil when there is none.
func chartOf(m *Model, title string) []string {
	lines := strings.Split(ansi.ReplaceAllString(m.View(), ""), "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, title) })
	if at < 0 {
		return nil
	}
	chart := lines[at+1 : at+7]
	for i, l := range chart {
		chart[i] = strings.TrimRight(strings.TrimPrefix(l, " "), " ")
	}
	return chart
}

// lineOf is the index of the first line of the detail view that has text.
func lineOf(m *Model, text string) int {
	return slices.IndexFunc(strings.Split(ansi.ReplaceAllString(m.View(), ""), "\n"), func(l string) bool { return strings.Contains(l, text) })
}

// TestDetailCharts: under the metrics, Completed /s (1h) and Failed /s (1h)
// of the worker on its unit: 5 rows and an axis each, over the last hour,
// scaled to the peak, which is printed in the gutter of the top row; the 5
// minutes the store holds fill the right of the hour, the rest is blank.
func TestDetailCharts(t *testing.T) {
	f := historyFixture(t, time.Now())
	for _, width := range []int{120, 80, 200} { // side by side, stacked
		m := f.model("multi")
		deliver(m, tea.WindowSizeMsg{Width: width, Height: 80})
		openDetail(t, m)
		view := ansi.ReplaceAllString(m.View(), "")
		completed, failed := strings.Index(view, "Completed /s (1h)"), strings.Index(view, "Failed /s (1h)")
		if metrics := strings.Index(view, "Performance Metrics"); metrics < 0 || completed < metrics || failed < completed {
			t.Fatalf("%d columns: Performance Metrics at %d, Completed /s (1h) at %d, Failed /s (1h) at %d, want them in that order:\n%s", width, metrics, completed, failed, view)
		}

		plot := width - 4 - 8
		for _, tc := range []struct{ title, peak string }{{"Completed /s (1h)", "12.5/s"}, {"Failed /s (1h)", "2.0/s"}} {
			chart := chartOf(m, tc.title)
			if len(chart) != 6 {
				t.Fatalf("%d columns: %s has no chart", width, tc.title)
			}
			if strings.Fields(chart[0])[0] != tc.peak {
				t.Errorf("%d columns: %s top row = %q, want the peak %s in the gutter", width, tc.title, chart[0], tc.peak)
			}
			for r := 1; r < 5; r++ {
				if !strings.HasPrefix(chart[r], strings.Repeat(" ", 8)) {
					t.Errorf("%d columns: %s row %d = %q, want its gutter blank", width, tc.title, r, chart[r])
				}
			}
			// Steady: every column the 5 minutes touch is the peak, to the top row: those
			// from 55 minutes back, the history's oldest, to now, the plot's right edge.
			want := plot - plot*11/12
			for r := range 5 {
				row := []rune(chart[r])
				if n := strings.Count(chart[r], "█"); n != want || row[len(row)-1] != '█' {
					t.Errorf("%d columns: %s row %d = %q: %d full columns, want %d at the right", width, tc.title, r, chart[r], n, want)
				}
			}
			// The hour before is blank, the axis under the plot.
			if axis := chart[5]; !strings.HasPrefix(axis, strings.Repeat(" ", 8)+"-60m") || !strings.HasSuffix(axis, "now") || utf8.RuneCountInString(axis) != width-4 {
				t.Errorf("%d columns: %s axis = %q, want -60m … now across the plot (%d wide)", width, tc.title, axis, width-4)
			}
		}
	}
}

// TestDetailChartsOfTheUnit: the charts are of the unit and worker the
// screen shows — one unit's series, not the merged — and a zero rate is a
// baseline with the peak 0, not nothing.
func TestDetailChartsOfTheUnit(t *testing.T) {
	m := historyFixture(t, time.Now()).model("multi")
	deliver(m, tea.WindowSizeMsg{Width: 120, Height: 80})
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.dashboardScreen().selectedLabel() != "instance-2" {
		t.Fatal("the second row is not instance-2")
	}
	openDetail(t, m)

	completed, failed := chartOf(m, "Completed /s (1h)"), chartOf(m, "Failed /s (1h)")
	if completed == nil || failed == nil {
		t.Fatal("no charts")
	}
	if fields := strings.Fields(completed[0]); fields[0] != "4.0/s" {
		t.Errorf("instance-2 completes %q, want 4.0/s", completed[0])
	}
	if fields := strings.Fields(failed[0]); fields[0] != "0/s" {
		t.Errorf("instance-2 fails %q, want 0/s", failed[0])
	}
	for r := 1; r < 4; r++ {
		if strings.TrimSpace(failed[r]) != "" {
			t.Errorf("failed row %d = %q, want it blank above the baseline", r, failed[r])
		}
	}
	if top := strings.TrimSpace(failed[0]); top != "0/s" {
		t.Errorf("failed top row = %q, want only the peak, 0/s", top)
	}
	if bottom := failed[4]; strings.Count(bottom, "▁") != 9 || strings.ContainsAny(bottom, "▂▃▄▅▆▇█") || !strings.HasSuffix(bottom, "▁") {
		t.Errorf("failed bottom row = %q, want a baseline across the 5 minutes: 9 columns", bottom)
	}
}

// TestDetailChartsBlankUntilTheyArrive: the series are built by the sync
// command, so a screen that has none shows its charts blank — titles, rows
// and axis, so that the layout does not jump when they come — until it does.
func TestDetailChartsBlankUntilTheyArrive(t *testing.T) {
	m := historyFixture(t, time.Now()).model("multi")
	deliver(m, tea.WindowSizeMsg{Width: 120, Height: 80})
	m.pushScreen(newDetailScreen("orders", "instance-1"))

	for _, title := range []string{"Completed /s (1h)", "Failed /s (1h)"} {
		chart := chartOf(m, title)
		if len(chart) != 6 {
			t.Fatalf("%s: no chart area before the series arrive", title)
		}
		for r := range 5 {
			if chart[r] != "" {
				t.Errorf("%s row %d = %q before the series arrive, want it blank", title, r, chart[r])
			}
		}
		if !strings.HasSuffix(chart[5], "now") {
			t.Errorf("%s axis = %q", title, chart[5])
		}
	}
	failedAt := lineOf(m, "Failed /s (1h)")

	syncNow(t, m)
	if chart := chartOf(m, "Completed /s (1h)"); !strings.Contains(chart[4], "█") {
		t.Errorf("Completed row after the sync = %q, want it drawn", chart[4])
	}
	if got := lineOf(m, "Failed /s (1h)"); got != failedAt {
		t.Errorf("Failed /s (1h) is at line %d with the series, %d without: the layout jumped", got, failedAt)
	}
}

// countDetail makes m build its detail series through a counter, and returns it.
func countDetail(m *Model) *atomic.Int64 {
	var n atomic.Int64
	build := m.buildDetail
	m.buildDetail = func(snaps []*fleet.Snapshot, label, worker string) detailSeries {
		n.Add(1)
		return build(snaps, label, worker)
	}
	return &n
}

// TestDetailChartsBuiltInTheCommand: the series of the charts are built by the
// command, off the UI goroutine, once for each snapshot and view — not by
// Update, nor by View, a resize or a sync that brings no snapshot.
func TestDetailChartsBuiltInTheCommand(t *testing.T) {
	end := time.Now()
	f := historyFixture(t, end)
	m := f.model("multi")
	built := countDetail(m)

	// On the dashboard none are built: no screen shows them.
	syncNow(t, m)
	if n := built.Load(); n != 0 {
		t.Fatalf("detail series built %d times with no detail screen, want none", n)
	}

	// Opening the screen makes the command, which builds them; the press does not.
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if n := built.Load(); n != 0 {
		t.Fatalf("opening the screen built the series (%d)", n)
	}
	if cmd == nil {
		t.Fatal("opening the screen made no command")
	}
	msg := cmd()
	if n := built.Load(); n != 1 {
		t.Fatalf("running the command built the series %d times, want 1", n)
	}
	deliver(m, msg)
	_ = m.View()

	// Nothing new: the command finds the series it has; Update, View and a
	// resize build none.
	for range 3 {
		syncNow(t, m)
		_ = m.View()
	}
	for _, width := range []int{92, 106, 160, 80} {
		deliver(m, tea.WindowSizeMsg{Width: width, Height: 60})
		_ = m.View()
	}
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	_ = m.View()
	if n := built.Load(); n != 1 {
		t.Fatalf("series built %d times with no new snapshot, want still 1", n)
	}
	before := chartOf(m, "Completed /s (1h)")

	// A new snapshot: making the command builds nothing, running it does, and
	// Update takes the series in without building.
	at := end.Add(15 * time.Second).Round(time.Millisecond)
	f.write("multi", snapAt(at, failing(at, "instance-1", "http://a.invalid", 50, 2), failing(at, "instance-2", "http://b.invalid", 4, 0)))
	cmd = m.syncCmd(m.feed, m.gen)
	if n := built.Load(); n != 1 {
		t.Fatalf("making the sync command built the series (%d)", n)
	}
	msg = cmd()
	if n := built.Load(); n != 2 {
		t.Fatalf("running the sync command built the series %d times, want 2", n)
	}
	deliver(m, msg)
	_ = m.View()
	if n := built.Load(); n != 2 {
		t.Errorf("series built %d times by Update and View, want still 2", n)
	}
	if after := chartOf(m, "Completed /s (1h)"); strings.Fields(after[0])[0] == strings.Fields(before[0])[0] {
		t.Errorf("the peak did not change with the newest snapshot:\n%q\n%q", before, after)
	}
	if d := m.activeScreen().(*detailScreen); d.series.key.latest != m.latest() {
		t.Error("the series installed are not of the newest snapshot")
	}
}

// TestDetailChartsOfAnotherViewAreIgnored: series that are not of what the
// screen shows now — another feed, another unit or worker, an older snapshot —
// are not taken in, though their sync is: they would draw another unit's
// history.
func TestDetailChartsOfAnotherViewAreIgnored(t *testing.T) {
	end := time.Now()
	f := historyFixture(t, end)
	m := f.model("multi")
	openDetail(t, m)
	d := m.activeScreen().(*detailScreen)
	want := chartOf(m, "Completed /s (1h)")
	installed := d.series.key

	at := end.Add(15 * time.Second).Round(time.Millisecond)
	older := m.latest()
	f.write("multi", snapAt(at, failing(at, "instance-1", "http://a.invalid", 50, 2), failing(at, "instance-2", "http://b.invalid", 4, 0)))
	good := m.syncCmd(m.feed, m.gen)().(syncedMsg)
	if good.detail.key.feed == nil {
		t.Fatal("the sync of an open detail screen built no series")
	}
	otherFeed := store.NewFeed(f.dir, "multi", 7*time.Second)
	t.Cleanup(func() { _ = otherFeed.Close() })

	for _, tc := range []struct {
		name   string
		change func(*detailKey)
	}{
		{"another feed", func(k *detailKey) { k.feed = otherFeed }},
		{"another unit", func(k *detailKey) { k.label = "instance-2" }},
		{"another worker", func(k *detailKey) { k.worker = "billing" }},
		{"an older snapshot", func(k *detailKey) { k.latest = older }},
		{"no feed", func(k *detailKey) { k.feed = nil }},
	} {
		msg := good
		tc.change(&msg.detail.key)
		deliver(m, msg)
		if d.series.key != installed {
			t.Errorf("%s: the series were taken in", tc.name)
		}
		if got := chartOf(m, "Completed /s (1h)"); !slices.Equal(got, want) {
			t.Errorf("%s: the chart changed to %q", tc.name, got)
		}
	}

	deliver(m, good)
	if d.series.key == installed || d.series.key.latest != m.latest() {
		t.Error("the series of the newest snapshot were not taken in")
	}
	if got := chartOf(m, "Completed /s (1h)"); slices.Equal(got, want) {
		t.Error("the chart did not change with the newest snapshot")
	}
}

// TestDetailChartsNeverShowAnotherUnitsHistory: a screen opened on another
// unit, or after the server shown changed, draws blank until its own series
// come, not those held for the last.
func TestDetailChartsNeverShowAnotherUnitsHistory(t *testing.T) {
	m := historyFixture(t, time.Now()).model("multi")
	openDetail(t, m)
	if chart := chartOf(m, "Completed /s (1h)"); !strings.Contains(chart[4], "█") {
		t.Fatal("instance-1's chart is not drawn")
	}
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // instance-2's screen, its command not run yet
	if chart := chartOf(m, "Completed /s (1h)"); strings.TrimSpace(strings.Join(chart[:5], "")) != "" {
		t.Errorf("instance-2's chart before its series come = %q, want blank: instance-1's is not its", chart)
	}
}

// TestDetailChartsSkippedUnder20Columns: a chart is width − 4 columns wide and
// there is none under 20, titles and all: the screen has room for neither.
func TestDetailChartsSkippedUnder20Columns(t *testing.T) {
	f := historyFixture(t, time.Now())
	for _, tc := range []struct {
		width int
		shown bool
	}{{10, false}, {20, false}, {23, false}, {24, true}, {25, true}, {60, true}} {
		m := f.model("multi")
		deliver(m, tea.WindowSizeMsg{Width: tc.width, Height: 200})
		openDetail(t, m)
		view := m.View()
		for _, title := range []string{"Completed /s (1h)", "Failed /s (1h)"} {
			if got := strings.Contains(view, title); got != tc.shown {
				t.Errorf("%d columns: %q shown = %v, want %v", tc.width, title, got, tc.shown)
			}
		}
		if tc.shown {
			chart := chartOf(m, "Completed /s (1h)")
			if axis := chart[5]; utf8.RuneCountInString(axis) != tc.width-4 {
				t.Errorf("%d columns: the chart is %d wide, want %d: %q", tc.width, utf8.RuneCountInString(axis), tc.width-4, axis)
			}
		}
	}
}

// TestDetailChartsFitTheScreen: whatever the width, a chart line is no wider
// than the screen less the body's padding.
func TestDetailChartsFitTheScreen(t *testing.T) {
	m := historyFixture(t, time.Now()).model("multi")
	openDetail(t, m)
	for width := 24; width <= 220; width++ {
		deliver(m, tea.WindowSizeMsg{Width: width, Height: 200})
		for _, title := range []string{"Completed /s (1h)", "Failed /s (1h)"} {
			chart := chartOf(m, title)
			if chart == nil {
				t.Fatalf("%d columns: no %s chart", width, title)
			}
			for r, line := range chart {
				if w := utf8.RuneCountInString(line); w > width-4 {
					t.Fatalf("%d columns: %s line %d is %d wide: %q", width, title, r, w, line)
				}
			}
			if w := utf8.RuneCountInString(chart[5]); w != width-4 {
				t.Fatalf("%d columns: the axis is %d wide, want %d", width, w, width-4)
			}
		}
	}
}

// TestDetailChartsWhileStale: the history stays drawn when the newest
// snapshot is stale — the values are —, the charts are not.
func TestDetailChartsWhileStale(t *testing.T) {
	m := historyFixture(t, time.Now().Add(-time.Minute)).model("multi")
	openDetail(t, m)
	if !m.stale(time.Now()) {
		t.Fatal("the fixture is not stale")
	}
	chart := chartOf(m, "Completed /s (1h)")
	if chart == nil || !strings.Contains(chart[4], "█") || strings.Fields(chart[0])[0] != "12.5/s" {
		t.Errorf("stale chart = %q, want the history drawn", chart)
	}
}

// TestDetailChartsNeedASample: no chart where the screen shows a notice
// instead of the worker's values.
func TestDetailChartsNeedASample(t *testing.T) {
	for _, label := range []string{"instance-2", "instance-3", "instance-9"} {
		m := detailModel(t, label, 120, 60)
		if view := m.View(); strings.Contains(view, "(1h)") {
			t.Errorf("%s: a chart beside the notice:\n%s", label, view)
		}
	}
}

// TestChartPaint: a chart's gutter and axis are dim, its plot solid — but dim
// where its columns are partial, each run of columns painted at once. The
// styles here mark what they paint (a colour is not in a test's output).
func TestChartPaint(t *testing.T) {
	solid := lipgloss.NewStyle().Transform(func(s string) string { return "{" + s + "}" })
	dim := lipgloss.NewStyle().Transform(func(s string) string { return "(" + s + ")" })

	lines, partial := blockPlot([]fleet.Point{{V: 1}, {V: 2, Partial: true}, {V: 3, Partial: true}, {V: 4}}, 20, 1)
	got := plot{lines, partial}.paint(solid, dim)
	want := "(  4.0/s )" + "{" + strings.Repeat(" ", 8) + "▂}" + "(▄▆)" + "{█}" + "\n" +
		"(" + strings.Repeat(" ", 10) + "-60m … now)"
	if got != want {
		t.Errorf("painted chart =\n%s\nwant\n%s", got, want)
	}
}

// TestDetailChartsDrawnByUpdate: the charts are drawn when the series or the
// width change, by Update, and View prints that drawing: it makes none while
// they stay what they are.
func TestDetailChartsDrawnByUpdate(t *testing.T) {
	f := historyFixture(t, time.Now())
	m := f.model("multi")
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // opened: its command is not run yet
	d := m.activeScreen().(*detailScreen)
	if d.drawn.key.series.feed != nil {
		t.Fatal("charts drawn of series that have not arrived")
	}
	if d.drawn.key.width != m.width-4 {
		t.Fatalf("on opening, the (blank) charts are drawn for width %d, want %d: View would draw them", d.drawn.key.width, m.width-4)
	}

	// The series arrive: Update draws them, for the width, with no View.
	deliver(m, m.syncCmd(m.feed, m.gen)())
	if d.drawn.key.series != d.series.key || d.drawn.key.series.feed == nil || d.drawn.key.width != m.width-4 {
		t.Fatalf("after the sync the drawing is of %+v, want the series and the width, %d", d.drawn.key, m.width-4)
	}
	first := d.drawn.plots[0].lines
	for range 3 {
		_ = m.View()
		syncNow(t, m)
	}
	if &d.drawn.plots[0].lines[0] != &first[0] {
		t.Error("View or a sync that brought nothing drew the charts again")
	}

	// A resize: Update draws them for the new width.
	deliver(m, tea.WindowSizeMsg{Width: 90, Height: 60})
	if d.drawn.key.width != 86 || lipgloss.Width(d.drawn.plots[0].lines[0]) != 86 {
		t.Errorf("after a resize to 90 columns the drawing is of width %d, want 86", d.drawn.key.width)
	}
}
