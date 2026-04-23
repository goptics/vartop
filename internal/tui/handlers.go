package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Event handlers for different message types
// All handlers use pointer receivers to modify model state

// handleHealth processes health check results
func (m *Model) handleHealth(msg healthMsg) tea.Cmd {
	var cmds []tea.Cmd

	m.isHealthy = msg.ok
	if m.isHealthy && !m.showSwitcher {
		if m.showWorkerDetail && m.currentWorkerName != "" {
			// Refresh worker details when in detail view
			cmds = append(cmds, m.fetchWorkerDetailsCmd(m.currentWorkerName))
		} else if !m.showWorkerDetail {
			// Refresh dashboard when not in detail view
			cmds = append(cmds, m.fetchWorkersCmd())
		}
	}

	return tea.Batch(cmds...)
}

// handleWorkers processes worker list updates
func (m *Model) handleWorkers(msg workersMsg) tea.Cmd {
	m.lastUpdate = time.Now()
	m.err = nil

	rows := make([]table.Row, len(msg))
	for i, w := range msg {
		rows[i] = table.Row{
			w.Name,
			w.Status,
			lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.Concurrency)),
			lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.NumProcessing)),
			lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render(fmt.Sprint(w.NumPending)),
			lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render(fmt.Sprint(w.NumIdleWorkers)),
		}
	}

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
	return nil
}

// handleError processes error messages
func (m *Model) handleError(msg errMsg) tea.Cmd {
	m.err = msg.err
	return nil
}

// handleWindowSize processes terminal resize events
func (m *Model) handleWindowSize(msg tea.WindowSizeMsg) {
	m.width = msg.Width
	m.height = msg.Height

	// Update table dimensions - reserve more space for header
	tHeight := msg.Height - 12  // Increased from 8 to leave room for header
	fixedWidths := 15 + 12 + 12 + 10 + 8 + 10
	nameWidth := msg.Width - fixedWidths
	if nameWidth < 15 {
		nameWidth = 15
	}

	columns := []table.Column{
		{Title: "Name", Width: nameWidth},
		{Title: "Status", Width: 15},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
		{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
		{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
	}

	s := TableStyles()
	selected := m.table.Cursor()
	rows := m.table.Rows()

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

	// Update switcher dimensions
	lHeight := msg.Height - 6
	if lHeight > 0 {
		m.switcher.SetSize(msg.Width-2, lHeight)
	}

	// Hide default help, we'll show custom footer
	m.switcher.SetShowHelp(false)
}
