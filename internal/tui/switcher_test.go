package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
)

// runeKey builds a printable-rune key message such as "j" or "q".
func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// isQuit reports whether cmd is tea.Quit.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// switcherTestModel returns a sized model whose config holds three servers:
// "local" (single endpoint, active), "multi" (3 static endpoints) and "solo"
// (single endpoint). The sorted order is local, multi, solo.
func switcherTestModel(t *testing.T) *Model {
	t.Helper()
	m := testModel(t)
	m.cfg.Servers["multi"] = &config.Server{
		Name:      "multi",
		Endpoints: []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080", "http://10.0.0.3:8080"},
	}
	m.cfg.Servers["solo"] = &config.Server{Name: "solo", Endpoints: []string{"http://solo:1"}}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	*m = next.(Model)
	m.switcher.rebuild(m)
	return m
}

func openSwitcher(t *testing.T, m *Model) {
	t.Helper()
	press(m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.isSwitcherActive() {
		t.Fatalf("active screen after tab = %T, want *switcherScreen", m.activeScreen())
	}
}

// selectSwitcherItem moves the cursor with KeyDown until match is true for the
// highlighted item.
func selectSwitcherItem(t *testing.T, m *Model, match func(any) bool) {
	t.Helper()
	n := len(m.switcher.switcher.Items())
	for i := 0; i < n; i++ {
		if match(m.switcher.switcher.SelectedItem()) {
			return
		}
		press(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if match(m.switcher.switcher.SelectedItem()) {
		return
	}
	t.Fatalf("no matching switcher item among %d items", n)
}

func serverItem(name string) func(any) bool {
	return func(it any) bool {
		s, ok := it.(switchServerItem)
		return ok && s.name == name
	}
}

func podItems(m *Model) []switchPodItem {
	var pods []switchPodItem
	for _, it := range m.switcher.switcher.Items() {
		if p, ok := it.(switchPodItem); ok {
			pods = append(pods, p)
		}
	}
	return pods
}

func TestSwitcherEnterSingleEndpointSelectsServer(t *testing.T) {
	m := switcherTestModel(t)
	m.selectedPodOrigin = "http://stale:1"
	m.showNodeColumn = false

	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("solo"))
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.isSwitcherActive() {
		t.Fatal("switcher still active after selecting a single-endpoint server")
	}
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("active screen = %T, want *dashboardScreen", m.activeScreen())
	}
	if cmd == nil {
		t.Fatal("enter returned nil command, want a server switch command")
	}
	if m.selectedPodOrigin != "" {
		t.Fatalf("selectedPodOrigin = %q, want empty", m.selectedPodOrigin)
	}
	if !m.showNodeColumn {
		t.Fatal("showNodeColumn = false, want true after merged-view reset")
	}
	if m.switchSeq != 1 {
		t.Fatalf("switchSeq = %d, want 1 (one connect issued)", m.switchSeq)
	}
}

func TestSwitcherMultiEndpointExpandsThenSelectsMerged(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("multi"))

	if cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("first enter on a multi-endpoint server returned a command, want expand only")
	}
	if !m.isSwitcherActive() {
		t.Fatal("switcher closed on first enter, want it to stay open and expand")
	}
	if m.switcher.expanded != "multi" {
		t.Fatalf("expanded = %q, want multi", m.switcher.expanded)
	}
	pods := podItems(m)
	if len(pods) != 3 {
		t.Fatalf("pod rows = %d, want 3", len(pods))
	}
	for _, p := range pods {
		if p.server != "multi" {
			t.Fatalf("pod %q belongs to %q, want multi", p.name, p.server)
		}
	}

	// The cursor stays on the server row; second enter selects the merged view.
	m.selectedPodOrigin = "http://stale:1"
	m.showNodeColumn = false
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.isSwitcherActive() {
		t.Fatal("switcher still active after second enter")
	}
	if cmd == nil {
		t.Fatal("second enter returned nil command, want a server switch command")
	}
	if m.selectedPodOrigin != "" || !m.showNodeColumn {
		t.Fatalf("merged view not restored: origin=%q showNode=%v", m.selectedPodOrigin, m.showNodeColumn)
	}
}

func TestSwitcherEnterPodSelectsSinglePod(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("multi"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // expand

	const want = "http://10.0.0.2:8080"
	selectSwitcherItem(t, m, func(it any) bool {
		p, ok := it.(switchPodItem)
		return ok && p.origin == want
	})
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.isSwitcherActive() {
		t.Fatal("switcher still active after selecting a pod")
	}
	if m.selectedPodOrigin != want {
		t.Fatalf("selectedPodOrigin = %q, want %q", m.selectedPodOrigin, want)
	}
	if m.showNodeColumn {
		t.Fatal("showNodeColumn = true, want false in single-pod view")
	}
	if cmd == nil {
		t.Fatal("pod enter returned nil command, want a server switch command")
	}
}

func TestSwitcherCollapseKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"h", runeKey('h')},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := switcherTestModel(t)
			openSwitcher(t, m)
			selectSwitcherItem(t, m, serverItem("multi"))
			press(m, tea.KeyMsg{Type: tea.KeyEnter})
			if len(podItems(m)) != 3 {
				t.Fatalf("precondition: want 3 pod rows, got %d", len(podItems(m)))
			}

			if cmd := press(m, tc.key); cmd != nil {
				t.Fatalf("%s returned a command, want nil", tc.name)
			}
			if m.switcher.expanded != "" {
				t.Fatalf("expanded = %q after %s, want collapsed", m.switcher.expanded, tc.name)
			}
			if len(podItems(m)) != 0 {
				t.Fatalf("pod rows after %s = %d, want 0", tc.name, len(podItems(m)))
			}
			if !m.isSwitcherActive() {
				t.Fatalf("%s on an expanded server closed the switcher, want it to stay open", tc.name)
			}
		})
	}
}

func TestSwitcherEscClosesWhenCollapsed(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.isSwitcherActive() {
		t.Fatal("esc on a collapsed switcher did not close it")
	}
}

func TestSwitcherCollapseKeysNoopWhenCollapsed(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	press(m, runeKey('h'))
	press(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if !m.isSwitcherActive() {
		t.Fatal("collapse keys closed the switcher while collapsed")
	}
	if m.switcher.expanded != "" {
		t.Fatalf("expanded = %q, want empty", m.switcher.expanded)
	}
}

func TestSwitcherQuit(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	if cmd := press(m, runeKey('q')); !isQuit(cmd) {
		t.Fatal("q in switcher did not return tea.Quit")
	}
}

func TestSwitcherNavigationMovesCursor(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	if got := m.switcher.switcher.Index(); got != 0 {
		t.Fatalf("initial index = %d, want 0", got)
	}
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.switcher.switcher.Index(); got != 1 {
		t.Fatalf("index after down = %d, want 1", got)
	}
	press(m, tea.KeyMsg{Type: tea.KeyUp})
	if got := m.switcher.switcher.Index(); got != 0 {
		t.Fatalf("index after up = %d, want 0", got)
	}
}

func TestSwitcherViewShowsServerNames(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	view := m.View()
	for _, name := range []string{"local", "multi", "solo"} {
		if !strings.Contains(view, name) {
			t.Fatalf("switcher view missing server %q:\n%s", name, view)
		}
	}

	// Expanded footer advertises collapse.
	selectSwitcherItem(t, m, serverItem("multi"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if view := m.View(); !strings.Contains(view, "collapse") {
		t.Fatalf("expanded switcher footer missing collapse hint:\n%s", view)
	}
}

func TestSwitcherViewTinySize(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("multi"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter})

	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	*m = next.(Model)
	if view := m.View(); view == "" {
		t.Fatal("tiny switcher view is empty")
	}
	// Keys keep working at tiny sizes.
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, tea.KeyMsg{Type: tea.KeyUp})
	_ = m.View()
}
