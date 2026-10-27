package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/daemon"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
)

// asVartopEnv makes the test binary run as vartop itself: a test that
// re-executes it as a daemon sets it (TestDetachedSpawnSubprocess).
const asVartopEnv = "VARTOP_TEST_AS_VARTOP"

func TestMain(m *testing.M) {
	if os.Getenv(asVartopEnv) == "1" {
		Execute()
		os.Exit(0)
	}
	// A test running the daemon in this process must not send the test
	// binary's own crash output to its temporary daemon.log.
	setCrashOutput = func(*os.File, debug.CrashOptions) error { return nil }
	os.Exit(m.Run())
}

// isolate points every directory vartop or a daemon could use at temporary
// ones, and returns the data dir.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	dataDir := filepath.Join(home, "data")
	t.Setenv("VARTOP_DATA_DIR", dataDir)
	t.Setenv("KUBECONFIG", "")
	return dataDir
}

// fakeSupervisor is a login service that records what it is asked.
// Install starts the service's daemon by running onStart, as the real ones
// do — unless installErr is set: then it fails, and changes nothing;
// Uninstall runs onUninstall first.
type fakeSupervisor struct {
	mu          sync.Mutex
	installed   bool
	installErr  error
	calls       []string
	exe         string
	env         map[string]string
	onStart     func()
	onUninstall func()
}

func (f *fakeSupervisor) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeSupervisor) Installed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed
}

func (f *fakeSupervisor) Install(exe string, env map[string]string) error {
	f.record("install")
	if f.installErr != nil {
		return f.installErr
	}
	f.mu.Lock()
	f.installed, f.exe, f.env = true, exe, env
	f.mu.Unlock()
	f.started()
	return nil
}

func (f *fakeSupervisor) Uninstall() error {
	if f.onUninstall != nil {
		f.onUninstall()
	}
	f.record("uninstall")
	f.mu.Lock()
	f.installed = false
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) Stop() error { f.record("stop"); return nil }

func (f *fakeSupervisor) started() {
	if f.onStart != nil {
		f.onStart()
	}
}

func (f *fakeSupervisor) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeClock is a clock whose sleeps pass at once.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testDaemon is a daemon setup over a temporary data dir: a real control
// file, a fake login service (not installed), a spawner that starts
// nothing and a kill that kills nothing, recorded in spawns and kills.
// localDaemon returns it until the test ends.
type testDaemon struct {
	o      ensureOptions
	sup    *fakeSupervisor
	mu     sync.Mutex
	spawns int
	kills  []int
}

func newTestDaemon(t *testing.T) *testDaemon {
	t.Helper()
	dataDir := isolate(t)
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	d := &testDaemon{sup: &fakeSupervisor{}}
	d.o = ensureOptions{
		dataDir: dataDir,
		ctl:     ctl,
		want:    ownLease(),
		sup:     d.sup,
		exe:     "/opt/vartop/bin/vartop",
		env:     map[string]string{"PATH": "/usr/bin", "KUBECONFIG": "/home/u/.kube/config"},
		spawn: func() error {
			d.mu.Lock()
			d.spawns++
			d.mu.Unlock()
			return nil
		},
		kill: func(pid int) error {
			d.mu.Lock()
			d.kills = append(d.kills, pid)
			d.mu.Unlock()
			return nil
		},
	}
	orig := localDaemon
	localDaemon = func() (ensureOptions, func(), error) { return d.o, func() {}, nil }
	t.Cleanup(func() { localDaemon = orig })
	return d
}

func (d *testDaemon) spawned() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spawns
}

func (d *testDaemon) killed() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.kills...)
}

// claim claims the lease as l, as a daemon starting would, and returns its
// token.
func claim(t *testing.T, ctl *store.Control, l store.Lease) string {
	t.Helper()
	token, ok, err := ctl.Claim(l, time.Now())
	if err != nil || !ok {
		t.Fatalf("Claim() = %v, %v; want a claim", ok, err)
	}
	return token
}

// beatingDaemon claims the lease as l and renews it, as a running daemon
// does, until the test ends: a daemon started meanwhile sees its beat move,
// and leaves it the lease (a lease claimed and never renewed is a crashed
// daemon's, which it would take over).
func beatingDaemon(t *testing.T, ctl *store.Control, l store.Lease) {
	t.Helper()
	token := claim(t, ctl, l)
	quit, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(quit); <-done })
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				_, _ = ctl.Beat(token, time.Now())
			}
		}
	}()
}

// startsAs makes the daemon spawned or started by the login service the
// one claiming the lease as l. Install starts none while one runs: the
// tests stop the old one first, as vartop does.
func (d *testDaemon) startsAs(t *testing.T, l store.Lease) {
	spawn := d.o.spawn
	d.o.spawn = func() error {
		claim(t, d.o.ctl, l)
		return spawn()
	}
	d.sup.onStart = func() {
		if _, running := lease(t, d.o.ctl); !running {
			claim(t, d.o.ctl, l)
		}
	}
}

// politeDaemon claims the lease as l and lets it go once a stop is
// requested, as a daemon does; the returned channel is closed then.
func politeDaemon(t *testing.T, ctl *store.Control, l store.Lease) (stopped <-chan struct{}) {
	t.Helper()
	token := claim(t, ctl, l)
	done := make(chan struct{})
	quit := make(chan struct{})
	t.Cleanup(func() { close(quit) })
	go func() {
		for {
			select {
			case <-quit:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if cur, _, err := ctl.Lease(); err == nil && cur.Token == token && cur.Stop > 0 {
				_ = ctl.Release(token)
				close(done)
				return
			}
		}
	}()
	return done
}

// lease is the lease in the control file.
func lease(t *testing.T, ctl *store.Control) (store.Lease, bool) {
	t.Helper()
	l, fresh, err := ctl.Lease()
	if err != nil {
		t.Fatal(err)
	}
	return l, fresh
}

func TestEnsureDaemonStartsWhenAbsent(t *testing.T) {
	t.Run("detached", func(t *testing.T) {
		d := newTestDaemon(t)
		d.startsAs(t, d.o.want)
		if err := ensureDaemon(d.o); err != nil {
			t.Fatalf("ensureDaemon() = %v", err)
		}
		if d.spawned() != 1 || len(d.sup.called()) != 0 {
			t.Errorf("spawns = %d, service calls = %v; want 1 spawn and no service call", d.spawned(), d.sup.called())
		}
		if l, fresh := lease(t, d.o.ctl); !fresh || l.Build != d.o.want.Build {
			t.Errorf("lease = %+v (fresh %v), want the spawned daemon's", l, fresh)
		}
	})
	t.Run("through the login service", func(t *testing.T) {
		d := newTestDaemon(t)
		d.sup.installed = true
		d.startsAs(t, d.o.want)
		if err := ensureDaemon(d.o); err != nil {
			t.Fatalf("ensureDaemon() = %v", err)
		}
		if got := strings.Join(d.sup.called(), ","); got != "install" || d.spawned() != 0 {
			t.Errorf("service calls = %s, spawns = %d; want install alone and no spawn", got, d.spawned())
		}
		if d.sup.exe != d.o.exe || fmt.Sprint(d.sup.env) != fmt.Sprint(d.o.env) {
			t.Errorf("service rewritten to %q %v, want %q %v", d.sup.exe, d.sup.env, d.o.exe, d.o.env)
		}
	})
	t.Run("not starting", func(t *testing.T) {
		d := newTestDaemon(t)
		clock := &fakeClock{t: time.Now()}
		d.o.now, d.o.sleep = clock.now, clock.sleep
		began := clock.now()
		err := ensureDaemon(d.o)
		want := fmt.Sprintf("vartop daemon did not start; see %s", filepath.Join(d.o.dataDir, "daemon.log"))
		if err == nil || err.Error() != want {
			t.Fatalf("ensureDaemon() = %v, want %q", err, want)
		}
		if waited := clock.now().Sub(began); waited < startWait || waited > startWait+time.Second {
			t.Errorf("waited %v for the lease, want %v", waited, startWait)
		}
	})
}

// TestWaitStartedOutlastsCrashedDaemon (R34): with a crashed daemon's
// lease still fresh when the new one starts, the new one watches it for
// store.DeadAfter before it takes it over: waitStarted waits that much
// longer, rather than say the daemon did not start.
func TestWaitStartedOutlastsCrashedDaemon(t *testing.T) {
	setup := func(t *testing.T) (*testDaemon, string, *fakeClock) {
		d := newTestDaemon(t)
		crashed := claim(t, d.o.ctl, store.Lease{PID: 99, Build: "crashed"}) // fresh throughout, by the real clock
		clock := &fakeClock{t: time.Now()}
		d.o.now, d.o.sleep = clock.now, clock.sleep
		return d, crashed, clock
	}
	t.Run("taken over", func(t *testing.T) {
		d, crashed, clock := setup(t)
		began := clock.now()
		d.o.sleep = func(dur time.Duration) {
			clock.sleep(dur)
			if clock.now().Sub(began) >= startWait+store.DeadAfter/2 {
				if l, _ := lease(t, d.o.ctl); l.Token == crashed {
					if _, ok, err := d.o.ctl.TakeOver(crashed, d.o.want, time.Now()); !ok || err != nil {
						t.Errorf("TakeOver = %v, %v", ok, err)
					}
				}
			}
		}
		l, err := waitStarted(d.o, crashed)
		if err != nil || l.Token == crashed || l.Build != d.o.want.Build {
			t.Errorf("waitStarted = %+v, %v; want the new daemon's lease", l, err)
		}
	})
	t.Run("never taken over", func(t *testing.T) {
		d, crashed, clock := setup(t)
		began := clock.now()
		if _, err := waitStarted(d.o, crashed); err == nil {
			t.Fatal("waitStarted = nil, want did not start")
		}
		if waited, want := clock.now().Sub(began), startWait+store.DeadAfter; waited < want || waited > want+time.Second {
			t.Errorf("waited %v for the lease, want %v", waited, want)
		}
	})
}

// TestLaunchFallsBackToDetached (R32): with the login service installed,
// a daemon is spawned detached when Install fails or starts no daemon; the
// service's failure is then a warning, or, should the spawned daemon not
// start either, part of the error.
func TestLaunchFallsBackToDetached(t *testing.T) {
	installErr := errors.New("systemctl --user daemon-reload: exit status 1: Failed to connect to bus")
	for _, tc := range []struct {
		name        string
		installErr  error
		serviceRuns bool  // Install starts a daemon
		spawnErr    error // of the spawn
		spawnRuns   bool  // the daemon spawned claims the lease
		wantErr     []string
		wantWarn    []string
	}{
		{name: "install fails", installErr: installErr, spawnRuns: true,
			wantWarn: []string{"vartop daemon started detached; its login service failed: ", installErr.Error()}},
		{name: "service starts none", spawnRuns: true,
			wantWarn: []string{"vartop daemon started detached; its login service failed: ", "vartop daemon did not start; see "}},
		{name: "install and spawn fail", installErr: installErr, spawnErr: errors.New("fork/exec: no such file"),
			wantErr: []string{installErr.Error(), "start the vartop daemon: fork/exec: no such file"}},
		{name: "neither starts one", installErr: installErr,
			wantErr: []string{installErr.Error(), "vartop daemon did not start; see "}},
		{name: "service starts one", serviceRuns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDaemon(t)
			d.sup.installed, d.sup.installErr = true, tc.installErr
			d.onFakeClock()
			var warn bytes.Buffer
			d.o.warn = &warn
			d.startsAs(t, d.o.want)
			if !tc.serviceRuns {
				d.sup.onStart = nil
			}
			spawn := d.o.spawn
			d.o.spawn = func() error {
				if tc.spawnErr != nil {
					return tc.spawnErr
				}
				if !tc.spawnRuns {
					d.mu.Lock()
					d.spawns++
					d.mu.Unlock()
					return nil
				}
				return spawn()
			}
			err := ensureDaemon(d.o)
			if len(tc.wantErr) == 0 && err != nil {
				t.Fatalf("ensureDaemon() = %v, want nil", err)
			}
			for _, want := range tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ensureDaemon() = %v, want %q in it", err, want)
				}
			}
			for _, want := range tc.wantWarn {
				if !strings.Contains(warn.String(), want) {
					t.Errorf("warned %q, want %q in it", warn.String(), want)
				}
			}
			if len(tc.wantWarn) == 0 && warn.Len() != 0 {
				t.Errorf("warned %q, want no warning", warn.String())
			}
			if calls := strings.Join(d.sup.called(), ","); calls != "install" {
				t.Errorf("service calls = %q, want install", calls)
			}
			wantSpawns := 1
			if tc.serviceRuns || tc.spawnErr != nil {
				wantSpawns = 0
			}
			if d.spawned() != wantSpawns {
				t.Errorf("spawns = %d, want %d", d.spawned(), wantSpawns)
			}
			if l, fresh := lease(t, d.o.ctl); (tc.serviceRuns || tc.spawnRuns) != (fresh && l.Build == d.o.want.Build) {
				t.Errorf("lease = %+v (fresh %v); want a daemon of this build exactly when one started", l, fresh)
			}
		})
	}
}

// TestServiceWarningShown: the warning of a daemon started detached
// because the login service failed reaches the user of `daemon start`
// and of a UI opening (startCollecting).
func TestServiceWarningShown(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) string
	}{
		{"daemon start", func(t *testing.T) string {
			out, err := runDaemonCmd(t, daemonStartCmd)
			if err != nil || !strings.Contains(out, "vartop daemon is running (pid ") {
				t.Errorf("daemon start = %q, %v; want it running", out, err)
			}
			return out
		}},
		{"a UI opening", func(t *testing.T) string {
			var w bytes.Buffer
			startCollecting(&w)
			return w.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDaemon(t)
			d.sup.installed, d.sup.installErr = true, errors.New("launchctl bootstrap: exit status 5")
			d.startsAs(t, d.o.want)
			if out := tc.run(t); !strings.Contains(out, "vartop daemon started detached; its login service failed: launchctl bootstrap: exit status 5") {
				t.Errorf("printed %q, want the warning", out)
			}
		})
	}
}

// TestInstallFailureKeepsCollecting: `daemon install` that fails has not
// stopped collection for good: the daemon it stopped first is started
// again, detached, and the error says so.
func TestInstallFailureKeepsCollecting(t *testing.T) {
	d := newTestDaemon(t)
	d.sup.installErr = errors.New("launchctl bootstrap: exit status 5")
	stopped := politeDaemon(t, d.o.ctl, d.o.want)
	d.startsAs(t, d.o.want)
	_, err := runDaemonCmd(t, daemonInstallCmd)
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap: exit status 5") || !strings.Contains(err.Error(), "runs detached") {
		t.Errorf("daemon install = %v, want the install error, and that the daemon runs detached", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("the running daemon was not stopped first")
	}
	if l, fresh := lease(t, d.o.ctl); !fresh || l.Build != d.o.want.Build || d.spawned() != 1 {
		t.Errorf("lease = %+v (fresh %v), spawns = %d; want one detached daemon collecting", l, fresh, d.spawned())
	}
}

// TestServiceEnvAbsoluteDataDir: the login service gets the data dir a
// relative VARTOP_DATA_DIR names from vartop's working dir, absolute: the
// service manager runs the daemon in a working dir of its own.
func TestServiceEnvAbsoluteDataDir(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("VARTOP_DATA_DIR", filepath.Join("rel", "data"))
	o, release, err := localDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !filepath.IsAbs(o.dataDir) || !strings.HasSuffix(o.dataDir, filepath.Join("rel", "data")) || o.env["VARTOP_DATA_DIR"] != o.dataDir {
		t.Errorf("data dir %q, service VARTOP_DATA_DIR %q; want both %s, absolute", o.dataDir, o.env["VARTOP_DATA_DIR"], filepath.Join(dir, "rel", "data"))
	}

	t.Setenv("VARTOP_DATA_DIR", "")
	o, release2, err := localDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if v, ok := o.env["VARTOP_DATA_DIR"]; !ok || v != "" {
		t.Errorf("service VARTOP_DATA_DIR = %q (set %v), want it written empty: unset in vartop", v, ok)
	}
}

func TestEnsureDaemonNoopWhenSameBuildAndEnv(t *testing.T) {
	d := newTestDaemon(t)
	d.sup.installed = true
	token := claim(t, d.o.ctl, d.o.want)
	if err := ensureDaemon(d.o); err != nil {
		t.Fatalf("ensureDaemon() = %v", err)
	}
	l, fresh := lease(t, d.o.ctl)
	if !fresh || l.Token != token || l.Stop != 0 {
		t.Errorf("lease = %+v (fresh %v), want the running daemon's, no stop requested", l, fresh)
	}
	if d.spawned() != 0 || len(d.sup.called()) != 0 || len(d.killed()) != 0 {
		t.Errorf("spawns %d, service calls %v, kills %v; want none", d.spawned(), d.sup.called(), d.killed())
	}
}

func TestEnsureDaemonPathDifferenceIgnored(t *testing.T) {
	d := newTestDaemon(t)
	running := d.o.want
	running.PathEnv = "/somewhere/else:" + running.PathEnv
	token := claim(t, d.o.ctl, running)
	if err := ensureDaemon(d.o); err != nil {
		t.Fatalf("ensureDaemon() = %v", err)
	}
	if l, _ := lease(t, d.o.ctl); l.Token != token || l.Stop != 0 || d.spawned() != 0 {
		t.Errorf("lease = %+v, spawns = %d; want the daemon left alone", l, d.spawned())
	}
}

func TestEnsureDaemonRestartsOnBuildMismatch(t *testing.T) {
	d := newTestDaemon(t)
	old := d.o.want
	old.Build = "/old/vartop (1 bytes, modified 2020-01-01T00:00:00Z)"
	stopped := politeDaemon(t, d.o.ctl, old)
	d.startsAs(t, d.o.want)
	if err := ensureDaemon(d.o); err != nil {
		t.Fatalf("ensureDaemon() = %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("the old daemon was not asked to stop")
	}
	if l, fresh := lease(t, d.o.ctl); !fresh || l.Build != d.o.want.Build {
		t.Errorf("lease = %+v (fresh %v), want a daemon of this build", l, fresh)
	}
	if d.spawned() != 1 || len(d.killed()) != 0 {
		t.Errorf("spawns = %d, kills = %v; want 1 spawn, no kill", d.spawned(), d.killed())
	}
}

func TestEnsureDaemonRestartsOnConfigPathOrKubeconfigMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		differ func(*store.Lease)
	}{
		{"config path", func(l *store.Lease) { l.ConfigPath = "/elsewhere/vartop/config.json" }},
		{"KUBECONFIG", func(l *store.Lease) { l.Kubeconfig = "/elsewhere/kubeconfig" }},
	} {
		for _, installed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, service installed %v", tc.name, installed), func(t *testing.T) {
				d := newTestDaemon(t)
				d.sup.installed = installed
				old := d.o.want
				tc.differ(&old)
				stopped := politeDaemon(t, d.o.ctl, old)
				d.startsAs(t, d.o.want)
				if err := ensureDaemon(d.o); err != nil {
					t.Fatalf("ensureDaemon() = %v", err)
				}
				select {
				case <-stopped:
				default:
					t.Error("the old daemon was not asked to stop")
				}
				calls := strings.Join(d.sup.called(), ",")
				if installed {
					if calls != "install" || d.spawned() != 0 {
						t.Errorf("service calls = %s, spawns = %d; want install alone and no spawn", calls, d.spawned())
					}
					if fmt.Sprint(d.sup.env) != fmt.Sprint(d.o.env) {
						t.Errorf("service environment = %v, want this vartop's %v", d.sup.env, d.o.env)
					}
				} else if calls != "" || d.spawned() != 1 {
					t.Errorf("service calls = %s, spawns = %d; want none and 1 spawn", calls, d.spawned())
				}
				if l, fresh := lease(t, d.o.ctl); !fresh || l.ConfigPath != d.o.want.ConfigPath || l.Kubeconfig != d.o.want.Kubeconfig {
					t.Errorf("lease = %+v (fresh %v), want a daemon of this environment", l, fresh)
				}
			})
		}
	}
}

// onFakeClock makes vartop's clock a fake one, starting at the real now,
// whose sleeps pass at once; each sleep then runs hooks.
func (d *testDaemon) onFakeClock(hooks ...func()) *fakeClock {
	clock := &fakeClock{t: time.Now()}
	d.o.now = clock.now
	d.o.sleep = func(dur time.Duration) {
		clock.sleep(dur)
		for _, hook := range hooks {
			hook()
		}
	}
	return clock
}

// beats is a sleep hook of a daemon that renews the lease of token, as one
// alive does: at the real now (a lease is fresh by the real clock), a
// millisecond further each time, so that every beat differs.
func (d *testDaemon) beats(token string) func() {
	n := 0
	return func() {
		n++
		_, _ = d.o.ctl.Beat(token, time.Now().Add(time.Duration(n)*time.Millisecond))
	}
}

// takesOver is a sleep hook of the daemon vartop spawns finding the lease
// of token dead fresh: once it has watched it for store.DeadAfter on clock
// without a beat, it takes it over as l, as daemon.Run does (R34).
func (d *testDaemon) takesOver(t *testing.T, clock *fakeClock, dead string, l store.Lease) func() {
	var spawnedAt time.Time
	spawn := d.o.spawn
	d.o.spawn = func() error {
		spawnedAt = clock.now()
		return spawn()
	}
	return func() {
		if spawnedAt.IsZero() || clock.now().Sub(spawnedAt) < store.DeadAfter {
			return
		}
		if cur, _ := lease(t, d.o.ctl); cur.Token == dead {
			if _, ok, err := d.o.ctl.TakeOver(dead, l, time.Now()); !ok || err != nil {
				t.Errorf("TakeOver = %v, %v", ok, err)
			}
		}
	}
}

// TestEnsureDaemonKillsHungDaemon: a daemon that does not stop when asked,
// but beats, is killed after stopWait, by the pid of its lease, and its
// lease let go, so that the new one can claim it at once.
func TestEnsureDaemonKillsHungDaemon(t *testing.T) {
	d := newTestDaemon(t)
	old := d.o.want
	old.Build, old.PID = "old", 4242
	clock := d.onFakeClock(d.beats(claim(t, d.o.ctl, old)))
	d.startsAs(t, d.o.want)
	began := clock.now()
	if err := ensureDaemon(d.o); err != nil {
		t.Fatalf("ensureDaemon() = %v", err)
	}
	if got := d.killed(); len(got) != 1 || got[0] != 4242 {
		t.Errorf("killed %v, want [4242]", got)
	}
	if waited := clock.now().Sub(began); waited < stopWait {
		t.Errorf("killed after %v, want after %v", waited, stopWait)
	}
	if l, fresh := lease(t, d.o.ctl); !fresh || l.Build != d.o.want.Build {
		t.Errorf("lease = %+v (fresh %v), want the new daemon's", l, fresh)
	}
}

// TestStopDaemonKillsOnlyABeatingDaemon (R33): a daemon that does not stop
// when asked is killed only while its beat moves — it is alive, so the pid
// of its lease is still its own. A lease that looks fresh but does not
// beat (its daemon crashed, and the clock stepped back, or less than
// LeaseTTL passed) is left alone, as is a daemon that stops late.
func TestStopDaemonKillsOnlyABeatingDaemon(t *testing.T) {
	hung := store.Lease{PID: 4242, Build: "hung"}
	for _, tc := range []struct {
		name      string
		beatAt    time.Duration // of the claim, from now
		beating   bool
		releaseAt time.Duration // on the fake clock; 0: never
		want      stopped
	}{
		{name: "beating", beating: true, want: stoppedKilled},
		{name: "not beating", want: notResponding},
		{name: "not beating, beat ahead of the clock", beatAt: 3 * time.Second, want: notResponding},
		{name: "stops while watched", releaseAt: stopWait + time.Second, want: stoppedAsked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDaemon(t)
			token, ok, err := d.o.ctl.Claim(hung, time.Now().Add(tc.beatAt))
			if err != nil || !ok {
				t.Fatalf("Claim() = %v, %v", ok, err)
			}
			var hooks []func()
			clock := d.onFakeClock(func() {
				for _, hook := range hooks {
					hook()
				}
			})
			began := clock.now()
			if tc.beating {
				hooks = append(hooks, d.beats(token))
			}
			if tc.releaseAt > 0 {
				hooks = append(hooks, func() {
					if clock.now().Sub(began) >= tc.releaseAt {
						_ = d.o.ctl.Release(token)
					}
				})
			}
			l, how, err := stopDaemon(d.o)
			if err != nil || how != tc.want || l.Token != token {
				t.Fatalf("stopDaemon() = %v, %v, %v; want %v, the lease found, no error", l.Token == token, how, err, tc.want)
			}
			kills := d.killed()
			cur, fresh := lease(t, d.o.ctl)
			switch tc.want {
			case stoppedKilled:
				if len(kills) != 1 || kills[0] != 4242 || cur.Token != "" {
					t.Errorf("kills %v, lease %+v; want 4242 killed and its lease let go", kills, cur)
				}
				if waited := clock.now().Sub(began); waited < stopWait || waited > stopWait+store.DeadAfter {
					t.Errorf("killed after %v, want between %v and %v", waited, stopWait, stopWait+store.DeadAfter)
				}
			case notResponding:
				if len(kills) != 0 || !fresh || cur.Token != token {
					t.Errorf("kills %v, lease %+v (fresh %v); want no kill and the lease kept", kills, cur, fresh)
				}
				if waited := clock.now().Sub(began); waited < stopWait+store.DeadAfter {
					t.Errorf("gave up after %v, want after %v: the beat is watched store.DeadAfter", waited, stopWait+store.DeadAfter)
				}
			default:
				if len(kills) != 0 {
					t.Errorf("killed %v; it stopped", kills)
				}
			}
		})
	}
}

// TestEnsureDaemonOverDeadDaemon: a daemon of another build that neither
// stops nor beats is not killed; the daemon started takes its lease over
// (R34), and vartop waits for that.
func TestEnsureDaemonOverDeadDaemon(t *testing.T) {
	d := newTestDaemon(t)
	old := d.o.want
	old.Build, old.PID = "old", 4242
	dead := claim(t, d.o.ctl, old)
	var clock *fakeClock
	var takeOver func()
	clock = d.onFakeClock(func() { takeOver() })
	takeOver = d.takesOver(t, clock, dead, d.o.want)
	if err := ensureDaemon(d.o); err != nil {
		t.Fatalf("ensureDaemon() = %v", err)
	}
	if kills := d.killed(); len(kills) != 0 {
		t.Errorf("killed %v; a daemon that does not beat is never killed", kills)
	}
	if l, fresh := lease(t, d.o.ctl); !fresh || l.Token == dead || l.Build != d.o.want.Build || d.spawned() != 1 {
		t.Errorf("lease = %+v (fresh %v), spawns = %d; want one daemon of this build, spawned", l, fresh, d.spawned())
	}
}

// TestEnsureDaemonSuspectLease (R42): a fresh lease of this build and
// environment whose beat is more than 2·BeatEvery old may be a crashed
// daemon's: vartop starts a daemon anyway, which takes a dead one's lease
// over and leaves a live one its lease. Either way ensureDaemon succeeds.
func TestEnsureDaemonSuspectLease(t *testing.T) {
	setup := func(t *testing.T) (*testDaemon, string) {
		d := newTestDaemon(t)
		token, ok, err := d.o.ctl.Claim(d.o.want, time.Now().Add(-5*time.Second))
		if err != nil || !ok {
			t.Fatalf("Claim() = %v, %v", ok, err)
		}
		return d, token
	}
	t.Run("dead", func(t *testing.T) {
		d, dead := setup(t)
		var takeOver func()
		clock := d.onFakeClock(func() { takeOver() })
		takeOver = d.takesOver(t, clock, dead, d.o.want)
		if err := ensureDaemon(d.o); err != nil {
			t.Fatalf("ensureDaemon() = %v", err)
		}
		if l, fresh := lease(t, d.o.ctl); !fresh || l.Token == dead || d.spawned() != 1 {
			t.Errorf("lease = %+v (fresh %v), spawns = %d; want a daemon spawned, holding a new lease", l, fresh, d.spawned())
		}
		if l, _ := lease(t, d.o.ctl); l.Stop != 0 || len(d.killed()) != 0 {
			t.Errorf("lease %+v, kills %v; want no stop asked, no kill", l, d.killed())
		}
	})
	t.Run("slow, alive", func(t *testing.T) {
		d, token := setup(t)
		// It beats once vartop has waited a second; the daemon spawned
		// then leaves it the lease (it exits 0).
		var clock *fakeClock
		beat := d.beats(token)
		var began time.Time
		clock = d.onFakeClock(func() {
			if clock.now().Sub(began) >= time.Second {
				beat()
			}
		})
		began = clock.now()
		if err := ensureDaemon(d.o); err != nil {
			t.Fatalf("ensureDaemon() = %v, want nil: a daemon of this build collects", err)
		}
		if l, fresh := lease(t, d.o.ctl); !fresh || l.Token != token || l.Stop != 0 || d.spawned() != 1 {
			t.Errorf("lease = %+v (fresh %v), spawns = %d; want the live daemon's lease kept, one spawn", l, fresh, d.spawned())
		}
	})
	t.Run("beating", func(t *testing.T) {
		d := newTestDaemon(t)
		token := claim(t, d.o.ctl, d.o.want)
		d.onFakeClock()
		if err := ensureDaemon(d.o); err != nil || d.spawned() != 0 {
			t.Fatalf("ensureDaemon() = %v, spawns %d; want nil and none", err, d.spawned())
		}
		if l, _ := lease(t, d.o.ctl); l.Token != token {
			t.Errorf("lease = %+v, want the running daemon's", l)
		}
	})
}

// inProcess runs daemons in this process over dataDir, as spawned
// daemons would: each claims the lease as l. They stop when the test ends.
type inProcess struct {
	t       *testing.T
	dataDir string
	lease   store.Lease
	ctx     context.Context
	wg      sync.WaitGroup

	mu    sync.Mutex
	codes []int // of the Runs that returned
	errs  []error
}

func newInProcess(t *testing.T, dataDir string, l store.Lease) *inProcess {
	ctx, cancel := context.WithCancel(context.Background())
	p := &inProcess{t: t, dataDir: dataDir, lease: l, ctx: ctx}
	t.Cleanup(func() {
		cancel()
		p.wg.Wait()
	})
	return p
}

// start runs one daemon in the background.
func (p *inProcess) start() {
	p.wg.Go(func() {
		code, err := daemon.Run(p.ctx, daemon.Options{
			DataDir: p.dataDir,
			Config:  readConfig,
			Lease:   p.lease,
			Now:     time.Now,
		})
		p.mu.Lock()
		p.codes, p.errs = append(p.codes, code), append(p.errs, err)
		p.mu.Unlock()
	})
}

func (p *inProcess) returned() ([]int, []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.codes...), append([]error(nil), p.errs...)
}

// saveOneServer saves a config of one server, "prod", polling a fake varmq
// every second.
func saveOneServer(t *testing.T) {
	t.Helper()
	_, srv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{
		"orders": {Status: "Running", Concurrency: 1, Idle: 1},
	})
	cfg := &config.Config{DefaultServer: "prod", Servers: map[string]*config.Server{
		"prod": {Name: "prod", Endpoints: []string{srv.URL}, Interval: 1},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureDaemonRace (Review Focus 1): two UIs opened at the same moment
// with no daemon both start one; exactly one collects, the other's Run
// returns 0, and both UIs see the data.
func TestEnsureDaemonRace(t *testing.T) {
	dataDir := isolate(t)
	saveOneServer(t)
	daemons := newInProcess(t, dataDir, ownLease())

	// Each UI's daemon waits until both UIs have spawned theirs: neither
	// can then have seen the other's lease.
	var spawned sync.WaitGroup
	spawned.Add(2)
	ui := func() ensureOptions {
		ctl, err := store.OpenControl(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ctl.Close() })
		return ensureOptions{
			dataDir: dataDir,
			ctl:     ctl,
			want:    ownLease(),
			sup:     &fakeSupervisor{},
			spawn: func() error {
				spawned.Done()
				go func() {
					spawned.Wait()
					daemons.start()
				}()
				return nil
			},
			kill: func(pid int) error { return fmt.Errorf("kill %d: not in this test", pid) },
		}
	}
	uis := []ensureOptions{ui(), ui()}
	errs := make([]error, len(uis))
	var wg sync.WaitGroup
	for i, o := range uis {
		wg.Go(func() { errs[i] = ensureDaemon(o) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("UI %d: ensureDaemon() = %v", i, err)
		}
	}

	fleettest.WaitUntil(t, 5*time.Second, "the losing daemon to return", func() bool {
		codes, _ := daemons.returned()
		return len(codes) >= 1
	})
	time.Sleep(500 * time.Millisecond) // the winner must still be collecting
	codes, runErrs := daemons.returned()
	if len(codes) != 1 || codes[0] != 0 || runErrs[0] != nil {
		t.Fatalf("Runs returned %v %v, want exactly one, with 0 and no error", codes, runErrs)
	}
	if _, fresh := lease(t, uis[0].ctl); !fresh {
		t.Fatal("no daemon holds the lease")
	}
	for i := range uis {
		fleettest.WaitUntil(t, 10*time.Second, fmt.Sprintf("UI %d to see a snapshot", i), func() bool {
			db, err := store.OpenServer(dataDir, "prod", false)
			if err != nil {
				return false
			}
			defer db.Close()
			snap, err := db.Latest()
			return err == nil && snap != nil && len(snap.Pods) == 1
		})
	}
}

// runDaemonCmd runs one of the daemon commands and returns what it printed.
func runDaemonCmd(t *testing.T, c *cobra.Command) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c.SetOut(&out)
	t.Cleanup(func() { c.SetOut(nil) })
	err := c.RunE(c, nil)
	return out.String(), err
}

func TestDaemonStatusExitCodes(t *testing.T) {
	d := newTestDaemon(t)
	cfg := &config.Config{DefaultServer: "prod", Servers: map[string]*config.Server{
		"prod": {Name: "prod", Endpoints: []string{"http://a:8080/varmq"}},
		"idle": {Name: "idle", Endpoints: []string{"http://b:8080/varmq"}},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	out, err := runDaemonCmd(t, daemonStatusCmd)
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 3 {
		t.Fatalf("status of no daemon = %v, want exit code 3", err)
	}
	if !strings.Contains(out, "not running") || !strings.Contains(out, d.o.dataDir) {
		t.Errorf("status of no daemon printed:\n%s\nwant \"not running\" and the data dir", out)
	}

	token := claim(t, d.o.ctl, store.Lease{PID: 4242, Build: "/usr/bin/vartop (7 bytes, modified x)",
		ConfigPath: "/home/u/.config/vartop/config.json", Kubeconfig: "/home/u/.kube/prod", PathEnv: "/opt/bin:/usr/bin"})
	db, err := store.OpenServer(d.o.dataDir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	clock := &fakeClock{t: at.Add(3 * time.Second)}
	d.o.now = clock.now
	snap := &fleet.Snapshot{At: at, Pods: []fleet.PodSnapshot{
		{Key: "http://a:8080/varmq", Label: "instance-1"},
		{Key: "http://c:8080/varmq", Label: "instance-2", Err: "connection refused"},
	}}
	if _, err := db.Append(token, fleet.Tick{Snap: snap}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := d.o.ctl.SetServerErr("idle", "discovery failed: no such host"); err != nil {
		t.Fatal(err)
	}

	out, err = runDaemonCmd(t, daemonStatusCmd)
	if err != nil {
		t.Fatalf("status of a running daemon = %v, want exit code 0", err)
	}
	for _, want := range []string{"is running", "4242", "/usr/bin/vartop (7 bytes, modified x)",
		"/home/u/.config/vartop/config.json", "/home/u/.kube/prod", "PATH", "/opt/bin:/usr/bin", d.o.dataDir,
		"prod", "3s ago", "2 units, 1 healthy", "idle", "no data", "discovery failed: no such host"} {
		if !strings.Contains(out, want) {
			t.Errorf("status printed:\n%s\nwant %q in it", out, want)
		}
	}
}

func TestDaemonStopCopy(t *testing.T) {
	d := newTestDaemon(t)
	daemons := newInProcess(t, d.o.dataDir, d.o.want)
	daemons.start()
	fleettest.WaitUntil(t, 5*time.Second, "the daemon to claim the lease", func() bool {
		_, fresh := lease(t, d.o.ctl)
		return fresh
	})

	out, err := runDaemonCmd(t, daemonStopCmd)
	if err != nil {
		t.Fatalf("daemon stop = %v", err)
	}
	if want := "vartop daemon stopped. It starts again at your next login or the next time you open vartop.\n"; out != want {
		t.Errorf("daemon stop printed %q, want %q", out, want)
	}
	fleettest.WaitUntil(t, 2*time.Second, "Run to return", func() bool {
		codes, _ := daemons.returned()
		return len(codes) == 1
	})
	if codes, errs := daemons.returned(); codes[0] != 0 || errs[0] != nil {
		t.Errorf("Run returned %v %v, want 0 and no error", codes, errs)
	}
	if kills := d.killed(); len(kills) != 0 {
		t.Errorf("killed %v; the daemon stopped when asked", kills)
	}

	out, err = runDaemonCmd(t, daemonStopCmd)
	if err != nil || out != "vartop daemon is not running\n" {
		t.Errorf("daemon stop with none running = %q, %v; want it said and no error", out, err)
	}
}

// TestDaemonRestart: `daemon restart` stops the daemon running and leaves
// exactly one of this vartop's identity — through Install alone when the
// login service is installed.
func TestDaemonRestart(t *testing.T) {
	for _, installed := range []bool{true, false} {
		t.Run(fmt.Sprintf("service installed %v", installed), func(t *testing.T) {
			d := newTestDaemon(t)
			d.sup.installed = installed
			old := d.o.want
			old.Build = "old"
			stopped := politeDaemon(t, d.o.ctl, old)
			d.startsAs(t, d.o.want)
			out, err := runDaemonCmd(t, daemonRestartCmd)
			if err != nil {
				t.Fatalf("daemon restart = %v", err)
			}
			select {
			case <-stopped:
			default:
				t.Error("the old daemon was not asked to stop")
			}
			want, spawns := "install", 0
			if !installed {
				want, spawns = "", 1
			}
			if calls := strings.Join(d.sup.called(), ","); calls != want || d.spawned() != spawns {
				t.Errorf("service calls = %q, spawns = %d; want %q and %d", calls, d.spawned(), want, spawns)
			}
			l, fresh := lease(t, d.o.ctl)
			if !fresh || l.Build != d.o.want.Build || !strings.Contains(out, "restarted") {
				t.Errorf("lease = %+v (fresh %v), printed %q; want one daemon of this build", l, fresh, out)
			}
		})
	}
}

// TestDaemonUninstallStopsFirst: the daemon is stopped, gracefully, before
// the service is removed.
func TestDaemonUninstallStopsFirst(t *testing.T) {
	d := newTestDaemon(t)
	d.sup.installed = true
	stopped := politeDaemon(t, d.o.ctl, d.o.want)
	var runningAtUninstall bool
	d.sup.onUninstall = func() { _, runningAtUninstall = lease(t, d.o.ctl) }
	if _, err := runDaemonCmd(t, daemonUninstallCmd); err != nil {
		t.Fatalf("daemon uninstall = %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("the daemon was not asked to stop")
	}
	if runningAtUninstall || strings.Join(d.sup.called(), ",") != "uninstall" || len(d.killed()) != 0 {
		t.Errorf("running at Uninstall %v, service calls %v, kills %v; want stopped first, uninstall, no kill",
			runningAtUninstall, d.sup.called(), d.killed())
	}
}

// TestDaemonStopKilledStaysStopped: a daemon of the login service killed
// for not stopping is stopped through the service too, so that its
// restart-on-failure does not bring it back; one that stops when asked is
// left alone.
func TestDaemonStopKilledStaysStopped(t *testing.T) {
	t.Run("killed", func(t *testing.T) {
		d := newTestDaemon(t)
		d.sup.installed = true
		hung := d.o.want
		hung.PID = 4242
		d.onFakeClock(d.beats(claim(t, d.o.ctl, hung)))
		out, err := runDaemonCmd(t, daemonStopCmd)
		if err != nil || !strings.HasPrefix(out, "vartop daemon stopped.") {
			t.Fatalf("daemon stop = %q, %v", out, err)
		}
		if kills, calls := d.killed(), strings.Join(d.sup.called(), ","); len(kills) != 1 || calls != "stop" {
			t.Errorf("kills %v, service calls %q; want the pid killed, then stop", kills, calls)
		}
	})
	t.Run("asked", func(t *testing.T) {
		d := newTestDaemon(t)
		d.sup.installed = true
		politeDaemon(t, d.o.ctl, d.o.want)
		if _, err := runDaemonCmd(t, daemonStopCmd); err != nil {
			t.Fatal(err)
		}
		if calls := d.sup.called(); len(calls) != 0 {
			t.Errorf("service calls %v; want none", calls)
		}
	})
}

// TestDaemonStopNotResponding (R33): `daemon stop` of a daemon that
// neither stops nor beats kills nothing, and says why it did not stop.
func TestDaemonStopNotResponding(t *testing.T) {
	d := newTestDaemon(t)
	d.sup.installed = true
	dead := d.o.want
	dead.PID = 4242
	claim(t, d.o.ctl, dead)
	d.onFakeClock()
	out, err := runDaemonCmd(t, daemonStopCmd)
	if want := "the vartop daemon is not responding; its lease expires within 15s"; err == nil || err.Error() != want {
		t.Errorf("daemon stop = %q, %v; want the error %q", out, err, want)
	}
	if kills, calls := d.killed(), d.sup.called(); len(kills) != 0 || len(calls) != 0 {
		t.Errorf("kills %v, service calls %v; want none", kills, calls)
	}
}

// TestLosingRunKeepsLog: a `daemon run` that finds another daemon
// collecting never renames the log that daemon writes.
func TestLosingRunKeepsLog(t *testing.T) {
	d := newTestDaemon(t)
	beatingDaemon(t, d.o.ctl, store.Lease{PID: 4242, Build: "other"})
	path := filepath.Join(d.o.dataDir, "daemon.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), logMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := runDaemon(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("runDaemon() = %d, %v; want 0 (lost the claim)", code, err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("%s.1: %v; want the live log not renamed", path, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() <= logMaxBytes {
		t.Errorf("log: %v %v; want the live log kept (and appended to)", fi, err)
	}
}

// TestBareDaemonRuns: `vartop daemon` alone runs the daemon, as `daemon
// run` does (here it loses the claim to the daemon running and returns).
func TestBareDaemonRuns(t *testing.T) {
	d := newTestDaemon(t)
	beatingDaemon(t, d.o.ctl, store.Lease{PID: 4242, Build: "other"})
	t.Cleanup(func() {
		signal.Reset(syscall.SIGHUP) // the run ignores it
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs([]string{"daemon"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("vartop daemon = %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(d.o.dataDir, "daemon.log"))
	if !bytes.Contains(log, []byte("another vartop daemon is collecting")) {
		t.Errorf("daemon.log:\n%s\nwant the run's log", log)
	}
	if err := daemonCmd.Args(daemonCmd, []string{"bogus"}); err == nil {
		t.Error("vartop daemon bogus accepted")
	}
}

// TestOnboardingOffer: the login service is offered once a first config is
// saved; yes installs it with this vartop's environment, and an installed
// service is not offered again.
func TestOnboardingOffer(t *testing.T) {
	d := newTestDaemon(t)
	d.startsAs(t, d.o.want)
	asked := 0
	yes := func() (bool, error) { asked++; return true, nil }
	captureStdout(t, func() error { offerLoginService(yes); return nil })
	if asked != 1 || strings.Join(d.sup.called(), ",") != "install" {
		t.Fatalf("asked %d times, service calls %v; want 1 and install", asked, d.sup.called())
	}
	if fmt.Sprint(d.sup.env) != fmt.Sprint(d.o.env) {
		t.Errorf("service installed with %v, want %v", d.sup.env, d.o.env)
	}
	captureStdout(t, func() error { offerLoginService(yes); return nil })
	if asked != 1 {
		t.Errorf("asked again with the service installed")
	}
}

// TestIdleLogRotates: with no daemon collecting, a log past logMaxBytes is
// renamed to daemon.log.1 and a new one started.
func TestIdleLogRotates(t *testing.T) {
	d := newTestDaemon(t)
	path := filepath.Join(d.o.dataDir, "daemon.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), logMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateIdleLog(d.o.ctl, d.o.dataDir)
	f, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != logMaxBytes+1 {
		t.Errorf("rotated log: %v, want the old log at %s.1", err, path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("new log: %v %v, want an empty 0600 file", fi, err)
	}
}

// TestDetachedSpawnSubprocess re-executes the test binary as `vartop daemon
// run`, detached: it claims the lease and collects, and `daemon stop` ends
// it.
func TestDetachedSpawnSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon process")
	}
	dataDir := isolate(t)
	t.Setenv(asVartopEnv, "1")
	saveOneServer(t)
	o, closeCtl, err := localDaemon()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeCtl)
	o.sup = &fakeSupervisor{} // never the real login service
	t.Cleanup(func() {
		if _, _, err := stopDaemon(o); err != nil {
			t.Errorf("stopping the daemon: %v", err)
		}
	})
	orig := localDaemon
	localDaemon = func() (ensureOptions, func(), error) { return o, func() {}, nil }
	t.Cleanup(func() { localDaemon = orig })

	if err := ensureDaemon(o); err != nil {
		log, _ := os.ReadFile(filepath.Join(dataDir, "daemon.log"))
		t.Fatalf("ensureDaemon() = %v; log:\n%s", err, log)
	}
	l, fresh := lease(t, o.ctl)
	if !fresh || l.PID == os.Getpid() || l.Build != buildID() {
		t.Fatalf("lease = %+v (fresh %v), want one of another process of this build", l, fresh)
	}
	if runtime.GOOS == "linux" {
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", l.PID)); err != nil || cwd != dataDir {
			t.Errorf("daemon's working dir = %q, %v; want the data dir %q", cwd, err, dataDir)
		}
	}
	fleettest.WaitUntil(t, 10*time.Second, "a snapshot from the detached daemon", func() bool {
		db, err := store.OpenServer(dataDir, "prod", false)
		if err != nil {
			return false
		}
		defer db.Close()
		snap, err := db.Latest()
		return err == nil && snap != nil
	})

	out, err := runDaemonCmd(t, daemonStopCmd)
	if err != nil || !strings.HasPrefix(out, "vartop daemon stopped.") {
		t.Fatalf("daemon stop = %q, %v", out, err)
	}
	if after, _ := lease(t, o.ctl); after.Token != "" {
		t.Errorf("lease after stop = %+v, want it released by the daemon", after)
	}
	log, _ := os.ReadFile(filepath.Join(dataDir, "daemon.log"))
	if !bytes.Contains(log, []byte("collecting")) {
		t.Errorf("daemon.log:\n%s\nwant the daemon's log in it", log)
	}
}

// TestCrashOutputToLog: a daemon's crash (a panic nothing recovers, a
// fatal error) is written to daemon.log too — launchd and Task Scheduler
// send its stderr nowhere — unless stderr is daemon.log already, as a
// detached daemon's is.
func TestCrashOutputToLog(t *testing.T) {
	var got []string
	orig := setCrashOutput
	setCrashOutput = func(f *os.File, _ debug.CrashOptions) error {
		got = append(got, f.Name())
		return nil
	}
	t.Cleanup(func() { setCrashOutput = orig })

	dataDir := isolate(t)
	path := filepath.Join(dataDir, "daemon.log")
	_, closeLog := daemonLogger(dataDir) // the test's stderr is not daemon.log
	closeLog()
	if len(got) != 1 || got[0] != path {
		t.Fatalf("crash output set to %v, want %s", got, path)
	}

	got = nil
	log, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	stderr, err := openLog(path) // a detached daemon's stderr: the log itself
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	if err := crashToLog(log, stderr); err != nil || len(got) != 0 {
		t.Errorf("crashToLog(log, log) = %v, set to %v; want nothing set", err, got)
	}
	other, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := crashToLog(log, other); err != nil || len(got) != 1 || got[0] != path {
		t.Errorf("crashToLog(log, another file) = %v, set to %v; want %s", err, got, path)
	}
}

// TestSameEnv (R31, for the UIs): two daemons collect in the same
// environment when their config path and KUBECONFIG match, whatever their
// build or PATH; sameEnvAsOwn checks a lease against this vartop's.
func TestSameEnv(t *testing.T) {
	isolate(t)
	t.Setenv("KUBECONFIG", "/home/u/.kube/prod")
	own := ownLease()
	for _, tc := range []struct {
		name   string
		differ func(*store.Lease)
		same   bool
	}{
		{"identical", func(*store.Lease) {}, true},
		{"build", func(l *store.Lease) { l.Build = "/old/vartop (1 bytes, modified x)" }, true},
		{"PATH", func(l *store.Lease) { l.PathEnv = "/elsewhere" }, true},
		{"pid", func(l *store.Lease) { l.PID++ }, true},
		{"config path", func(l *store.Lease) { l.ConfigPath = "/elsewhere/vartop/config.json" }, false},
		{"KUBECONFIG", func(l *store.Lease) { l.Kubeconfig = "/home/u/.kube/staging" }, false},
		{"KUBECONFIG unset", func(l *store.Lease) { l.Kubeconfig = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := own
			tc.differ(&l)
			if got := sameEnv(l, own); got != tc.same {
				t.Errorf("sameEnv() = %v, want %v", got, tc.same)
			}
			if got := sameEnvAsOwn()(l); got != tc.same {
				t.Errorf("sameEnvAsOwn()() = %v, want %v", got, tc.same)
			}
			if got, want := sameDaemon(l, own), tc.same && l.Build == own.Build; got != want {
				t.Errorf("sameDaemon() = %v, want %v", got, want)
			}
		})
	}
}
