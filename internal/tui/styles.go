package tui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

var (
	// Colors
	ColorBg        = lipgloss.Color("#1A1B26") // Tokyo Night dark
	ColorFg        = lipgloss.Color("#C0CAF5")
	ColorHighlight = lipgloss.Color("#7AA2F7") // Blue
	ColorSuccess   = lipgloss.Color("#9ECE6A") // Green
	ColorWarning   = lipgloss.Color("#E0AF68") // Yellow
	ColorError     = lipgloss.Color("#F7768E") // Red
	ColorSubtle    = lipgloss.Color("#565F89") // Grey

	ColorHeaderBg = lipgloss.Color("#24283B")

	// Global Styles
	BaseStyle = lipgloss.NewStyle().
			Foreground(ColorFg)

	HeaderStyle = lipgloss.NewStyle().
			Foreground(ColorHighlight).
			Bold(true).
			Padding(0, 1).
			MarginBottom(1)

	TitleStyle = lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorHighlight).
			Bold(true).
			Padding(0, 1).
			MarginRight(1)

	StatusStyleRunning = lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true)
	StatusStylePaused  = lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	StatusStyleStopped = lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	StatusStyleIdle    = lipgloss.NewStyle().Foreground(ColorFg)

	FooterStyle = lipgloss.NewStyle().
			Foreground(ColorSubtle).
			MarginTop(1)

	HelpKeyStyle = lipgloss.NewStyle().
			Foreground(ColorHighlight).
			Bold(true)

	HelpDescStyle = lipgloss.NewStyle().
			Foreground(ColorSubtle)
)

func TableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(ColorSubtle).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(ColorBg).
		Background(ColorHighlight).
		Bold(true)
	return s
}
