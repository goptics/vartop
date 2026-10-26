package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recordRunner is a runner that runs nothing and records each command.
type recordRunner struct{ cmds []string }

func (r *recordRunner) run(name string, args ...string) error {
	r.cmds = append(r.cmds, strings.Join(append([]string{name}, args...), " "))
	return nil
}

// TestSystemdService: install writes the unit (0600) under
// XDG_CONFIG_HOME, enables it and restarts it — which starts it, or
// replaces a daemon of the old unit; stop and uninstall go through
// systemctl --user too.
func TestSystemdService(t *testing.T) {
	isolate(t)
	r := &recordRunner{}
	sup := newSupervisor(r.run)
	if sup.Installed() {
		t.Fatal("Installed() = true before Install")
	}
	unit := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "vartop.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("an older unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sup.Install("/opt/vartop dir/vartop", goldenEnv); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("unit mode = %v, want 0600: its variables may hold a proxy's credentials", fi.Mode().Perm())
	}
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "vartop.service", string(data))
	if !sup.Installed() {
		t.Error("Installed() = false after Install")
	}
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := sup.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("unit after Uninstall: %v, want it removed", err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable vartop.service",
		"systemctl --user restart vartop.service",
		"systemctl --user stop vartop.service",
		"systemctl --user disable --now vartop.service",
		"systemctl --user daemon-reload",
	}
	if !slices.Equal(r.cmds, want) {
		t.Errorf("commands run:\n%s\nwant:\n%s", strings.Join(r.cmds, "\n"), strings.Join(want, "\n"))
	}
}
