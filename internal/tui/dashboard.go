package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// dashboardScreen is the worker list view and the base of the screen stack.
type dashboardScreen struct {
	table             table.Model
	origins           []string // origin endpoint per table row, same order
	inActionSelection bool
	keys              dashboardKeyMap
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

	return &dashboardScreen{table: t, keys: newDashboardKeyMap()}
}

func (d *dashboardScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		d.table, cmd = d.table.Update(msg)
		return cmd
	}
	return d.handleKey(m, keyMsg)
}

func (d *dashboardScreen) handleKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	if d.inActionSelection {
		switch {
		case key.Matches(msg, d.keys.Back):
			d.inActionSelection = false
			return nil
		case key.Matches(msg, d.keys.Pause):
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(), "pause")
		case key.Matches(msg, d.keys.Resume):
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(), "resume")
		case key.Matches(msg, d.keys.Stop):
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(), "stop")
		case key.Matches(msg, d.keys.Restart):
			d.inActionSelection = false
			return m.actionWorkerCmd(d.selectedWorkerName(), d.selectedWorkerOrigin(), "restart")
		}
		// Unhandled keys fall through so navigation still works while the
		// action menu is open.
	}

	switch {
	case key.Matches(msg, d.keys.Quit):
		return tea.Quit
	case key.Matches(msg, d.keys.Refresh):
		m.err = nil
		m.sourceErr = false
		if m.pool == nil {
			// The source never came up (e.g. bad kube context): retry it.
			return m.connect(m.server)
		}
		return tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
	case key.Matches(msg, d.keys.Actions):
		if len(d.table.Rows()) > 0 {
			d.inActionSelection = !d.inActionSelection
		}
		return nil
	case key.Matches(msg, d.keys.Switch):
		m.pushScreen(m.switcher)
		return nil
	case key.Matches(msg, d.keys.Detail):
		// Navigate to worker detail view
		if m.isHealthy && len(d.table.Rows()) > 0 {
			selectedRow := d.table.SelectedRow()
			if selectedRow != nil {
				workerName := selectedRow[0]
				origin := d.selectedWorkerOrigin()
				m.pushScreen(newDetailScreen(workerName, origin))
				return m.fetchWorkerDetailsCmd(workerName, origin)
			}
		}
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
func (d *dashboardScreen) selectedWorkerOrigin() string {
	i := d.table.Cursor()
	if i < 0 || i >= len(d.origins) {
		return ""
	}
	return d.origins[i]
}

// setRows replaces the table rows (and their origins) while retaining the
// current selection.
func (d *dashboardScreen) setRows(rows []table.Row, origins []string) {
	selected := d.table.Cursor()
	d.origins = origins
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
		d.origins = nil
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

// renderHeader renders the dashboard status bar: server name, connection
// (kube context/namespace or endpoint), pod health and the last refresh time.
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

	leftContent := lipgloss.JoinHorizontal(lipgloss.Left,
		title,
		statusIndicator,
		"  ",
		d.renderConnection(m),
		"  ",
		d.renderPodHealth(m),
	)

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

	return StatusBarStyle.Render(headerContent)
}

// renderConnection describes how the active server is reached: kube
// context/namespace for kubernetes servers, otherwise an endpoint.
func (d *dashboardScreen) renderConnection(m *Model) string {
	return lipgloss.NewStyle().Foreground(ColorSubtle).Render("→ " + m.connectionLabel())
}

// connectionLabel names what the dashboard is connected to.
func (m *Model) connectionLabel() string {
	switch {
	case m.selectedPodOrigin != "":
		return m.podName(m.selectedPodOrigin)
	case m.server.Kubernetes != nil:
		return fmt.Sprintf("%s/%s", m.server.Kubernetes.Context, m.server.Kubernetes.Namespace)
	default:
		return m.server.DisplayEndpoint()
	}
}

// renderPodHealth summarizes healthy/total discovered pods with a status dot.
func (d *dashboardScreen) renderPodHealth(m *Model) string {
	endpoints := m.poolEndpoints()
	total := len(endpoints)
	healthy := 0
	for _, ep := range endpoints {
		if m.podHealth[ep] {
			healthy++
		}
	}

	dot := StatusStyleStopped.Render("○")
	switch {
	case total > 0 && healthy == total:
		dot = StatusStyleRunning.Render("●")
	case healthy > 0:
		dot = StatusStyleWarning.Render("◐")
	}

	text := lipgloss.NewStyle().Foreground(ColorSubtle).Render(fmt.Sprintf("%d/%d pods", healthy, total))
	return lipgloss.JoinHorizontal(lipgloss.Left, dot, " ", text)
}

// poolEndpoints returns the endpoint URL of every discovered pod. The pool
// reconciles against the active discovery source (static or kubernetes).
func (m *Model) poolEndpoints() []string {
	if m.pool == nil {
		return nil
	}
	clients := m.pool.Clients()
	endpoints := make([]string, 0, len(clients))
	for _, c := range clients {
		endpoints = append(endpoints, c.PrimaryEndpoint())
	}
	return endpoints
}

// podName labels a pod endpoint, preferring the discovered pod name.
func (m *Model) podName(origin string) string {
	if m.pool == nil {
		return config.ShortOrigin(origin)
	}
	return m.pool.PodName(origin)
}

// renderBody renders the dashboard body content.
func (d *dashboardScreen) renderBody(m *Model) string {
	if m.pool == nil && m.err == nil {
		connecting := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			Render(fmt.Sprintf("Connecting to %s...", m.connectionLabel()))
		return lipgloss.Place(m.width, d.table.Height()+2, lipgloss.Center, lipgloss.Center, connecting)
	}

	if !m.isHealthy {
		text := fmt.Sprintf("Server is unreachable at %s\nPolling in background...", m.connectionLabel())
		// A source/pool construction error (e.g. a bad kube context) is more
		// useful than the placeholder endpoint; show it instead.
		if m.err != nil {
			text = fmt.Sprintf("⚠️  %v\nPolling in background...", m.err)
		}

		offlineMsg := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorError).
			Render(text)

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
	// Only polling info now (updated time moved to status bar)
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5
	}
	pollInfo := fmt.Sprintf("Polling: %ds", interval)

	keysView := helpFooter(d.keyMap(), m.width-lipgloss.Width(pollInfo)-4)

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}
