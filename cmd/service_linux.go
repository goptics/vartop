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

func (s *systemdService) Install(exe string, env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, []byte(systemdUnit(exe, env)), 0o644); err != nil {
		return err
	}
	if err := s.systemctl("daemon-reload"); err != nil {
		return err
	}
	return s.systemctl("enable", "--now", serviceUnit)
}

func (s *systemdService) Uninstall() error {
	err := s.systemctl("disable", "--now", serviceUnit)
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return errors.Join(err, rmErr)
	}
	return errors.Join(err, s.systemctl("daemon-reload"))
}

func (s *systemdService) Start() error   { return s.systemctl("start", serviceUnit) }
func (s *systemdService) Restart() error { return s.systemctl("restart", serviceUnit) }

func (s *systemdService) systemctl(args ...string) error {
	return s.run("systemctl", append([]string{"--user"}, args...)...)
}
