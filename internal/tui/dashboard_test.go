package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestDashboardActionRoutesToRowPod drives the dashboard's own action keys
// (a, then p) on the second row of a merged view where both pods run the same
// worker: the PATCH must reach only that row's pod.
func TestDashboardActionRoutesToRowPod(t *testing.T) {
	pod1 := newFakePod(t)
	pod2 := newFakePod(t)
	m := poolModel(t, pod1, pod2)
	m.handleWorkers(workersMsg{
		{Name: "orders", Origin: pod1.url()},
		{Name: "orders", Origin: pod2.url()},
	})
	m.dashboardScreen().table.SetCursor(1)

	press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !m.dashboardScreen().inActionSelection {
		t.Fatal("a did not enter action mode")
	}
	cmd := press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if m.dashboardScreen().inActionSelection {
		t.Fatal("action key did not leave action mode")
	}
	if cmd == nil {
		t.Fatal("pause returned no command")
	}
	runCmd(cmd)

	want := "PATCH /workers/orders/actions/pause"
	if pod2.count(want) != 1 || pod1.count(want) != 0 {
		t.Fatalf("pause routed wrong: pod1=%v pod2=%v", pod1.seen(), pod2.seen())
	}
}

// TestDashboardActionModeNeedsRows: with no workers listed there is nothing to
// act on, so a must not enter action mode.
func TestDashboardActionModeNeedsRows(t *testing.T) {
	m := testModel(t)
	press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if m.dashboardScreen().inActionSelection {
		t.Fatal("entered action mode with an empty table")
	}
}

// TestInitBuildsPool covers startup: Init schedules the background pool build
// (plus the poll tick), and the resulting serverSwitchMsg installs the pool.
func TestInitBuildsPool(t *testing.T) {
	m := testModel(t)
	if m.Init() == nil {
		t.Fatal("Init returned no command")
	}
	// Run only the pool build: the tick half of Init's batch sleeps.
	sw, ok := m.switchServerCmd(m.server, m.switchSeq)().(serverSwitchMsg)
	if !ok || sw.err != nil || sw.pool == nil {
		t.Fatalf("pool build failed: %+v", sw)
	}
	next, _ := m.Update(sw)
	*m = next.(Model)
	if m.pool != sw.pool {
		t.Fatal("pool from Init not applied")
	}
	_ = m.Close()
}
