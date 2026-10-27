package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

func newFeed(t *testing.T, dataDir, key string) *Feed {
	t.Helper()
	f := NewFeed(dataDir, key, time.Second)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func sync1(t *testing.T, f *Feed) {
	t.Helper()
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestFeedWaitsForFile(t *testing.T) {
	dir := t.TempDir()
	f := newFeed(t, dir, "prod")
	sync1(t, f)
	if f.Latest() != nil || len(f.Snapshots()) != 0 {
		t.Errorf("feed with no file yet: %d snapshots; want none", len(f.Snapshots()))
	}
	if got := f.Interval(); got != time.Second {
		t.Errorf("Interval = %v, want 1s", got)
	}

	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	appendAll(t, db, token, tick(base, 1))
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{1}) {
		t.Errorf("feed once the file exists: %v, want 1", got)
	}
}

func TestFeedWindowThenTail(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	now := time.Now().Round(0)
	appendAll(t, db, token,
		tick(at(now.Add(-2*time.Hour).UnixMilli()), 1), // outside the window
		tick(at(now.Add(-30*time.Minute).UnixMilli()), 2),
		tick(at(now.Add(-time.Minute).UnixMilli()), 3),
	)
	f := newFeed(t, dir, "prod")
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{2, 3}) {
		t.Fatalf("first Sync: %v, want the last hour: 2 3", got)
	}
	appendAll(t, db, token, tick(at(now.UnixMilli()), 4))
	sync1(t, f)
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Errorf("after a Tail: %v, want 2 3 4", got)
	}
	if l := f.Latest(); l == nil || l.Pods[0].Workers["w"].Metrics.Completed != 4 {
		t.Errorf("Latest = %v, want the newest", l)
	}
}

// TestFeedSurvivesReset is Review Focus 4: a reader holds a server file
// while the collector resets it: a version change drops the snapshots (and ids
// start over, so the new rows may reach the feed's cursor before it looks
// again), a corrupt file is renamed aside and recreated, a prune empties
// it, a removed server's file is created anew. The next Sync reloads from
// the new contents: no error, no snapshot twice, no loop.
func TestFeedSurvivesReset(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	path := PathFor(dir, "prod")
	now := time.Now().Round(0)
	n := uint64(0)
	next := func() fleet.Tick {
		n++
		return tick(at(now.Add(time.Duration(n)*time.Second).UnixMilli()), n)
	}

	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	reopen := func() {
		_ = db.Close()
		if db, err = OpenServer(dir, "prod", true); err != nil {
			t.Fatal(err)
		}
	}
	appendAll(t, db, token, next(), next(), next())
	f := newFeed(t, dir, "prod")
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("before any reset: %v", got)
	}

	for _, step := range []struct {
		name   string
		reset  func()
		before int // rows appended after the reset, before the feed syncs
	}{
		{"version change", func() { _ = db.Close(); setVersion(t, path, StoreVersion+1); reopen() }, 5},
		{"corrupt file", func() {
			// Empty the WAL, so that the garbage is what the next open reads.
			if _, err := db.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			corrupt(t, path)
			reopen()
		}, 1},
		{"pruned empty", func() {
			if err := db.Prune(now.Add(48 * time.Hour)); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"removed and re-added", func() {
			_ = db.Close()
			if err := RemoveServerFile(dir, "prod"); err != nil {
				t.Skipf("file held open (Windows): %v", err)
			}
			reopen()
		}, 1},
	} {
		step.reset()
		var want []uint64
		for range step.before {
			appendAll(t, db, token, next())
			want = append(want, n)
		}
		for i := range 3 {
			if err := f.Sync(); err != nil {
				t.Fatalf("%s: Sync %d: %v", step.name, i, err)
			}
		}
		appendAll(t, db, token, next())
		sync1(t, f)
		if got, want := completed(f.Snapshots()), append(want, n); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: feed holds %v, want %v", step.name, got, want)
		}
	}

	// A fresh reader sees what the feed sees: the stale reader's handles,
	// closed on the way, took nothing of the recreated file with them.
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	snaps, _, err := r.Window(time.Time{})
	if err != nil || !reflect.DeepEqual(completed(snaps), completed(f.Snapshots())) {
		t.Errorf("fresh reader: %v, %v; feed: %v", completed(snaps), err, completed(f.Snapshots()))
	}
	_ = db.Close()
}

// TestFeedWaitsForEmptyFile: a file the collector has created but not yet
// initialised (0 bytes) is waited for like a missing one, not an error.
func TestFeedWaitsForEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := PathFor(dir, "prod")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFeed(t, dir, "prod")
	for range 2 {
		sync1(t, f)
	}
	if len(f.Snapshots()) != 0 {
		t.Errorf("feed of an empty file: %d snapshots, want none", len(f.Snapshots()))
	}

	_, token := claimed(t, dir)
	appendAll(t, newWriter(t, dir, "prod"), token, tick(base, 1))
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{1}) {
		t.Errorf("feed once the file is written: %v, want 1", got)
	}
}

// TestFeedWaitsForFileBeingSetUp: a file the collector is creating is a
// database before it has its tables — its first connection writes the
// header — and a reader that opens it then finds nothing yet, not an
// error. Nor does the collector's status.
func TestFeedWaitsForFileBeingSetUp(t *testing.T) {
	dir := t.TempDir()
	path := PathFor(dir, "prod")
	if _, err := prepareFile(path); err != nil {
		t.Fatal(err)
	}
	db := openDB(path, true, "")
	if _, err := db.Exec(`PRAGMA user_version`); err != nil { // connects: the WAL header is written
		t.Fatal(err)
	}
	_ = db.Close()
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("file being set up: %v, size %d; want a header", err, fi.Size())
	}

	f := newFeed(t, dir, "prod")
	for range 2 {
		sync1(t, f)
	}
	if len(f.Snapshots()) != 0 {
		t.Errorf("feed of a file being set up: %d snapshots, want none", len(f.Snapshots()))
	}
	if _, err := OpenServer(dir, "prod", false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenServer of a file being set up = %v, want an error wrapping fs.ErrNotExist", err)
	}

	_, token := claimed(t, dir)
	appendAll(t, newWriter(t, dir, "prod"), token, tick(base, 1))
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{1}) {
		t.Errorf("feed once the file is set up: %v, want 1", got)
	}
}

// TestFeedFileRemoved: a server file deleted under a feed empties it.
func TestFeedFileRemoved(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, tick(base, 1))
	f := newFeed(t, dir, "prod")
	sync1(t, f)
	_ = db.Close()
	if err := RemoveServerFile(dir, "prod"); err != nil {
		t.Skipf("file held open (Windows): %v", err)
	}
	sync1(t, f)
	if len(f.Snapshots()) != 0 || f.Latest() != nil {
		t.Errorf("feed after its file was removed: %d snapshots, want none", len(f.Snapshots()))
	}
}

func TestConcurrentReadersWriter(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	now := time.Now().Round(0)
	const ticks = 60

	feeds := make([]*Feed, 4)
	for i := range feeds {
		feeds[i] = newFeed(t, dir, "prod")
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for _, f := range feeds {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if err := f.Sync(); err != nil {
					t.Errorf("Sync: %v", err)
					return
				}
				_ = f.Snapshots()
				_ = f.Latest()
			}
		})
	}
	// Two web requests may sync one feed at once.
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if err := feeds[0].Sync(); err != nil {
				t.Errorf("Sync: %v", err)
				return
			}
		}
	})
	for i := range ticks {
		if _, err := db.Append(token, tick(at(now.Add(time.Duration(i)*time.Millisecond).UnixMilli()), uint64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()

	want := make([]uint64, ticks)
	for i := range want {
		want[i] = uint64(i + 1)
	}
	for i, f := range feeds {
		sync1(t, f)
		if got := completed(f.Snapshots()); !reflect.DeepEqual(got, want) {
			t.Errorf("feed %d holds %v, want 1..%d once each", i, got, ticks)
		}
	}
}

// TestReaderIsReadOnly: a reader never writes the file, even by mistake.
func TestReaderIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	claimed(t, dir)
	newWriter(t, dir, "prod")
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.db.Exec(`DELETE FROM meta`); err == nil {
		t.Error("a reader deleted rows")
	}
}

// TestFeedTailBounded: a feed left unsynced for hours (hidden) takes in no
// more than the window on its next Sync, not every snapshot since.
func TestFeedTailBounded(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	t0 := time.UnixMilli(time.Now().Add(-3 * time.Hour).UnixMilli())
	n := uint64(0)
	rows := func(from, to time.Time) {
		for at := from; !at.After(to); at = at.Add(time.Minute) {
			n++
			appendAll(t, db, token, tick(at, n))
		}
	}
	rows(t0.Add(-59*time.Minute), t0) // the hour before t0
	f := newFeed(t, dir, "prod")
	clock := t0
	f.now = func() time.Time { return clock }
	sync1(t, f)
	if got := len(f.Snapshots()); got != 60 {
		t.Fatalf("first Sync: %d snapshots, want the hour's 60", got)
	}

	rows(t0.Add(time.Minute), t0.Add(3*time.Hour)) // 3h unsynced
	clock = t0.Add(3 * time.Hour)
	sync1(t, f)
	since := clock.Add(-time.Hour - 3*f.Interval())
	snaps := f.Snapshots()
	for _, s := range snaps {
		if s.At.Before(since) {
			t.Fatalf("held a snapshot of %v, older than the window (since %v); %d held", s.At, since, len(snaps))
		}
	}
	if len(snaps) != 61 || f.Latest().Pods[0].Workers["w"].Metrics.Completed != n {
		t.Errorf("%d snapshots held, want the window's 61 up to the newest", len(snaps))
	}

	// Tailing on from there takes the new rows, but none dated before the
	// window (the clock stepped back while the collector wrote them).
	appendAll(t, db, token, tick(since.Add(-time.Minute), 1000), tick(clock.Add(time.Second), 1001))
	sync1(t, f)
	if got := completed(f.Snapshots()); got[len(got)-1] != 1001 || slices.Contains(got, 1000) {
		t.Errorf("after a tail: %v, want 1001 last and no 1000", got[len(got)-3:])
	}
}

// TestFeedKeepsHistoryWhenIdle: a feed nothing new reaches for longer
// than its window (the collector down) keeps what it holds, for the UIs to
// show stale ("the collector is not running"), not as never collected;
// once rows come again it takes in the window.
func TestFeedKeepsHistoryWhenIdle(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	t0 := time.UnixMilli(time.Now().Add(-3 * time.Hour).UnixMilli())
	appendAll(t, db, token, tick(t0.Add(-time.Minute), 1), tick(t0, 2))
	f := newFeed(t, dir, "prod")
	clock := t0
	f.now = func() time.Time { return clock }
	sync1(t, f)

	clock = t0.Add(2 * time.Hour) // nothing written since t0
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("after 2h with nothing new: %v, want 1 2 kept", got)
	}
	sync1(t, f)
	if l := f.Latest(); l == nil || !l.At.Equal(t0) {
		t.Errorf("Latest = %v, want the snapshot of t0", l)
	}

	appendAll(t, db, token, tick(clock, 3))
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{3}) {
		t.Errorf("once collected again: %v, want the window's 3", got)
	}
}

// TestFeedSeedsWithNewest (R43): a feed loading a window that is empty
// while the file has rows — the collector down for longer than the window —
// holds the file's newest row, so that the UIs show it stale, with its
// age, instead of waiting for a first snapshot. It stays while nothing
// new comes, and the window replaces it once rows do. An undecodable
// newest row seeds nothing and is no error.
func TestFeedSeedsWithNewest(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	t0 := time.UnixMilli(time.Now().Add(-3 * time.Hour).UnixMilli())
	ids := appendAll(t, db, token, tick(t0.Add(-time.Minute), 1), tick(t0, 2))
	f := newFeed(t, dir, "prod")
	clock := t0.Add(2 * time.Hour) // the window, the hour before, holds no row
	f.now = func() time.Time { return clock }

	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{2}) {
		t.Fatalf("a first load of a window with no row: %v, want the file's newest, 2", got)
	}
	sync1(t, f)
	if l := f.Latest(); l == nil || !l.At.Equal(t0) || len(f.Snapshots()) != 1 {
		t.Errorf("nothing new since: Latest %v of %d, want the snapshot of t0 alone", l, len(f.Snapshots()))
	}
	appendAll(t, db, token, tick(clock, 3))
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{3}) {
		t.Errorf("once collected again: %v, want the window's 3", got)
	}

	// A fresh feed whose file's newest row cannot be decoded: nothing to
	// show, and no error.
	if _, err := db.db.Exec(`DELETE FROM snapshots WHERE id > ?`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE snapshots SET data = x'00ff' WHERE id = ?`, ids[1]); err != nil {
		t.Fatal(err)
	}
	g := newFeed(t, dir, "prod")
	g.now = f.now
	sync1(t, g)
	if l := g.Latest(); l != nil {
		t.Errorf("newest row undecodable: Latest = %v, want none", l)
	}
}

// TestFeedSeedAgreesWithCursor (R43): the row a load with an empty window
// holds is the one its Window counted to, not one the collector appended
// right after: the next Sync takes that one in, once.
func TestFeedSeedAgreesWithCursor(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	t0 := time.UnixMilli(time.Now().Add(-3 * time.Hour).UnixMilli())
	appendAll(t, db, token, tick(t0, 1))
	f := newFeed(t, dir, "prod")
	clock := t0.Add(2 * time.Hour) // the window, the hour before, holds no row
	f.now = func() time.Time { return clock }
	f.windowRead = func() {
		f.windowRead = nil
		appendAll(t, db, token, tick(clock, 2)) // the collector, started as the UI loads
	}

	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{1}) {
		t.Errorf("first load: %v, want the row its window counted to, 1", got)
	}
	sync1(t, f)
	if got := completed(f.Snapshots()); !reflect.DeepEqual(got, []uint64{2}) {
		t.Errorf("next Sync: %v, want the row appended meanwhile, once", got)
	}
}

// TestFeedCloseIsFinal: a Sync after Close (one in flight when the feed
// was dropped) opens nothing, and keeps what the feed held.
func TestFeedCloseIsFinal(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	appendAll(t, db, token, tick(base, 1))
	f := newFeed(t, dir, "prod")
	sync1(t, f)
	if f.db == nil || f.Latest() == nil {
		t.Fatalf("after Sync: db %v, Latest %v; want both", f.db, f.Latest())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Errorf("Sync after Close = %v, want nil", err)
	}
	if f.db != nil {
		t.Error("Sync after Close opened the file again")
	}
	if l := f.Latest(); l == nil || l.Pods[0].Workers["w"].Metrics.Completed != 1 {
		t.Errorf("Latest after Close = %v, want the snapshot held", l)
	}
}
