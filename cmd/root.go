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
// returned. Onboarding without any config (the first run) also offers the
// daemon's login service, when there is a terminal to ask on.
func loadOrOnboard() (*config.Config, error) {
	cfg, err := config.Load()
	switch {
	case err == nil && len(cfg.Servers) > 0:
		return cfg, nil
	case err == nil || errors.Is(err, config.ErrConfigNotFound):
		firstRun := err != nil
		cfg, err := config.PromptOnboarding()
		if err == nil && firstRun && isTerminal(os.Stdin) {
			offerLoginService(config.PromptDaemonAtLogin)
		}
		return cfg, err
	default:
		return nil, err
	}
}

// Execute runs vartop and exits non-zero on an error: with the code of an
// exitError, else 1.
func Execute() {
	err := rootCmd.Execute()
	if err == nil {
		return
	}
	code := 1
	var exit exitError
	if errors.As(err, &exit) {
		code, err = exit.code, exit.err
	}
	if err != nil {
		fmt.Println(err)
	}
	os.Exit(code)
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&serverFlag, "server", "s", "", "Specify the varmq server name to connect to (overrides default)")
}
