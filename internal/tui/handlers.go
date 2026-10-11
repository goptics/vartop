package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
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
	m.err = nil

	rows := make([]table.Row, 0, len(msg))
	origins := make(map[string]string, len(msg))

	for _, w := range msg {
		origin := config.ShortOrigin(w.Origin)
		origins[w.Name] = w.Origin

		if m.showNodeColumn {
			rows = append(rows, table.Row{
				w.Name,
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(origin),
				w.Status,
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.Concurrency)),
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.NumProcessing)),
				lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render(fmt.Sprint(w.NumPending)),
				lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render(fmt.Sprint(w.NumIdleWorkers)),
			})
		} else {
			rows = append(rows, table.Row{
				w.Name,
				w.Status,
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.Concurrency)),
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.NumProcessing)),
				lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render(fmt.Sprint(w.NumPending)),
				lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render(fmt.Sprint(w.NumIdleWorkers)),
			})
		}
	}

	m.workerOrigins = origins

	// Retain selection if possible
	m.dashboardScreen().setRows(rows)

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
	if msg.err != nil {
		m.err = msg.err
		return nil
	}

	// Stop the previous server's source watcher (informer), if any.
	if m.pool != nil {
		_ = m.pool.Close()
	}
	m.server = msg.srv
	m.pool = msg.pool
	m.err = nil
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
