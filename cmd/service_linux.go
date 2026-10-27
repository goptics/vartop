package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// systemdService is the login service on Linux: a systemd user unit,
// vartop.service, in $XDG_CONFIG_HOME/systemd/user (~/.config by default).
type systemdService struct {
	path string // the unit file
	run  runner
}

func newSupervisor(run runner) supervisor {
	dir, err := os.UserConfigDir()
	if err != nil {
		return noService{err}
	}
	return &systemdService{path: filepath.Join(dir, "systemd", "user", serviceUnit), run: run}
}

func (s *systemdService) Installed() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Install writes the unit, enables it and restarts it: restart starts a
// unit not running and, gracefully, replaces a daemon of the old unit
// (one systemd restarted after a kill, say). A unit it wrote anew is
// uninstalled again should systemctl fail (installServiceFile).
func (s *systemdService) Install(exe string, env map[string]string) error {
	return installServiceFile(s.path, systemdUnit(exe, env), func() error {
		if err := s.systemctl("daemon-reload"); err != nil {
			return err
		}
		if err := s.systemctl("enable", serviceUnit); err != nil {
			return err
		}
		return s.systemctl("restart", serviceUnit)
	}, s.Uninstall)
}

func (s *systemdService) Uninstall() error {
	err := s.systemctl("disable", "--now", serviceUnit)
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return errors.Join(err, rmErr)
	}
	return errors.Join(err, s.systemctl("daemon-reload"))
}

func (s *systemdService) Stop() error { return s.systemctl("stop", serviceUnit) }

func (s *systemdService) systemctl(args ...string) error {
	return s.run("systemctl", append([]string{"--user"}, args...)...)
}
