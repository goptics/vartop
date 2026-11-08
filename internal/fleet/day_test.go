package fleet

import (
	"fmt"
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

	pts, peak := d.Rates("", false, DayWindow, end, NounFor(nil))
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

	pts, peak = d.Rates("", true, DayWindow, end, NounFor(nil))
	if p := last(t, pts); p.V != 2 || peak != 3 {
		t.Errorf("failed point = %v, peak %v; want 2 and 3", p.V, peak)
	}
}

// TestDayMergedAndPerUnit: merged, a point sums the units; for a label,
// it is that unit's alone.
func TestDayMergedAndPerUnit(t *testing.T) {
	d := NewDay("orders")
	d.Add(orders(dayT0, [2]float64{1, 0}, [2]float64{3, 0}))
	pts, _ := d.Rates("", false, DayWindow, dayT0, NounFor(nil))
	if p := last(t, pts); p.V != 4 || !strings.HasSuffix(p.Title, "· 2/2 instances") {
		t.Errorf("merged = %v %q, want 4 over 2/2 instances", p.V, p.Title)
	}
	pts, _ = d.Rates("instance-2", false, DayWindow, dayT0, NounFor(nil))
	if p := last(t, pts); p.V != 3 || !strings.HasSuffix(p.Title, "· 1/1 instances") {
		t.Errorf("instance-2 = %v %q, want 3 over 1/1 instances", p.V, p.Title)
	}
	if pts, peak := d.Rates("instance-9", false, DayWindow, dayT0, NounFor(nil)); !last(t, pts).Gap || peak != 0 {
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

	pts, _ := d.Rates("", false, DayWindow, end, NounFor(nil))
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
// is partial, and says how few answered; a poll with no rate counts toward
// that, not toward the average. A poll where no unit answered names no
// worker, and adds nothing.
func TestDayPartialAndNoData(t *testing.T) {
	d := NewDay("orders")
	d.Add(orders(dayT0, [2]float64{2, 0}, [2]float64{2, 0}))
	down := orders(dayT0.Add(time.Minute), [2]float64{6, 0}, [2]float64{0, 0})
	down.Pods[1] = PodSnapshot{Key: "instance-2", Via: "instance-2", Label: "instance-2", Err: "connection refused"}
	d.Add(down)
	starting := orders(dayT0.Add(2*time.Minute), [2]float64{0, 0}, [2]float64{0, 0})
	for _, u := range starting.Pods {
		u.Workers["orders"].RateOK = false // the first poll after a restart: no rate yet
	}
	d.Add(starting)
	none := orders(dayT0.Add(3 * time.Minute))
	none.Pods = []PodSnapshot{{Key: "instance-1", Via: "instance-1", Label: "instance-1", Err: "connection refused"}}
	d.Add(none)

	pts, _ := d.Rates("", false, DayWindow, dayT0, NounFor(nil))
	p := last(t, pts)
	if p.V != 5 || !p.Partial || !strings.HasSuffix(p.Title, "· 0/2 instances") {
		t.Errorf("point = %v partial %v %q; want avg 5 of the polls with a rate, partial, 0/2 instances at the poll with none", p.V, p.Partial, p.Title)
	}
}

// TestDayOtherWorkers: snapshots of other workers add nothing.
func TestDayOtherWorkers(t *testing.T) {
	d := NewDay("emails")
	d.Add(orders(dayT0, [2]float64{3, 1}))
	if pts, peak := d.Rates("", false, DayWindow, dayT0, NounFor(nil)); !last(t, pts).Gap || peak != 0 {
		t.Errorf("emails from orders' snapshots = %+v, peak %v; want a gap", last(t, pts), peak)
	}
}

// TestDayPoints: what one snapshot adds to each worker's day — merged and
// per label, its rates and how many units contributed of those expected —
// and nothing for a view no unit is expected in.
func TestDayPoints(t *testing.T) {
	s := orders(dayT0, [2]float64{1, 0.5}, [2]float64{3, 0})
	s.Pods[1].Workers["emails"] = rated(smp("Running", 1, 0, 1, 0), 2, 0)
	s.Pods = append(s.Pods, PodSnapshot{Key: "instance-3", Via: "instance-3", Label: "instance-3", Err: "connection refused"})

	got := map[string]DayPoint{}
	for _, p := range DayPoints(s) {
		got[p.Worker+"/"+p.Label] = p
	}
	want := map[string]DayPoint{
		"orders/":           {Worker: "orders", Completed: 4, Failed: 0.5, Contributing: 2, Expected: 3},
		"orders/instance-1": {Worker: "orders", Label: "instance-1", Completed: 1, Failed: 0.5, Contributing: 1, Expected: 1},
		"orders/instance-2": {Worker: "orders", Label: "instance-2", Completed: 3, Contributing: 1, Expected: 1},
		"orders/instance-3": {Worker: "orders", Label: "instance-3", Expected: 1},
		"emails/":           {Worker: "emails", Completed: 2, Contributing: 1, Expected: 2},
		"emails/instance-2": {Worker: "emails", Label: "instance-2", Completed: 2, Contributing: 1, Expected: 1},
		"emails/instance-3": {Worker: "emails", Label: "instance-3", Expected: 1},
	}
	if len(got) != len(want) {
		t.Errorf("DayPoints = %+v, want %d points (none for emails on instance-1, which lacks it)", got, len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
}

// TestDayPut: a day loaded from stored bucket stats charts as one summed
// from the snapshots.
func TestDayPut(t *testing.T) {
	summed := NewDay("orders")
	loaded := NewDay("orders")
	for i, r := range []float64{2, 4, 6} {
		summed.Add(orders(dayT0.Add(time.Duration(i)*time.Minute), [2]float64{r, 1}))
	}
	var merged DayStat
	for _, r := range []float64{2, 4, 6} {
		merged.Add(DayPoint{Worker: "orders", Completed: r, Failed: 1, Contributing: 1, Expected: 1})
	}
	loaded.Put(dayT0, "", merged)
	end := dayT0.Add(2 * time.Minute)
	a, pa := summed.Rates("", false, DayWindow, end, NounFor(nil))
	b, pb := loaded.Rates("", false, DayWindow, end, NounFor(nil))
	if last(t, a) != last(t, b) || pa != pb {
		t.Errorf("loaded = %+v (peak %v), summed = %+v (peak %v); want the same", last(t, b), pb, last(t, a), pa)
	}
}

// TestDayWorstPollByMissing: a bucket shows its poll missing the most
// units, whatever the order: one of 3/3 and 3/4 (a fourth unit just
// started, no rate yet) is partial and says 3/4, not 3/3.
func TestDayWorstPollByMissing(t *testing.T) {
	full := orders(dayT0, [2]float64{1, 0}, [2]float64{1, 0}, [2]float64{1, 0})
	growing := orders(dayT0.Add(time.Minute), [2]float64{1, 0}, [2]float64{1, 0}, [2]float64{1, 0}, [2]float64{0, 0})
	growing.Pods[3].Workers["orders"].RateOK = false
	for name, order := range map[string][]*Snapshot{"full first": {full, growing}, "growing first": {growing, full}} {
		d := NewDay("orders")
		for _, s := range order {
			d.Add(s)
		}
		pts, _ := d.Rates("", false, DayWindow, dayT0, NounFor(nil))
		if p := last(t, pts); !p.Partial || !strings.HasSuffix(p.Title, "· 3/4 instances") {
			t.Errorf("%s: %q partial %v, want 3/4 instances, partial", name, p.Title, p.Partial)
		}
	}
}

// TestStepFor: a chart's step is the shortest of 5m, 10m, 15m, 20m, 30m
// and 1h giving it at most 720 points; every history a server may set
// gets one that divides the hour, so steps stay on the clock.
func TestStepFor(t *testing.T) {
	day := 24 * time.Hour
	for span, want := range map[time.Duration]time.Duration{
		day: 5 * time.Minute, 2 * day: 5 * time.Minute, 3 * day: 10 * time.Minute,
		7 * day: 15 * time.Minute, 14 * day: 30 * time.Minute, 30 * day: time.Hour,
	} {
		if got := StepFor(span); got != want {
			t.Errorf("StepFor(%v) = %v, want %v", span, got, want)
		}
	}
	for n := 1; n <= 30; n++ {
		span := time.Duration(n) * day
		if step := StepFor(span); span/step > 720 || time.Hour%step != 0 {
			t.Errorf("%dd: step %v gives %d points, or does not divide the hour", n, step, span/step)
		}
	}
}

// TestDayMergesIntoSteps: a longer chart's point merges the 5-minute
// buckets of its step into what summing the step's polls directly gives —
// average, peak, and the worst poll however the buckets split it (a unit
// down in one bucket, a scale-up in another) — whatever the order merged.
func TestDayMergesIntoSteps(t *testing.T) {
	span := 7 * DayWindow // 15-minute steps
	step := StepFor(span)
	full := func(at time.Time, r float64) *Snapshot { return orders(at, [2]float64{r, 1}, [2]float64{r, 0}) }
	down := orders(dayT0.Add(6*time.Minute), [2]float64{9, 2}, [2]float64{0, 0})
	down.Pods[1] = PodSnapshot{Key: "instance-2", Via: "instance-2", Label: "instance-2", Err: "connection refused"}
	grown := orders(dayT0.Add(11*time.Minute), [2]float64{1, 0}, [2]float64{1, 0}, [2]float64{0, 0})
	grown.Pods[2].Workers["orders"].RateOK = false
	snaps := []*Snapshot{full(dayT0, 2), full(dayT0.Add(time.Minute), 4), down, grown, full(dayT0.Add(14*time.Minute), 6)}

	d := NewDay("orders")
	for _, s := range snaps {
		d.Add(s)
	}
	for _, label := range []string{"", "instance-1", "instance-2"} {
		var want DayStat
		for i := len(snaps) - 1; i >= 0; i-- { // the other order
			for _, p := range DayPoints(snaps[i]) {
				if p.Worker == "orders" && p.Label == label {
					want.Add(p)
				}
			}
		}
		for _, failed := range []bool{false, true} {
			pts, peak := d.Rates(label, failed, span, dayT0, NounFor(nil))
			p := last(t, pts)
			sum, top := want.Completed, want.PeakCompleted
			if failed {
				sum, top = want.Failed, want.PeakFailed
			}
			if !p.At.Equal(dayT0) || p.V != sum/float64(want.Polls) || peak != top || p.Partial != (want.Fewest < want.Of) ||
				!strings.HasSuffix(p.Title, fmt.Sprintf("· %d/%d instances", want.Fewest, want.Of)) {
				t.Errorf("label %q failed %v: %+v (peak %v), want avg %v peak %v of %d/%d over %v",
					label, failed, p, peak, sum/float64(want.Polls), top, want.Fewest, want.Of, step)
			}
		}
	}
}

// TestDayLongerSpan: a longer chart has a point per step of its span, at
// each step's start, and dated tooltips: across days a time alone says
// nothing of which day.
func TestDayLongerSpan(t *testing.T) {
	d := NewDay("orders")
	end := dayT0.Add(30 * DayWindow)
	d.Add(orders(dayT0.Add(10*time.Minute), [2]float64{5, 0}))
	d.Add(orders(end, [2]float64{7, 0}))
	pts, peak := d.Rates("", false, 30*DayWindow, end, NounFor(nil))
	if len(pts) != 721 || peak != 7 {
		t.Fatalf("%d points, peak %v; want 721 hourly points, peak 7", len(pts), peak)
	}
	if p := pts[0]; !p.At.Equal(dayT0) || !p.At.Equal(ChartStart(30*DayWindow, end)) || p.V != 5 {
		t.Errorf("first point = %+v, want 5 at %v, the chart's start", p, dayT0)
	}
	from := dayT0.Local()
	want := from.Format("Mon 2 Jan 15:04") + "–" + from.Add(time.Hour).Format("15:04") + " · avg 5.0/s"
	if !strings.HasPrefix(pts[0].Title, want) {
		t.Errorf("tooltip = %q, want it to start %q", pts[0].Title, want)
	}
	if p := pts[1]; !p.Gap || !strings.HasSuffix(p.Title, " · no data") {
		t.Errorf("an empty step = %+v, want a gap", p)
	}
}

// TestDayStatMerge: merging stats keeps the worse worst poll — a tie on
// units missing going to fewer contributing — in either order, and
// merging an empty stat changes nothing.
func TestDayStatMerge(t *testing.T) {
	twoOfThree := DayStat{Polls: 1, Completed: 2, PeakCompleted: 2, Fewest: 2, Of: 3}
	threeOfFour := DayStat{Polls: 2, Completed: 6, PeakCompleted: 4, Fewest: 3, Of: 4}
	for name, order := range map[string][2]DayStat{"2/3 first": {twoOfThree, threeOfFour}, "3/4 first": {threeOfFour, twoOfThree}} {
		var st DayStat
		st.Merge(order[0])
		st.Merge(order[1])
		if st.Fewest != 2 || st.Of != 3 || st.Polls != 3 || st.Completed != 8 || st.PeakCompleted != 4 {
			t.Errorf("%s: %+v, want 3 polls summing 8, peak 4, worst 2/3", name, st)
		}
	}
	st := DayStat{Polls: 1, Completed: 1, PeakCompleted: 1, Fewest: 2, Of: 2}
	want := st
	st.Merge(DayStat{})
	if st != want {
		t.Errorf("merging an empty stat: %+v, want %+v", st, want)
	}
}
