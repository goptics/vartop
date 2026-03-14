package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

func getClient() (*api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config (%w). run vartop config add first", err)
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

		fmt.Printf("Sending '%s' action to worker '%s'...\n", action, workerName)
		if err := client.Action(workerName, action); err != nil {
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

		fmt.Printf("Updating concurrency of worker '%s' to %d...\n", workerName, concurrency)
		if err := client.TuneConcurrency(workerName, concurrency); err != nil {
			return err
		}

		fmt.Println("Success.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(actionCmd)
	rootCmd.AddCommand(concurrencyCmd)
}
