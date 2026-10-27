package collect

import (
	"bytes"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

	_ "modernc.org/sqlite"
)

// labels returns the snapshot's unit labels by unit key.
func labels(s *fleet.Snapshot) map[string]string {
	out := make(map[string]string, len(s.Pods))
	for _, p := range s.Pods {
		out[p.Key] = p.Label
	}
	return out
}

func labelList(s *fleet.Snapshot) []string {
	var out []string
	for _, p := range s.Pods {
		out = append(out, p.Label)
	}
	return out
}

// TestCollectsEveryServer: every configured server gets a file of its own,
// filled every interval, its units numbered instance-N.
func TestCollectsEveryServer(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	_, behind := fleettest.NewInstances(t, "c1", "c2")
	lb := fleettest.NewRoundRobinLB(t, behind...)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{
		"a":  {Interval: 1, Endpoints: []string{a.URL}},
		"b":  {Interval: 1, Endpoints: []string{b.URL}},
		"lb": {Interval: 1, Endpoints: []string{lb.URL}},
	})
	h.start(7)

	want := map[string][]string{"a": {"instance-1"}, "b": {"instance-1"}, "lb": {"instance-1", "instance-2"}}
	for name, labels := range want {
		fleettest.WaitUntil(t, 5*time.Second, name+" labelled "+labels[0]+"…", func() bool {
			s := h.latest(name)
			return s != nil && slices.Equal(labelList(s), labels)
		})
	}
	for name := range want {
		h.waitSnaps(name, 3, 3*time.Second) // one a second
	}
	if errs, err := h.control().ServerErrs(); err != nil || len(errs) != 0 {
		t.Errorf("ServerErrs = %v, %v; want none", errs, err)
	}
}

// TestHungDiscoveryIsolated (Review Focus 2): a server whose pool never
// gets built shows "connecting…" every interval while the others are
// polled every interval, and holds up no stop.
func TestHungDiscoveryIsolated(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // after the collector stops
	h := newHarness(t)
	h.build = func(s *config.Server) (*api.Pool, error) {
		if s.Name == "hung" {
			<-release
			return nil, errors.New("released")
		}
		return api.PoolFor(s)
	}
	h.writeConfig(map[string]*config.Server{
		"a":    {Interval: 1, Endpoints: []string{a.URL}},
		"b":    {Interval: 1, Endpoints: []string{b.URL}},
		"hung": {Interval: 1, Endpoints: []string{"http://127.0.0.1:1"}},
	})
	r := h.start(7)

	time.Sleep(4 * time.Second)
	for _, name := range []string{"a", "b"} {
		snaps := h.snaps(name)
		if len(snaps) < 3 {
			t.Errorf("%s: %d snapshots in 4s, want one a second", name, len(snaps))
		}
		for i := 1; i < len(snaps); i++ {
			if gap := snaps[i].At.Sub(snaps[i-1].At); gap > 1500*time.Millisecond {
				t.Errorf("%s: snapshot %d came %v after the one before, want about 1s", name, i, gap)
			}
		}
	}
	hung := h.snaps("hung")
	if len(hung) < 2 {
		t.Errorf("hung: %d snapshots in 4s, want an error snapshot a second", len(hung))
	}
	for _, s := range hung {
		if s.Err != "connecting…" {
			t.Errorf("hung: snapshot Err = %q, want connecting…", s.Err)
		}
	}
	if errs, _ := h.control().ServerErrs(); errs["hung"] != "connecting…" {
		t.Errorf("ServerErrs = %v, want hung connecting…", errs)
	}

	if code, err := r.stop(t); code != 0 || err != nil {
		t.Errorf("Run = %d, %v; want 0, nil", code, err)
	}
}

// TestDiscoveryFailureRetries: a pool build that fails is shown with when
// it is tried again, and reported as the server's error.
func TestDiscoveryFailureRetries(t *testing.T) {
	h := newHarness(t)
	h.build = func(*config.Server) (*api.Pool, error) { return nil, errors.New("no such context") }
	h.writeConfig(oneServer("http://127.0.0.1:1"))
	h.start(7)

	const want = "discovery failed: no such context — retrying in 5s"
	fleettest.WaitUntil(t, 2*time.Second, "the failure stored", func() bool {
		s := h.latest("a")
		return s != nil && s.Err == want
	})
	if errs, _ := h.control().ServerErrs(); errs["a"] != want {
		t.Errorf("ServerErrs = %v, want a: %q", errs, want)
	}
}

// TestPokeTriggersPoll: a poke polls the server at once; pokes close
// together poll it at most once a MinPokeGap.
func TestPokeTriggersPoll(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{"a": {Interval: 10, Endpoints: []string{srv.URL}}})
	h.start(7)
	h.waitSnaps("a", 1, 3*time.Second)

	ctl := h.control()
	if _, err := ctl.Poke("a"); err != nil {
		t.Fatal(err)
	}
	n := len(h.waitSnaps("a", 2, time.Second))

	for range 5 {
		if _, err := ctl.Poke("a"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond)
	if extra := len(h.snaps("a")) - n; extra < 1 || extra > 2 {
		t.Errorf("5 pokes in 200ms polled %d times, want 1 or 2", extra)
	}

	// Poked every 100ms for a second: polled every MinPokeGap, not every
	// time the collector looks.
	n = len(h.snaps("a"))
	for range 10 {
		if _, err := ctl.Poke("a"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if extra := len(h.snaps("a")) - n; extra > 2 {
		t.Errorf("10 pokes in 1s polled %d times by their end, want 2 at most", extra)
	}
}

// panicSource is a discovery source of endpoints that panics while
// panicking is set.
type panicSource struct {
	endpoints []string
	panicking atomic.Bool
}

func (s *panicSource) Endpoints() []string {
	if s.panicking.Load() {
		panic("source broke")
	}
	return s.endpoints
}

func (*panicSource) Close() error { return nil }

// TestPanicBecomesErrorSnapshot: a tick that panics stores an error
// snapshot and reports the server's error, logged once with its stack,
// while other servers are collected; it clears once ticks are clean again.
func TestPanicBecomesErrorSnapshot(t *testing.T) {
	_, boom := fleettest.NewVarmq(t, "boom", nil)
	_, ok := fleettest.NewVarmq(t, "ok", nil)
	src := &panicSource{endpoints: []string{boom.URL}}
	src.panicking.Store(true)
	h := newHarness(t)
	h.build = func(s *config.Server) (*api.Pool, error) {
		if s.Name == "boom" {
			return api.NewPool(src, ""), nil
		}
		return api.PoolFor(s)
	}
	h.writeConfig(map[string]*config.Server{
		"boom": {Interval: 1, Endpoints: []string{boom.URL}},
		"ok":   {Interval: 1, Endpoints: []string{ok.URL}},
	})
	h.start(7)

	const internal = "internal error (see vartop web's log)"
	snaps := h.waitSnaps("boom", 2, 3*time.Second)
	for _, s := range snaps {
		if s.Err != internal || len(s.Pods) != 0 {
			t.Errorf("boom: snapshot Err = %q with %d units, want %q and none", s.Err, len(s.Pods), internal)
		}
	}
	if errs, _ := h.control().ServerErrs(); errs["boom"] != internal {
		t.Errorf("ServerErrs = %v, want boom: %q", errs, internal)
	}
	if n := h.logs.count(`msg="tick panicked" server=boom panic="source broke" stack=`); n != 1 {
		t.Errorf("panic logged %d times, want once with its stack; log:\n%s", n, &h.logs)
	}
	h.waitSnaps("ok", 2, 3*time.Second)

	src.panicking.Store(false)
	fleettest.WaitUntil(t, 3*time.Second, "a clean boom snapshot", func() bool {
		s := h.latest("boom")
		return s != nil && s.Err == "" && len(s.Pods) == 1
	})
	fleettest.WaitUntil(t, time.Second, "boom's error cleared", func() bool {
		errs, _ := h.control().ServerErrs()
		_, failing := errs["boom"]
		return !failing
	})
}

// TestLabelsSurviveRestart: a restarted collector numbers the units as the
// last one did, and a newcomer takes the lowest number free.
func TestLabelsSurviveRestart(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "x", "y", "z")
	urls := []string{srvs[0].URL, srvs[1].URL, srvs[2].URL}
	slices.Sort(urls)
	h := newHarness(t)

	h.writeConfig(map[string]*config.Server{"s": {Interval: 1, Endpoints: urls[1:]}})
	first := h.start(1)
	want := map[string]string{urls[1]: "instance-1", urls[2]: "instance-2"}
	fleettest.WaitUntil(t, 3*time.Second, "the first collector's labels", func() bool {
		s := h.latest("s")
		return s != nil && equalMaps(labels(s), want)
	})
	if code, err := first.stop(t); code != 0 || err != nil {
		t.Fatalf("first collector: Run = %d, %v; want 0, nil", code, err)
	}

	// urls[0] sorts first: numbered afresh, it would be instance-1.
	h.writeConfig(map[string]*config.Server{"s": {Interval: 1, Endpoints: urls}})
	n := len(h.snaps("s"))
	h.start(2)
	snaps := h.waitSnaps("s", n+1, 3*time.Second)
	want[urls[0]] = "instance-3"
	if got := labels(snaps[n]); !equalMaps(got, want) {
		t.Errorf("after the restart, labels %v, want %v", got, want)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestRatesContinueAfterRestart: the first snapshot a restarted collector
// takes is rated against the last one stored.
func TestRatesContinueAfterRestart(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"w": {Status: "Running", Completed: 10}})
	h := newHarness(t)
	h.writeConfig(oneServer(srv.URL))
	rated := func(s *fleet.Snapshot) bool {
		return s != nil && len(s.Pods) == 1 && s.Pods[0].Workers["w"] != nil && s.Pods[0].Workers["w"].RateOK
	}

	first := h.start(1)
	fleettest.WaitUntil(t, 5*time.Second, "a rated snapshot", func() bool { return rated(h.latest("a")) })
	if code, err := first.stop(t); code != 0 || err != nil {
		t.Fatalf("first collector: Run = %d, %v; want 0, nil", code, err)
	}

	n := len(h.snaps("a"))
	h.start(2)
	snaps := h.waitSnaps("a", n+1, 3*time.Second)
	if gap := snaps[n].At.Sub(snaps[n-1].At); gap > 3*time.Second {
		t.Fatalf("restart took %v, want the first snapshot within 3 intervals", gap)
	}
	if !rated(snaps[n]) {
		t.Errorf("first snapshot after the restart: %+v, want it rated", snaps[n].Pods)
	}
}

// corruptTable overwrites the root page of table in the SQLite file at
// path with garbage, leaving the rest of the file (its header, its schema)
// as it was: the file opens, and reading table fails.
func corruptTable(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var root, size int64
	err = db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = ?`, table).Scan(&root)
	if err == nil {
		err = db.QueryRow(`PRAGMA page_size`).Scan(&size)
	}
	if err == nil {
		_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xa5}, int(size)), (root-1)*size); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptFileSetAside: a server file that opens but whose snapshots
// cannot be read is set aside and started afresh; other servers' files are
// left alone.
func TestCorruptFileSetAside(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{
		"a": {Interval: 1, Endpoints: []string{a.URL}},
		"b": {Interval: 1, Endpoints: []string{b.URL}},
	})
	first := h.start(1)
	h.waitSnaps("a", 2, 3*time.Second)
	h.waitSnaps("b", 2, 3*time.Second)
	if code, err := first.stop(t); code != 0 || err != nil {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	nb := len(h.snaps("b"))
	corruptTable(t, store.PathFor(h.dir, "a"), "snapshots")

	h.start(2)
	fleettest.WaitUntil(t, 3*time.Second, "a collected afresh", func() bool {
		s := h.latest("a")
		return s != nil && s.Err == "" && len(s.Pods) == 1
	})
	if aside, _ := filepath.Glob(store.PathFor(h.dir, "a") + ".corrupt-*"); len(aside) == 0 {
		t.Error("a's corrupt file not set aside")
	}
	if aside, _ := filepath.Glob(store.PathFor(h.dir, "b") + ".corrupt-*"); len(aside) != 0 {
		t.Errorf("b's file set aside: %v", aside)
	}
	h.waitSnaps("b", nb+1, 3*time.Second) // its history kept, and added to
}

// labelsFixture is a collector of server a over a fresh store, and its
// writer, neither running: a test hands ticks to the sink and has the
// writer write them itself. saved is the label number of #i the file has.
type labelsFixture struct {
	t   *testing.T
	dir string
	c   *collector
	w   *writer
}

func newLabelsFixture(t *testing.T) *labelsFixture {
	t.Helper()
	dir := t.TempDir()
	ctl, err := store.OpenControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	token, _, err := ctl.Claim(store.Lease{PID: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenServer(dir, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	d := &holder{o: Options{DataDir: dir, Log: slog.New(slog.DiscardHandler)}, ctl: ctl, token: token, exit: make(chan exit, 1)}
	f := &labelsFixture{t: t, dir: dir, c: &collector{d: d, name: "a", queue: make(chan queued, queueLen)}}
	f.w = &writer{c: f.c, db: db}
	t.Cleanup(func() {
		if f.w.db != nil {
			_ = f.w.db.Close()
		}
	})
	return f
}

// sink hands the sink a tick whose label state numbers #i n, or none when
// n is 0.
func (f *labelsFixture) sink(n int) {
	t := fleet.Tick{Snap: &fleet.Snapshot{At: time.Now().Round(0)}}
	if n > 0 {
		t.Labels = &fleet.LabelState{Numbers: map[string]fleet.LabelNumber{"#i": {N: n, Seen: time.UnixMilli(60000)}}}
	}
	_ = f.c.sink(t)
}

// drain has the writer write the next n ticks queued.
func (f *labelsFixture) drain(n int) {
	for range n {
		f.w.write(<-f.c.queue)
	}
}

// saved is the number of #i in the label state the file has.
func (f *labelsFixture) saved() int {
	f.t.Helper()
	db, err := store.OpenServer(f.dir, "a", false)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	l, ok, err := db.Labels()
	if err != nil || !ok {
		f.t.Fatalf("Labels = %v, %v", ok, err)
	}
	return l.Numbers["#i"].N
}

// TestDroppedTickKeepsLabels: the label state of a tick dropped with the
// writer behind is saved with the next tick written, and the newest label
// state the poller made is what the file ends with, whatever the order the
// writer meets them in.
func TestDroppedTickKeepsLabels(t *testing.T) {
	t.Run("dropped after the queue's head", func(t *testing.T) {
		f := newLabelsFixture(t)
		f.sink(1)
		for range queueLen - 1 {
			f.sink(0)
		}
		f.sink(2) // dropped: the queue is full
		f.drain(1)
		if n := f.saved(); n != 2 {
			t.Errorf("after a tick with state 2 was dropped, state %d saved, want 2", n)
		}
		f.drain(queueLen - 1)
		if n := f.saved(); n != 2 {
			t.Errorf("state %d saved, want 2 kept", n)
		}
	})
	t.Run("older state queued behind", func(t *testing.T) {
		f := newLabelsFixture(t)
		f.sink(0)
		f.sink(1)
		for range queueLen - 2 {
			f.sink(0)
		}
		f.sink(2) // dropped
		f.drain(queueLen)
		if n := f.saved(); n != 2 {
			t.Errorf("state %d saved, want the newest, 2", n)
		}
	})
	t.Run("write failed, older state queued behind", func(t *testing.T) {
		f := newLabelsFixture(t)
		f.sink(0)
		f.sink(1)
		for range queueLen - 2 {
			f.sink(0)
		}
		f.sink(2) // dropped
		_ = f.w.db.Close()
		f.drain(1) // fails: the file is closed
		f.w.db = nil
		f.drain(queueLen - 1)
		if n := f.saved(); n != 2 {
			t.Errorf("state %d saved, want the newest, 2", n)
		}
	})
	t.Run("newer state queued after a drop", func(t *testing.T) {
		f := newLabelsFixture(t)
		f.sink(1)
		f.drain(1)
		for range queueLen {
			f.sink(0)
		}
		f.sink(3) // dropped
		for range queueLen {
			<-f.c.queue // taken, not written
		}
		f.sink(4) // queued: newer than the state kept
		f.drain(1)
		if n := f.saved(); n != 4 {
			t.Errorf("state %d saved, want the newest, 4", n)
		}
	})
}

// TestWriterReopensRemovedFile (R36): a writer whose server file was
// deleted under it (servers/ cleared to drop history) notices at its next
// prune and writes a new file at the path, which the UIs read, with the
// label state it saved last.
func TestWriterReopensRemovedFile(t *testing.T) {
	f := newLabelsFixture(t)
	f.sink(1)
	f.drain(1)
	path := store.PathFor(f.dir, "a")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Skipf("file held open (Windows): %v", err)
		}
	}
	f.w.prune(time.Now())
	f.sink(0)
	f.drain(1)
	if n := len(f.snaps()); n != 1 {
		t.Errorf("new file: %d snapshots, want the one written since", n)
	}
	if n := f.saved(); n != 1 {
		t.Errorf("new file: label state %d, want the one saved last, 1", n)
	}
}

// snaps returns the snapshots in a's file, read as a UI does.
func (f *labelsFixture) snaps() []*fleet.Snapshot {
	f.t.Helper()
	db, err := store.OpenServer(f.dir, "a", false)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	snaps, _, err := db.Tail(0)
	if err != nil {
		f.t.Fatal(err)
	}
	return snaps
}

// TestFreshFileGetsSavedLabels (triage T4): a file the writer starts
// afresh — opened again after it could not be, or set aside as corrupt —
// gets the label state saved last with its first snapshot, not only at the
// next label change: a restart before then would number the units anew.
func TestFreshFileGetsSavedLabels(t *testing.T) {
	t.Run("opened again", func(t *testing.T) {
		f := newLabelsFixture(t)
		f.sink(1)
		f.drain(1)
		if n := f.saved(); n != 1 {
			t.Fatalf("saved %d, want 1", n)
		}
		_ = f.w.db.Close()
		path := store.PathFor(f.dir, "a")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		}
		f.w.db = nil
		f.sink(0)
		f.drain(1)
		if n := f.saved(); n != 1 {
			t.Errorf("the file opened afresh has label state %d, want 1", n)
		}
	})
	t.Run("set aside as corrupt", func(t *testing.T) {
		f := newLabelsFixture(t)
		f.sink(1)
		f.drain(1)
		_ = f.w.db.Close() // its page cache could mask the damage
		path := store.PathFor(f.dir, "a")
		corruptTable(t, path, "snapshots")
		db, err := store.OpenServer(f.dir, "a", true)
		if err != nil {
			t.Fatal(err)
		}
		f.w.db = db
		f.sink(0)
		f.drain(1)
		if aside, _ := filepath.Glob(path + ".corrupt-*"); len(aside) == 0 {
			t.Fatal("the corrupt file was not set aside")
		}
		if n := f.saved(); n != 1 {
			t.Errorf("the file started afresh has label state %d, want 1", n)
		}
	})
}

// TestStoreErrorRecorded (triage T4): a snapshot the writer cannot store
// is the server's error for the UIs, "store: …", until one is stored.
func TestStoreErrorRecorded(t *testing.T) {
	f := newLabelsFixture(t)
	_ = f.w.db.Close() // Append: sql: database is closed
	f.sink(0)
	f.drain(1)
	errs, err := f.c.d.ctl.ServerErrs()
	if err != nil || !strings.HasPrefix(errs["a"], "store:") {
		t.Errorf("ServerErrs = %v, %v; want a's store: error", errs, err)
	}
	f.w.db = nil // opened again for the next tick
	f.sink(0)
	f.drain(1)
	if errs, err := f.c.d.ctl.ServerErrs(); err != nil || errs["a"] != "" {
		t.Errorf("after a clean write, ServerErrs = %v, %v; want a's cleared", errs, err)
	}
}

// TestControlCorruptionKeepsServerFiles: control.db damaged under a
// running writer (the reviewer's probe: its collector table's root page
// overwritten) makes the write fail, but sets no server file aside.
func TestControlCorruptionKeepsServerFiles(t *testing.T) {
	f := newLabelsFixture(t)
	f.sink(0)
	f.drain(1)
	corruptTable(t, filepath.Join(f.dir, "control.db"), "daemon") // the lease table
	f.sink(0)
	f.drain(1)
	path := store.PathFor(f.dir, "a")
	if aside, _ := filepath.Glob(path + ".corrupt-*"); len(aside) != 0 {
		t.Errorf("server file set aside for control.db's damage: %v", aside)
	}
	if n := len(f.snaps()); n != 1 {
		t.Errorf("server file: %d snapshots, want its 1 kept", n)
	}
}

// TestUndecodableSavedState: a saved label state or latest snapshot that
// cannot be decoded is logged once and started without — units numbered
// afresh, rates from the next tick — rather than keep the server from
// being collected at all.
func TestUndecodableSavedState(t *testing.T) {
	f := newLabelsFixture(t)
	f.sink(1)
	f.drain(1)
	_ = f.w.db.Close()
	f.w.db = nil
	raw, err := sql.Open("sqlite", store.PathFor(f.dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE labels SET state = x'00ff'`, `UPDATE snapshots SET data = x'00ff'`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	var logs bytes.Buffer
	f.c.d.o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	for range 2 {
		db, init, err := f.c.load()
		if err != nil {
			t.Fatalf("load = %v, want the file opened", err)
		}
		_ = db.Close()
		if init.Labels != nil || init.Last != nil {
			t.Errorf("init = labels %v, last %v; want neither", init.Labels, init.Last)
		}
	}
	for _, msg := range []string{`msg="saved label state cannot be decoded`, `msg="latest snapshot cannot be decoded`} {
		if n := strings.Count(logs.String(), msg); n != 1 {
			t.Errorf("%s logged %d times, want once:\n%s", msg, n, &logs)
		}
	}
}

// breaker makes the requests a fresh connection sends to its hosts panic,
// while on is set, on the goroutine that sends them: a stand-in for a reply
// that breaks whatever reads it, in a goroutine a tick fans out to. It
// works through the transport's Proxy func, which the api package's fresh
// connections copy from http.DefaultTransport when their client is made,
// and which a transport calls on the sending goroutine.
type breaker struct {
	on    atomic.Bool
	mu    sync.Mutex
	hosts map[string]bool
}

// newBreaker installs a breaker until the test ends. Make it before the
// servers it breaks, so that it is removed after they have stopped.
func newBreaker(t *testing.T) *breaker {
	t.Helper()
	b := &breaker{hosts: make(map[string]bool)}
	tr := http.DefaultTransport.(*http.Transport)
	orig := tr.Proxy
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		b.mu.Lock()
		broken := b.hosts[req.URL.Host]
		b.mu.Unlock()
		if broken && b.on.Load() {
			panic("reply broke")
		}
		return orig(req)
	}
	t.Cleanup(func() { tr.Proxy = orig })
	return b
}

// add breaks the host of the URL u.
func (b *breaker) add(t *testing.T, u string) {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hosts[parsed.Host] = true
}

// TestFanOutPanicIsolated (R14): a panic in a goroutine a tick fans out
// to (a direct endpoint's poll; a balancer pass's attempt) stores an error
// snapshot for that server, logged once with the stack it was raised on,
// while the collector runs on and other servers are collected.
func TestFanOutPanicIsolated(t *testing.T) {
	b := newBreaker(t)
	_, direct := fleettest.NewVarmq(t, "d", nil)
	_, behind := fleettest.NewInstances(t, "c1", "c2")
	lb := fleettest.NewRoundRobinLB(t, behind...)
	_, ok := fleettest.NewVarmq(t, "ok", nil)
	b.add(t, direct.URL)
	b.add(t, lb.URL)
	h := newHarness(t)
	h.writeConfig(map[string]*config.Server{
		"d":  {Interval: 1, Endpoints: []string{direct.URL}},
		"lb": {Interval: 1, Endpoints: []string{lb.URL}},
		"ok": {Interval: 1, Endpoints: []string{ok.URL}},
	})
	r := h.start(7)
	// Once lb is known as a balancer, its polls go through the balancer
	// pass and its reacher's attempts.
	fleettest.WaitUntil(t, 5*time.Second, "lb polled as a balancer", func() bool {
		s := h.latest("lb")
		return s != nil && slices.Equal(labelList(s), []string{"instance-1", "instance-2"})
	})

	b.on.Store(true)
	const internal = "internal error (see vartop web's log)"
	for _, name := range []string{"d", "lb"} {
		fleettest.WaitUntil(t, 3*time.Second, name+"'s error snapshot", func() bool {
			s := h.latest(name)
			return s != nil && s.Err == internal
		})
	}
	n := len(h.snaps("ok"))
	h.waitSnaps("ok", n+2, 3*time.Second)
	select {
	case <-r.done:
		t.Fatalf("Run ended: %d, %v", r.code, r.err)
	default:
	}
	if errs, _ := h.control().ServerErrs(); errs["d"] != internal || errs["lb"] != internal || errs["ok"] != "" {
		t.Errorf("ServerErrs = %v, want d and lb %q, ok none", errs, internal)
	}
	for name, frame := range map[string]string{"d": "pollDirect", "lb": "listFresh"} {
		prefix := `msg="tick panicked" server=` + name + ` panic="reply broke" stack=`
		if n := h.logs.count(prefix); n != 1 {
			t.Errorf("%s's panic logged %d times, want once; log:\n%s", name, n, &h.logs)
		}
		if n := h.logs.count(prefix + `"goroutine`); n != 1 || !strings.Contains(h.logs.String(), frame) {
			t.Errorf("%s's panic logged without the stack it was raised on (through %s)", name, frame)
		}
	}

	b.on.Store(false)
	for _, name := range []string{"d", "lb"} {
		fleettest.WaitUntil(t, 3*time.Second, name+" collected again", func() bool {
			s := h.latest(name)
			return s != nil && s.Err == "" && len(s.Pods) > 0
		})
	}
}

// TestFillsStoredHistoryIntoBuckets: a server file whose snapshots were
// stored before it had day buckets (by an older vartop) gets them filled
// in the background by the collector, so that its 24h charts show the
// history already stored.
func TestFillsStoredHistoryIntoBuckets(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	h := newHarness(t)

	// An older vartop's file: a snapshot two hours old, no buckets.
	ctl := h.control()
	token, ok, err := ctl.Claim(store.Lease{PID: 1}, time.Now())
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	sm := &fleet.Sample{At: old, CompletedRate: 7, RateOK: true}
	sm.Name, sm.Status = "legacy", "running"
	w, err := store.OpenServer(h.dir, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(token, fleet.Tick{Snap: &fleet.Snapshot{At: old, Pods: []fleet.PodSnapshot{
		{Key: "http://x", Via: "http://x", Label: "instance-1", Workers: map[string]*fleet.Sample{"legacy": sm}}}}}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if err := ctl.Release(token); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.PathFor(h.dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE buckets; DELETE FROM meta WHERE k IN ('fill_to', 'filled')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	h.writeConfig(oneServer(srv.URL))
	h.start(1)
	fleettest.WaitUntil(t, 5*time.Second, "the stored snapshot summed into its bucket", func() bool {
		r, err := store.OpenServer(h.dir, "a", false)
		if err != nil {
			return false
		}
		defer r.Close()
		d, err := r.Day("legacy", time.Now().Add(-fleet.DayWindow))
		if err != nil {
			return false
		}
		_, peak := d.Rates("", false, time.Now(), fleet.NounFor(nil))
		return peak == 7
	})
}
