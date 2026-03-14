package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage vartop server connections and contexts",
}

var serverListCmd = &cobra.Command{
	Use:   "list",
	Short: "Display all configured mux servers and their health status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		fmt.Printf("Default Server: %s\n", cfg.DefaultServer)
		fmt.Println("\nConfigured Servers:")
		for name, server := range cfg.Servers {
			prefix := "  "
			if name == cfg.DefaultServer {
				prefix = "* "
			}

			// Check health
			client := api.NewClient(server)
			status := "Active"
			if err := client.GetHealth(); err != nil {
				status = "Inactive"
			}

			fmt.Printf("%s%s [%s]\n", prefix, name, status)
			fmt.Printf("    Endpoint: %s\n", server.Endpoint)
			if server.Token != "" {
				fmt.Printf("    Token:    ***\n")
			}
			fmt.Printf("    Interval: %ds\n", server.Interval)
		}
		return nil
	},
}

var serverAddCmd = &cobra.Command{
	Use:   "add <name> <endpoint> [token]",
	Short: "Add a new varmq server connection",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			if err == config.ErrConfigNotFound {
				// Initialize an empty config if one doesn't exist
				cfg = &config.Config{Servers: make(map[string]*config.Server)}
			} else {
				return err
			}
		}

		name := args[0]
		endpoint := args[1]
		token := ""
		if len(args) == 3 {
			token = args[2]
		}

		if _, exists := cfg.Servers[name]; exists {
			return fmt.Errorf("server '%s' already exists", name)
		}

		cfg.Servers[name] = &config.Server{
			Name:     name,
			Endpoint: endpoint,
			Token:    token,
			Interval: 5, // Default polling
		}

		// Use as default if it's the first one
		if len(cfg.Servers) == 1 {
			cfg.DefaultServer = name
		}

		if err := config.Save(cfg); err != nil {
			return err
		}

		fmt.Printf("Added server '%s' (%s)\n", name, endpoint)
		return nil
	},
}

var serverUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the default varmq server connection",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		name := args[0]
		if _, exists := cfg.Servers[name]; !exists {
			return fmt.Errorf("server '%s' not found", name)
		}

		cfg.DefaultServer = name
		if err := config.Save(cfg); err != nil {
			return err
		}

		fmt.Printf("Default server set to '%s'\n", name)
		return nil
	},
}

var removeAll bool

var serverRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a varmq server connection",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		if removeAll {
			cfg.Servers = make(map[string]*config.Server)
			cfg.DefaultServer = ""
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Println("Removed all servers.")
			return nil
		}

		if len(args) == 0 {
			return fmt.Errorf("must specify a server name to remove or use --all flag")
		}

		name := args[0]
		if _, exists := cfg.Servers[name]; !exists {
			return fmt.Errorf("server '%s' not found", name)
		}

		delete(cfg.Servers, name)

		if len(cfg.Servers) == 0 {
			cfg.DefaultServer = ""
			fmt.Println("Removed the last server. vartop will prompt for a new one on next run.")
		} else if cfg.DefaultServer == name {
			// Reassign default server if we just deleted the default
			for remainingName := range cfg.Servers {
				cfg.DefaultServer = remainingName
				fmt.Printf("Default server automatically reassigned to '%s'\n", remainingName)
				break
			}
		}

		if err := config.Save(cfg); err != nil {
			return err
		}

		if !removeAll {
			fmt.Printf("Removed server '%s'\n", name)
		}
		return nil
	},
}

var serverSetCmd = &cobra.Command{
	Use:   "set <name> <key> <value>",
	Short: "Update a specific property (endpoint, token, interval) of a server",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		name := args[0]
		key := args[1]
		value := args[2]

		server, exists := cfg.Servers[name]
		if !exists {
			return fmt.Errorf("server '%s' not found", name)
		}

		switch key {
		case "endpoint":
			if value == "" {
				return fmt.Errorf("endpoint cannot be empty")
			}
			server.Endpoint = value
		case "token":
			server.Token = value // allow empty token to clear it
		case "interval":
			// We need to parse interval as an int
			interval, err := strconv.Atoi(value)
			if err != nil || interval <= 0 {
				return fmt.Errorf("interval must be a positive integer")
			}
			server.Interval = interval
		default:
			return fmt.Errorf("unknown key '%s'. valid keys: endpoint, token, interval", key)
		}

		if err := config.Save(cfg); err != nil {
			return err
		}

		fmt.Printf("Set '%s' to '%s' for server '%s'\n", key, value, name)
		return nil
	},
}

func init() {
	serverRemoveCmd.Flags().BoolVar(&removeAll, "all", false, "Remove all configured servers")
	serverCmd.AddCommand(serverListCmd)
	serverCmd.AddCommand(serverAddCmd)
	serverCmd.AddCommand(serverUseCmd)
	serverCmd.AddCommand(serverRemoveCmd)
	serverCmd.AddCommand(serverSetCmd)
	rootCmd.AddCommand(serverCmd)
}
