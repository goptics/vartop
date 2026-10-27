package store

import (
	"database/sql"
	"errors"
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

// sameDay fails the test unless got charts, merged and for instance-2,
// completed and failed, as want does, up to end.
func sameDay(t *testing.T, got, want *fleet.Day, end time.Time) {
	t.Helper()
	for _, label := range []string{"", "instance-2"} {
		for _, failed := range []bool{false, true} {
			g, gp := got.Rates(label, failed, end, fleet.NounFor(nil))
			w, wp := want.Rates(label, failed, end, fleet.NounFor(nil))
			if gp != wp {
				t.Errorf("label %q failed %v: peak %v, want %v", label, failed, gp, wp)
			}
			for i := range w {
				if g[i] != w[i] {
					t.Fatalf("label %q failed %v: point %d = %+v, want %+v", label, failed, i, g[i], w[i])
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
	got, err := db.Day("w", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sameDay(t, got, want, bucketT0.Add(6*time.Minute))

	// Only the window since since.
	got, err = db.Day("w", bucketT0.Add(fleet.DayBucket))
	if err != nil {
		t.Fatal(err)
	}
	if pts, _ := got.Rates("", false, bucketT0.Add(6*time.Minute), fleet.NounFor(nil)); !pts[len(pts)-2].Gap || pts[len(pts)-1].Gap {
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
	if err := db.Prune(bucketT0); err != nil {
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
	d, err := db.Day("w", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pts, peak := d.Rates("", false, bucketT0, fleet.NounFor(nil)); !pts[len(pts)-1].Gap || peak != 0 {
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
// gets them filled, a batch at a time, as the collector holding the lease:
// resumed where it stopped, never summed twice, and nothing for one that
// does not hold it.
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

	if _, err := db.FillBuckets("not-the-lease", bucketT0.Add(-time.Hour), 2); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("FillBuckets without the lease = %v, want ErrLeaseLost", err)
	}
	more, err := db.FillBuckets(token, bucketT0.Add(-time.Hour), 2)
	if err != nil || !more {
		t.Fatalf("first batch = %v, %v; want more to fill", more, err)
	}
	_ = db.Close() // stopped half way
	db = newWriter(t, dir, "prod")
	for range 10 {
		if more, err = db.FillBuckets(token, bucketT0.Add(-time.Hour), 2); err != nil || !more {
			break
		}
	}
	if err != nil || more {
		t.Fatalf("FillBuckets = %v, %v; want done", more, err)
	}
	got, err := db.Day("w", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sameDay(t, got, want, bucketT0.Add(6*time.Minute))
	if more, err := db.FillBuckets(token, bucketT0.Add(-time.Hour), 2); more || err != nil {
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
	d, err := r.Day("w", bucketT0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Day = %v, want an empty day", err)
	}
	if pts, _ := d.Rates("", false, bucketT0, fleet.NounFor(nil)); !pts[len(pts)-1].Gap {
		t.Errorf("Day of an old file = %+v, want a gap", pts[len(pts)-1])
	}
}

var _ = sql.ErrNoRows
