package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// at is a wall-clock instant at millisecond precision, as stored: times
// built any other way (a UTC location, nanoseconds) never deep-equal what
// the store decodes.
func at(ms int64) time.Time { return time.UnixMilli(ms) }

// base is the instant the tests' snapshots are taken around.
var base = time.UnixMilli(time.Now().UnixMilli())

// claimed opens dataDir's control file and claims the lease, returning the
// token every Append needs.
func claimed(t testing.TB, dataDir string) (*Control, string) {
	t.Helper()
	ctl, err := OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	token, ok, err := ctl.Claim(Lease{PID: os.Getpid()}, time.Now())
	if err != nil || !ok {
		t.Fatalf("Claim = %q, %v, %v; want a token", token, ok, err)
	}
	return ctl, token
}

// newWriter opens key's file for writing, closed when the test ends.
func newWriter(t testing.TB, dataDir, key string) *ServerDB {
	t.Helper()
	db, err := OpenServer(dataDir, key, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tick is a minimal snapshot at at: one direct unit with one worker whose
// completed counter is n.
func tick(at time.Time, n uint64) fleet.Tick {
	sm := &fleet.Sample{At: at}
	sm.Name, sm.Status, sm.Instance, sm.Origin = "w", "running", "a", "http://a"
	sm.Metrics.Completed = n
	return fleet.Tick{Snap: &fleet.Snapshot{At: at, Pods: []fleet.PodSnapshot{{
		Key: "http://a", Via: "http://a", Instance: "a", Label: "instance-1",
		Workers: map[string]*fleet.Sample{"w": sm},
	}}}}
}

func appendAll(t *testing.T, db *ServerDB, token string, ticks ...fleet.Tick) []int64 {
	t.Helper()
	ids := make([]int64, len(ticks))
	for i, tk := range ticks {
		id, err := db.Append(token, tk)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

// completed lists the counter tick put in each snapshot, to compare runs.
func completed(snaps []*fleet.Snapshot) []uint64 {
	out := make([]uint64, len(snaps))
	for i, s := range snaps {
		out[i] = s.Pods[0].Workers["w"].Metrics.Completed
	}
	return out
}

// sample is one worker at snapshot time + offMs.
func sample(snapAt time.Time, offMs int64, name, instance, origin string, completed, failed uint64, cr, fr float64, ok bool) *fleet.Sample {
	s := &fleet.Sample{At: at(snapAt.UnixMilli() + offMs), CompletedRate: cr, FailedRate: fr, RateOK: ok}
	s.Name, s.Status, s.Concurrency = name, "running", 8
	s.NumProcessing, s.NumPending, s.NumIdleWorkers = 3, 120, 5
	s.Instance, s.Origin = instance, origin
	s.Metrics.Submitted, s.Metrics.Completed, s.Metrics.Successful, s.Metrics.Failed = completed+120, completed, completed-failed, failed
	return s
}

// fullSnapshot has every kind of unit: direct, balanced, dead (balanced, not
// reached) and errored, a sample answered by another instance than its
// unit's, a sample with no instance on a unit that has one, valid, zero and
// invalid rates, unreached workers, Behind, and a server error. Rates carry
// more than 3 decimals; round gives what the store keeps.
func fullSnapshot(round bool) *fleet.Snapshot {
	r := func(full, rounded float64) float64 {
		if round {
			return rounded
		}
		return full
	}
	snapAt := base
	const lb = "http://lb.example:8080/varmq"
	const direct = "http://10.0.0.1:8080/varmq"
	return &fleet.Snapshot{
		At: snapAt,
		Pods: []fleet.PodSnapshot{
			{
				Key: direct, Via: direct, Instance: "inst-a", Label: "instance-1",
				Workers: map[string]*fleet.Sample{
					"emails":  sample(snapAt, 120, "emails", "inst-a", direct, 1000, 4, r(12.34567, 12.346), r(0.0004, 0), true),
					"reports": sample(snapAt, 95, "reports", "inst-a", direct, 50, 0, 0, 0, false),
					"moved":   sample(snapAt, 140, "moved", "inst-z", "http://10.0.0.9:8080/varmq", 7, 1, 0, 0, true),
					"anon":    sample(snapAt, 77, "anon", "", direct, 9, 0, r(1.0/3, 0.333), 0, true),
				},
				Unreached: []string{"slow"},
			},
			{
				Key: "#inst-b", Via: lb, Instance: "inst-b", Label: "instance-2", Balanced: true,
				Workers: map[string]*fleet.Sample{
					"emails": sample(snapAt, -30, "emails", "inst-b", lb, 2000, 10, r(99.99999, 100), r(2.5, 2.5), true),
				},
			},
			{
				Key: "#inst-c", Via: lb, Instance: "inst-c", Label: "instance-3", Balanced: true,
				Err: "not reached this refresh · last seen 15s ago",
			},
			{
				Key: "http://10.0.0.4:8080/varmq", Via: "http://10.0.0.4:8080/varmq", Label: "instance-4",
				Err: "failed to fetch workers from 10.0.0.4:8080: connection refused",
			},
			{
				Key: "http://10.0.0.5:8080/varmq", Via: "http://10.0.0.5:8080/varmq", Instance: "inst-e", Label: "instance-5",
				Workers: map[string]*fleet.Sample{},
			},
		},
		Behind: map[string]int{lb: 2},
		Err:    "",
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")

	errSnap := &fleet.Snapshot{At: base.Add(5 * time.Second), Err: "discovery failed: no pods — retrying in 5s"}
	for _, tc := range []struct {
		name string
		in   *fleet.Snapshot
		want *fleet.Snapshot
	}{
		{"units", fullSnapshot(false), fullSnapshot(true)},
		{"server error", errSnap, errSnap},
	} {
		if _, err := db.Append(token, fleet.Tick{Snap: tc.in}); err != nil {
			t.Fatalf("%s: Append: %v", tc.name, err)
		}
		got, err := db.Latest()
		if err != nil {
			t.Fatalf("%s: Latest: %v", tc.name, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: round trip:\n got  %s\n want %s", tc.name, dump(got), dump(tc.want))
		}
	}
}

// dump prints a snapshot with its samples, which %+v shows as pointers.
func dump(s *fleet.Snapshot) string {
	if s == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{At:%v Err:%q Behind:%v\n", s.At, s.Err, s.Behind)
	for _, p := range s.Pods {
		fmt.Fprintf(&b, "  {Key:%s Via:%s Instance:%q Label:%s Balanced:%v Err:%q Unreached:%v Workers(nil=%v):\n",
			p.Key, p.Via, p.Instance, p.Label, p.Balanced, p.Err, p.Unreached, p.Workers == nil)
		for name, w := range p.Workers {
			fmt.Fprintf(&b, "    %s: %+v\n", name, *w)
		}
		b.WriteString("  }\n")
	}
	return b.String() + "}"
}

func TestLatestEmpty(t *testing.T) {
	dir := t.TempDir()
	claimed(t, dir)
	db := newWriter(t, dir, "prod")
	if s, err := db.Latest(); s != nil || err != nil {
		t.Errorf("Latest of an empty file = %v, %v; want nil, nil", s, err)
	}
}

func TestTailNoDuplicates(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")

	appendAll(t, db, token, tick(base, 1), tick(base.Add(time.Second), 2), tick(base.Add(2*time.Second), 3))
	snaps, cursor, err := db.Tail(0)
	if err != nil || !reflect.DeepEqual(completed(snaps), []uint64{1, 2, 3}) {
		t.Fatalf("Tail(0) = %v, %v; want 1 2 3", completed(snaps), err)
	}
	appendAll(t, db, token, tick(base.Add(3*time.Second), 4), tick(base.Add(4*time.Second), 5))
	snaps, next, err := db.Tail(cursor)
	if err != nil || !reflect.DeepEqual(completed(snaps), []uint64{4, 5}) {
		t.Fatalf("Tail(%d) = %v, %v; want 4 5", cursor, completed(snaps), err)
	}
	snaps, last, err := db.Tail(next)
	if err != nil || len(snaps) != 0 || last != next {
		t.Errorf("Tail(%d) with nothing new = %v, %d, %v; want nothing, cursor %d", next, completed(snaps), last, err, next)
	}
}

func TestWindowOnlyLastHour(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")

	ids := appendAll(t, db, token,
		tick(base.Add(-2*time.Hour), 1),
		tick(base.Add(-61*time.Minute), 2),
		tick(base.Add(-59*time.Minute), 3),
		tick(base.Add(-time.Minute), 4),
	)
	snaps, cursor, err := db.Window(base.Add(-time.Hour))
	if err != nil || !reflect.DeepEqual(completed(snaps), []uint64{3, 4}) || cursor != ids[3] {
		t.Errorf("Window(1h) = %v, cursor %d, %v; want 3 4, cursor %d", completed(snaps), cursor, err, ids[3])
	}
	// Nothing in the window: the cursor still says where the file ends,
	// so a Tail from it sees only what comes next.
	snaps, cursor, err = db.Window(base)
	if err != nil || len(snaps) != 0 || cursor != ids[3] {
		t.Errorf("Window(now) = %v, cursor %d, %v; want nothing, cursor %d", completed(snaps), cursor, err, ids[3])
	}
}

// usedBytes is the bytes of the pages of db's file in use.
func usedBytes(t *testing.T, db *ServerDB) int64 {
	t.Helper()
	n, err := db.used()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// count is the number of snapshots in db's file.
func count(db *ServerDB) (int64, error) {
	var n int64
	err := db.db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&n)
	return n, err
}

func TestPruneRetentionAndCap(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "big")
	other := newWriter(t, dir, "small")

	appendAll(t, other, token, tick(base.Add(-time.Hour), 1))
	appendAll(t, db, token,
		tick(base.Add(-25*time.Hour), 1),
		tick(base.Add(-Retention-time.Millisecond), 2),
		tick(base.Add(-Retention+time.Second), 3),
		tick(base, 4),
	)
	if err := db.Prune(base); err != nil {
		t.Fatal(err)
	}
	if snaps, _, err := db.Window(time.Time{}); err != nil || !reflect.DeepEqual(completed(snaps), []uint64{3, 4}) {
		t.Errorf("after Prune: %v, %v; want the 2 rows under %v old", completed(snaps), err, Retention)
	}

	// The cap: a full snapshot per row, so that rows take pages of their
	// own; batches small enough to stop short of emptying the file.
	for i := range 200 {
		s := fullSnapshot(false)
		s.At = base.Add(time.Duration(i) * time.Second)
		if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := count(db)
	if err != nil {
		t.Fatal(err)
	}
	db.capBytes, db.batch = usedBytes(t, db)/2, 10
	if err := db.Prune(base); err != nil {
		t.Fatal(err)
	}
	after, err := count(db)
	if err != nil {
		t.Fatal(err)
	}
	if used := usedBytes(t, db); used > db.capBytes {
		t.Errorf("after Prune: %d bytes used, over the cap of %d", used, db.capBytes)
	}
	if after == 0 || after >= before {
		t.Errorf("after Prune: %d of %d rows left, want some deleted and some kept", after, before)
	}
	latest, err := db.Latest()
	if err != nil || latest == nil || !latest.At.Equal(base.Add(199*time.Second)) {
		t.Errorf("after Prune: latest = %v, %v; want the newest row kept", latest, err)
	}
	snaps, _, err := db.Window(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(snaps); i++ {
		if !snaps[i].At.Equal(snaps[i-1].At.Add(time.Second)) {
			t.Fatalf("after Prune: rows kept are not the newest run: %v then %v", snaps[i-1].At, snaps[i].At)
		}
	}
	// Another server's file is its own: untouched.
	if s, err := other.Latest(); err != nil || s == nil {
		t.Errorf("other server after Prune: %v, %v; want its row", s, err)
	}
}

func TestAppendReaderRefused(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	newWriter(t, dir, "prod")
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Append(token, tick(base, 1)); err == nil {
		t.Error("Append on a read-only ServerDB succeeded")
	}
}

func TestOpenReaderMissingFile(t *testing.T) {
	_, err := OpenServer(t.TempDir(), "nope", false)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenServer(reader) on a missing file = %v, want os.ErrNotExist", err)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")

	if _, found, err := db.Labels(); found || err != nil {
		t.Errorf("Labels of a new file = found %v, %v; want none", found, err)
	}
	labels := fleet.LabelState{
		Numbers:  map[string]fleet.LabelNumber{"inst-a": {N: 1, Seen: at(base.Truncate(time.Minute).UnixMilli())}, "http://x": {N: 2, Seen: at(base.UnixMilli() - 60_000)}},
		Followed: map[string]string{"http://10.0.0.1": "inst-a", "#inst-b": "http://x"},
	}
	tk := tick(base, 1)
	tk.Labels = &labels
	appendAll(t, db, token, tk, tick(base.Add(time.Second), 2)) // the second tick leaves labels alone
	got, found, err := db.Labels()
	if err != nil || !found || !reflect.DeepEqual(got, labels) {
		t.Errorf("Labels = %+v, %v, %v; want %+v", got, found, err, labels)
	}
}

// setVersion stamps the file at path with another store version, as an
// older vartop would have left it.
func setVersion(t *testing.T, path string, v int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE meta SET v = ? WHERE k = 'version'`, v); err != nil {
		t.Fatal(err)
	}
}

func TestVersionResetKeepsLabels(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	labels := fleet.LabelState{
		Numbers:  map[string]fleet.LabelNumber{"inst-a": {N: 3, Seen: at(base.Truncate(time.Minute).UnixMilli())}},
		Followed: map[string]string{"http://a": "inst-a"},
	}
	tk := tick(base, 1)
	tk.Labels = &labels
	appendAll(t, db, token, tk, tick(base.Add(time.Second), 2))
	_ = db.Close()

	setVersion(t, PathFor(dir, "prod"), StoreVersion+1)
	db = newWriter(t, dir, "prod")
	if s, err := db.Latest(); s != nil || err != nil {
		t.Errorf("after a version change: Latest = %v, %v; want the snapshots gone", s, err)
	}
	if got, found, err := db.Labels(); err != nil || !found || !reflect.DeepEqual(got, labels) {
		t.Errorf("after a version change: Labels = %+v, %v, %v; want kept %+v", got, found, err, labels)
	}
	if id := appendAll(t, db, token, tick(base.Add(2*time.Second), 3)); id[0] <= 0 {
		t.Errorf("Append after reset: id %d", id[0])
	}
}

// corrupt overwrites the file at path with bytes that are no SQLite file.
func corrupt(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("not a database ", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptFileRecovered(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	for _, key := range []string{"bad", "good"} {
		db, err := OpenServer(dir, key, true)
		if err != nil {
			t.Fatal(err)
		}
		appendAll(t, db, token, tick(base, 1))
		_ = db.Close()
	}
	bad := PathFor(dir, "bad")
	corrupt(t, bad)

	db := newWriter(t, dir, "bad")
	if s, err := db.Latest(); s != nil || err != nil {
		t.Errorf("recovered file: Latest = %v, %v; want a fresh, empty file", s, err)
	}
	appendAll(t, db, token, tick(base.Add(time.Second), 2))
	aside, _ := filepath.Glob(bad + ".corrupt-*")
	var kept bool
	for _, p := range aside {
		if b, err := os.ReadFile(p); err == nil && strings.HasPrefix(string(b), "not a database") {
			kept = true
		}
	}
	if !kept {
		t.Errorf("corrupt file not renamed aside: %v", aside)
	}

	good := newWriter(t, dir, "good")
	if s, err := good.Latest(); err != nil || s == nil || s.Pods[0].Workers["w"].Metrics.Completed != 1 {
		t.Errorf("other server after recovery: %v, %v; want its row untouched", s, err)
	}
	if others, _ := filepath.Glob(PathFor(dir, "good") + ".corrupt-*"); len(others) != 0 {
		t.Errorf("other server's file renamed: %v", others)
	}
}

// TestSetAside: a file set aside on demand is kept beside, and the next
// writer starts afresh.
func TestSetAside(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, tick(base, 1))
	_ = db.Close()
	if err := SetAside(dir, "prod"); err != nil {
		t.Fatal(err)
	}
	if aside, _ := filepath.Glob(PathFor(dir, "prod") + ".corrupt-*"); len(aside) != 1 {
		t.Errorf("set aside as %v, want one file", aside)
	}
	if s, err := newWriter(t, dir, "prod").Latest(); s != nil || err != nil {
		t.Errorf("after SetAside: Latest = %v, %v; want a fresh, empty file", s, err)
	}
}

func TestRemoveServerFile(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db, err := OpenServer(dir, "gone", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, tick(base, 1))
	_ = db.Close()
	if err := RemoveServerFile(dir, "gone"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(PathFor(dir, "gone") + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there after RemoveServerFile: %v", suffix, err)
		}
	}
	if err := RemoveServerFile(dir, "gone"); err != nil {
		t.Errorf("RemoveServerFile of a removed file = %v, want nil", err)
	}
}

// TestOpenWriterLeavesNoFileOnFailure: a writer that cannot open (here: no
// control.db to check the lease in) removes the file it created, so no
// reader finds an empty one; a file that was there stays as it was.
func TestOpenWriterLeavesNoFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	if db, err := OpenServer(dir, "prod", true); err == nil {
		_ = db.Close()
		t.Fatal("OpenServer(writer) with no control.db succeeded")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(PathFor(dir, "prod") + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s left behind: %v", suffix, err)
		}
	}

	ctl, err := OpenControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := ctl.Claim(Lease{PID: 1}, time.Now())
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, tick(base, 1))
	_ = db.Close()
	_ = ctl.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dir, "control.db") + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Skipf("control.db held open (Windows): %v", err)
		}
	}
	if db, err := OpenServer(dir, "prod", true); err == nil {
		_ = db.Close()
		t.Fatal("OpenServer(writer) with control.db removed succeeded")
	}
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		t.Fatalf("the file that was there: %v", err)
	}
	defer r.Close()
	if s, err := r.Latest(); err != nil || s == nil {
		t.Errorf("the file that was there: Latest = %v, %v; want its row", s, err)
	}
}

func TestIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := PathFor(dir, "bad")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt(t, path)
	_, err := OpenServer(dir, "bad", false)
	if !IsCorrupt(err) {
		t.Errorf("IsCorrupt(%v) = false, want true", err)
	}
	for _, err := range []error{nil, errors.New("file is not a database"), ErrLeaseLost} {
		if IsCorrupt(err) {
			t.Errorf("IsCorrupt(%v) = true, want false", err)
		}
	}
}

// TestRemoveStale: the files under servers/ of no key given, last written
// before the cutoff, go with their -wal and -shm (a reader's -shm write is
// no write); the files of keys given and recent ones stay.
func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	if removed, err := RemoveStale(dir, nil, time.Now()); err != nil || len(removed) != 0 {
		t.Errorf("RemoveStale with no servers/ = %v, %v; want nothing, nil", removed, err)
	}
	_, token := claimed(t, dir)
	for _, key := range []string{"kept", "gone", "recent"} {
		db, err := OpenServer(dir, key, true)
		if err != nil {
			t.Fatal(err)
		}
		appendAll(t, db, token, tick(base, 1))
		_ = db.Close()
	}
	gone, aside := PathFor(dir, "gone"), PathFor(dir, "gone")+".corrupt-1"
	for _, p := range []string{gone + "-wal", gone + "-shm", aside} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-25 * time.Hour)
	for _, p := range []string{PathFor(dir, "kept"), gone, gone + "-wal", aside} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := RemoveStale(dir, []string{"kept"}, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{gone, aside}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for _, p := range []string{gone, gone + "-wal", gone + "-shm", aside} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	for _, key := range []string{"kept", "recent"} {
		if _, err := os.Stat(PathFor(dir, key)); err != nil {
			t.Errorf("%s's file: %v, want it kept", key, err)
		}
	}
}

func TestFilesPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits on Windows")
	}
	dir := filepath.Join(t.TempDir(), "data")
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	appendAll(t, db, token, tick(base, 1))
	for _, p := range []string{filepath.Join(dir, "control.db"), PathFor(dir, "prod")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode %v, want no group/other access", p, perm)
		}
	}
	for _, d := range []string{dir, filepath.Join(dir, "servers")} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode %v, want 0700", d, perm)
		}
	}
}

// TestServerFileReplaced: a writer notices its file removed, or another
// put in its place.
func TestServerFileReplaced(t *testing.T) {
	dir := t.TempDir()
	openControl(t, dir)
	db := newWriter(t, dir, "prod")
	if gone, err := db.Replaced(); gone || err != nil {
		t.Fatalf("Replaced = %v, %v; want false", gone, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "servers")); err != nil {
		t.Fatal(err)
	}
	if gone, err := db.Replaced(); !gone || err != nil {
		t.Errorf("Replaced after servers/ was removed = %v, %v; want true", gone, err)
	}
	newWriter(t, dir, "prod") // made anew
	if gone, err := db.Replaced(); !gone || err != nil {
		t.Errorf("Replaced once made anew = %v, %v; want true", gone, err)
	}
}

// corruptRoot overwrites the root page of table in the SQLite file at path
// with garbage, leaving its header and schema as they were: the file
// opens, and reading table fails.
func corruptRoot(t *testing.T, path, table string) {
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

// TestControlCorruptionNotServerFile: a damaged control.db makes Append
// fail, but not as a corrupt server file: the collector would set a healthy
// one aside.
func TestControlCorruptionNotServerFile(t *testing.T) {
	dir := t.TempDir()
	ctl, token := claimed(t, dir)
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, tick(base, 1))
	_ = db.Close()
	_ = ctl.Close()
	corruptRoot(t, controlPath(dir), "daemon") // the lease table

	db = newWriter(t, dir, "prod")
	_, err = db.Append(token, tick(base.Add(time.Second), 2))
	if err == nil || IsCorrupt(err) {
		t.Errorf("Append with control.db damaged = %v (corrupt %v); want an error, not the server file's", err, IsCorrupt(err))
	}
	if s, err := db.Latest(); err != nil || s == nil || s.Pods[0].Workers["w"].Metrics.Completed != 1 {
		t.Errorf("server file: Latest = %v, %v; want its row", s, err)
	}
}

// TestCorruptBeginBlamed: a corrupt error from a statement that takes
// control.db and the server file both is the server file's only when the
// server file itself is damaged.
func TestCorruptBeginBlamed(t *testing.T) {
	other := t.TempDir()
	bad := PathFor(other, "bad")
	if err := os.MkdirAll(filepath.Dir(bad), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt(t, bad)
	_, corruptErr := OpenServer(other, "bad", false)
	if !IsCorrupt(corruptErr) {
		t.Fatalf("no corrupt error to blame: %v", corruptErr)
	}

	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	appendAll(t, db, token, tick(base, 1))
	if err := db.blame(corruptErr); err == nil || IsCorrupt(err) {
		t.Errorf("blame with the server file sound = %v (corrupt %v); want it not the file's", err, IsCorrupt(err))
	}
	if err := db.blame(ErrLeaseLost); err != ErrLeaseLost {
		t.Errorf("blame(ErrLeaseLost) = %v, want it as it is", err)
	}
	_ = db.Close()
	corrupt(t, PathFor(dir, "prod"))
	if err := db.blame(corruptErr); !IsCorrupt(err) {
		t.Errorf("blame with the server file damaged = %v; want it corrupt", err)
	}
}

// captureLog has the store log into a buffer until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { SetLogger(nil) })
	return &buf
}

// TestUndecodableRecords: a stored blob that cannot be decoded (bit rot
// SQLite does not see) is an ErrUndecodable for Latest and Labels, and a
// snapshot Window and Tail skip, logged once per file opened.
func TestUndecodableRecords(t *testing.T) {
	log := captureLog(t)
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	lt := tick(base.Add(2*time.Second), 3)
	lt.Labels = &fleet.LabelState{Numbers: map[string]fleet.LabelNumber{"#a": {N: 1}}}
	ids := appendAll(t, db, token, tick(base, 1), tick(base.Add(time.Second), 2), lt)
	if _, err := db.db.Exec(`UPDATE snapshots SET data = x'00ff' WHERE id = ?`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE labels SET state = x'00ff'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Labels(); !errors.Is(err, ErrUndecodable) {
		t.Errorf("Labels = %v, want ErrUndecodable", err)
	}
	if _, err := db.db.Exec(`UPDATE snapshots SET data = x'00ff' WHERE id = ?`, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Latest(); !errors.Is(err, ErrUndecodable) {
		t.Errorf("Latest = %v, want ErrUndecodable", err)
	}

	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	snaps, newest, err := r.Window(base.Add(-time.Hour))
	if err != nil || newest != ids[2] || !reflect.DeepEqual(completed(snaps), []uint64{1}) {
		t.Errorf("Window = %v, %d, %v; want the one sound snapshot, newest %d", completed(snaps), newest, err, ids[2])
	}
	if snaps, _, err := r.Tail(0); err != nil || !reflect.DeepEqual(completed(snaps), []uint64{1}) {
		t.Errorf("Tail = %v, %v; want the one sound snapshot", completed(snaps), err)
	}
	if n := strings.Count(log.String(), "cannot be decoded"); n != 1 {
		t.Errorf("logged %d times, want once per file:\n%s", n, log)
	}
}
