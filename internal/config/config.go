package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	configDirName  = ".vartop"
	configFileName = "settings.json"
)

var (
	ErrConfigNotFound = errors.New("config file not found")
)

// Server represents a single varmq server connection definition.
type Server struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token,omitempty"`
	Interval int    `json:"interval,omitempty"` // Polling interval in seconds (0 for default)
}

// Config represents the schema of the ~/.vartop.json configuration file.
type Config struct {
	DefaultServer string             `json:"default_server"`
	Servers       map[string]*Server `json:"servers"`
}

// configFilePath returns the absolute path to the ~/.vartop/settings.json file.
// Creates the .vartop directory if it doesn't exist.
func configFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine user home directory: %w", err)
	}

	configDir := filepath.Join(home, configDirName)

	// Create config directory if it doesn't exist
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}

	return filepath.Join(configDir, configFileName), nil
}

// Load reads the configuration from the ~/.vartop/settings.json file.
// If the old ~/.vartop.json exists, it migrates it to the new location.
func Load() (*Config, error) {
	// Check for old config location and migrate if needed
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not determine user home directory: %w", err)
	}

	oldConfigPath := filepath.Join(home, ".vartop.json")
	if _, err := os.Stat(oldConfigPath); err == nil {
		// Old config exists, migrate it
		newConfigPath, pathErr := configFilePath()
		if pathErr != nil {
			return nil, pathErr
		}

		// Read old config
		data, err := os.ReadFile(oldConfigPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read old config: %w", err)
		}

		// Write to new location
		if err := os.WriteFile(newConfigPath, data, 0600); err != nil {
			return nil, fmt.Errorf("failed to migrate config: %w", err)
		}

		// Remove old config
		os.Remove(oldConfigPath)
	}

	path, err := configFilePath()
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

	return &cfg, nil
}

// Save writes the configuration to the ~/.vartop.json file.
func Save(cfg *Config) error {
	path, err := configFilePath()
	if err != nil {
		return err
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
