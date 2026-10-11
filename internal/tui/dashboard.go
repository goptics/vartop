package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// dashboardScreen is the worker list view and the base of the screen stack.
type dashboardScreen struct {
	table             table.Model
	inActionSelection bool
}

func newDashboardScreen() *dashboardScreen {
	columns := []table.Column{
		{Title: "Name", Width: 18},
		{Title: "Node", Width: 12},
		{Title: "Status", Width: 12},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
		{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
		{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(6), // Reduced from 10 to leave more room for header
	)
	t.SetStyles(TableStyles())

	return &dashboardScreen{table: t}
}

func (d *dashboardScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		d.table, cmd = d.table.Update(msg)
		return cmd
	}
	return d.handleKey(m, key)
}

func (d *dashboardScreen) handleKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	if d.inActionSelection {
		switch msg.String() {
		case "esc":
			d.inActionSelection = false
			return nil
		case "p":
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(m), "pause")
		case "r":
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(m), "resume")
		case "s":
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(m), "stop")
		case "R":
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(m), "restart")
		}
		// Unhandled keys fall through so navigation still works while the
		// action menu is open.
	}

	switch msg.String() {
	case "ctrl+c", "q":
		return tea.Quit
	case "r":
		m.err = nil
		return tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
	case "a", "A":
		if len(d.table.Rows()) > 0 {
			d.inActionSelection = !d.inActionSelection
		}
		return nil
	case "tab":
		m.pushScreen(m.switcher)
		return nil
	case "enter":
		// Navigate to worker detail view
		if m.isHealthy && len(d.table.Rows()) > 0 {
			selectedRow := d.table.SelectedRow()
			if selectedRow != nil {
				workerName := selectedRow[0]
				origin := m.workerOrigins[workerName]
				m.pushScreen(newDetailScreen(workerName, origin))
				return m.fetchWorkerDetailsCmd(workerName, origin)
			}
		}
		return nil
	case "esc":
		// Return to dashboard from detail view (detail handles its own esc).
		return nil
	}

	if !m.isHealthy || len(d.table.Rows()) == 0 {
		return nil
	}

	var cmd tea.Cmd
	d.table, cmd = d.table.Update(msg)
	return cmd
}

// selectedWorkerName returns the name of the currently selected table row.
func (d *dashboardScreen) selectedWorkerName() string {
	if len(d.table.Rows()) == 0 {
		return ""
	}
	return d.table.SelectedRow()[0]
}

// selectedWorkerOrigin returns the origin endpoint for the currently selected
// table row.
func (d *dashboardScreen) selectedWorkerOrigin(m *Model) string {
	if len(d.table.Rows()) == 0 {
		return ""
	}
	return m.workerOrigins[d.table.SelectedRow()[0]]
}

// setRows replaces the table rows while retaining the current selection.
func (d *dashboardScreen) setRows(rows []table.Row) {
	selected := d.table.Cursor()
	d.table.SetRows(rows)
	if selected < len(rows) {
		d.table.SetCursor(selected)
	}
}

// rebuildTable reconstructs the table with the current width and showNodeColumn setting.
func (d *dashboardScreen) rebuildTable(m *Model) {
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
	selected := d.table.Cursor()
	rows := d.table.Rows()

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

	d.table = t
}

// View renders the dashboard.
func (d *dashboardScreen) View(m *Model) string {
	// Header
	header := d.renderHeader(m)

	// Body
	bodyView := d.renderBody(m)

	// Footer
	footer := d.renderFooter(m)

	// Build main content - reduced gap
	mainContent := lipgloss.JoinVertical(lipgloss.Left, header, bodyView)

	// Calculate spacing for footer positioning
	footerHeight := lipgloss.Height(footer)
	bodyHeight := lipgloss.Height(mainContent)
	spacing := createSpacing(m.height, bodyHeight, footerHeight, 0)

	// Create full view with footer anchored at bottom
	fullContent := lipgloss.JoinVertical(
		lipgloss.Left,
		mainContent,
		strings.Repeat("\n", spacing),
		footer,
	)

	return BaseStyle.Render(fullContent)
}

// renderHeader renders the dashboard header.
func (d *dashboardScreen) renderHeader(m *Model) string {
	var statusStyle lipgloss.Style
	var statusText string
	if m.isHealthy {
		statusStyle = StatusStyleRunning
		statusText = "● Active"
	} else {
		statusStyle = StatusStyleStopped
		statusText = "○ Inactive"
	}

	statusIndicator := statusStyle.Render(statusText)

	title := TitleStyle.Render(fmt.Sprintf(" vartop [%s] ", m.server.Name))

	// Build endpoint/pod info
	var endpointInfo string
	endpoints := m.server.GetEndpoints()
	if m.selectedPodOrigin != "" {
		// Single pod view
		short := config.ShortOrigin(m.selectedPodOrigin)
		var dot string
		if m.podHealth[m.selectedPodOrigin] {
			dot = StatusStyleRunning.Render("●")
		} else {
			dot = StatusStyleStopped.Render("○")
		}
		endpointInfo = lipgloss.NewStyle().Foreground(ColorSubtle).Render(
			fmt.Sprintf("→ %s %s", dot, short),
		)
	} else if len(endpoints) > 1 {
		// Merged view: compact summary
		total := len(endpoints)
		healthy := 0
		for _, ep := range endpoints {
			if m.podHealth[ep] {
				healthy++
			}
		}
		endpointInfo = lipgloss.NewStyle().Foreground(ColorSubtle).Render(
			fmt.Sprintf("→ %d pods, %d healthy", total, healthy),
		)
	} else {
		endpointInfo = lipgloss.NewStyle().Foreground(ColorSubtle).Render(fmt.Sprintf("→ %s", m.server.DisplayEndpoint()))
	}

	// Left side: title, status, endpoint
	leftContent := lipgloss.JoinHorizontal(lipgloss.Left, title, statusIndicator, " ", endpointInfo)

	// Updated time in top-right
	lastUpdateStr := "Never"
	if !m.lastUpdate.IsZero() {
		lastUpdateStr = m.lastUpdate.Format("15:04:05")
	}
	updatedInfo := lipgloss.NewStyle().Foreground(ColorSubtle).Render(fmt.Sprintf("Updated: %s", lastUpdateStr))

	// Proper justification: calculate spacing between left and right
	leftWidth := lipgloss.Width(leftContent)
	rightWidth := lipgloss.Width(updatedInfo)
	spacing := m.width - leftWidth - rightWidth - 4 // -4 for padding

	headerContent := lipgloss.JoinHorizontal(lipgloss.Left,
		leftContent,
		strings.Repeat(" ", max(0, spacing)),
		updatedInfo,
	)

	return lipgloss.NewStyle().
		Padding(1, 2).
		Render(headerContent)
}

// renderBody renders the dashboard body content.
func (d *dashboardScreen) renderBody(m *Model) string {
	if !m.isHealthy {
		offlineMsg := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorError).
			Render(fmt.Sprintf("Server is unreachable at %s\nPolling in background...", m.server.DisplayEndpoint()))

		return lipgloss.Place(m.width, d.table.Height()+2, lipgloss.Center, lipgloss.Center, offlineMsg)
	}

	// Display errors if any
	var errorView string
	if m.err != nil {
		errorView = lipgloss.NewStyle().Foreground(ColorError).Padding(0, 1).Render(fmt.Sprintf("⚠️  Error: %v", m.err))
		errorView += "\n"
	}

	tableView := lipgloss.NewStyle().Padding(1, 1).Render(d.table.View())
	return lipgloss.JoinVertical(lipgloss.Left, errorView, tableView)
}

// renderFooter renders the dashboard footer.
func (d *dashboardScreen) renderFooter(m *Model) string {
	// Only polling info now (updated time moved to header)
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5
	}
	pollInfo := fmt.Sprintf("Polling: %ds", interval)

	// Key bindings
	var keysView string
	if d.inActionSelection {
		keysView = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" back • "),
			HelpKeyStyle.Render("p"), HelpDescStyle.Render(" pause • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" resume • "),
			HelpKeyStyle.Render("s"), HelpDescStyle.Render(" stop • "),
			HelpKeyStyle.Render("R"), HelpDescStyle.Render(" restart"),
		)
	} else {
		keysView = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" move • "),
			HelpKeyStyle.Render("enter"), HelpDescStyle.Render(" view details • "),
			HelpKeyStyle.Render("a"), HelpDescStyle.Render(" actions • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" refresh • "),
			HelpKeyStyle.Render("tab"), HelpDescStyle.Render(" switch • "),
			HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit"),
		)
	}

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}
