package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// logoLines is the Lookout mark in half blocks, sampled from the 16 px favicon
// (internal/web/static/favicon.svg): one char = one pixel wide, two tall.
var logoLines = []string{
	"   ▄   ██   ▄",
	"   ██▄████▄██",
	"  ████████████",
	"  ███  ██  ███",
	"   ▀████████▀",
	" ██████████████",
}

// banner renders the mark in the logo green with the wordmark beside its eyes.
func banner() string {
	green := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#1B6E5A", Dark: "#44BF9E"})
	bold := lipgloss.NewStyle().Bold(true)
	var b strings.Builder
	b.WriteString("\n")
	for i, line := range logoLines {
		b.WriteString(green.Render(line))
		if i == 3 { // the eye row
			b.WriteString(strings.Repeat(" ", 16-len([]rune(line))) + bold.Render("var") + bold.Inherit(green).Render("top"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// PromptOnboarding interactively asks the user for their initial
// server configuration and saves it. If the prompt is cancelled or
// fails, it returns an error.
func PromptOnboarding() (*Config, error) {
	textStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#8BE9FD")). // Cyan retro vibe
		Padding(1, 0, 1, 0)

	fmt.Println(banner())

	introText := "Welcome to the Dashboard!\nIt looks like you don't have a configuration file yet.\nLet's set up your first varmq server connection to get started."
	fmt.Println(textStyle.Render(introText))
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	server, generated, err := PromptServer()
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
		fmt.Print(HubAdded(server, generated))
	}
	fmt.Println()
	return cfg, nil
}

// HubAdded is what to tell the user once hub server srv is saved: where
// its instances connect, and its token only when vartop made it (generated):
// a token typed in is not echoed to the terminal.
func HubAdded(srv *Server, generated bool) string {
	if generated {
		return fmt.Sprintf("Start vartop web with --hub-listen, and give the VarMQ instances this token:\n  %s\n", srv.Token)
	}
	return "Start vartop web with --hub-listen, and give the VarMQ instances the token you entered.\n"
}

// PromptServer interactively asks the user for a single server
// configuration; generated reports a hub token vartop made.
func PromptServer() (server *Server, generated bool, err error) {
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
		return nil, false, fmt.Errorf("server prompt interrupted: %w", err)
	}

	server = &Server{
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
			server.Token, generated = NewHubToken(), true
		}
	}

	if err := server.Validate(); err != nil {
		return nil, false, err
	}

	return server, generated, nil
}
