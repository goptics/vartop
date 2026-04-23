package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/pterm/pterm"
)

// PromptOnboarding interactively asks the user for their initial
// server configuration and saves it. If the prompt is cancelled or
// fails, it returns an error.
func PromptOnboarding() (*Config, error) {
	// Retro ASCII Art and Styling
	textStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#8BE9FD")). // Cyan retro vibe
		Padding(1, 0, 1, 0)

	logo, _ := pterm.DefaultBigText.WithLetters(
		pterm.NewLettersFromStringWithStyle("var", pterm.NewStyle(pterm.FgCyan)),
		pterm.NewLettersFromStringWithStyle("top", pterm.NewStyle(pterm.FgLightMagenta)),
	).Srender()

	fmt.Println(logo)

	introText := "Welcome to the Dashboard!\nIt looks like you don't have a configuration file yet.\nLet's set up your first varmq server connection to get started."
	fmt.Println(textStyle.Render(introText))
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	var (
		name     string
		endpoint string
		token    string
	)

	// Create a new form
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Server Name").
				Description("A unique identifier for this connection (e.g., local)").
				Value(&name).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("name cannot be empty")
					}
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Mux Server Endpoint").
				Description("The full URL to the varmq mux API").
				Value(&endpoint).
				Placeholder("http://localhost:8080/varmq").
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("endpoint cannot be empty")
					}
					// Keep simple validation for now. Can add URL parsing verification later.
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Auth Token (Optional)").
				Description("Bearer token if your server requires authentication").
				Value(&token),
		),
	)

	err := form.Run()
	if err != nil {
		return nil, fmt.Errorf("onboarding interrupted: %w", err)
	}

	cfg := &Config{
		DefaultServer: name,
		Servers: map[string]*Server{
			name: {
				Name:     name,
				Endpoint: endpoint,
				Token:    token,
				Interval: 5, // Default polling interval
			},
		},
	}

	err = Save(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to save initial config: %w", err)
	}

	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, configDirName, configFileName)
	fmt.Printf("\n✓ Configuration saved to %s\n\n", configPath)
	return cfg, nil
}
