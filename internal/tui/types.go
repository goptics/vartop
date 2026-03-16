package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
)

// Message types for Bubble Tea

// tickMsg is sent periodically to trigger data refresh
type tickMsg time.Time

// workersMsg carries the list of workers from the API
type workersMsg []api.WorkerSummary

// workerDetailsMsg carries detailed information about a single worker
type workerDetailsMsg *api.WorkerDetails

// healthMsg carries the health check result
type healthMsg struct{ ok bool }

// errMsg carries an error that should be displayed to the user
type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

// List item types

// serverItem implements list.Item for the server switcher
type serverItem struct {
	name     string
	endpoint string
}

func (i serverItem) Title() string       { return i.name }
func (i serverItem) Description() string { return i.endpoint }
func (i serverItem) FilterValue() string { return i.name }

// Helper functions

// getStatusStyle returns the appropriate style for a worker status
func getStatusStyle(status string) lipgloss.Style {
	switch status {
	case "Running":
		return StatusStyleRunning
	case "Paused":
		return StatusStylePaused
	case "Stopped":
		return StatusStyleStopped
	default:
		return StatusStyleIdle
	}
}

// buildDataRow creates a formatted data row with label and value
func buildDataRow(label, value string) string {
	labelStyle := lipgloss.NewStyle().
		Foreground(ColorSubtle).
		Width(15).
		Render(label)

	valueStyle := lipgloss.NewStyle().
		Foreground(ColorFg).
		Bold(true).
		Render(value)

	return lipgloss.JoinHorizontal(lipgloss.Left, labelStyle, valueStyle)
}

// createSpacing calculates the spacing needed between content and footer
func createSpacing(totalHeight, contentHeight, footerHeight, margin int) int {
	spacing := totalHeight - contentHeight - footerHeight - margin
	if spacing < 1 {
		return 1 // Minimum spacing
	}
	return spacing
}

// Error wrapping helper
func wrapError(action string, err error) error {
	return fmt.Errorf("%s: %w", action, err)
}
