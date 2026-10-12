package cmd

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

var actionOrigin string

// fanOut runs fn against every pod of the active server (only --origin when
// set) concurrently and prints one result line per pod. It fails if any pod
// fails.
func fanOut(w io.Writer, desc string, fn func(*api.Client) error) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config (%w). run vartop server add first", err)
	}

	server, err := cfg.GetActiveServer(serverFlag)
	if err != nil {
		return err
	}

	pool, err := api.PoolFor(server)
	if err != nil {
		return err
	}
	defer pool.Close()

	clients := pool.Clients()
	if actionOrigin != "" {
		var match []*api.Client
		for _, c := range clients {
			if c.PrimaryEndpoint() == actionOrigin {
				match = append(match, c)
			}
		}
		if len(match) == 0 {
			return fmt.Errorf("no varmq pod found for endpoint %s", actionOrigin)
		}
		clients = match
	}
	if len(clients) == 0 {
		return fmt.Errorf("no varmq pods discovered")
	}

	fmt.Fprintf(w, "%s on %d pod(s)...\n", desc, len(clients))

	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() { errs[i] = fn(c) })
	}
	wg.Wait()

	failed := 0
	for i, c := range clients {
		origin := config.ShortOrigin(c.PrimaryEndpoint())
		if errs[i] != nil {
			failed++
			fmt.Fprintf(w, "  %s: failed: %v\n", origin, errs[i])
			continue
		}
		// Name the instance that answered: behind a load balancer the
		// action reached only that one.
		if id := c.LastInstance(); id != "" && id != origin {
			fmt.Fprintf(w, "  %s: ok (instance %s)\n", origin, id)
			continue
		}
		fmt.Fprintf(w, "  %s: ok\n", origin)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d pod(s) failed", failed, len(clients))
	}
	return nil
}

var actionCmd = &cobra.Command{
	Use:       "action <worker_name> <pause|resume|restart|stop>",
	Short:     "Send a state-modifying action to a worker",
	Args:      cobra.ExactArgs(2),
	ValidArgs: []string{"pause", "resume", "restart", "stop"},
	RunE: func(cmd *cobra.Command, args []string) error {
		workerName := args[0]
		action := args[1]

		switch action {
		case "pause", "resume", "restart", "stop":
			// valid
		default:
			return fmt.Errorf("invalid action '%s', must be one of: pause, resume, restart, stop", action)
		}

		desc := fmt.Sprintf("Sending '%s' action to worker '%s'", action, workerName)
		return fanOut(cmd.OutOrStdout(), desc, func(c *api.Client) error {
			return c.Action(workerName, action, c.PrimaryEndpoint())
		})
	},
}

var concurrencyCmd = &cobra.Command{
	Use:   "concurrency <worker_name> <number>",
	Short: "Update the concurrency pool size of a worker",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		workerName := args[0]
		numberStr := args[1]

		concurrency, err := strconv.Atoi(numberStr)
		if err != nil || concurrency < 1 {
			return fmt.Errorf("invalid concurrency number, must be a positive integer > 0")
		}

		desc := fmt.Sprintf("Updating concurrency of worker '%s' to %d", workerName, concurrency)
		return fanOut(cmd.OutOrStdout(), desc, func(c *api.Client) error {
			return c.TuneConcurrency(workerName, concurrency, c.PrimaryEndpoint())
		})
	},
}

func init() {
	actionCmd.Flags().StringVar(&actionOrigin, "origin", "", "Target only this pod endpoint (default: all pods)")
	concurrencyCmd.Flags().StringVar(&actionOrigin, "origin", "", "Target only this pod endpoint (default: all pods)")
	rootCmd.AddCommand(actionCmd)
	rootCmd.AddCommand(concurrencyCmd)
}
