package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// detailScreen is the worker detail overlay. It owns the fetched details and
// the scroll/action state for the worker it was opened for.
type detailScreen struct {
	details    *api.WorkerDetails
	workerName string
	origin     string

	inAction  bool // action mode
	scrollY   int  // vertical scroll offset
	maxScroll int  // maximum scroll value (0 if no overflow)

	keys detailKeyMap
}

func newDetailScreen(workerName, origin string) *detailScreen {
	return &detailScreen{workerName: workerName, origin: origin, keys: newDetailKeyMap()}
}

func (d *detailScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	if d.inAction {
		return d.handleActionKey(m, keyMsg)
	}
	return d.handleKey(m, keyMsg)
}

// handleActionKey processes keys while the action menu is open.
func (d *detailScreen) handleActionKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, d.keys.Quit):
		return tea.Quit
	case key.Matches(msg, d.keys.Back):
		d.inAction = false
	case key.Matches(msg, d.keys.Pause):
		d.inAction = false
		return m.actionCurrentWorkerCmd(d.workerName, d.origin, "pause")
	case key.Matches(msg, d.keys.Resume):
		d.inAction = false
		return m.actionCurrentWorkerCmd(d.workerName, d.origin, "resume")
	case key.Matches(msg, d.keys.Stop):
		d.inAction = false
		return m.actionCurrentWorkerCmd(d.workerName, d.origin, "stop")
	case key.Matches(msg, d.keys.Restart):
		d.inAction = false
		return m.actionCurrentWorkerCmd(d.workerName, d.origin, "restart")
	}
	return nil
}

// handleKey processes keys in the normal (non-action) mode.
func (d *detailScreen) handleKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, d.keys.Quit):
		return tea.Quit
	case key.Matches(msg, d.keys.Back):
		m.popScreen()
		d.details = nil
		d.workerName = ""
		d.origin = ""
		d.inAction = false
		d.scrollY = 0 // Reset scroll
		return tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
	case key.Matches(msg, d.keys.Actions):
		if d.workerName != "" {
			d.inAction = !d.inAction
		}
	case key.Matches(msg, d.keys.Refresh):
		if d.workerName != "" {
			d.details = nil // Clear to show loading state
			d.scrollY = 0
			d.maxScroll = 0
			return m.fetchWorkerDetailsCmd(d.workerName, d.origin)
		}
	case key.Matches(msg, d.keys.Up):
		if d.scrollY > 0 {
			d.scrollY--
		}
	case key.Matches(msg, d.keys.Down):
		if d.scrollY < d.maxScroll {
			d.scrollY++
		}
	case key.Matches(msg, d.keys.PageUp):
		d.scrollY -= 10
		if d.scrollY < 0 {
			d.scrollY = 0
		}
	case key.Matches(msg, d.keys.PageDown):
		d.scrollY += 10
		if d.scrollY > d.maxScroll {
			d.scrollY = d.maxScroll
		}
	case key.Matches(msg, d.keys.Home):
		d.scrollY = 0
	case key.Matches(msg, d.keys.End):
		d.scrollY = d.maxScroll
	}
	return nil
}

// recalculateMaxScroll updates the maximum scroll value for the detail view.
func (d *detailScreen) recalculateMaxScroll(height int) {
	if d.details == nil {
		d.maxScroll = 0
		return
	}

	// Estimate content height based on actual worker details
	// Header ~6 lines, config ~15 lines (5 items * 3 lines), metrics ~20 lines
	estimatedContentHeight := 45
	footerHeight := 3
	availableHeight := height - footerHeight - 2

	maxScroll := estimatedContentHeight - availableHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	d.maxScroll = maxScroll

	// Clamp current scroll position
	if d.scrollY > d.maxScroll {
		d.scrollY = d.maxScroll
	}
}

// View renders the complete worker detail view.
func (d *detailScreen) View(m *Model) string {
	footer := d.renderFooter(m)
	footerHeight := lipgloss.Height(footer)

	if d.details == nil {
		loadingMsg := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorHighlight).
			Render(fmt.Sprintf("Loading worker details for %s...", d.workerName))

		bodyView := lipgloss.Place(m.width, m.height-footerHeight-2, lipgloss.Center, lipgloss.Center, loadingMsg)
		spacing := createSpacing(m.height, lipgloss.Height(bodyView), footerHeight, 0)
		fullContent := lipgloss.JoinVertical(lipgloss.Left, bodyView, strings.Repeat("\n", spacing), footer)
		return BaseStyle.Render(fullContent)
	}

	w := *d.details

	// Header
	header := d.renderHeader(m)

	// Content sections
	configSection := d.renderModernConfigSection(w)
	metricsSection := d.renderModernMetricsSection(w)

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
	if d.scrollY > maxScroll {
		d.scrollY = maxScroll
	}
	if d.scrollY < 0 {
		d.scrollY = 0
	}

	// Apply scrolling if content overflows
	var visibleContent string
	if contentHeight > availableHeight {
		lines := strings.Split(fullBodyContent, "\n")
		start := d.scrollY
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

// renderHeader renders the worker detail view header.
func (d *detailScreen) renderHeader(m *Model) string {
	if d.details == nil {
		return ""
	}

	w := *d.details

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
		showEndpoint := w.Origin
		if showEndpoint == "" {
			showEndpoint = m.server.DisplayEndpoint()
		}
		endpointInfo := fmt.Sprintf("→ %s", config.ShortOrigin(showEndpoint))
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
	showEndpoint := w.Origin
	if showEndpoint == "" {
		showEndpoint = m.server.DisplayEndpoint()
	}
	endpointValue := lipgloss.NewStyle().Foreground(ColorSubtle).Render(config.ShortOrigin(showEndpoint))

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

// renderFooter renders the worker detail view footer.
func (d *detailScreen) renderFooter(m *Model) string {
	// Only polling info now (updated time moved to header)
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5
	}

	// Add scroll indicator if scrolling is active
	pollInfo := fmt.Sprintf("Polling: %ds", interval)
	if d.scrollY > 0 {
		pollInfo = fmt.Sprintf("↓ %d | Polling: %ds", d.scrollY, interval)
	}

	keysView := helpFooter(d.keyMap(), m.width-lipgloss.Width(pollInfo)-4)

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}

// renderModernConfigSection renders the configuration section with modern styling
func (d *detailScreen) renderModernConfigSection(w api.WorkerDetails) string {
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("⚙️  Configuration")

	// Status indicator with color
	statusStyle := getStatusStyle(w.Status)
	statusDot := statusStyle.Render("●")
	statusText := statusStyle.Render(w.Status)

	// Build rows with proper spacing
	rows := []string{
		buildDataRow("Status", lipgloss.JoinHorizontal(lipgloss.Left, statusDot, " ", statusText)),
		buildDataRow("Concurrency", fmt.Sprintf("%d workers", w.Concurrency)),
		buildDataRow("Processing", fmt.Sprintf("%d jobs", w.NumProcessing)),
		buildDataRow("Pending", fmt.Sprintf("%d jobs", w.NumPending)),
		buildDataRow("Idle Workers", fmt.Sprintf("%d available", w.NumIdleWorkers)),
	}

	// Add separator
	separator := lipgloss.NewStyle().
		Foreground(ColorSubtle).
		Render(strings.Repeat("─", 30))

	content := lipgloss.JoinVertical(lipgloss.Left,
		"",
		rows[0], separator, rows[1], rows[2], rows[3], rows[4],
	)

	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(ColorHighlight).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, title, content))
}

// renderModernMetricsSection renders the performance metrics section with modern styling
func (d *detailScreen) renderModernMetricsSection(w api.WorkerDetails) string {
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("📊 Performance Metrics")

	// Calculate success rate based on completed attempts (successful + failed)
	var successRate float64
	totalAttempts := w.Metrics.Successful + w.Metrics.Failed
	if totalAttempts > 0 {
		successRate = float64(w.Metrics.Successful) / float64(totalAttempts) * 100
	}

	// Build metric rows with icons and colors
	metricsData := []struct {
		label string
		value string
		color lipgloss.Color
		icon  string
	}{
		{"Submitted", fmt.Sprintf("%d", w.Metrics.Submitted), ColorHighlight, "📤"},
		{"Completed", fmt.Sprintf("%d", w.Metrics.Completed), ColorSuccess, "✅"},
		{"Successful", fmt.Sprintf("%d", w.Metrics.Successful), ColorSuccess, "✓"},
		{"Failed", fmt.Sprintf("%d", w.Metrics.Failed), ColorError, "✗"},
		{"Success Rate", fmt.Sprintf("%.1f%%", successRate), ColorWarning, "📈"},
	}

	var rows []string
	for _, metric := range metricsData {
		icon := lipgloss.NewStyle().Foreground(metric.color).Render(metric.icon)
		value := lipgloss.NewStyle().
			Foreground(metric.color).
			Bold(true).
			Render(metric.value)

		row := buildDataRow(metric.label, lipgloss.JoinHorizontal(lipgloss.Left, icon, " ", value))
		rows = append(rows, row)
	}

	// Add separator
	separator := lipgloss.NewStyle().
		Foreground(ColorSubtle).
		Render(strings.Repeat("─", 30))

	content := lipgloss.JoinVertical(lipgloss.Left,
		"",
		rows[0], separator, rows[1], rows[2], rows[3], rows[4],
	)

	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(ColorHighlight).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, title, content))
}
