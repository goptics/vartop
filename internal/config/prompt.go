package config

import (
	"fmt"
	"strconv"
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

	server, err := PromptServer()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		DefaultServer: server.Name,
		Servers: map[string]*Server{
			server.Name: server,
		},
	}

	if err := Save(cfg); err != nil {
		return nil, fmt.Errorf("failed to save initial config: %w", err)
	}

	path, _ := Path()
	fmt.Printf("\n✓ Configuration saved to %s\n", path)
	if server.Hub {
		fmt.Printf("Start vartop web with --hub-listen, and give the VarMQ instances this token:\n  %s\n", server.Token)
	}
	fmt.Println()
	return cfg, nil
}

// PromptServer interactively asks the user for a single server configuration.
func PromptServer() (*Server, error) {
	var (
		name       string
		serverType string
		endpoint   string
		dnsHost    string
		dnsPort    string
		token      string
	)

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
			huh.NewSelect[string]().
				Title("Server Type").
				Description("How should vartop discover this server?").
				Options(
					huh.NewOption("Static endpoints", "endpoints"),
					huh.NewOption("DNS (all addresses of a hostname)", "dns"),
					huh.NewOption("Hub (instances connect to vartop web)", "hub"),
				).
				Value(&serverType),
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
					return nil
				}),
		).WithHideFunc(func() bool { return serverType != "endpoints" }),
		huh.NewGroup(
			huh.NewInput().
				Title("DNS Host").
				Description("Hostname resolving to every instance, e.g. a headless service or compose service name").
				Value(&dnsHost).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("host cannot be empty")
					}
					return nil
				}),
			huh.NewInput().
				Title("Port").
				Description("Port each instance serves the varmq mux on").
				Value(&dnsPort).
				Placeholder("8080").
				Validate(func(s string) error {
					if p, err := strconv.Atoi(s); err != nil || p <= 0 || p > 65535 {
						return fmt.Errorf("port must be between 1 and 65535")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return serverType != "dns" }),
		huh.NewGroup(
			huh.NewInput().
				Title("Auth Token (Optional)").
				Description("Bearer token if your server requires authentication").
				Value(&token),
		).WithHideFunc(func() bool { return serverType == "hub" }),
		huh.NewGroup(
			huh.NewInput().
				Title("Hub Token").
				Description(fmt.Sprintf("The token VarMQ instances connect with (at least %d characters); leave empty to generate one", MinHubToken)).
				Value(&token).
				EchoMode(huh.EchoModePassword).
				Validate(func(s string) error {
					if s == "" {
						return nil
					}
					return ValidHubToken(s)
				}),
		).WithHideFunc(func() bool { return serverType != "hub" }),
	)

	if err := form.Run(); err != nil {
		return nil, fmt.Errorf("server prompt interrupted: %w", err)
	}

	server := &Server{
		Name:     name,
		Token:    token,
		Interval: 5,
	}

	switch serverType {
	case "endpoints":
		server.Endpoints = []string{endpoint}
	case "dns":
		port, _ := strconv.Atoi(dnsPort) // validated by the form
		server.DNS = &DNSRef{Host: dnsHost, Port: port}
	case "hub":
		server.Hub = true
		if server.Token == "" {
			server.Token = NewHubToken()
		}
	}

	if err := server.Validate(); err != nil {
		return nil, err
	}

	return server, nil
}
