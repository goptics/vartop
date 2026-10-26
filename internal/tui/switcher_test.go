package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
)

// switcherFixture's config holds three servers: "local" (one endpoint),
// "multi" (three) and "solo" (one), each with a snapshot of its units
// stored. The sorted order is local, multi, solo.
func switcherFixture(t *testing.T) *fixture {
	t.Helper()
	multi := []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080", "http://10.0.0.3:8080"}
	f := newFixture(t, &config.Config{DefaultServer: "local", Servers: map[string]*config.Server{
		"local": {Endpoints: []string{"http://localhost:9000"}, Interval: 5},
		"multi": {Endpoints: multi, Interval: 5},
		"solo":  {Endpoints: []string{"http://solo:1"}, Interval: 5},
	}})
	f.claim()
	orders := func() map[string]*fleet.Sample { return map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)} }
	f.write("local", fresh(unit("instance-1", "http://localhost:9000", orders())))
	f.write("multi", fresh(
		unit("instance-1", multi[0], orders()),
		unit("instance-2", multi[1], orders()),
		unit("instance-3", multi[2], orders()),
	))
	f.write("solo", fresh(unit("instance-1", "http://solo:1", orders())))
	return f
}

// switcherTestModel is a sized model over switcherFixture, showing "local".
func switcherTestModel(t *testing.T) *Model {
	t.Helper()
	m := switcherFixture(t).model("local")
	deliver(m, tea.WindowSizeMsg{Width: 80, Height: 40})
	return m
}

// openSwitcher presses tab, and delivers what the switcher read from the
// store.
func openSwitcher(t *testing.T, m *Model) {
	t.Helper()
	cmd := press(m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.isSwitcherActive() {
		t.Fatalf("active screen after tab = %T, want *switcherScreen", m.activeScreen())
	}
	if cmd == nil {
		t.Fatal("tab returned no command, want the switcher's store read")
	}
	deliver(m, cmd())
}

// selectSwitcherItem moves the cursor to the top, then down with KeyDown
// until match is true for the highlighted item.
func selectSwitcherItem(t *testing.T, m *Model, match func(any) bool) {
	t.Helper()
	n := len(m.switcher.switcher.Items())
	for m.switcher.switcher.Index() > 0 {
		press(m, tea.KeyMsg{Type: tea.KeyUp})
	}
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

// TestSwitcherHealthFromStore: the switcher shows each configured server's
// units and their health from its newest stored snapshot, kept current
// while it is open; a server with nothing stored shows no count. Switching
// is instant: a server's feed, once opened, is kept, so going back shows
// its rows at once.
func TestSwitcherHealthFromStore(t *testing.T) {
	f := switcherFixture(t)
	f.setConfig(&config.Config{DefaultServer: "local", Servers: map[string]*config.Server{
		"local": {Endpoints: []string{"http://localhost:9000"}, Interval: 5},
		"multi": {Endpoints: []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080", "http://10.0.0.3:8080"}, Interval: 5},
		"solo":  {Endpoints: []string{"http://solo:1"}, Interval: 5},
		"empty": {Endpoints: []string{"http://empty:1"}, Interval: 5},
	}})
	f.write("multi", fresh(
		unit("instance-1", "http://10.0.0.1:8080", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		downUnit("instance-2", "http://10.0.0.2:8080", "connection refused"),
		unit("instance-3", "http://10.0.0.3:8080", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
	))
	m := f.model("local")
	reloadConfig(t, m)
	openSwitcher(t, m)

	titles := func() string {
		var s []string
		for _, it := range m.switcher.switcher.Items() {
			s = append(s, it.(interface{ Title() string }).Title())
		}
		return strings.Join(s, "\n")
	}
	got := titles()
	for _, want := range []string{"multi (3 instances, 2 healthy)", "▶ empty", "▶ solo"} {
		if !strings.Contains(got, want) {
			t.Errorf("switcher lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "empty (") {
		t.Errorf("a server with nothing stored shows a count:\n%s", got)
	}

	selectSwitcherItem(t, m, serverItem("multi"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // expand
	var units []string
	for _, p := range podItems(m) {
		units = append(units, p.label)
		if want := p.label != "instance-2"; p.healthy != want {
			t.Errorf("%s healthy = %v, want %v", p.label, p.healthy, want)
		}
	}
	if want := []string{"instance-1", "instance-2", "instance-3"}; !slices.Equal(units, want) {
		t.Fatalf("multi's units = %v, want %v", units, want)
	}

	// The store moves on while the switcher is open: a tick reads it again.
	f.write("multi", fresh(
		unit("instance-1", "http://10.0.0.1:8080", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		unit("instance-2", "http://10.0.0.2:8080", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		unit("instance-3", "http://10.0.0.3:8080", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
	))
	at := m.switcher.switcher.Index()
	deliver(m, m.switcherCmd()())
	if got := titles(); !strings.Contains(got, "multi (3 instances, 3 healthy)") {
		t.Errorf("the switcher kept old health:\n%s", got)
	}
	if got := m.switcher.switcher.Index(); got != at {
		t.Errorf("the cursor moved from %d to %d when the switcher read the store again", at, got)
	}

	// Selecting a unit shows it, on its server.
	selectSwitcherItem(t, m, func(it any) bool { p, ok := it.(switchPodItem); return ok && p.label == "instance-3" })
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.isSwitcherActive() || m.key != "multi" || m.selectedLabel != "instance-3" {
		t.Fatalf("after selecting instance-3: switcher %v, server %q, label %q", m.isSwitcherActive(), m.key, m.selectedLabel)
	}
	if cmd == nil {
		t.Fatal("selecting a unit returned no command, want its feed's sync")
	}
	deliver(m, cmd())
	if got, want := rowsOf(m), []string{"orders|Running|4|1|0|3"}; !slices.Equal(got, want) {
		t.Fatalf("instance-3's rows = %v, want %v", got, want)
	}

	// Back to local: its feed is still open, its rows there before any sync.
	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("local"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.key != "local" {
		t.Fatalf("server = %q, want local", m.key)
	}
	if got, want := rowsOf(m), []string{"orders|instance-1|Running|4|1|0|3"}; !slices.Equal(got, want) {
		t.Fatalf("local's rows at once = %v, want %v", got, want)
	}
}

// TestSwitcherKubernetesServer: on a Kubernetes server the units are pods,
// labelled by pod name, and the switcher counts them as such.
func TestSwitcherKubernetesServer(t *testing.T) {
	f := newFixture(t, &config.Config{DefaultServer: "k8s", Servers: map[string]*config.Server{
		"k8s": {Kubernetes: &config.KubernetesRef{Context: "prod", Namespace: "jobs", Service: "app"}, Interval: 5},
	}})
	f.claim()
	f.write("k8s", fresh(
		unit("app-a", "http://10.0.0.1:8080/varmq", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
		unit("app-b", "http://10.0.0.2:8080/varmq", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}),
	))
	m := f.model("k8s")
	if got, want := rowsOf(m), []string{"orders|app-a|Running|4|1|0|3", "orders|app-b|Running|4|1|0|3"}; !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if view := m.View(); !strings.Contains(view, "2/2 pods") || !strings.Contains(view, "prod/jobs") {
		t.Errorf("header:\n%s", view)
	}

	openSwitcher(t, m)
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // expand
	var pods []string
	for _, it := range m.switcher.switcher.Items() {
		switch it := it.(type) {
		case switchServerItem:
			if !strings.Contains(it.Title(), "k8s (2 pods, 2 healthy)") {
				t.Errorf("server row = %q", it.Title())
			}
		case switchPodItem:
			pods = append(pods, it.label)
		}
	}
	if want := []string{"app-a", "app-b"}; !slices.Equal(pods, want) {
		t.Fatalf("pod rows = %v, want %v", pods, want)
	}
}

func TestSwitcherEnterSingleEndpointSelectsServer(t *testing.T) {
	m := switcherTestModel(t)
	m.selectedLabel = "instance-9"
	m.showNodeColumn = false
	gen := m.gen

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
		t.Fatal("enter returned nil command, want the server's sync")
	}
	if m.selectedLabel != "" {
		t.Fatalf("selectedLabel = %q, want empty", m.selectedLabel)
	}
	if !m.showNodeColumn {
		t.Fatal("showNodeColumn = false, want true after merged-view reset")
	}
	if m.key != "solo" || m.gen != gen+1 {
		t.Fatalf("server %q gen %d, want solo, %d (one switch)", m.key, m.gen, gen+1)
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
			t.Fatalf("pod %q belongs to %q, want multi", p.label, p.server)
		}
	}

	// The cursor stays on the server row; second enter selects the merged view.
	m.selectedLabel = "instance-9"
	m.showNodeColumn = false
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.isSwitcherActive() {
		t.Fatal("switcher still active after second enter")
	}
	if cmd == nil {
		t.Fatal("second enter returned nil command, want the server's sync")
	}
	if m.selectedLabel != "" || !m.showNodeColumn {
		t.Fatalf("merged view not restored: label=%q showNode=%v", m.selectedLabel, m.showNodeColumn)
	}
}

func TestSwitcherEnterPodSelectsSinglePod(t *testing.T) {
	m := switcherTestModel(t)
	openSwitcher(t, m)
	selectSwitcherItem(t, m, serverItem("multi"))
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // expand

	const want = "instance-2"
	selectSwitcherItem(t, m, func(it any) bool {
		p, ok := it.(switchPodItem)
		return ok && p.label == want
	})
	cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.isSwitcherActive() {
		t.Fatal("switcher still active after selecting a pod")
	}
	if m.selectedLabel != want || m.key != "multi" {
		t.Fatalf("shows %q on %q, want %q on multi", m.selectedLabel, m.key, want)
	}
	if m.showNodeColumn {
		t.Fatal("showNodeColumn = true, want false in single-pod view")
	}
	if cmd == nil {
		t.Fatal("pod enter returned nil command, want the server's sync")
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

	deliver(m, tea.WindowSizeMsg{Width: 20, Height: 5})
	if view := m.View(); view == "" {
		t.Fatal("tiny switcher view is empty")
	}
	// Keys keep working at tiny sizes.
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, tea.KeyMsg{Type: tea.KeyUp})
	_ = m.View()
}
