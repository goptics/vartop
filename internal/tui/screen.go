package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// screen is a full-screen view in the TUI's navigation stack. Each screen owns
// its own state and key handling; it receives the root model to read and mutate
// shared state and to navigate the stack.
type screen interface {
	// Update processes a message destined for this screen.
	Update(m *Model, msg tea.Msg) tea.Cmd
	// View renders the screen using the root model's shared state.
	View(m *Model) string
}

// activeScreen returns the screen on top of the stack. The dashboard is always
// the base, so the stack is never empty.
func (m *Model) activeScreen() screen {
	return m.screens[len(m.screens)-1]
}

// pushScreen makes s the active screen.
func (m *Model) pushScreen(s screen) {
	m.screens = append(m.screens, s)
}

// popScreen removes the active screen. The base (dashboard) is never popped,
// mirroring the old flag logic where esc on the dashboard was a no-op.
func (m *Model) popScreen() {
	if len(m.screens) > 1 {
		m.screens = m.screens[:len(m.screens)-1]
	}
}

// isSwitcherActive reports whether the switcher is the active screen.
func (m *Model) isSwitcherActive() bool {
	_, ok := m.activeScreen().(*switcherScreen)
	return ok
}

// isDetailActive reports whether the worker detail screen is active.
func (m *Model) isDetailActive() bool {
	_, ok := m.activeScreen().(*detailScreen)
	return ok
}

// detailScreen returns the active detail screen, if one is active.
func (m *Model) detailScreen() (*detailScreen, bool) {
	d, ok := m.activeScreen().(*detailScreen)
	return d, ok
}

// dashboardScreen returns the dashboard at the base of the stack.
func (m *Model) dashboardScreen() *dashboardScreen {
	d, _ := m.screens[0].(*dashboardScreen)
	return d
}
