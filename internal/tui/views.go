package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// View rendering functions

// renderSwitcherView renders the server switcher with custom footer
func (m Model) renderSwitcherView() string {
	switcherContent := m.switcher.View()

	// Custom footer for switcher
	footerKeys := lipgloss.JoinHorizontal(lipgloss.Center,
		HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit • "),
		HelpKeyStyle.Render("enter"), HelpDescStyle.Render(" select • "),
		HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" back • "),
		HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" navigate"),
	)

	footer := FooterStyle.Width(m.width).Padding(0, 1).Render(footerKeys)

	// Calculate spacing
	switcherHeight := lipgloss.Height(switcherContent)
	footerHeight := lipgloss.Height(footer)
	spacing := createSpacing(m.height, switcherHeight, footerHeight, 2)

	// Join with padding
	mainContent := lipgloss.NewStyle().Padding(1, 2).Render(switcherContent)
	fullContent := lipgloss.JoinVertical(
		lipgloss.Left,
		mainContent,
		strings.Repeat("\n", spacing),
		footer,
	)

	return BaseStyle.Render(fullContent)
}

// renderDashboardView renders the main dashboard view
func (m Model) renderDashboardView() string {
	// Header
	header := m.renderDashboardHeader()

	// Body
	bodyView := m.renderDashboardBody()

	// Footer
	footer := m.renderDashboardFooter()

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

// renderDashboardHeader renders the dashboard header
func (m Model) renderDashboardHeader() string {
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
	endpointInfo := lipgloss.NewStyle().Foreground(ColorSubtle).Render(fmt.Sprintf("→ %s", m.server.Endpoint))

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

// renderDashboardBody renders the dashboard body content
func (m Model) renderDashboardBody() string {
	if !m.isHealthy {
		offlineMsg := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorError).
			Render(fmt.Sprintf("Server is unreachable at %s\nPolling in background...", m.server.Endpoint))

		return lipgloss.Place(m.width, m.table.Height()+2, lipgloss.Center, lipgloss.Center, offlineMsg)
	}

	// Display errors if any
	var errorView string
	if m.err != nil {
		errorView = lipgloss.NewStyle().Foreground(ColorError).Padding(0, 1).Render(fmt.Sprintf("⚠️  Error: %v", m.err))
		errorView += "\n"
	}

	tableView := lipgloss.NewStyle().Padding(1, 1).Render(m.table.View())
	return lipgloss.JoinVertical(lipgloss.Left, errorView, tableView)
}

// renderDashboardFooter renders the dashboard footer
func (m Model) renderDashboardFooter() string {
	// Only polling info now (updated time moved to header)
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5
	}
	pollInfo := fmt.Sprintf("Polling: %ds", interval)

	// Key bindings
	var keysView string
	if m.inActionSelection {
		keysView = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" back • "),
			HelpKeyStyle.Render("p"), HelpDescStyle.Render(" pause • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" resume • "),
			HelpKeyStyle.Render("s"), HelpDescStyle.Render(" stop • "),
			HelpKeyStyle.Render("R"), HelpDescStyle.Render(" restart"),
		)
	} else {
		keysView = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit • "),
			HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" move • "),
			HelpKeyStyle.Render("enter"), HelpDescStyle.Render(" view details • "),
			HelpKeyStyle.Render("a"), HelpDescStyle.Render(" actions • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" refresh • "),
			HelpKeyStyle.Render("tab"), HelpDescStyle.Render(" switch server"),
		)
	}

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}

// renderWorkerDetailHeaderView renders the worker detail view header
func (m Model) renderWorkerDetailHeaderView() string {
	if m.workerDetails == nil {
		return ""
	}

	w := *m.workerDetails

	// Updated time
	lastUpdateStr := "Never"
	if !m.lastUpdate.IsZero() {
		lastUpdateStr = m.lastUpdate.Format("15:04:05")
	}

	// Responsive header layout
	minWidthForCompact := 80
	if m.width < minWidthForCompact {
		// Compact layout for narrow screens - stack vertically
		title := lipgloss.NewStyle().
			Foreground(ColorHighlight).
			Bold(true).
			Render("Worker Details")

		workerInfo := lipgloss.NewStyle().
			Foreground(ColorFg).
			Bold(true).
			Render(w.Name)

		serverInfo := fmt.Sprintf("Server: %s", m.server.Name)
		endpointInfo := fmt.Sprintf("→ %s", m.server.Endpoint)
		updatedInfo := fmt.Sprintf("Updated: %s", lastUpdateStr)

		headerContent := lipgloss.JoinVertical(lipgloss.Left,
			title,
			workerInfo,
			lipgloss.NewStyle().Foreground(ColorSubtle).Render(serverInfo),
			lipgloss.NewStyle().Foreground(ColorSubtle).Render(endpointInfo),
			lipgloss.NewStyle().Foreground(ColorSubtle).Render(updatedInfo),
		)

		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorHighlight).
			Padding(1, 1).
			Render(headerContent)
	}

	// Normal layout for wider screens
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("Worker Details")

	workerInfo := lipgloss.NewStyle().
		Foreground(ColorFg).
		Bold(true).
		Render(w.Name)

	// Server and endpoint info
	serverLabel := lipgloss.NewStyle().Foreground(ColorSubtle).Render("Server:")
	serverValue := lipgloss.NewStyle().Foreground(ColorFg).Render(m.server.Name)
	endpointLabel := lipgloss.NewStyle().Foreground(ColorSubtle).Render("Endpoint:")
	endpointValue := lipgloss.NewStyle().Foreground(ColorSubtle).Render(m.server.Endpoint)

	updatedInfo := lipgloss.NewStyle().Foreground(ColorSubtle).Render(fmt.Sprintf("Updated: %s", lastUpdateStr))

	// Build header rows
	row1 := lipgloss.JoinHorizontal(lipgloss.Left, title, " • ", workerInfo)
	row2Left := lipgloss.JoinHorizontal(lipgloss.Left,
		serverLabel, " ", serverValue, "  |  ",
		endpointLabel, " ", endpointValue,
	)
	separator := lipgloss.NewStyle().Foreground(ColorSubtle).Render(" | ")
	row2 := lipgloss.JoinHorizontal(lipgloss.Left, row2Left, separator, updatedInfo)

	headerContent := lipgloss.JoinVertical(lipgloss.Left, row1, row2)

	// Worker detail keeps border for visual separation
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorHighlight).
		Padding(1, 2).
		Render(headerContent)
}

// renderWorkerDetailView renders the complete worker detail view
func (m Model) renderWorkerDetailView() string {
	footer := m.renderWorkerDetailFooter()
	footerHeight := lipgloss.Height(footer)

	if m.workerDetails == nil {
		loadingMsg := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorHighlight).
			Render(fmt.Sprintf("Loading worker details for %s...", m.currentWorkerName))

		bodyView := lipgloss.Place(m.width, m.height-footerHeight-2, lipgloss.Center, lipgloss.Center, loadingMsg)
		spacing := createSpacing(m.height, lipgloss.Height(bodyView), footerHeight, 0)
		fullContent := lipgloss.JoinVertical(lipgloss.Left, bodyView, strings.Repeat("\n", spacing), footer)
		return BaseStyle.Render(fullContent)
	}

	w := *m.workerDetails

	// Header
	header := m.renderWorkerDetailHeaderView()

	// Content sections
	configSection := m.renderModernConfigSection(w)
	metricsSection := m.renderModernMetricsSection(w)

	// Responsive layout
	minWidthForHorizontal := 100
	var bodyContent string

	if m.width >= minWidthForHorizontal {
		// Side-by-side layout
		halfWidth := (m.width - 8) / 2
		configSection = lipgloss.NewStyle().Width(halfWidth).Render(configSection)
		metricsSection = lipgloss.NewStyle().Width(halfWidth).Render(metricsSection)
		bodyContent = lipgloss.JoinHorizontal(lipgloss.Top, configSection, "", metricsSection)
	} else {
		// Vertical layout
		bodyContent = lipgloss.JoinVertical(lipgloss.Left, configSection, "", metricsSection)
	}

	bodyContent = lipgloss.NewStyle().Padding(0, 1).Render(bodyContent)
	fullBodyContent := lipgloss.JoinVertical(lipgloss.Left, "", header, "", bodyContent)

	// Calculate viewport and scrolling
	availableHeight := m.height - footerHeight - 2
	contentHeight := lipgloss.Height(fullBodyContent)

	// Clamp scroll offset
	maxScroll := max(0, contentHeight-availableHeight)
	if m.workerDetailScrollY > maxScroll {
		m.workerDetailScrollY = maxScroll
	}
	if m.workerDetailScrollY < 0 {
		m.workerDetailScrollY = 0
	}

	// Apply scrolling if content overflows
	var visibleContent string
	if contentHeight > availableHeight {
		lines := strings.Split(fullBodyContent, "\n")
		start := m.workerDetailScrollY
		end := min(start+availableHeight, len(lines))
		if start >= len(lines) {
			start = len(lines) - 1
		}
		if end > len(lines) {
			end = len(lines)
		}
		visibleContent = strings.Join(lines[start:end], "\n")
	} else {
		visibleContent = fullBodyContent
	}

	// Anchor footer at bottom with proper spacing
	bodyViewHeight := lipgloss.Height(visibleContent)
	spacing := createSpacing(m.height, bodyViewHeight, footerHeight, 0)
	fullContent := lipgloss.JoinVertical(lipgloss.Left, visibleContent, strings.Repeat("\n", spacing), footer)

	return BaseStyle.Render(fullContent)
}

// renderWorkerDetailFooter renders the worker detail view footer
func (m Model) renderWorkerDetailFooter() string {
	// Only polling info now (updated time moved to header)
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5
	}

	// Add scroll indicator if scrolling is active
	pollInfo := fmt.Sprintf("Polling: %ds", interval)
	if m.workerDetailScrollY > 0 {
		pollInfo = fmt.Sprintf("↓ %d | Polling: %ds", m.workerDetailScrollY, interval)
	}

	// Key bindings
	var footerKeys string
	if m.inWorkerDetailAction {
		footerKeys = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" back • "),
			HelpKeyStyle.Render("p"), HelpDescStyle.Render(" pause • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" resume • "),
			HelpKeyStyle.Render("s"), HelpDescStyle.Render(" stop • "),
			HelpKeyStyle.Render("R"), HelpDescStyle.Render(" restart"),
		)
	} else {
		footerKeys = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" scroll • "),
			HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" back • "),
			HelpKeyStyle.Render("a"), HelpDescStyle.Render(" actions • "),
			HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit"),
		)
	}

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(footerKeys)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}
