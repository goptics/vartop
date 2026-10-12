package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

const (
	detailTestWorker = "email-sender"
	detailTestOrigin = "http://localhost:9000"
)

func detailTestPayload() *api.WorkerDetails {
	d := &api.WorkerDetails{}
	d.Name = detailTestWorker
	d.Status = "Running"
	d.Concurrency = 7
	d.NumProcessing = 3
	d.NumPending = 11
	d.NumIdleWorkers = 4
	d.Origin = detailTestOrigin
	d.Metrics.Submitted = 1234
	d.Metrics.Completed = 1200
	d.Metrics.Successful = 1100
	d.Metrics.Failed = 100
	return d
}

// detailTestModel opens the detail screen for a worker (details still nil).
func detailTestModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := testModel(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	*m = next.(Model)
	m.pushScreen(newDetailScreen(detailTestWorker, detailTestOrigin))
	return m
}

// loadDetails delivers a workerDetailsMsg through Update.
func loadDetails(t *testing.T, m *Model) {
	t.Helper()
	next, _ := m.Update(workerDetailsMsg(detailTestPayload()))
	*m = next.(Model)
}

func loadedDetailModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := detailTestModel(t, width, height)
	loadDetails(t, m)
	return m
}

func TestDetailViewLoadingState(t *testing.T) {
	m := detailTestModel(t, 80, 24)
	if !m.isDetailActive() {
		t.Fatal("detail screen not active")
	}
	want := "Loading worker details for " + detailTestWorker
	if view := m.View(); !strings.Contains(view, want) {
		t.Fatalf("view missing %q:\n%s", want, view)
	}
}

func TestDetailViewShowsWorkerAndMetrics(t *testing.T) {
	m := loadedDetailModel(t, 120, 60)
	view := m.View()
	if strings.Contains(view, "Loading worker details") {
		t.Fatalf("still showing loading state after details arrived:\n%s", view)
	}
	for _, want := range []string{
		detailTestWorker, "Worker Details", "Submitted", "1234", "Completed", "1200",
		"Successful", "1100", "Failed", "100", "Success Rate", "91.7%",
		"Concurrency", "7 workers", "11 jobs",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewNarrowLayoutShowsWorker(t *testing.T) {
	m := loadedDetailModel(t, 60, 80)
	view := m.View()
	for _, want := range []string{detailTestWorker, "Server: local", "Submitted", "1234"} {
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

func TestDetailScrollKeysBeforeDetailsLoad(t *testing.T) {
	m := detailTestModel(t, 80, 15)
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyDown}, {Type: tea.KeyPgDown}, runeKey('G'), {Type: tea.KeyPgUp}, {Type: tea.KeyUp}, runeKey('g'),
	} {
		press(m, k)
		if view := m.View(); !strings.Contains(view, "Loading worker details") {
			t.Fatalf("loading view lost after %q:\n%s", k.String(), view)
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

func TestDetailActionKeyWithoutPoolReportsError(t *testing.T) {
	m := loadedDetailModel(t, 120, 40)
	press(m, runeKey('a'))
	cmd := press(m, runeKey('p'))
	msg := cmd()
	scoped, ok := msg.(scopedMsg)
	if !ok {
		t.Fatalf("command produced %T, want scopedMsg", msg)
	}
	if _, ok := scoped.msg.(errMsg); !ok {
		t.Fatalf("scoped payload = %T, want errMsg (no pool connected)", scoped.msg)
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

func TestDetailEscPopsToDashboardWithRefresh(t *testing.T) {
	m := loadedDetailModel(t, 80, 24)
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen = %T, want *dashboardScreen", m.activeScreen())
	}
	if cmd == nil {
		t.Fatal("esc returned nil command, want a refresh command")
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

func TestDetailRefreshShowsLoadingAgain(t *testing.T) {
	m := loadedDetailModel(t, 80, 24)
	cmd := press(m, runeKey('r'))
	if cmd == nil {
		t.Fatal("r returned nil command, want a details fetch")
	}
	if view := m.View(); !strings.Contains(view, "Loading worker details") {
		t.Fatalf("view after refresh is not the loading state:\n%s", view)
	}
}

func TestResizeAllScreensDoesNotPanic(t *testing.T) {
	widths := []int{20, 80, 200}
	heights := []int{5, 24, 60}

	screens := []struct {
		name  string
		setup func(t *testing.T) *Model
	}{
		{"dashboard", func(t *testing.T) *Model {
			m := testModel(t)
			m.handleWorkers(workersMsg{
				{Name: "orders", Status: "Running", Origin: "http://10.0.0.1:8080"},
				{Name: "emails", Status: "Paused", Origin: "http://10.0.0.2:8080"},
			})
			return m
		}},
		{"switcher", func(t *testing.T) *Model {
			m := switcherTestModel(t)
			openSwitcher(t, m)
			selectSwitcherItem(t, m, serverItem("multi"))
			press(m, tea.KeyMsg{Type: tea.KeyEnter})
			return m
		}},
		{"detail-loading", func(t *testing.T) *Model {
			return detailTestModel(t, 80, 24)
		}},
		{"detail", func(t *testing.T) *Model {
			return loadedDetailModel(t, 80, 24)
		}},
		{"detail-action", func(t *testing.T) *Model {
			m := loadedDetailModel(t, 80, 24)
			press(m, runeKey('a'))
			return m
		}},
	}

	for _, sc := range screens {
		for _, w := range widths {
			for _, h := range heights {
				t.Run(fmt.Sprintf("%s/%dx%d", sc.name, w, h), func(t *testing.T) {
					m := sc.setup(t)
					next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
					*m = next.(Model)
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

func TestConnectionLabelDNS(t *testing.T) {
	m := testModel(t)
	m.server = &config.Server{Name: "d", DNS: &config.DNSRef{Host: "varmq", Port: 8080}}
	if got := m.connectionLabel(); got != "dns:varmq:8080" {
		t.Fatalf("connectionLabel = %q, want dns:varmq:8080", got)
	}
	if view := m.View(); !strings.Contains(view, "dns:varmq:8080") {
		t.Fatalf("dashboard view missing dns label:\n%s", view)
	}
}

func TestConnectionLabelKubernetes(t *testing.T) {
	m := testModel(t)
	m.server = &config.Server{
		Name:       "k",
		Kubernetes: &config.KubernetesRef{Context: "ctx", Namespace: "ns", Service: "app"},
	}
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
	m.server = &config.Server{Name: "m", Endpoints: []string{"http://a:1", "http://b:1"}}
	if got := m.connectionLabel(); got != "2 pods" {
		t.Fatalf("static multi endpoint label = %q, want 2 pods", got)
	}
	m.selectedPodOrigin = "http://b:1"
	if got := m.connectionLabel(); got != "b" {
		t.Fatalf("selected pod label = %q, want b", got)
	}
}
