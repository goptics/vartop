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
	m.rebuildSwitcherItems()

	if m.isHealthy && !m.showSwitcher {
		if m.showWorkerDetail && m.currentWorkerName != "" {
			cmds = append(cmds, m.fetchWorkerDetailsCmd(m.currentWorkerName, m.currentWorkerOrigin))
		} else if !m.showWorkerDetail {
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
	selected := m.table.Cursor()
	m.table.SetRows(rows)
	if selected < len(rows) {
		m.table.SetCursor(selected)
	}

	return nil
}

// handleWorkerDetails processes worker detail updates
func (m *Model) handleWorkerDetails(msg workerDetailsMsg) tea.Cmd {
	m.lastUpdate = time.Now()
	m.err = nil
	m.workerDetails = msg
	m.recalculateMaxScroll()
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
	m.rebuildSwitcherItems()

	return tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
}

// rebuildTable reconstructs the table with the current width and showNodeColumn setting.
func (m *Model) rebuildTable() {
	tHeight := m.height - 12

	var columns []table.Column
	var fixedWidths int
	if m.showNodeColumn {
		// 7 columns + cell padding (7 × 2 = 14)
		fixedWidths = 12 + 12 + 12 + 12 + 10 + 8 + 14
		nameWidth := m.width - fixedWidths
		if nameWidth < 12 {
			nameWidth = 12
		}
		columns = []table.Column{
			{Title: "Name", Width: nameWidth},
			{Title: "Node", Width: 12},
			{Title: "Status", Width: 12},
			{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
			{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
			{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
			{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
		}
	} else {
		// 6 columns + cell padding (6 × 2 = 12)
		fixedWidths = 12 + 12 + 12 + 10 + 8 + 12
		nameWidth := m.width - fixedWidths
		if nameWidth < 12 {
			nameWidth = 12
		}
		columns = []table.Column{
			{Title: "Name", Width: nameWidth},
			{Title: "Status", Width: 12},
			{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
			{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
			{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
			{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
		}
	}

	s := TableStyles()
	selected := m.table.Cursor()
	rows := m.table.Rows()

	// If existing rows have a different cell count than new columns,
	// clear them to avoid a panic in table.New (rows will be repopulated by the next fetch).
	if len(rows) > 0 && len(rows[0]) != len(columns) {
		rows = nil
		selected = 0
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
	)
	if tHeight > 0 {
		t.SetHeight(tHeight)
	}
	t.SetStyles(s)
	t.SetCursor(selected)

	m.table = t
}

// handleWindowSize processes terminal resize events
func (m *Model) handleWindowSize(msg tea.WindowSizeMsg) {
	m.width = msg.Width
	m.height = msg.Height

	m.rebuildTable()

	// Update switcher dimensions
	lHeight := msg.Height - 6
	if lHeight > 0 {
		m.switcher.SetSize(msg.Width-2, lHeight)
	}

	// Hide default help, we'll show custom footer
	m.switcher.SetShowHelp(false)

	// Recalculate scroll limits for worker detail view
	m.recalculateMaxScroll()
}

// recalculateMaxScroll updates the maximum scroll value for worker detail view
func (m *Model) recalculateMaxScroll() {
	if !m.showWorkerDetail || m.workerDetails == nil {
		m.workerDetailMaxScroll = 0
		return
	}

	// Estimate content height based on actual worker details
	// Header ~6 lines, config ~15 lines (5 items * 3 lines), metrics ~20 lines
	estimatedContentHeight := 45
	footerHeight := 3
	availableHeight := m.height - footerHeight - 2

	maxScroll := estimatedContentHeight - availableHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	m.workerDetailMaxScroll = maxScroll

	// Clamp current scroll position
	if m.workerDetailScrollY > m.workerDetailMaxScroll {
		m.workerDetailScrollY = m.workerDetailMaxScroll
	}
}
