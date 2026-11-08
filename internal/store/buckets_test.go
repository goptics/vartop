package store

import (
	"slices"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// bucketT0 is a bucket boundary near base.
var bucketT0 = base.Truncate(fleet.DayBucket)

// ratedTick is a tick at at of two units running worker "w" at completed
// (each), failed 1.
func ratedTick(at time.Time, completed float64) fleet.Tick {
	unit := func(label string) fleet.PodSnapshot {
		sm := &fleet.Sample{At: at, CompletedRate: completed, FailedRate: 1, RateOK: true}
		sm.Name, sm.Status = "w", "running"
		return fleet.PodSnapshot{Key: "http://" + label, Via: "http://" + label, Label: label,
			Workers: map[string]*fleet.Sample{"w": sm}}
	}
	return fleet.Tick{Snap: &fleet.Snapshot{At: at, Pods: []fleet.PodSnapshot{unit("instance-1"), unit("instance-2")}}}
}

// sameDay fails the test unless worker w's buckets read from db since
// since chart, merged and per label, completed and failed, over a day and
// over a week, as want does, up to end.
func sameDay(t *testing.T, db *ServerDB, since time.Time, want *fleet.Day, end time.Time) {
	t.Helper()
	for _, label := range []string{"", "instance-2", "i1", "i2", "i3", "i4"} {
		got, err := db.Day("w", label, since)
		if err != nil {
			t.Fatal(err)
		}
		for _, span := range []time.Duration{fleet.DayWindow, 7 * fleet.DayWindow} {
			for _, failed := range []bool{false, true} {
				g, gp := got.Rates(label, failed, span, end, fleet.NounFor(nil))
				w, wp := want.Rates(label, failed, span, end, fleet.NounFor(nil))
				if gp != wp {
					t.Errorf("label %q span %v failed %v: peak %v, want %v", label, span, failed, gp, wp)
				}
				for i := range w {
					if g[i] != w[i] {
						t.Fatalf("label %q span %v failed %v: point %d = %+v, want %+v", label, span, failed, i, g[i], w[i])
					}
				}
			}
		}
	}
}

// TestAppendStoresBuckets: each Append sums its snapshot into the day's
// 5-minute buckets in the same transaction; Day reads a worker's back,
// charting as the snapshots themselves would, without reading one.
func TestAppendStoresBuckets(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	want := fleet.NewDay("w")
	for i, rate := range []float64{2, 4, 6, 8} {
		tk := ratedTick(bucketT0.Add(time.Duration(i)*2*time.Minute), rate) // two buckets
		appendAll(t, db, token, tk)
		want.Add(tk.Snap)
	}
	sameDay(t, db, bucketT0.Add(-time.Hour), want, bucketT0.Add(6*time.Minute))

	// Only the window since since.
	got, err := db.Day("w", "", bucketT0.Add(fleet.DayBucket))
	if err != nil {
		t.Fatal(err)
	}
	if pts, _ := got.Rates("", false, fleet.DayWindow, bucketT0.Add(6*time.Minute), fleet.NounFor(nil)); !pts[len(pts)-2].Gap || pts[len(pts)-1].Gap {
		t.Errorf("Day since the second bucket = %+v, %+v; want the first bucket left out", pts[len(pts)-2], pts[len(pts)-1])
	}
}

// TestPruneDropsOldBuckets: buckets past the retention go with the
// snapshots.
func TestPruneDropsOldBuckets(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	old := bucketT0.Add(-Retention - time.Hour)
	appendAll(t, db, token, ratedTick(old, 5), ratedTick(bucketT0, 7))
	if err := db.Prune(bucketT0, fleet.DayWindow); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT count(*) FROM buckets WHERE start < ?`, bucketT0.Add(-Retention).Unix()).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d buckets older than the retention, %v; want none", n, err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM buckets WHERE start = ?`, bucketT0.Unix()).Scan(&n); err != nil || n == 0 {
		t.Errorf("the recent bucket's rows = %d, %v; want them kept", n, err)
	}
}

// TestVersionResetDropsBuckets: a reset of the snapshots resets their
// buckets too.
func TestVersionResetDropsBuckets(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, ratedTick(bucketT0, 5))
	_ = db.Close()
	setVersion(t, PathFor(dir, "prod"), StoreVersion+1)
	db = newWriter(t, dir, "prod")
	d, err := db.Day("w", "", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pts, peak := d.Rates("", false, fleet.DayWindow, bucketT0, fleet.NounFor(nil)); !pts[len(pts)-1].Gap || peak != 0 {
		t.Errorf("after a reset: %+v, peak %v; want no buckets", pts[len(pts)-1], peak)
	}
}

// oldFile is a server file as a vartop before the buckets table left it:
// snapshots, no buckets, no fill markers.
func oldFile(t *testing.T, dir, token string, ticks ...fleet.Tick) {
	t.Helper()
	db, err := OpenServer(dir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, db, token, ticks...)
	if _, err := db.db.Exec(`DROP TABLE buckets; DELETE FROM meta WHERE k IN ('fill_to', 'filled')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

// TestFillBuckets: a file whose snapshots were stored before buckets were
// gets them filled, a batch at a time: resumed where it stopped, and never
// summed twice.
func TestFillBuckets(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	var ticks []fleet.Tick
	want := fleet.NewDay("w")
	for i := range 5 {
		tk := ratedTick(bucketT0.Add(time.Duration(i)*time.Minute), float64(i+1))
		ticks = append(ticks, tk)
		want.Add(tk.Snap)
	}
	oldFile(t, dir, token, ticks...)

	db := newWriter(t, dir, "prod")
	live := ratedTick(bucketT0.Add(6*time.Minute), 9) // stored after the upgrade: summed as it is
	appendAll(t, db, token, live)
	want.Add(live.Snap)

	more, err := db.FillBuckets(bucketT0.Add(-time.Hour), 2)
	if err != nil || !more {
		t.Fatalf("first batch = %v, %v; want more to fill", more, err)
	}
	_ = db.Close() // stopped half way
	db = newWriter(t, dir, "prod")
	for range 10 {
		if more, err = db.FillBuckets(bucketT0.Add(-time.Hour), 2); err != nil || !more {
			break
		}
	}
	if err != nil || more {
		t.Fatalf("FillBuckets = %v, %v; want done", more, err)
	}
	sameDay(t, db, bucketT0.Add(-time.Hour), want, bucketT0.Add(6*time.Minute))
	if more, err := db.FillBuckets(bucketT0.Add(-time.Hour), 2); more || err != nil {
		t.Errorf("FillBuckets once done = %v, %v; want nothing to do", more, err)
	}
}

// TestReaderOfAnOldFile: a reader of a file its writer has not given a
// buckets table yet reads an empty day, not an error.
func TestReaderOfAnOldFile(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	oldFile(t, dir, token, ratedTick(bucketT0, 5))
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	d, err := r.Day("w", "", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Day = %v, want an empty day", err)
	}
	if pts, _ := d.Rates("", false, fleet.DayWindow, bucketT0, fleet.NounFor(nil)); !pts[len(pts)-1].Gap {
		t.Errorf("Day of an old file = %+v, want a gap", pts[len(pts)-1])
	}
}

// TestFillLeavesTheLeaseFree: filling takes no lock on control.db, so the
// collector's beat (and a page's poke) never waits on a fill, however long
// it runs: a beat kept waiting past busy_timeout loses the lease's renewal
// and stops the collector.
func TestFillLeavesTheLeaseFree(t *testing.T) {
	dir := t.TempDir()
	ctl, token := claimed(t, dir)
	oldFile(t, dir, token)
	db := newWriter(t, dir, "prod")
	// The snapshots of an older vartop, stored in one go, as they were
	// before the buckets: what fills them is under test, not Append.
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	snaps := fleetTicks(300, 10, 20, 5*time.Second)
	for _, snap := range snaps {
		data, err := encodeSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO snapshots (at, data) VALUES (?, ?)`, snap.At.UnixMilli(), data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (k, v) VALUES ('fill_to', (SELECT max(id) FROM snapshots)), ('filled', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		for {
			more, err := db.FillBuckets(snaps[0].At, 25)
			if err != nil || !more {
				done <- err
				return
			}
		}
	}()
	var slowest time.Duration
	for filling := true; filling; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			filling = false
		case <-time.After(20 * time.Millisecond):
			began := time.Now()
			if err := ctl.Beat(token, time.Now()); err != nil {
				t.Fatal(err)
			}
			slowest = max(slowest, time.Since(began))
		}
	}
	if slowest > 100*time.Millisecond {
		t.Errorf("a beat waited %v on the fill, want it never to wait", slowest)
	}
}

// mixedTicks are polls of worker "w" in one bucket and the next, in the
// states that test what the buckets sum: rates that fall as well as rise,
// a unit down, a unit that lists the worker but did not answer for it, a
// poll with no rate on any unit (the first after a restart), a unit just
// started (3/3 then 3/4, whose worst is the second), and a poll no unit
// answered.
func mixedTicks() []fleet.Tick {
	unit := func(label string, rate float64, ok bool) fleet.PodSnapshot {
		sm := &fleet.Sample{CompletedRate: rate, FailedRate: rate / 4, RateOK: ok}
		sm.Name, sm.Status = "w", "running"
		return fleet.PodSnapshot{Key: "http://" + label, Via: "http://" + label, Label: label, Workers: map[string]*fleet.Sample{"w": sm}}
	}
	down := func(label string) fleet.PodSnapshot {
		return fleet.PodSnapshot{Key: "http://" + label, Via: "http://" + label, Label: label, Err: "connection refused"}
	}
	unreached := func(label string) fleet.PodSnapshot {
		p := unit(label, 0, false)
		delete(p.Workers, "w")
		p.Workers["other"] = &fleet.Sample{}
		p.Unreached = []string{"w"}
		return p
	}
	polls := [][]fleet.PodSnapshot{
		{unit("i1", 6, true), unit("i2", 2, true), unit("i3", 4, true)},
		{unit("i1", 1, true), down("i2"), unit("i3", 9, true)},
		{unit("i1", 3, false), unit("i2", 3, false), unit("i3", 3, false)},
		{unit("i1", 5, true), unreached("i2"), unit("i3", 2, true)},
		{down("i1"), down("i2"), down("i3")},
		{unit("i1", 2, true), unit("i2", 2, true), unit("i3", 2, true)},
		{unit("i1", 2, true), unit("i2", 2, true), unit("i3", 2, true), unit("i4", 0, false)},
		{unit("i1", 7, true), unit("i2", 1, true), unit("i3", 5, true)},
	}
	var ticks []fleet.Tick
	for i, pods := range polls {
		at := bucketT0.Add(time.Duration(i) * time.Minute) // polls 5 to 7 in the next bucket
		for _, p := range pods {
			for _, sm := range p.Workers {
				sm.At = at
			}
		}
		ticks = append(ticks, fleet.Tick{Snap: &fleet.Snapshot{At: at, Pods: pods}})
	}
	return ticks
}

// TestBucketsSumAsDay: what Append sums into the buckets, and what
// FillBuckets sums from stored snapshots, chart exactly as fleet.Day sums
// the same polls: averages, peaks, the worst poll's units, partial marks.
func TestBucketsSumAsDay(t *testing.T) {
	want := fleet.NewDay("w")
	ticks := mixedTicks()
	for _, tk := range ticks {
		want.Add(tk.Snap)
	}
	end := bucketT0.Add(8 * time.Minute)

	t.Run("append", func(t *testing.T) {
		dir := t.TempDir()
		_, token := claimed(t, dir)
		db := newWriter(t, dir, "prod")
		appendAll(t, db, token, ticks...)
		sameDay(t, db, bucketT0.Add(-time.Hour), want, end)
	})
	t.Run("fill", func(t *testing.T) {
		dir := t.TempDir()
		_, token := claimed(t, dir)
		oldFile(t, dir, token, ticks...)
		db := newWriter(t, dir, "prod")
		for {
			more, err := db.FillBuckets(bucketT0.Add(-time.Hour), 3)
			if err != nil {
				t.Fatal(err)
			}
			if !more {
				break
			}
		}
		sameDay(t, db, bucketT0.Add(-time.Hour), want, end)
	})
}

// TestPruneKeepsBucketsForHistory: buckets stay for the server's history,
// past the snapshots' day, and go once older; a history under the day
// keeps the day's.
func TestPruneKeepsBucketsForHistory(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	days := func(n int) time.Time { return bucketT0.Add(-time.Duration(n) * fleet.DayWindow) }
	appendAll(t, db, token, ratedTick(days(8), 1), ratedTick(days(3), 2), ratedTick(bucketT0.Add(-time.Hour), 3), ratedTick(bucketT0, 4))
	starts := func() (out []int64) {
		rows, err := db.db.Query(`SELECT DISTINCT start FROM buckets ORDER BY start`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s int64
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	if err := db.Prune(bucketT0, 7*fleet.DayWindow); err != nil {
		t.Fatal(err)
	}
	want := []int64{days(3).Unix(), bucketT0.Add(-time.Hour).Unix(), bucketT0.Unix()}
	if got := starts(); !slices.Equal(got, want) {
		t.Errorf("after a 7d prune: bucket starts %v, want %v (3 days back kept, 8 gone)", got, want)
	}
	if n, err := count(db); err != nil || n != 2 {
		t.Errorf("%d snapshots, %v; want the 2 of the last day", n, err)
	}
	if err := db.Prune(bucketT0, 0); err != nil {
		t.Fatal(err)
	}
	if got := starts(); !slices.Equal(got, want[1:]) {
		t.Errorf("after a prune with no history: bucket starts %v, want the day's %v", got, want[1:])
	}
}

// TestCapCountsSnapshotsOnly: the size cap is on the snapshots' bytes; a
// long history's buckets, however many, cost no snapshot.
func TestCapCountsSnapshotsOnly(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	appendAll(t, db, token, ratedTick(bucketT0.Add(-time.Minute), 1), ratedTick(bucketT0, 2))
	db.capBytes = 4 * usedBytes(t, db)
	// A month of buckets, many times the cap.
	if _, err := db.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		INSERT INTO buckets SELECT 'w' || (i % 20), ? - (i / 20) * 300, 'instance-' || (i % 7), 1, 1, 0, 1, 0, 1, 1 FROM n`, bucketT0.Unix()-300); err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := db.db.QueryRow(`SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&total); err != nil || total <= db.capBytes {
		t.Fatalf("file %d bytes, %v; want it past the cap of %d for the test to mean anything", total, err, db.capBytes)
	}
	if err := db.Prune(bucketT0, 30*fleet.DayWindow); err != nil {
		t.Fatal(err)
	}
	if n, err := count(db); err != nil || n != 2 {
		t.Errorf("%d snapshots after Prune, %v; want both kept: the buckets are not counted", n, err)
	}
}

// TestDayReadsOneLabel: Day reads the buckets of the label asked for
// alone, merged ("") or of one unit.
func TestDayReadsOneLabel(t *testing.T) {
	dir := t.TempDir()
	_, token := claimed(t, dir)
	db := newWriter(t, dir, "prod")
	appendAll(t, db, token, ratedTick(bucketT0, 5))
	d, err := db.Day("w", "instance-2", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pts, _ := d.Rates("instance-2", false, fleet.DayWindow, bucketT0, fleet.NounFor(nil)); last(pts).V != 5 {
		t.Errorf("instance-2 = %+v, want 5", last(pts))
	}
	if pts, peak := d.Rates("", false, fleet.DayWindow, bucketT0, fleet.NounFor(nil)); !last(pts).Gap || peak != 0 {
		t.Errorf("merged, from a read of instance-2 = %+v, peak %v; want nothing read", last(pts), peak)
	}
}

// last is the newest point of a series.
func last(pts []fleet.Point) fleet.Point { return pts[len(pts)-1] }

// TestPruneKeepsTheFirstStep: a prune keeps every bucket of the first
// step of a chart as long as the history, read at the prune's time or
// later: that point stays whole, not the average of its last buckets.
func TestPruneKeepsTheFirstStep(t *testing.T) {
	for _, days := range []int{7, 30} {
		history := time.Duration(days) * fleet.DayWindow
		dir := t.TempDir()
		claimed(t, dir)
		db := newWriter(t, dir, "prod")
		now := time.Date(2026, 10, 27, 12, 58, 30, 0, time.UTC)
		// A bucket every 5 minutes, back past the history; each its minute of the hour.
		if _, err := db.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
			INSERT INTO buckets SELECT 'w', ? - i * 300, '', 1, ((? - i * 300) % 3600) / 60, 0, 0, 0, 1, 1 FROM n`,
			(days+1)*288, now.Truncate(fleet.DayBucket).Unix(), now.Truncate(fleet.DayBucket).Unix()); err != nil {
			t.Fatal(err)
		}
		first := func() fleet.Point {
			d, err := db.Day("w", "", fleet.ChartStart(history, now))
			if err != nil {
				t.Fatal(err)
			}
			pts, _ := d.Rates("", false, history, now, fleet.NounFor(nil))
			return pts[0]
		}
		before := first()
		if err := db.Prune(now, history); err != nil {
			t.Fatal(err)
		}
		if after := first(); after != before {
			t.Errorf("%dd: first point after a prune %+v, want it as before, %+v", days, after, before)
		}
	}
}
