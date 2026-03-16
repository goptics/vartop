package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
)

// Section rendering functions for worker details

// renderModernConfigSection renders the configuration section with modern styling
func (m Model) renderModernConfigSection(w api.WorkerDetails) string {
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
func (m Model) renderModernMetricsSection(w api.WorkerDetails) string {
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("📊 Performance Metrics")

	// Calculate success rate
	var successRate float64
	if w.Metrics.Submitted > 0 {
		successRate = float64(w.Metrics.Successful) / float64(w.Metrics.Submitted) * 100
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

// renderConfigSection renders the basic configuration section (legacy)
func (m Model) renderConfigSection(w api.WorkerDetails) string {
	sectionTitle := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("⚙️  Configuration")

	configData := []struct {
		label string
		value string
	}{
		{"Status", w.Status},
		{"Concurrency", fmt.Sprintf("%d workers", w.Concurrency)},
		{"Processing", fmt.Sprintf("%d jobs", w.NumProcessing)},
		{"Pending", fmt.Sprintf("%d jobs", w.NumPending)},
		{"Idle Workers", fmt.Sprintf("%d", w.NumIdleWorkers)},
	}

	var rows []string
	for _, item := range configData {
		label := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Width(15).
			Render(item.label)

		value := item.value
		if item.label == "Status" {
			value = getStatusStyle(item.value).Render(item.value)
		} else {
			value = lipgloss.NewStyle().
				Foreground(ColorFg).
				Bold(true).
				Render(value)
		}

		row := lipgloss.JoinHorizontal(lipgloss.Left, label, value)
		rows = append(rows, row)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, rows...)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorSubtle).
		Padding(1).
		Render(lipgloss.JoinVertical(lipgloss.Left, sectionTitle, content))
}

// renderCompactMetricsSection renders a compact metrics section
func (m Model) renderCompactMetricsSection(w api.WorkerDetails) string {
	sectionTitle := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("📊 Performance Metrics")

	// Calculate success rate
	var successRate float64
	if w.Metrics.Submitted > 0 {
		successRate = float64(w.Metrics.Successful) / float64(w.Metrics.Submitted) * 100
	}

	// Create compact metric rows
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
		iconStyle := lipgloss.NewStyle().Foreground(metric.color).Render(metric.icon)
		labelStyle := lipgloss.NewStyle().Foreground(ColorSubtle).Render(metric.label)
		valueStyle := lipgloss.NewStyle().
			Foreground(metric.color).
			Bold(true).
			Render(metric.value)

		row := lipgloss.JoinHorizontal(lipgloss.Left,
			iconStyle, " ",
			labelStyle, " ",
			lipgloss.NewStyle().Foreground(metric.color).Render("→"),
			" ", valueStyle,
		)
		rows = append(rows, row)
	}

	content := lipgloss.JoinVertical(lipgloss.Left, rows...)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorSubtle).
		Padding(1).
		Render(lipgloss.JoinVertical(lipgloss.Left, sectionTitle, content))
}

// renderMetricsSection renders the full metrics section with cards
func (m Model) renderMetricsSection(w api.WorkerDetails) string {
	sectionTitle := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("📊 Performance Metrics")

	// Calculate success rate
	var successRate float64
	if w.Metrics.Submitted > 0 {
		successRate = float64(w.Metrics.Successful) / float64(w.Metrics.Submitted) * 100
	}

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

	// Create metric cards
	var metricCards []string
	for _, metric := range metricsData {
		card := m.renderMetricCard(metric.icon, metric.label, metric.value, metric.color)
		metricCards = append(metricCards, card)
	}

	// Arrange metrics in a grid layout (2 columns)
	var leftColumn []string
	var rightColumn []string
	for i, card := range metricCards {
		if i%2 == 0 {
			leftColumn = append(leftColumn, card)
		} else {
			rightColumn = append(rightColumn, card)
		}
	}

	leftCol := lipgloss.JoinVertical(lipgloss.Left, leftColumn...)
	rightCol := lipgloss.JoinVertical(lipgloss.Left, rightColumn...)

	content := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, "  ", rightCol)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorSubtle).
		Padding(1).
		Render(lipgloss.JoinVertical(lipgloss.Left, sectionTitle, content))
}

// renderMetricCard creates a styled metric card
func (m Model) renderMetricCard(icon, label, value string, color lipgloss.Color) string {
	iconStyle := lipgloss.NewStyle().Foreground(color).Render(icon)
	labelStyle := lipgloss.NewStyle().Foreground(ColorSubtle).Render(label)
	valueStyle := lipgloss.NewStyle().
		Foreground(color).
		Bold(true).
		Render(value)

	topRow := lipgloss.JoinHorizontal(lipgloss.Left, iconStyle, " ", labelStyle)
	bottomRow := lipgloss.NewStyle().PaddingLeft(2).Render(valueStyle)

	return lipgloss.NewStyle().
		Width((m.width-8)/2 - 4).
		Padding(1).
		Border(lipgloss.NormalBorder()).
		BorderForeground(color).
		Render(lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow))
}
