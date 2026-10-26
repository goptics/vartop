package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
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
	widths := []int{20, 80, 200}
	heights := []int{5, 24, 60}

	// The stores, made once; each subtest gets a model of its own.
	detail, switcher, empty := detailFixture(t), switcherFixture(t), newFixture(t, localConfig())
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
