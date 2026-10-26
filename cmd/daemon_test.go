package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fakeSupervisor is a login service that records what it is asked, and
// starts a daemon by running onStart: Install starts it too, as the real
// ones do.
type fakeSupervisor struct {
	mu        sync.Mutex
	installed bool
	calls     []string
	exe       string
	env       map[string]string
	onStart   func()
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
	f.mu.Lock()
	f.installed, f.exe, f.env = true, exe, env
	f.mu.Unlock()
	f.started()
	return nil
}

func (f *fakeSupervisor) Uninstall() error {
	f.record("uninstall")
	f.mu.Lock()
	f.installed = false
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) Start() error   { f.record("start"); f.started(); return nil }
func (f *fakeSupervisor) Restart() error { f.record("restart"); f.started(); return nil }

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

// startsAs makes the daemon spawned or started by the login service the
// one claiming the lease as l. The service starts none while one runs.
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
		if got := strings.Join(d.sup.called(), ","); got != "install,start" || d.spawned() != 0 {
			t.Errorf("service calls = %s, spawns = %d; want install,start and no spawn", got, d.spawned())
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
					if calls != "install,restart" || d.spawned() != 0 {
						t.Errorf("service calls = %s, spawns = %d; want install,restart and no spawn", calls, d.spawned())
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

// TestEnsureDaemonKillsHungDaemon: a daemon that does not stop when asked
// is killed after stopWait, by the pid of its lease, and its lease let go,
// so that the new one can claim it at once.
func TestEnsureDaemonKillsHungDaemon(t *testing.T) {
	d := newTestDaemon(t)
	clock := &fakeClock{t: time.Now()}
	d.o.now, d.o.sleep = clock.now, clock.sleep
	old := d.o.want
	old.Build, old.PID = "old", 4242
	claim(t, d.o.ctl, old)
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
		ConfigPath: "/home/u/.config/vartop/config.json", Kubeconfig: "/home/u/.kube/prod"})
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
		"/home/u/.config/vartop/config.json", "/home/u/.kube/prod", d.o.dataDir,
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

func TestOpenLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), logMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
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
