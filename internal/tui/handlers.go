package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/store"
)

// Event handlers for different message types
// All handlers use pointer receivers to modify model state

// handleSynced takes in a sync of the shown server's feed: the series of the
// sparklines it built, if they are of what is shown, and the rows built again
// from its newest snapshot. A sync of another server, or of one shown before
// (an older generation), is dropped.
func (m *Model) handleSynced(msg syncedMsg) {
	if msg.server != m.key || msg.gen != m.gen {
		return
	}
	m.syncErr, m.down, m.srvErr = msg.err, msg.down, msg.srvErr
	d := m.dashboardScreen()
	d.install(m, msg.trends)
	d.refresh(m)
}

// handleConfig takes in the config read again. A server shown that was
// removed from it, or added back, or whose interval changed, is shown
// again: not configured, or with its feed. The feeds of servers no longer
// configured, or at an interval they no longer have, are forgotten, and
// closed by the command returned: an open file would keep the daemon from
// removing or setting it aside (Windows).
func (m *Model) handleConfig(msg configMsg) tea.Cmd {
	if msg.cfg == nil {
		return nil
	}
	m.cfg = msg.cfg
	defer m.switcher.rebuild(m)
	var cmds []tea.Cmd
	if srv := m.server(); (srv == nil) != (m.feed == nil) || srv != nil && m.feed.Interval() != srv.PollInterval() {
		cmds = append(cmds, m.show(m.key))
	}
	var unwanted []*store.Feed
	for k, f := range m.feeds {
		if srv := m.cfg.Servers[k.server]; srv == nil || srv.PollInterval() != k.interval {
			unwanted = append(unwanted, f)
			delete(m.feeds, k)
		}
	}
	if len(unwanted) > 0 {
		closeFeed := m.closeFeed
		cmds = append(cmds, func() tea.Msg {
			for _, f := range unwanted {
				_ = closeFeed(f)
			}
			return nil
		})
	}
	return tea.Batch(cmds...)
}

// handleActed takes in the end of an action or a refresh on the shown
// server: its error goes on the error line, and the server is synced
// again shortly, when the poke had the daemon poll it. It applies whatever
// generation it began in — the action may have been sent — but not to
// another server.
func (m *Model) handleActed(msg actedMsg) tea.Cmd {
	if msg.server != m.key {
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
