package cmd

import (
	"errors"
	"strings"
	"testing"
)

// TestTUIDaemon (R31, D3): the TUI is told how to start the daemon, how to
// check a daemon's environment against its own, and what making sure the
// daemon runs said as it opened — the failure, or the warning of a login
// service that failed — for its error line: printed before the TUI's
// screen, it would be hidden behind it.
func TestTUIDaemon(t *testing.T) {
	t.Run("did not start", func(t *testing.T) {
		d := newTestDaemon(t)
		d.onFakeClock() // the spawned daemon claims nothing: waitStarted gives up at once
		td := tuiDaemon()
		if td.StartErr == nil || !strings.Contains(td.StartErr.Error(), "vartop daemon did not start; see ") {
			t.Errorf("StartErr = %v, want the spawn failure", td.StartErr)
		}
		if d.spawned() != 1 {
			t.Errorf("spawns = %d, want 1", d.spawned())
		}
	})
	t.Run("login service failed", func(t *testing.T) {
		d := newTestDaemon(t)
		d.sup.installed, d.sup.installErr = true, errors.New("launchctl bootstrap: exit status 5")
		d.startsAs(t, d.o.want)
		td := tuiDaemon()
		if td.StartErr == nil || !strings.Contains(td.StartErr.Error(), "vartop daemon started detached; its login service failed: launchctl bootstrap: exit status 5") {
			t.Errorf("StartErr = %v, want the warning", td.StartErr)
		}
	})
	t.Run("started", func(t *testing.T) {
		d := newTestDaemon(t)
		d.startsAs(t, d.o.want)
		td := tuiDaemon()
		if td.StartErr != nil {
			t.Errorf("StartErr = %v, want none", td.StartErr)
		}
		if td.Start == nil {
			t.Error("no Start: r would start no daemon")
		}
		if td.SameEnv == nil {
			t.Fatal("no SameEnv: the TUI would act on another environment's daemon's units")
		}
		own := ownLease()
		if !td.SameEnv(own) {
			t.Error("SameEnv refuses this vartop's own environment")
		}
		other := own
		other.ConfigPath = "/elsewhere/vartop/config.json"
		if td.SameEnv(other) {
			t.Error("SameEnv takes a daemon reading another config file for this environment's")
		}
	})
}
