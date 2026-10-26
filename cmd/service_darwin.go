package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// launchdService is the login service on macOS: the LaunchAgent
// dev.goptics.vartop, in ~/Library/LaunchAgents.
type launchdService struct {
	path   string // the plist
	domain string // the user's GUI domain: gui/<uid>
	run    runner
}

func newSupervisor(run runner) supervisor {
	home, err := os.UserHomeDir()
	if err != nil {
		return noService{err}
	}
	return &launchdService{
		path:   filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"),
		domain: fmt.Sprintf("gui/%d", os.Getuid()),
		run:    run,
	}
}

// target is the service in launchctl's terms.
func (s *launchdService) target() string { return s.domain + "/" + serviceLabel }

func (s *launchdService) Installed() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Install unloads the agent when it is loaded, so that the new file is
// read, and loads it: RunAtLoad starts it.
func (s *launchdService) Install(exe string, env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, []byte(launchdPlist(exe, env)), 0o644); err != nil {
		return err
	}
	_ = s.run("launchctl", "bootout", s.target()) // fails when it is not loaded
	return s.run("launchctl", "bootstrap", s.domain, s.path)
}

func (s *launchdService) Uninstall() error {
	_ = s.run("launchctl", "bootout", s.target()) // fails when it is not loaded
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *launchdService) Start() error   { return s.run("launchctl", "kickstart", s.target()) }
func (s *launchdService) Restart() error { return s.run("launchctl", "kickstart", "-k", s.target()) }
