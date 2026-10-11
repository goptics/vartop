package tui

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
	"github.com/goptics/varmq/cmd/vartop/internal/discovery"
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

// TestActionRoutesToSelectedRowPod covers a cluster where every pod runs the
// same worker name: the selected row's pod, not the last one fetched, must be
// the action/detail target.
func TestActionRoutesToSelectedRowPod(t *testing.T) {
	m := testModel(t)
	m.handleWorkers(workersMsg{
		{Name: "orders", Origin: "http://10.0.0.1:8080/varmq"},
		{Name: "orders", Origin: "http://10.0.0.2:8080/varmq"},
		{Name: "orders", Origin: "http://10.0.0.3:8080/varmq"},
	})
	d := m.dashboardScreen()
	d.table.SetCursor(0)
	if got := d.selectedWorkerOrigin(); got != "http://10.0.0.1:8080/varmq" {
		t.Fatalf("row 0 origin = %q, want pod 10.0.0.1", got)
	}
	d.table.SetCursor(1)
	if got := d.selectedWorkerOrigin(); got != "http://10.0.0.2:8080/varmq" {
		t.Fatalf("row 1 origin = %q, want pod 10.0.0.2", got)
	}
}

// TestStaleResultsDropped covers replies from a pool that was replaced by a
// server switch: they must not overwrite the new server's table.
func TestStaleResultsDropped(t *testing.T) {
	m := testModel(t)
	m.pool = api.NewPool(discovery.NewStatic([]string{"http://new:1"}), "")
	old := api.NewPool(discovery.NewStatic([]string{"http://old:1"}), "")

	next, _ := m.Update(scopedMsg{pool: old, msg: workersMsg{{Name: "stale", Origin: "http://old:1"}}})
	*m = next.(Model)
	if rows := m.dashboardScreen().table.Rows(); len(rows) != 0 {
		t.Fatalf("stale pool result applied: %v", rows)
	}

	next, _ = m.Update(scopedMsg{pool: m.pool, msg: workersMsg{{Name: "fresh", Origin: "http://new:1"}}})
	*m = next.(Model)
	if rows := m.dashboardScreen().table.Rows(); len(rows) != 1 || rows[0][0] != "fresh" {
		t.Fatalf("current pool result not applied: %v", rows)
	}
}

// TestSupersededSwitchDiscarded covers two quick switches where the first
// (slow) build finishes last: it must be dropped, not applied.
func TestSupersededSwitchDiscarded(t *testing.T) {
	m := testModel(t)
	other := &config.Server{Name: "other", Endpoints: []string{"http://other:1"}}
	m.cfg.Servers["other"] = other

	m.connect(m.server) // seq 1, superseded
	m.connect(other)    // seq 2, latest

	stale := api.NewPool(discovery.NewStatic([]string{"http://localhost:9000"}), "")
	m.handleServerSwitch(serverSwitchMsg{seq: 1, srv: m.server, pool: stale})
	if m.pool == stale {
		t.Fatal("superseded switch was applied")
	}

	latest := api.NewPool(discovery.NewStatic([]string{"http://other:1"}), "")
	m.handleServerSwitch(serverSwitchMsg{seq: 2, srv: other, pool: latest})
	if m.pool != latest || m.server != other {
		t.Fatal("latest switch was not applied")
	}
}

// TestRefreshRetriesFailedSource covers r after the source failed to build:
// it must retry the build, not just hide the error.
func TestRefreshRetriesFailedSource(t *testing.T) {
	m := testModel(t)
	m.err = errors.New("boom")
	m.sourceErr = true

	cmd := press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("refresh returned nil command")
	}
	msg, ok := cmd().(serverSwitchMsg)
	if !ok {
		t.Fatalf("refresh produced %T, want serverSwitchMsg", msg)
	}
	next, _ := m.Update(msg)
	*m = next.(Model)
	if m.pool == nil {
		t.Fatal("refresh did not rebuild the pool")
	}
	_ = m.Close()
}

func TestConnectingStateBeforePool(t *testing.T) {
	m := testModel(t)
	if view := m.View(); !strings.Contains(view, "Connecting to") {
		t.Fatalf("missing connecting state:\n%s", view)
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
		if strings.Join(names, ",") != "alpha,local,mid,zeta" {
			t.Fatalf("switcher order = %v", names)
		}
	}
}

// fakeK8sSource stands in for a kubernetes discovery source: live endpoints
// plus pod names.
type fakeK8sSource struct{ pods map[string]string }

func (f fakeK8sSource) Endpoints() []string {
	return slices.Sorted(maps.Keys(f.pods))
}
func (f fakeK8sSource) Close() error              { return nil }
func (f fakeK8sSource) PodName(url string) string { return f.pods[url] }

// TestSwitcherKubernetesServer covers a kubernetes server, which has no static
// endpoints: when active, its pods come from the live pool and are labeled by
// pod name; when inactive, it shows no count.
func TestSwitcherKubernetesServer(t *testing.T) {
	m := testModel(t)
	k8s := &config.Server{Name: "k8s", Kubernetes: &config.KubernetesRef{Namespace: "default", Service: "app"}}
	m.cfg.Servers["k8s"] = k8s

	m.switcher.rebuild(m)
	for _, it := range m.switcher.switcher.Items() {
		if s := it.(switchServerItem); s.name == "k8s" && s.podCount != 0 {
			t.Fatalf("inactive k8s podCount = %d, want 0", s.podCount)
		}
	}

	m.server = k8s
	m.pool = api.NewPool(fakeK8sSource{pods: map[string]string{
		"http://10.0.0.1:8080/varmq": "app-a",
		"http://10.0.0.2:8080/varmq": "app-b",
	}}, "")
	m.switcher.expanded = "k8s"
	m.switcher.rebuild(m)

	var pods []string
	for _, it := range m.switcher.switcher.Items() {
		switch it := it.(type) {
		case switchServerItem:
			if it.name == "k8s" && it.podCount != 2 {
				t.Fatalf("active k8s podCount = %d, want 2", it.podCount)
			}
		case switchPodItem:
			pods = append(pods, it.name+"="+it.origin)
		}
	}
	want := "app-a=http://10.0.0.1:8080/varmq,app-b=http://10.0.0.2:8080/varmq"
	if got := strings.Join(pods, ","); got != want {
		t.Fatalf("pod rows = %s, want %s", got, want)
	}

	m.handleWorkers(workersMsg{{Name: "orders", Origin: "http://10.0.0.2:8080/varmq"}})
	if row := m.dashboardScreen().table.Rows()[0]; !strings.Contains(row[1], "app-b") {
		t.Fatalf("node column = %q, want pod name app-b", row[1])
	}
}
