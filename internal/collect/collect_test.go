package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
)

// harness runs collectors in-process over a temporary data dir and config
// file.
type harness struct {
	t       *testing.T
	dir     string // the data dir
	cfgPath string
	mtime   time.Time    // of the config file last written, so that every write changes it
	offset  atomic.Int64 // added to the collectors' clock
	logs    logBuffer
	// build makes a server's pool; api.PoolFor when nil. Set it before
	// start.
	build func(*config.Server) (*api.Pool, error)
	// sleep is the collectors' Options.Sleep; set it before start.
	sleep func(time.Duration)

	ctlOnce sync.Once
	ctl     *store.Control
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		t:       t,
		dir:     filepath.Join(t.TempDir(), "data"),
		cfgPath: filepath.Join(t.TempDir(), "config.json"),
		mtime:   time.Now().Add(-time.Hour).Truncate(time.Second),
	}
}

// writeConfig writes a config of servers, each named by its key.
func (h *harness) writeConfig(servers map[string]*config.Server) {
	h.t.Helper()
	for name, s := range servers {
		s.Name = name
	}
	data, err := json.Marshal(&config.Config{Servers: servers})
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeRaw(data)
}

// writeRaw writes data as the config file, with an mtime a second past the
// last one's, so that the collector sees a change even on a coarse clock.
func (h *harness) writeRaw(data []byte) {
	h.t.Helper()
	if err := os.WriteFile(h.cfgPath, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.mtime = h.mtime.Add(time.Second)
	if err := os.Chtimes(h.cfgPath, h.mtime, h.mtime); err != nil {
		h.t.Fatal(err)
	}
}

// readConfig is the collector's Options.Config: config.Load, but of the
// harness's file.
func (h *harness) readConfig() (*config.Config, string, error) {
	data, err := os.ReadFile(h.cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, h.cfgPath, config.ErrConfigNotFound
	}
	if err != nil {
		return nil, h.cfgPath, err
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, h.cfgPath, err
	}
	for name, s := range cfg.Servers {
		if s == nil {
			return nil, h.cfgPath, fmt.Errorf("server %q: empty definition", name)
		}
		if err := s.Validate(); err != nil {
			return nil, h.cfgPath, err
		}
	}
	return &cfg, h.cfgPath, nil
}

func (h *harness) now() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) }

// collectorRun is a Run in progress.
type collectorRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	code   int
	err    error
}

// start runs a collector claiming the lease as pid; it is stopped when the
// test ends, before the fakes it polls shut down.
func (h *harness) start(pid int) *collectorRun {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &collectorRun{cancel: cancel, done: make(chan struct{})}
	o := Options{
		DataDir:   h.dir,
		Config:    h.readConfig,
		BuildPool: h.build,
		Lease:     store.Lease{PID: pid},
		Now:       h.now,
		Sleep:     h.sleep,
		Log:       slog.New(slog.NewTextHandler(&h.logs, nil)),
	}
	go func() {
		defer close(r.done)
		r.code, r.err = Run(ctx, o)
	}()
	h.t.Cleanup(func() { r.stop(h.t) })
	return r
}

// wait returns Run's result, failing the test if it takes over timeout.
func (r *collectorRun) wait(t *testing.T, timeout time.Duration) (int, error) {
	t.Helper()
	select {
	case <-r.done:
		return r.code, r.err
	case <-time.After(timeout):
		t.Fatalf("Run still running after %v", timeout)
		return 0, nil
	}
}

// stop cancels Run and returns its result.
func (r *collectorRun) stop(t *testing.T) (int, error) {
	t.Helper()
	r.cancel()
	return r.wait(t, 5*time.Second)
}

// control is the data dir's control file, opened as a UI does.
func (h *harness) control() *store.Control {
	h.t.Helper()
	h.ctlOnce.Do(func() {
		ctl, err := store.OpenControl(h.dir)
		if err != nil {
			h.t.Fatal(err)
		}
		h.t.Cleanup(func() { _ = ctl.Close() })
		h.ctl = ctl
	})
	return h.ctl
}

// snaps returns the snapshots stored for server, oldest first: none while
// its file is missing or not readable yet.
func (h *harness) snaps(server string) []*fleet.Snapshot {
	db, err := store.OpenServer(h.dir, server, false)
	if err != nil {
		return nil
	}
	defer db.Close()
	snaps, _, err := db.Tail(0)
	if err != nil {
		return nil
	}
	return snaps
}

// latest is the newest snapshot stored for server, or nil.
func (h *harness) latest(server string) *fleet.Snapshot {
	snaps := h.snaps(server)
	if len(snaps) == 0 {
		return nil
	}
	return snaps[len(snaps)-1]
}

// waitSnaps waits until server has n snapshots stored, and returns them.
func (h *harness) waitSnaps(server string, n int, timeout time.Duration) []*fleet.Snapshot {
	h.t.Helper()
	var snaps []*fleet.Snapshot
	fleettest.WaitUntil(h.t, timeout, fmt.Sprintf("%d snapshots of %s", n, server), func() bool {
		snaps = h.snaps(server)
		return len(snaps) >= n
	})
	return snaps
}

// waitLog waits until the collector log has a line containing s.
func (h *harness) waitLog(s string, timeout time.Duration) {
	h.t.Helper()
	fleettest.WaitUntil(h.t, timeout, "log "+s, func() bool { return h.logs.count(s) > 0 })
}

// exists reports whether server's file is there.
func (h *harness) exists(server string) bool {
	_, err := os.Stat(store.PathFor(h.dir, server))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.t.Fatal(err)
	}
	return err == nil
}

// logBuffer collects the collector log.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// count is how many lines of the log contain s.
func (l *logBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.Lines(l.b.String()) {
		if strings.Contains(line, s) {
			n++
		}
	}
	return n
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// oneServer is a config of one server, a over the endpoint url polled
// every second.
func oneServer(url string) map[string]*config.Server {
	return map[string]*config.Server{"a": {Interval: 1, Endpoints: []string{url}}}
}

func TestSecondCollectorExitsZero(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(srv.URL))
	h.start(1)
	n := len(h.waitSnaps("a", 1, 3*time.Second))

	// It watches the first one beat (once, BeatEvery at most), and leaves
	// it the lease.
	second := h.start(2)
	if code, err := second.wait(t, store.DeadAfter); code != 0 || err != nil {
		t.Errorf("second collector: Run = %d, %v; want 0, nil", code, err)
	}
	if l, fresh, err := h.control().Lease(); err != nil || !fresh || l.PID != 1 {
		t.Errorf("Lease = %+v, fresh %v, %v; want the first collector's, fresh", l, fresh, err)
	}
	h.waitSnaps("a", n+1, 3*time.Second) // the first one collects on
}

// waitBeats waits until the collector has renewed its lease n more times: a
// span of time, with no sleep of a guessed length.
func (h *harness) waitBeats(n int) {
	h.t.Helper()
	ctl := h.control()
	first, _, err := ctl.Lease()
	if err != nil {
		h.t.Fatal(err)
	}
	fleettest.WaitUntil(h.t, time.Duration(n+1)*store.BeatEvery, fmt.Sprintf("%d beats", n), func() bool {
		l, _, err := ctl.Lease()
		return err == nil && l.Beat.Sub(first.Beat) >= time.Duration(n)*store.BeatEvery-500*time.Millisecond
	})
}

// fakeWatch makes a collector's watch of a lease holder's beat pass at once:
// each of its sleeps moves the collectors' clock on instead. The collectors'
// clock stays ahead of the real one afterwards, so a lease they renew is
// not fresh by it.
func (h *harness) fakeWatch() {
	h.sleep = func(d time.Duration) { h.offset.Add(int64(d)) }
}

// deadLease leaves the lease as a collector that died without letting it go
// (a crash, a SIGKILL) does: fresh, and never to be renewed. It returns
// its token.
func (h *harness) deadLease() string {
	h.t.Helper()
	token, ok, err := h.control().Claim(store.Lease{PID: 9}, h.now())
	if !ok || err != nil {
		h.t.Fatalf("Claim = %v, %v", ok, err)
	}
	return token
}

// TestReleasedHolderTakenOverQuietly: a collector watching the beat of a
// holder that lets the lease go — a vartop web stopped with Ctrl+C — takes
// it over at once, without logging it as a holder that does not beat.
func TestReleasedHolderTakenOverQuietly(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(srv.URL))
	ctl := h.control()
	token, ok, err := ctl.Claim(store.Lease{PID: 9}, h.now())
	if !ok || err != nil {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	began := time.Now()
	h.start(3)
	time.Sleep(300 * time.Millisecond) // it watches the beat
	if err := ctl.Release(token); err != nil {
		t.Fatal(err)
	}
	h.waitSnaps("a", 1, 3*time.Second)
	if took := time.Since(began); took > store.DeadAfter/2 {
		t.Errorf("took the released lease after %v, want at once", took)
	}
	if n := h.logs.count("does not beat"); n != 0 {
		t.Errorf("a released lease was logged as a holder that does not beat:\n%s", &h.logs)
	}
}

// TestCrashedHolderTakenOver (R34): a collector started while the lease of a
// dead one is still fresh — a supervisor restarts a crashed collector within
// LeaseTTL — watches its beat for DeadAfter, takes the lease over, and
// collects.
func TestCrashedHolderTakenOver(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(srv.URL))
	h.fakeWatch()
	dead := h.deadLease()
	h.start(3)
	h.waitSnaps("a", 1, 5*time.Second)
	if l, _, err := h.control().Lease(); err != nil || l.PID != 3 || l.Token == dead {
		t.Errorf("Lease = %+v, %v; want the new collector's", l, err)
	}
	if watched := time.Duration(h.offset.Load()); watched < store.DeadAfter || watched > store.DeadAfter+time.Second {
		t.Errorf("took the lease over after watching %v, want DeadAfter (%v)", watched, store.DeadAfter)
	}
}

// TestWatchEndsWithCtx: a collector watching a lease holder's beat (on the
// real clock) stops when told to, at once, taking nothing over.
func TestWatchEndsWithCtx(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{})
	dead := h.deadLease()
	r := h.start(3)
	began := time.Now()
	if code, err := r.stop(t); code != 0 || err != nil {
		t.Errorf("Run = %d, %v; want 0, nil", code, err)
	}
	if took := time.Since(began); took > store.DeadAfter/2 {
		t.Errorf("Run took %v to stop while watching, want it at once", took)
	}
	if l, _, err := h.control().Lease(); err != nil || l.Token != dead {
		t.Errorf("Lease = %+v, %v; want the dead collector's still", l, err)
	}
}

// TestTwoWatchersOneWins (R34): two collectors started together over a dead
// collector's fresh lease both watch it; exactly one takes it over and
// collects, the other exits 0.
func TestTwoWatchersOneWins(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(srv.URL))
	h.fakeWatch()
	h.deadLease()
	runs := map[int]*collectorRun{4: h.start(4), 5: h.start(5)}
	var loser, winner int
	fleettest.WaitUntil(t, 5*time.Second, "one collector to exit", func() bool {
		for pid, r := range runs {
			select {
			case <-r.done:
				loser = pid
				return true
			default:
			}
		}
		return false
	})
	if code, err := runs[loser].wait(t, time.Second); code != 0 || err != nil {
		t.Errorf("collector %d: Run = %d, %v; want 0, nil", loser, code, err)
	}
	winner = map[int]int{4: 5, 5: 4}[loser]
	n := len(h.waitSnaps("a", 1, 5*time.Second))
	if l, _, err := h.control().Lease(); err != nil || l.PID != winner {
		t.Errorf("Lease = %+v, %v; want %d's", l, err, winner)
	}
	h.waitSnaps("a", n+1, 5*time.Second)
	select {
	case <-runs[winner].done:
		t.Errorf("collector %d exited too: Run = %d, %v", winner, runs[winner].code, runs[winner].err)
	default:
	}
}

// TestDataDirRemovedExits (R36): a collector whose data dir is deleted under
// it — removed, or removed and made anew by a UI — exits 1 at its next
// beat, rather than collect on into files no UI reads while another
// collector claims the new control.db.
func TestDataDirRemovedExits(t *testing.T) {
	for _, anew := range []bool{false, true} {
		t.Run(fmt.Sprintf("made anew %v", anew), func(t *testing.T) {
			_, srv := fleettest.NewVarmq(t, "i", nil)
			h := newHarness(t)
			h.writeConfig(oneServer(srv.URL))
			r := h.start(7)
			h.waitSnaps("a", 1, 3*time.Second)
			if err := os.RemoveAll(h.dir); err != nil {
				t.Fatal(err)
			}
			if anew {
				ctl, err := store.OpenControl(h.dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ctl.Close() })
			}
			code, err := r.wait(t, store.BeatEvery+2*time.Second)
			if code != 1 || err == nil || !strings.Contains(err.Error(), "data directory removed") {
				t.Errorf("Run = %d, %v; want 1, data directory removed", code, err)
			}
		})
	}
}

// TestConfigDeletedStopsCollecting (R37): deleting the config is a config
// of no servers: every collector stops, using its token no more, and the
// files stay; put back, it is collected again.
func TestConfigDeletedStopsCollecting(t *testing.T) {
	fa, a := fleettest.NewVarmq(t, "a", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(a.URL))
	h.start(7)
	h.waitSnaps("a", 1, 3*time.Second)

	if err := os.Remove(h.cfgPath); err != nil {
		t.Fatal(err)
	}
	h.waitLog(`msg="server removed" server=a`, 5*time.Second)
	h.waitBeats(1) // a tick in flight at the removal lands
	calls := len(fa.Calls())
	h.waitBeats(2) // past a's next ticks, 1s apart: none polls it
	if got := len(fa.Calls()); got != calls {
		t.Errorf("a polled %d more times after the config was deleted", got-calls)
	}
	if n := h.logs.count(`msg="config error`); n != 0 {
		t.Errorf("a deleted config logged as a config error:\n%s", &h.logs)
	}
	if !h.exists("a") {
		t.Error("a's file removed with the config")
	}

	since := time.Now()
	h.writeConfig(oneServer(a.URL))
	fleettest.WaitUntil(t, 5*time.Second, "a collected once the config is back", func() bool {
		s := h.latest("a")
		return s != nil && s.At.After(since)
	})
}

// TestTakeoverEndsOldCollector: a collector whose lease another claimed (its
// lease looked stale, as after a sleep) writes nothing more and exits 0.
func TestTakeoverEndsOldCollector(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(srv.URL))
	old := h.start(6)
	h.waitSnaps("a", 1, 3*time.Second)

	ctl := h.control()
	if _, ok, err := ctl.Claim(store.Lease{PID: 3}, time.Now().Add(store.LeaseTTL+time.Second)); !ok || err != nil {
		t.Fatalf("Claim = %v, %v; want the lease", ok, err)
	}
	n := len(h.snaps("a"))
	if code, err := old.wait(t, 2*time.Second); code != 0 || err != nil {
		t.Errorf("old collector: Run = %d, %v; want 0, nil", code, err)
	}
	if got := len(h.snaps("a")); got != n {
		t.Errorf("old collector stored %d snapshots after losing its lease", got-n)
	}
	if l, _, err := ctl.Lease(); err != nil || l.PID != 3 || l.Token == "" {
		t.Errorf("Lease = %+v, %v; want the new claimant's, still held", l, err)
	}
}

// TestConfigAddRemoveEdit: with no servers the collector idles; a server added
// to the config is collected, one edited is restarted (and no other), one
// removed stops being polled while its file stays.
func TestConfigAddRemoveEdit(t *testing.T) {
	fa, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{})
	h.start(7)
	h.waitLog(`msg=collecting`, time.Second)

	h.writeConfig(map[string]*config.Server{"a": {Interval: 1, Endpoints: []string{a.URL}}})
	h.waitSnaps("a", 1, 5*time.Second)

	// add
	h.writeConfig(map[string]*config.Server{
		"a": {Interval: 1, Endpoints: []string{a.URL}},
		"b": {Interval: 10, Endpoints: []string{b.URL}},
	})
	h.waitSnaps("b", 1, 5*time.Second)

	// edit: restarted at once, b polls every second from then on
	h.writeConfig(map[string]*config.Server{
		"a": {Interval: 1, Endpoints: []string{a.URL}},
		"b": {Interval: 1, Endpoints: []string{b.URL}},
	})
	h.waitSnaps("b", 3, 5*time.Second) // b's next regular tick was 10s away
	if n := h.logs.count(`msg="server restarted" server=b`); n != 1 {
		t.Errorf("b restarted %d times, want once", n)
	}
	if n := h.logs.count(`server=a`); n != 1 {
		t.Errorf("a logged %d times, want once (added); log:\n%s", n, &h.logs)
	}

	// remove
	h.writeConfig(map[string]*config.Server{"b": {Interval: 1, Endpoints: []string{b.URL}}})
	h.waitLog(`msg="server removed" server=a`, 5*time.Second)
	time.Sleep(300 * time.Millisecond) // a tick in flight lands
	calls, n := len(fa.Calls()), len(h.snaps("a"))
	time.Sleep(1500 * time.Millisecond)
	if got := len(fa.Calls()); got != calls {
		t.Errorf("a polled %d more times after its removal", got-calls)
	}
	if got := len(h.snaps("a")); got != n {
		t.Errorf("a stored %d more snapshots after its removal", got-n)
	}
	if !h.exists("a") {
		t.Error("a's file removed with a from the config, want it kept")
	}
}

// TestBadConfigKeepsLastGood (Review Focus 3): a half-written config is
// logged once, and the collector collects on with the last good one. Nothing
// it holds is taken for a removed server: with the clock past the grace,
// a sweep would otherwise remove every file.
func TestBadConfigKeepsLastGood(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{
		"a": {Interval: 1, Endpoints: []string{a.URL}},
		"b": {Interval: 1, Endpoints: []string{b.URL}},
	})
	h.start(7)
	h.waitSnaps("a", 1, 3*time.Second)
	h.waitSnaps("b", 1, 3*time.Second)

	h.writeRaw([]byte(`{"servers": {"a": {"interval": 1, "endpo`))
	h.waitLog(`msg="config error`, 5*time.Second)
	h.offset.Store(int64(RemovedGrace + time.Hour))
	// Past the next config check, which prunes and sweeps on the new clock.
	since := time.Now().Add(ConfigEvery + 500*time.Millisecond)
	for _, name := range []string{"a", "b"} {
		fleettest.WaitUntil(t, 5*time.Second, name+" collected after the bad config", func() bool {
			s := h.latest(name)
			return s != nil && s.At.After(since) && s.Err == "" && len(s.Pods) == 1
		})
		if !h.exists(name) {
			t.Errorf("%s's file removed", name)
		}
	}
	if n := h.logs.count(`msg="config error`); n != 1 {
		t.Errorf("config error logged %d times, want once", n)
	}
}

// TestBadConfigAtStartDeletesNothing: a collector that starts on a config it
// cannot read collects nothing and removes no file, however old; once the
// config is mended, it collects.
func TestBadConfigAtStartDeletesNothing(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	h := newHarness(t)
	h.writeConfig(oneServer(a.URL))
	first := h.start(1)
	h.waitSnaps("a", 1, 3*time.Second)
	if code, err := first.stop(t); code != 0 || err != nil {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}

	h.writeRaw([]byte(`{"servers": {"a": `))
	h.offset.Store(int64(RemovedGrace + time.Hour))
	h.start(2)
	h.waitLog(`msg="config error; no servers`, time.Second)
	time.Sleep(ConfigEvery + 500*time.Millisecond) // past a sweep
	if !h.exists("a") {
		t.Fatal("a's file removed while the config could not be read")
	}

	since := time.Now()
	h.writeConfig(oneServer(a.URL))
	fleettest.WaitUntil(t, 5*time.Second, "a collected once the config is mended", func() bool {
		s := h.latest("a")
		return s != nil && s.At.After(since)
	})
}

// TestRemovedServerFileDeletedAfterGrace: the file of a server gone from
// the config stays until RemovedGrace after its last write, then goes.
func TestRemovedServerFileDeletedAfterGrace(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{
		"a": {Interval: 1, Endpoints: []string{a.URL}},
		"b": {Interval: 1, Endpoints: []string{b.URL}},
	})
	h.start(7)
	h.waitSnaps("a", 1, 3*time.Second)
	h.waitSnaps("b", 1, 3*time.Second)
	h.writeConfig(map[string]*config.Server{"b": {Interval: 1, Endpoints: []string{b.URL}}})
	h.waitLog(`msg="server removed" server=a`, 5*time.Second)

	h.offset.Store(int64(RemovedGrace - time.Hour))
	time.Sleep(ConfigEvery + 500*time.Millisecond) // a sweep runs on the new clock
	if !h.exists("a") {
		t.Fatal("a's file removed before its grace ran out")
	}

	h.offset.Store(int64(RemovedGrace + 2*PruneEvery))
	fleettest.WaitUntil(t, 5*time.Second, "a's file removed", func() bool { return !h.exists("a") })
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(store.PathFor(h.dir, "a") + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a's %s left: %v", suffix, err)
		}
	}
	if !h.exists("b") {
		t.Error("b's file removed, want it kept")
	}
}
