package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/store"
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

		// The store and the daemon know a server by its config key.
		key := serverFlag
		if key == "" {
			key = cfg.DefaultServer
		}
		if _, err := cfg.GetActiveServer(key); err != nil {
			return err
		}
		daemon := tuiDaemon()

		dataDir, err := store.DataDir()
		if err != nil {
			return err
		}
		ctl, err := store.OpenControl(dataDir)
		if err != nil {
			return err
		}
		defer ctl.Close()

		// The TUI reads what the daemon collects; r starts the daemon when
		// none runs, or one collects with another configuration.
		m := tui.NewModel(cfg, key, dataDir, ctl, daemon)
		p := tea.NewProgram(m, tea.WithAltScreen())

		finalModel, err := p.Run()
		// Quit closes the server files the TUI's feeds opened. Every copy of
		// the model shares them, so the final one closes them all.
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

// tuiDaemon makes sure the daemon collects (ensureDaemon) before the TUI
// opens, as startCollecting does for vartop web, and is what the TUI is
// told of it: r starts it, a daemon's environment is checked against this
// vartop's (R31), and what making sure it runs said — that it did not start,
// or the warning of a login service that failed — goes on the TUI's error
// line: printed, it would be hidden behind the TUI's screen.
func tuiDaemon() tui.Daemon {
	var warn bytes.Buffer
	err := ensureLocal(&warn)
	if w := strings.TrimSpace(warn.String()); w != "" {
		err = errors.Join(err, errors.New(w))
	}
	return tui.Daemon{Start: ensureLocalDaemon, SameEnv: sameEnvAsOwn(), StartErr: err}
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
