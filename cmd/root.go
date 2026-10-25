package cmd

import (
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/tui"
)

var (
	serverFlag string
)

var rootCmd = &cobra.Command{
	Use:   "vartop",
	Short: "vartop is a CLI tool and TUI for monitoring and managing varmq workers",
	Long: `vartop provides a rich Terminal User Interface to view worker statuses, 
concurrencies, and metrics from varmq mux servers.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadOrOnboard()
		if err != nil {
			return err
		}

		server, err := cfg.GetActiveServer(serverFlag)
		if err != nil {
			return err
		}

		m := tui.NewModel(cfg, server)
		p := tea.NewProgram(m, tea.WithAltScreen())

		finalModel, err := p.Run()
		// Quit tears down the pool's discovery source (stops any informer).
		// Bubbletea swaps in a model copy on every Update (value receivers), so
		// only the final model holds the current pool after a server switch.
		var closeErr error
		switch m := finalModel.(type) {
		case tui.Model:
			closeErr = m.Close()
		case *tui.Model:
			closeErr = m.Close()
		}
		if err == nil {
			err = closeErr
		}
		return err
	},
}

// loadOrOnboard loads the configuration, starting the interactive onboarding
// when there is none yet (or it lists no servers). Any other load error is
// returned.
func loadOrOnboard() (*config.Config, error) {
	cfg, err := config.Load()
	switch {
	case err == nil && len(cfg.Servers) > 0:
		return cfg, nil
	case err == nil || errors.Is(err, config.ErrConfigNotFound):
		return config.PromptOnboarding()
	default:
		return nil, err
	}
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&serverFlag, "server", "s", "", "Specify the varmq server name to connect to (overrides default)")
}
