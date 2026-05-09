package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

var actionOrigin string

func getClient() (*api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config (%w). run vartop server add first", err)
	}

	server, err := cfg.GetActiveServer(serverFlag)
	if err != nil {
		return nil, err
	}

	return api.NewClient(server), nil
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

		client, err := getClient()
		if err != nil {
			return err
		}

		origin := actionOrigin
		if origin == "" {
			origin = client.PrimaryEndpoint()
		}

		fmt.Printf("Sending '%s' action to worker '%s' (%s)...\n", action, workerName, config.ShortOrigin(origin))
		if err := client.Action(workerName, action, origin); err != nil {
			return err
		}

		fmt.Println("Success.")
		return nil
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

		client, err := getClient()
		if err != nil {
			return err
		}

		origin := actionOrigin
		if origin == "" {
			origin = client.PrimaryEndpoint()
		}

		fmt.Printf("Updating concurrency of worker '%s' to %d (%s)...\n", workerName, concurrency, config.ShortOrigin(origin))
		if err := client.TuneConcurrency(workerName, concurrency, origin); err != nil {
			return err
		}

		fmt.Println("Success.")
		return nil
	},
}

func init() {
	actionCmd.Flags().StringVar(&actionOrigin, "origin", "", "Target endpoint origin (empty uses primary endpoint)")
	concurrencyCmd.Flags().StringVar(&actionOrigin, "origin", "", "Target endpoint origin (empty uses primary endpoint)")
	rootCmd.AddCommand(actionCmd)
	rootCmd.AddCommand(concurrencyCmd)
}
