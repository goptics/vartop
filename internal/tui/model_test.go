package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// TestDropsOtherGenSync: a sync that ends after the model moved on — to
// another server, or to the same one shown again — is dropped; only the
// current server and generation's applies.
func TestDropsOtherGenSync(t *testing.T) {
	f := newFixture(t, &config.Config{DefaultServer: "a", Servers: map[string]*config.Server{
		"a": {Endpoints: []string{"http://a.invalid"}, Interval: 5},
		"b": {Endpoints: []string{"http://b.invalid"}, Interval: 5},
	}})
	f.claim()
	f.write("a", fresh(unit("instance-1", "http://a.invalid", map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)})))
	f.write("b", fresh(unit("instance-1", "http://b.invalid", map[string]*fleet.Sample{"emails": smp("Paused", 2, 0, 2, 0)})))
	m := f.model("a")
	late := m.syncCmd(m.feed, m.gen) // a's sync, still running when the switch comes
	aGen := m.gen

	deliver(m, m.show("b")())
	bRows := []string{"emails|instance-1|Paused|2|0|0|2"}
	if got := rowsOf(m); !slices.Equal(got, bRows) {
		t.Fatalf("b's rows = %v, want %v", got, bRows)
	}

	boom := errors.New("boom")
	for _, msg := range []syncedMsg{
		late().(syncedMsg),
		{server: "a", gen: aGen, err: boom},
		{server: "a", gen: m.gen, err: boom},
		{server: "b", gen: aGen, err: boom},
	} {
		deliver(m, msg)
		if got := rowsOf(m); !slices.Equal(got, bRows) {
			t.Fatalf("after %+v: rows = %v, want b's", msg, got)
		}
		if view := m.View(); strings.Contains(view, "boom") {
			t.Fatalf("after %+v: another sync's error is shown:\n%s", msg, view)
		}
	}

	deliver(m, syncedMsg{server: "b", gen: m.gen, err: boom})
	if view := m.View(); !strings.Contains(view, "cannot read the history of b: boom") {
		t.Fatalf("the current sync's error is not shown:\n%s", view)
	}
}

// TestTUIRemovedServer: a TUI showing a server that is then removed from
// the config says it is not configured, shows none of its data, and sends
// nothing: no request to varmq, no poke — from the dashboard or a detail
// screen left open.
func TestTUIRemovedServer(t *testing.T) {
	fa, aSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 4, Busy: 1}})
	fb, bSrv := fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 4, Busy: 2}})
	f := newFixture(t, &config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"multi": {Interval: 1, Endpoints: []string{aSrv.URL, bSrv.URL}},
		"other": {Interval: 1, Endpoints: []string{"http://other.invalid"}},
	}})
	f.startDaemon()
	m := f.model("multi")
	waitRows(t, m, 2)
	press(m, tea.KeyMsg{Type: tea.KeyEnter}) // a detail screen, left open
	if !m.isDetailActive() {
		t.Fatal("enter did not open the detail screen")
	}

	f.stopDaemon() // so that every call the fakes get from now is the TUI's
	calls, pokes := len(fa.Calls())+len(fb.Calls()), f.pokes("multi")
	f.setConfig(&config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"other": {Interval: 1, Endpoints: []string{"http://other.invalid"}},
	}})
	reloadConfig(t, m)

	const copy = "Server multi is not configured"
	check := func(where string) {
		t.Helper()
		view := m.View()
		if !strings.Contains(view, copy) {
			t.Errorf("%s: view lacks %q:\n%s", where, copy, view)
		}
		if strings.Contains(view, "Running") || strings.Contains(view, "instance-1") {
			t.Errorf("%s: view shows the removed server's data:\n%s", where, view)
		}
		for _, k := range []tea.KeyMsg{runeKey('a'), runeKey('p'), runeKey('r'), runeKey('a'), runeKey('s'), {Type: tea.KeyEnter}} {
			if cmd := press(m, k); cmd != nil {
				if msg := cmd(); msg != nil {
					deliver(m, msg)
				}
			}
		}
		if cmd := m.syncCmd(m.feed, m.gen); cmd != nil {
			t.Errorf("%s: a tick would sync a server that is not configured", where)
		}
		deliver(m, tea.WindowSizeMsg{Width: 30, Height: 8})
		_ = m.View()
		deliver(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	check("detail")
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.activeScreen().(*dashboardScreen); !ok {
		t.Fatalf("esc left %T active", m.activeScreen())
	}
	check("dashboard")
	if rows := rowsOf(m); len(rows) != 0 {
		t.Errorf("rows of a removed server: %v", rows)
	}

	if now := len(fa.Calls()) + len(fb.Calls()); now != calls {
		t.Errorf("varmq got %d requests after the server was removed", now-calls)
	}
	if now := f.pokes("multi"); now != pokes {
		t.Errorf("the removed server was poked %d times", now-pokes)
	}

	// The switcher lists what is configured now.
	openSwitcher(t, m)
	var names []string
	for _, it := range m.switcher.switcher.Items() {
		names = append(names, it.(switchServerItem).name)
	}
	if !slices.Equal(names, []string{"other"}) {
		t.Errorf("switcher lists %v, want [other]", names)
	}
}
