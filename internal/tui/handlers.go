package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Event handlers for different message types
// All handlers use pointer receivers to modify model state

// handleSynced takes in a sync of the shown server's feed: the rows are
// built again from its newest snapshot. A sync of another server, or of
// one shown before (an older generation), is dropped.
func (m *Model) handleSynced(msg syncedMsg) {
	if msg.server != m.key || msg.gen != m.gen {
		return
	}
	m.synced = true
	m.syncErr, m.down, m.srvErr = msg.err, msg.down, msg.srvErr
	m.dashboardScreen().refresh(m)
}

// handleConfig takes in the config read again. A server shown that was
// removed from it, or added back, or whose interval changed, is shown
// again: not configured, or with its feed.
func (m *Model) handleConfig(msg configMsg) tea.Cmd {
	if msg.cfg == nil {
		return nil
	}
	m.cfg = msg.cfg
	defer m.switcher.rebuild(m)
	srv := m.server()
	if (srv == nil) != (m.feed == nil) || srv != nil && m.feed.Interval() != srv.PollInterval() {
		return m.show(m.key)
	}
	return nil
}

// handleActed takes in the end of an action or a refresh on the shown
// server: its error goes on the error line, and the server is synced
// again shortly, when the poke had the daemon poll it.
func (m *Model) handleActed(msg actedMsg) tea.Cmd {
	if msg.server != m.key || msg.gen != m.gen {
		return nil
	}
	m.err = msg.err
	key, gen := m.key, m.gen
	return tea.Tick(resyncAfter, func(time.Time) tea.Msg { return resyncMsg{server: key, gen: gen} })
}

// handleWindowSize processes terminal resize events
func (m *Model) handleWindowSize(msg tea.WindowSizeMsg) {
	m.width = msg.Width
	m.height = msg.Height

	m.dashboardScreen().rebuildTable(m)

	// Update switcher dimensions
	lHeight := msg.Height - 6
	if lHeight > 0 {
		m.switcher.switcher.SetSize(msg.Width-2, lHeight)
	}

	// Hide default help, we'll show custom footer
	m.switcher.switcher.SetShowHelp(false)
}
