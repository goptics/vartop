package fleet

import (
	"strings"
	"testing"
	"time"
)

// dayT0 is a bucket boundary: every DayBucket test below starts on one.
var dayT0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// orders is a snapshot at at of units each running "orders" at the
// completed/failed rates given, labelled instance-1, instance-2, ….
func orders(at time.Time, rates ...[2]float64) *Snapshot {
	var pods []PodSnapshot
	for i, r := range rates {
		label := "instance-" + string(rune('1'+i))
		pods = append(pods, PodSnapshot{Key: label, Via: label, Label: label,
			Workers: map[string]*Sample{"orders": rated(smp("Running", 4, 1, 3, 0), r[0], r[1])}})
	}
	return snap(at, pods...)
}

// last is the newest point of a day series.
func last(t *testing.T, pts []Point) Point {
	t.Helper()
	if len(pts) == 0 {
		t.Fatal("no points")
	}
	return pts[len(pts)-1]
}

// TestDayAveragesEachBucket: a day point is its bucket's average rate,
// summed over the units, with the bucket's peak in its tooltip and the
// day's peak beside the series.
func TestDayAveragesEachBucket(t *testing.T) {
	d := NewDay("orders")
	for i, r := range []float64{2, 4, 6} {
		d.Add(orders(dayT0.Add(time.Duration(i)*time.Minute), [2]float64{r, r / 2}))
	}
	end := dayT0.Add(2 * time.Minute)

	pts, peak := d.Rates("", false, end, NounFor(nil))
	if p := last(t, pts); p.Gap || p.V != 4 || !p.At.Equal(dayT0) {
		t.Errorf("completed point = %+v, want 4 (the average) at the bucket's start", p)
	}
	if peak != 6 {
		t.Errorf("completed peak = %v, want 6: the highest single poll", peak)
	}
	wantTip := dayT0.Local().Format("15:04") + "–" + dayT0.Add(DayBucket).Local().Format("15:04") + " · avg 4.0/s · peak 6.0/s · 1/1 instances"
	if got := last(t, pts).Title; got != wantTip {
		t.Errorf("tooltip = %q, want %q", got, wantTip)
	}

	pts, peak = d.Rates("", true, end, NounFor(nil))
	if p := last(t, pts); p.V != 2 || peak != 3 {
		t.Errorf("failed point = %v, peak %v; want 2 and 3", p.V, peak)
	}
}

// TestDayMergedAndPerUnit: merged, a point sums the units; for a label,
// it is that unit's alone.
func TestDayMergedAndPerUnit(t *testing.T) {
	d := NewDay("orders")
	d.Add(orders(dayT0, [2]float64{1, 0}, [2]float64{3, 0}))
	pts, _ := d.Rates("", false, dayT0, NounFor(nil))
	if p := last(t, pts); p.V != 4 || !strings.HasSuffix(p.Title, "· 2/2 instances") {
		t.Errorf("merged = %v %q, want 4 over 2/2 instances", p.V, p.Title)
	}
	pts, _ = d.Rates("instance-2", false, dayT0, NounFor(nil))
	if p := last(t, pts); p.V != 3 || !strings.HasSuffix(p.Title, "· 1/1 instances") {
		t.Errorf("instance-2 = %v %q, want 3 over 1/1 instances", p.V, p.Title)
	}
	if pts, peak := d.Rates("instance-9", false, dayT0, NounFor(nil)); !last(t, pts).Gap || peak != 0 {
		t.Errorf("a label never seen = %+v, peak %v; want gaps", last(t, pts), peak)
	}
}

// TestDaySpansTheDay: a series has one point per bucket of the day up to
// the newest, at each bucket's start; a bucket with no poll is a gap.
func TestDaySpansTheDay(t *testing.T) {
	d := NewDay("orders")
	end := dayT0.Add(DayWindow)
	d.Add(orders(dayT0.Add(time.Minute), [2]float64{5, 0}))
	d.Add(orders(end, [2]float64{7, 0}))

	pts, _ := d.Rates("", false, end, NounFor(nil))
	if want := int(DayWindow/DayBucket) + 1; len(pts) != want {
		t.Fatalf("%d points, want %d: one per bucket of the day", len(pts), want)
	}
	if !pts[0].At.Equal(dayT0) || pts[0].V != 5 || pts[0].Gap {
		t.Errorf("first point = %+v, want 5 at %v", pts[0], dayT0)
	}
	if p := pts[1]; !p.Gap || !strings.HasSuffix(p.Title, " · no data") {
		t.Errorf("an empty bucket = %+v, want a gap saying no data", p)
	}
	for i := 1; i < len(pts); i++ {
		if got := pts[i].At.Sub(pts[i-1].At); got != DayBucket {
			t.Fatalf("points %d and %d are %v apart, want %v", i-1, i, got, DayBucket)
		}
	}
}

// TestDayPartialAndNoData: a bucket in which a unit was down at some poll
// is partial, and says how few answered; a poll with no rate at all
// counts toward that, not toward the average.
func TestDayPartialAndNoData(t *testing.T) {
	d := NewDay("orders")
	d.Add(orders(dayT0, [2]float64{2, 0}, [2]float64{2, 0}))
	down := orders(dayT0.Add(time.Minute), [2]float64{6, 0}, [2]float64{0, 0})
	down.Pods[1] = PodSnapshot{Key: "instance-2", Via: "instance-2", Label: "instance-2", Err: "connection refused"}
	d.Add(down)
	none := orders(dayT0.Add(2 * time.Minute))
	none.Pods = []PodSnapshot{{Key: "instance-1", Via: "instance-1", Label: "instance-1", Err: "connection refused"}}
	d.Add(none)

	pts, _ := d.Rates("", false, dayT0, NounFor(nil))
	p := last(t, pts)
	if p.V != 5 || !p.Partial || !strings.HasSuffix(p.Title, "· 0/1 instances") {
		t.Errorf("point = %v partial %v %q; want avg 5 of the polls with a rate, partial, 0/1 instances at the worst poll", p.V, p.Partial, p.Title)
	}
}

// TestDayPrune: buckets that end before the cut are dropped.
func TestDayPrune(t *testing.T) {
	d := NewDay("orders")
	d.Add(orders(dayT0, [2]float64{9, 0}))
	d.Add(orders(dayT0.Add(DayBucket), [2]float64{1, 0}))
	d.Prune(dayT0.Add(DayBucket))

	end := dayT0.Add(DayBucket)
	pts, peak := d.Rates("", false, end, NounFor(nil))
	if peak != 1 {
		t.Errorf("peak after pruning = %v, want 1: the pruned bucket's 9 is gone", peak)
	}
	if p := pts[len(pts)-2]; !p.Gap {
		t.Errorf("pruned bucket = %+v, want a gap", p)
	}
}

// TestDayOtherWorkers: snapshots of other workers add nothing.
func TestDayOtherWorkers(t *testing.T) {
	d := NewDay("emails")
	d.Add(orders(dayT0, [2]float64{3, 1}))
	if pts, peak := d.Rates("", false, dayT0, NounFor(nil)); !last(t, pts).Gap || peak != 0 {
		t.Errorf("emails from orders' snapshots = %+v, peak %v; want a gap", last(t, pts), peak)
	}
}
