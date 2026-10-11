package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

func testModel(t *testing.T) *Model {
	t.Helper()
	server := &config.Server{
		Name:      "local",
		Endpoints: []string{"http://localhost:9000"},
		Interval:  5,
	}
	cfg := &config.Config{
		DefaultServer: "local",
		Servers:       map[string]*config.Server{"local": server},
	}
	return NewModel(cfg, server)
}

// press delivers a key to the root model and copies the returned model back,
// since Model.Update uses a value receiver.
func press(m *Model, key tea.KeyMsg) tea.Cmd {
	next, cmd := m.Update(key)
	*m = next.(Model)
	return cmd
}

func TestScreenStackStartsAtDashboard(t *testing.T) {
	m := testModel(t)
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen = %T, want *dashboardScreen", m.activeScreen())
	}
	if len(m.screens) != 1 {
		t.Fatalf("initial stack length = %d, want 1", len(m.screens))
	}
}

// TestScreenStackPushPop mirrors the old flag matrix transitions: dashboard is
// the base, overlays are pushed on top and popped back to it.
func TestScreenStackPushPop(t *testing.T) {
	m := testModel(t)

	m.pushScreen(newDetailScreen("worker-1", "http://localhost:9000"))
	m.pushScreen(m.switcher)

	if _, ok := m.activeScreen().(*switcherScreen); !ok {
		t.Fatalf("active screen after pushes = %T, want *switcherScreen", m.activeScreen())
	}

	m.popScreen()
	if _, ok := m.activeScreen().(*detailScreen); !ok {
		t.Fatalf("active screen after 1 pop = %T, want *detailScreen", m.activeScreen())
	}

	m.popScreen()
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen after 2 pops = %T, want *dashboardScreen", m.activeScreen())
	}

	// The dashboard is the base: popping it is a no-op, mirroring the old flag
	// logic where esc on the dashboard did nothing.
	m.popScreen()
	if len(m.screens) != 1 {
		t.Fatalf("stack length after popping base = %d, want 1", len(m.screens))
	}
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen after popping base = %T, want *dashboardScreen", m.activeScreen())
	}
}

func TestTabPushesSwitcherAndEscPops(t *testing.T) {
	m := testModel(t)

	press(m, tea.KeyMsg{Type: tea.KeyTab})
	if _, ok := m.activeScreen().(*switcherScreen); !ok {
		t.Fatalf("active screen after tab = %T, want *switcherScreen", m.activeScreen())
	}

	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen after esc = %T, want *dashboardScreen", m.activeScreen())
	}
}

func TestDashboardQuitKey(t *testing.T) {
	m := testModel(t)
	cmd := press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q returned nil command, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q command produced %T, want tea.QuitMsg", cmd())
	}
}

// TestConstructionErrorStaysVisible covers a failed pool/source build (e.g. a
// bad kube context): the error must render while the dashboard is offline and
// must survive a successful worker fetch (which otherwise clears m.err).
func TestConstructionErrorStaysVisible(t *testing.T) {
	m := testModel(t)
	m.pool = nil
	m.isHealthy = false
	m.err = errors.New(`building kube client for context "x": boom`)
	m.sourceErr = true

	if view := m.View(); !strings.Contains(view, "building kube client") {
		t.Fatalf("offline view missing construction error:\n%s", view)
	}

	m.handleWorkers(nil)
	if m.err == nil {
		t.Fatal("handleWorkers cleared a construction error")
	}

	// The flag is the switch that lets a normal fetch error clear again.
	m.sourceErr = false
	m.handleWorkers(nil)
	if m.err != nil {
		t.Fatalf("non-construction error not cleared: %v", m.err)
	}
}
