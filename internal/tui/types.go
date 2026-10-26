package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
)

// Message types for Bubble Tea

// tickMsg comes every syncEvery: the model syncs the feed of the server it
// shows, and reads the config again.
type tickMsg time.Time

// syncedMsg is a sync of the feed of the server keyed server ended, for the
// model's generation gen; Update drops it unless both are still current
// (see Model.gen). It also says what control.db said at the sync: whether
// no daemon holds a fresh lease (down), and the server's collection error.
type syncedMsg struct {
	server string
	gen    int
	err    error
	down   bool
	srvErr string
}

// configMsg carries the config read again.
type configMsg struct{ cfg *config.Config }

// switcherMsg carries the newest stored snapshot of each configured
// server, by key (nil: none yet), for the switcher.
type switcherMsg struct{ latest map[string]*fleet.Snapshot }

// actedMsg is an action or a refresh (r) on the server keyed server ended,
// for generation gen; err says why it failed or was refused.
type actedMsg struct {
	server string
	gen    int
	err    error
}

// resyncMsg asks for a sync of the server keyed server, generation gen,
// shortly after an action.
type resyncMsg struct {
	server string
	gen    int
}

// List item types

// switchServerItem represents a server (merged view of its units) in the
// switcher: units and healthy count its newest snapshot's units.
type switchServerItem struct {
	name           string
	noun           fleet.Noun
	units, healthy int
	expanded       bool
}

func (i switchServerItem) Title() string {
	indicator := "▶"
	if i.expanded {
		indicator = "▼"
	}

	if i.units <= 1 {
		return fmt.Sprintf("%s %s", indicator, i.name)
	}

	dot := StatusStyleRunning.Render("●")
	if i.healthy == 0 {
		dot = StatusStyleStopped.Render("○")
	} else if i.healthy < i.units {
		dot = StatusStyleWarning.Render("◐")
	}
	return fmt.Sprintf("%s %s %s (%d %s, %d healthy)", indicator, dot, i.name, i.units, i.noun.Many, i.healthy)
}
func (i switchServerItem) Description() string { return "" }
func (i switchServerItem) FilterValue() string { return i.name }

// switchPodItem represents one unit, by its label, under a server in the
// switcher.
type switchPodItem struct {
	label   string
	server  string
	healthy bool
}

func (i switchPodItem) Title() string {
	var dot string
	if i.healthy {
		dot = StatusStyleRunning.Render("●")
	} else {
		dot = StatusStyleStopped.Render("○")
	}
	return fmt.Sprintf("  └─ %s %s", dot, i.label)
}
func (i switchPodItem) Description() string { return "" }
func (i switchPodItem) FilterValue() string { return i.label }

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
