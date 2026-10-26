package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
)

// localConfig is one server, "local": a single endpoint polled every 5s.
func localConfig() *config.Config {
	return &config.Config{DefaultServer: "local", Servers: map[string]*config.Server{
		"local": {Endpoints: []string{"http://localhost:9000"}, Interval: 5},
	}}
}

// testModel is a TUI over a store with nothing in it yet, showing "local".
func testModel(t *testing.T) *Model {
	t.Helper()
	return newFixture(t, localConfig()).model("local")
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

	m.pushScreen(newDetailScreen("worker-1", "instance-1"))
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

	if cmd := press(m, tea.KeyMsg{Type: tea.KeyTab}); cmd == nil {
		t.Fatal("tab returned no command: the switcher reads the store")
	}
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
	if cmd := press(m, runeKey('q')); !isQuit(cmd) {
		t.Fatal("q did not return tea.Quit")
	}
}

func TestSwitcherOrderStable(t *testing.T) {
	m := testModel(t)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		m.cfg.Servers[n] = &config.Server{Name: n, Endpoints: []string{"http://" + n + ":1"}}
	}
	for i := 0; i < 20; i++ {
		m.switcher.rebuild(m)
		var names []string
		for _, it := range m.switcher.switcher.Items() {
			names = append(names, it.(switchServerItem).name)
		}
		if got := names; len(got) != 4 || got[0] != "alpha" || got[1] != "local" || got[2] != "mid" || got[3] != "zeta" {
			t.Fatalf("switcher order = %v", names)
		}
	}
}
