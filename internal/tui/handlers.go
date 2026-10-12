package tui

import (
	"slices"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// Event handlers for different message types
// All handlers use pointer receivers to modify model state

// handleHealth processes health check results
func (m *Model) handleHealth(msg healthMsg) tea.Cmd {
	var cmds []tea.Cmd

	m.isHealthy = msg.ok
	if msg.status != nil {
		m.podHealth = msg.status
	}

	// Rebuild switcher so health dots stay current
	m.switcher.rebuild(m)

	if m.isHealthy && !m.isSwitcherActive() {
		if d, ok := m.detailScreen(); ok && d.workerName != "" {
			cmds = append(cmds, m.fetchWorkerDetailsCmd(d.workerName, d.origin))
		} else if !m.isDetailActive() {
			cmds = append(cmds, m.fetchWorkersCmd())
		}
	}

	return tea.Batch(cmds...)
}

// handleWorkers processes worker list updates
func (m *Model) handleWorkers(msg workersMsg) tea.Cmd {
	m.lastUpdate = time.Now()
	// A pool/source construction failure stays visible until the pool is
	// replaced (server switch or manual refresh); other errors clear on a
	// successful fetch.
	if !m.sourceErr {
		m.err = nil
	}

	rows := make([]table.Row, 0, len(msg))
	// One origin per row: every pod in a cluster runs the same worker names,
	// so the name alone cannot route an action to the right pod.
	origins := make([]string, 0, len(msg))

	for _, w := range msg {
		// Label the row with the instance that actually served it; behind a
		// load balancer that differs from refresh to refresh.
		origin := w.Instance
		if origin == "" {
			origin = m.podName(w.Origin)
		}
		origins = append(origins, w.Origin)

		row := table.Row{
			w.Name,
			w.Status,
			center(12, w.Concurrency),
			center(12, w.NumProcessing),
			center(10, w.NumPending),
			center(8, w.NumIdleWorkers),
		}
		if m.showNodeColumn {
			row = slices.Insert(row, 1, center(12, origin))
		}
		rows = append(rows, row)
	}

	// Retain selection if possible
	m.dashboardScreen().setRows(rows, origins)

	return nil
}

// handleWorkerDetails processes worker detail updates
func (m *Model) handleWorkerDetails(msg workerDetailsMsg) tea.Cmd {
	m.lastUpdate = time.Now()
	m.err = nil
	if d, ok := m.detailScreen(); ok {
		d.details = msg
		d.recalculateMaxScroll(m.height)
	}
	return nil
}

// handleError processes error messages
func (m *Model) handleError(msg errMsg) tea.Cmd {
	m.err = msg.err
	return nil
}

// handleServerSwitch applies the pool built for a newly selected server.
// Construction errors keep the current pool and surface through the error display.
func (m *Model) handleServerSwitch(msg serverSwitchMsg) tea.Cmd {
	// A newer switch was requested while this one was building: discard it.
	if msg.seq != m.switchSeq {
		if msg.pool != nil {
			_ = msg.pool.Close()
		}
		return nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.sourceErr = true
		return nil
	}

	// Stop the previous server's source watcher (informer), if any.
	if m.pool != nil {
		_ = m.pool.Close()
	}
	m.server = msg.srv
	m.pool = msg.pool
	m.err = nil
	m.sourceErr = false
	m.switcher.rebuild(m)

	return tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
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

	// Recalculate scroll limits for worker detail view
	if d, ok := m.detailScreen(); ok {
		d.recalculateMaxScroll(m.height)
	}
}
