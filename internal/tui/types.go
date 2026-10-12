package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// Message types for Bubble Tea

// tickMsg is sent periodically to trigger data refresh
type tickMsg time.Time

// workersMsg carries the list of workers from the API
type workersMsg []api.WorkerSummary

// workerDetailsMsg carries detailed information about a single worker
type workerDetailsMsg *api.WorkerDetails

// healthMsg carries the health check result
type healthMsg struct {
	ok     bool
	status map[string]bool // per-endpoint health status
}

// errMsg carries an error that should be displayed to the user
type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

// serverSwitchMsg carries the result of building a pool for a newly selected
// server: either a ready pool, or the construction error.
type serverSwitchMsg struct {
	seq  int // matches Model.switchSeq for the latest requested build
	srv  *config.Server
	pool *api.Pool
	err  error
}

// scopedMsg tags a result with the pool that produced it; Update drops it if
// that pool is no longer active.
type scopedMsg struct {
	pool *api.Pool
	msg  tea.Msg
}

// List item types

// switchServerItem represents a server (merged pods view) in the switcher
type switchServerItem struct {
	name        string
	podCount    int
	healthyPods int
	expanded    bool
}

func (i switchServerItem) Title() string {
	indicator := "▶"
	if i.expanded {
		indicator = "▼"
	}

	if i.podCount <= 1 {
		return fmt.Sprintf("%s %s", indicator, i.name)
	}

	dot := StatusStyleRunning.Render("●")
	if i.healthyPods == 0 {
		dot = StatusStyleStopped.Render("○")
	} else if i.healthyPods < i.podCount {
		dot = StatusStyleWarning.Render("◐")
	}
	return fmt.Sprintf("%s %s %s (%d pods, %d healthy)", indicator, dot, i.name, i.podCount, i.healthyPods)
}
func (i switchServerItem) Description() string { return "" }
func (i switchServerItem) FilterValue() string { return i.name }

// switchPodItem represents an individual pod under a server in the switcher
type switchPodItem struct {
	name    string
	server  string
	origin  string
	healthy bool
}

func (i switchPodItem) Title() string {
	var dot string
	if i.healthy {
		dot = StatusStyleRunning.Render("●")
	} else {
		dot = StatusStyleStopped.Render("○")
	}
	return fmt.Sprintf("  └─ %s %s", dot, i.name)
}
func (i switchPodItem) Description() string { return "" }
func (i switchPodItem) FilterValue() string { return i.name }

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
