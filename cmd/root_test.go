package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootPrintsHelp: vartop without a subcommand prints its usage, naming
// the commands, and starts nothing.
func TestRootPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("vartop: %v", err)
	}
	for _, want := range []string{"Usage:", "web", "server", "workers"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("vartop printed %q, want it to name %q", out.String(), want)
		}
	}
}

// TestDaemonCommandGone: vartop has no daemon any more; vartop web
// collects while it runs.
func TestDaemonCommandGone(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "daemon" {
			t.Fatalf("vartop has a daemon command: %q", c.Short)
		}
	}
}
