package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	configDirName  = "vartop"
	configFileName = "config.json"
)

var (
	ErrConfigNotFound = errors.New("config file not found")
)

// KubernetesRef identifies a varmq mux endpoint discovered through Kubernetes.
type KubernetesRef struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Service   string `json:"service,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Port      int    `json:"port,omitempty"` // 0 = first port from EndpointSlice
	Path      string `json:"path,omitempty"` // default "/varmq"
}

// DNSRef discovers varmq instances by resolving a hostname to all of its
// addresses (e.g. a headless Kubernetes Service, Docker Compose service name,
// or Consul DNS) and dialing each one.
type DNSRef struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	Path string `json:"path,omitempty"` // default "/varmq"
}

// Server represents a single varmq server connection definition.
type Server struct {
	Name       string         `json:"name"`
	Endpoints  []string       `json:"endpoints,omitempty"`
	Kubernetes *KubernetesRef `json:"kubernetes,omitempty"`
	DNS        *DNSRef        `json:"dns,omitempty"`
	Token      string         `json:"token,omitempty"`
	Interval   int            `json:"interval,omitempty"` // Polling interval in seconds (0 for default)
}

// Validate checks that exactly one of Endpoints, Kubernetes or DNS is
// configured and that the chosen block has its required fields.
func (s *Server) Validate() error {
	hasEndpoints := len(s.Endpoints) > 0
	hasKubernetes := s.Kubernetes != nil
	hasDNS := s.DNS != nil

	set := 0
	for _, has := range []bool{hasEndpoints, hasKubernetes, hasDNS} {
		if has {
			set++
		}
	}
	if set > 1 {
		return fmt.Errorf("server %q: exactly one of endpoints, kubernetes or dns must be set, got %d", s.Name, set)
	}
	if set == 0 {
		return fmt.Errorf("server %q: exactly one of endpoints, kubernetes or dns must be set, got neither", s.Name)
	}

	if hasDNS {
		if s.DNS.Host == "" {
			return fmt.Errorf("server %q: dns host is required", s.Name)
		}
		if s.DNS.Port <= 0 || s.DNS.Port > 65535 {
			return fmt.Errorf("server %q: dns port must be between 1 and 65535", s.Name)
		}
	}

	if hasKubernetes {
		if s.Kubernetes.Context == "" {
			return fmt.Errorf("server %q: kubernetes context is required", s.Name)
		}
		if s.Kubernetes.Namespace == "" {
			return fmt.Errorf("server %q: kubernetes namespace is required", s.Name)
		}
		if s.Kubernetes.Service != "" && s.Kubernetes.Selector != "" {
			return fmt.Errorf("server %q: exactly one of kubernetes service or selector must be set, got both", s.Name)
		}
		if s.Kubernetes.Service == "" && s.Kubernetes.Selector == "" {
			return fmt.Errorf("server %q: exactly one of kubernetes service or selector must be set, got neither", s.Name)
		}
	}

	return nil
}

// GetEndpoints returns the list of endpoints to query.
func (s *Server) GetEndpoints() []string {
	return s.Endpoints
}

// DisplayEndpoint returns a human-readable representation of the endpoint(s).
// For single endpoints, returns the full URL; for multi-pod, returns "N pods".
func (s *Server) DisplayEndpoint() string {
	if len(s.Endpoints) == 0 {
		return "(no endpoints)"
	}
	if len(s.Endpoints) == 1 {
		return s.Endpoints[0]
	}
	return fmt.Sprintf("%d pods", len(s.Endpoints))
}

// ShortOrigin returns a compact display label for a given endpoint URL.
func ShortOrigin(endpoint string) string {
	// Strip scheme
	u := endpoint
	for _, prefix := range []string{"https://", "http://"} {
		if len(u) > len(prefix) && u[:len(prefix)] == prefix {
			u = u[len(prefix):]
			break
		}
	}
	// Take host only (before first / or :)
	for i, c := range u {
		if c == '/' || c == ':' {
			return u[:i]
		}
	}
	return u
}

// Config represents the schema of the vartop configuration file.
type Config struct {
	DefaultServer string             `json:"default_server"`
	Servers       map[string]*Server `json:"servers"`
}

// configPath returns the absolute path to the vartop config file.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not determine user config directory: %w", err)
	}

	return filepath.Join(dir, configDirName, configFileName), nil
}

// Load reads the configuration from os.UserConfigDir()/vartop/config.json.
// If the file does not exist, it returns ErrConfigNotFound.
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if cfg.Servers == nil {
		cfg.Servers = make(map[string]*Server)
	}

	for name, server := range cfg.Servers {
		if server == nil {
			return nil, fmt.Errorf("server %q: empty definition", name)
		}
		if err := server.Validate(); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}

// Save writes the configuration to os.UserConfigDir()/vartop/config.json.
func Save(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// GetActiveServer returns the Server block corresponding to the active context.
// If serverName is empty, it uses the DefaultServer defined in the Config.
func (cfg *Config) GetActiveServer(serverName string) (*Server, error) {
	name := serverName
	if name == "" {
		name = cfg.DefaultServer
	}

	if name == "" {
		return nil, errors.New("no default server configured")
	}

	server, exists := cfg.Servers[name]
	if !exists {
		return nil, fmt.Errorf("server '%s' not found in configuration", name)
	}

	return server, nil
}
