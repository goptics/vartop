package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// History is how far back the server's longer charts reach, in whole
	// days: "7d". Empty is DefaultHistory.
	History string `json:"history,omitempty"`
}

// defaultPollInterval is used when Server.Interval is unset or non-positive.
const defaultPollInterval = 5 * time.Second

// PollInterval returns the polling interval, falling back to 5s when
// Interval is 0 or negative.
func (s *Server) PollInterval() time.Duration {
	if s.Interval <= 0 {
		return defaultPollInterval
	}
	return time.Duration(s.Interval) * time.Second
}

const (
	// DefaultHistory is a server's history when it sets none: the 24h charts.
	DefaultHistory = 24 * time.Hour
	// MaxHistory is the longest history a server may set: a 30d chart is
	// 720 hourly points, a point per pixel of its 720 px.
	MaxHistory = 30 * 24 * time.Hour
)

// ParseHistory reads a server's history: whole days, "1d" to "30d", the
// number as written plainly — no sign, no leading zero.
func ParseHistory(v string) (time.Duration, error) {
	maxDays := int(MaxHistory / (24 * time.Hour))
	days, ok := strings.CutSuffix(v, "d")
	n, err := strconv.Atoi(days)
	if !ok || err != nil || days != strconv.Itoa(n) || n < 1 || n > maxDays {
		return 0, fmt.Errorf("history must be whole days from 1d to %dd, got %q", maxDays, v)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// HistorySpan returns History as a duration: DefaultHistory when it is
// unset, or not one Validate accepts.
func (s *Server) HistorySpan() time.Duration {
	d, err := ParseHistory(s.History)
	if err != nil {
		return DefaultHistory
	}
	return d
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

	if s.History != "" {
		if _, err := ParseHistory(s.History); err != nil {
			return fmt.Errorf("server %q: %w", s.Name, err)
		}
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

// ShortOrigin returns a compact display label (the host) for an endpoint URL.
func ShortOrigin(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return endpoint
}

// Config represents the schema of the vartop configuration file.
type Config struct {
	DefaultServer string             `json:"default_server"`
	Servers       map[string]*Server `json:"servers"`
}

// Path returns the absolute path to the vartop config file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("could not determine user config directory: %w", err)
	}

	return filepath.Join(dir, configDirName, configFileName), nil
}

// Load reads the configuration from os.UserConfigDir()/vartop/config.json.
// If the file does not exist, it returns ErrConfigNotFound.
func Load() (*Config, error) {
	path, err := Path()
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

// Save writes the configuration to os.UserConfigDir()/vartop/config.json. It
// writes a new temporary file (0600: it holds the tokens) beside it and
// renames that over it, so a reader never sees a half-written file; a
// write that fails leaves no temporary file behind. A config.json that is
// a symlink stays one: the file it links to is replaced.
func Save(cfg *Config) error {
	path, err := Path()
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

	// An older vartop wrote config.json.tmp, keeping the mode of one
	// there; one it left behind may hold tokens others can read.
	_ = os.Remove(path + ".tmp")
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
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
