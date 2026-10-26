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
	fmt.Printf("\n✓ Configuration saved to %s\n\n", path)
	return cfg, nil
}

// daemonOffer is the onboarding's question about the vartop daemon's
// login service.
const daemonOffer = "Start the vartop daemon at login? It keeps collecting history in the background."

// PromptDaemonAtLogin asks whether the vartop daemon should start at login.
// Onboarding asks it once, after saving the first config.
func PromptDaemonAtLogin() (bool, error) {
	yes := true
	err := huh.NewConfirm().Title(daemonOffer).Value(&yes).Run()
	return yes, err
}

// PromptServer interactively asks the user for a single server configuration.
func PromptServer() (*Server, error) {
	var (
		name          string
		serverType    string
		endpoint      string
		context       string
		namespace     string
		discoveryType string
		service       string
		selector      string
		dnsHost       string
		dnsPort       string
		token         string
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
					huh.NewOption("Kubernetes", "kubernetes"),
					huh.NewOption("DNS (all addresses of a hostname)", "dns"),
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
				Title("Kubernetes Context").
				Description("The kubectl context to use").
				Value(&context).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("context cannot be empty")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return serverType != "kubernetes" }),
		huh.NewGroup(
			huh.NewInput().
				Title("Kubernetes Namespace").
				Description("The namespace containing the varmq service").
				Value(&namespace).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("namespace cannot be empty")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return serverType != "kubernetes" }),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Kubernetes Discovery").
				Description("Discover pods by service name or label selector").
				Options(
					huh.NewOption("Service", "service"),
					huh.NewOption("Label selector", "selector"),
				).
				Value(&discoveryType),
		).WithHideFunc(func() bool { return serverType != "kubernetes" }),
		huh.NewGroup(
			huh.NewInput().
				Title("Service Name").
				Description("The Kubernetes service name").
				Value(&service).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("service cannot be empty")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return serverType != "kubernetes" || discoveryType != "service" }),
		huh.NewGroup(
			huh.NewInput().
				Title("Label Selector").
				Description("e.g., app=varmq-mux").
				Value(&selector).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("selector cannot be empty")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return serverType != "kubernetes" || discoveryType != "selector" }),
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
		),
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
	case "kubernetes":
		server.Kubernetes = &KubernetesRef{
			Context:   context,
			Namespace: namespace,
		}
		switch discoveryType {
		case "service":
			server.Kubernetes.Service = service
		case "selector":
			server.Kubernetes.Selector = selector
		}
	case "dns":
		port, _ := strconv.Atoi(dnsPort) // validated by the form
		server.DNS = &DNSRef{Host: dnsHost, Port: port}
	}

	if err := server.Validate(); err != nil {
		return nil, err
	}

	return server, nil
}
