package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recordRunner is a runner that runs nothing and records each command;
// those that hold fail fail.
type recordRunner struct {
	cmds []string
	fail string
}

func (r *recordRunner) run(name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	r.cmds = append(r.cmds, cmd)
	if r.fail != "" && strings.Contains(cmd, r.fail) {
		return fmt.Errorf("%s: exit status 1: Failed to connect to bus", cmd)
	}
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

// TestSystemdInstallRollsBack (R32): an Install that fails at systemctl
// removes the unit it wrote — Installed, which looks for it, then says
// no, and vartop spawns its daemon itself — undoing what systemctl did of
// it; a unit there before is kept, as systemd knows it from an install
// that worked.
func TestSystemdInstallRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, fail string
		existed    bool
		want       []string
	}{
		{name: "daemon-reload fails", fail: "daemon-reload", want: []string{
			"systemctl --user daemon-reload",
			"systemctl --user disable --now vartop.service",
			"systemctl --user daemon-reload",
		}},
		{name: "restart fails", fail: "restart", want: []string{
			"systemctl --user daemon-reload",
			"systemctl --user enable vartop.service",
			"systemctl --user restart vartop.service",
			"systemctl --user disable --now vartop.service",
			"systemctl --user daemon-reload",
		}},
		{name: "unit there before", fail: "daemon-reload", existed: true, want: []string{
			"systemctl --user daemon-reload",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			r := &recordRunner{fail: tc.fail}
			sup := newSupervisor(r.run)
			unit := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "vartop.service")
			if tc.existed {
				if err := writeServiceFile(unit, "an older unit"); err != nil {
					t.Fatal(err)
				}
			}
			if err := sup.Install("/opt/vartop/vartop", goldenEnv); err == nil || !strings.Contains(err.Error(), tc.fail) {
				t.Errorf("Install() = %v, want the failure of systemctl %s", err, tc.fail)
			}
			if _, err := os.Stat(unit); tc.existed == os.IsNotExist(err) {
				t.Errorf("unit after the failed Install: %v; want it there %v", err, tc.existed)
			}
			if sup.Installed() != tc.existed {
				t.Errorf("Installed() = %v, want %v", sup.Installed(), tc.existed)
			}
			if !slices.Equal(r.cmds, tc.want) {
				t.Errorf("commands run:\n%s\nwant:\n%s", strings.Join(r.cmds, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}
