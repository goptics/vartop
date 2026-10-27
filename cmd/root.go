package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/config"
)

var (
	serverFlag string
)

var rootCmd = &cobra.Command{
	Use:   "vartop",
	Short: "vartop monitors and manages varmq workers from the command line and a web dashboard",
	Long: `vartop monitors and manages the workers of varmq mux servers: from the
command line, and in a web dashboard (vartop web) that collects every
configured server while it runs.`,
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
