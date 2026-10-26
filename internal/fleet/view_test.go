package fleet

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var viewT0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// atTick is the timestamp of the n-th 5s poll after viewT0.
func atTick(n int) time.Time { return viewT0.Add(time.Duration(n) * 5 * time.Second) }

// stamp is the time prefix every Point.Title starts with.
func stamp(s *Snapshot) string { return s.At.Local().Format("15:04:05") }

// smp builds a sample with the fields the views read.
func smp(status string, conc, busy, idle, pending int) *Sample {
	s := &Sample{}
	s.Status, s.Concurrency, s.NumProcessing, s.NumIdleWorkers, s.NumPending = status, conc, busy, idle, pending
	return s
}

// rated marks s as having a valid rate.
func rated(s *Sample, completed, failed float64) *Sample {
	s.CompletedRate, s.FailedRate, s.RateOK = completed, failed, true
	return s
}

// okPod is a healthy direct unit whose label is its endpoint.
func okPod(endpoint string, workers map[string]*Sample) PodSnapshot {
	return PodSnapshot{Key: endpoint, Via: endpoint, Label: endpoint, Workers: workers}
}

func snap(at time.Time, pods ...PodSnapshot) *Snapshot { return &Snapshot{At: at, Pods: pods} }

func rowByName(t *testing.T, rows []RowView, name string) RowView {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row %q in %v", name, rowNames(rows))
	return RowView{}
}

func rowNames(rows []RowView) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return names
}

// threePodSnap is the merged-aggregation fixture: orders on all three pods,
// billing on p0 and p1 only, exports with one shared raw status everywhere.
func threePodSnap() *Snapshot {
	return snap(atTick(0),
		okPod("p0", map[string]*Sample{
			"orders":  smp("Running", 8, 6, 2, 80),
			"billing": smp("Paused", 2, 0, 2, 5),
			"exports": smp("Running", 4, 1, 3, 0),
		}),
		okPod("p1", map[string]*Sample{
			"orders":  smp("Idle", 8, 0, 8, 140),
			"billing": smp("Running", 3, 1, 2, 9),
			"exports": smp("Running", 4, 2, 2, 7),
		}),
		okPod("p2", map[string]*Sample{
			"orders":  smp("Running", 8, 4, 4, 20),
			"exports": smp("Running", 4, 0, 4, 1),
		}),
	)
}

// TestViewsUseNoun: the words in row labels and tooltips follow the Noun.
func TestViewsUseNoun(t *testing.T) {
	iv := 5 * time.Second
	// orders on three units; the third has no rate yet, so 2 of 3 contribute.
	s := snap(atTick(0),
		okPod("p0", map[string]*Sample{"orders": rated(smp("Running", 4, 0, 4, 3), 2, 0)}),
		okPod("p1", map[string]*Sample{"orders": rated(smp("Running", 4, 0, 4, 5), 4, 0)}),
		okPod("p2", map[string]*Sample{"orders": smp("Running", 4, 0, 4, 9)}),
	)
	for _, n := range []Noun{podNoun, instanceNoun} {
		t.Run(n.One, func(t *testing.T) {
			d := BuildDashboard([]*Snapshot{s}, "", iv, n)
			row := rowByName(t, d.Rows, "orders")
			if want := "4 ×3 " + n.Many; row.Concurrency != want {
				t.Errorf("Concurrency = %q, want %q", row.Concurrency, want)
			}
			rate := stamp(s) + " · 6.0/s · 2/3 " + n.Many
			if got := row.Spark[0].Title; got != rate {
				t.Errorf("worker rate title = %q, want %q", got, rate)
			}
			if got := d.KPIs.Completed[0].Title; got != rate {
				t.Errorf("fleet rate title = %q, want %q", got, rate)
			}
			if got, want := d.KPIs.Failed[0].Title, stamp(s)+" · 0.0/min · 2/3 "+n.Many; got != want {
				t.Errorf("failed title = %q, want %q", got, want)
			}
			if got, want := d.KPIs.Pending[0].Title, stamp(s)+" · 9 pending (max/"+n.One+")"; got != want {
				t.Errorf("pending title = %q, want %q", got, want)
			}
			det, ok := BuildDetail([]*Snapshot{s}, "", "orders", iv, n)
			if !ok || det.Completed1h[0].Title != rate || det.Failed1h[0].Title != stamp(s)+" · 0.0/s · 2/3 "+n.Many {
				t.Errorf("detail titles = %q / %q, want %q / ...0.0/s · 2/3 %s", det.Completed1h[0].Title, det.Failed1h[0].Title, rate, n.Many)
			}
		})
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[string]string{
		"Idle":      "active",
		"Running":   "active",
		"Initiated": "transitional",
		"Pausing":   "transitional",
		"Stopping":  "transitional",
		"Paused":    "paused",
		"Stopped":   "stopped",
		"Weird":     "unknown",
		"":          "unknown",
	}
	for status, want := range cases {
		if got := StatusClass(status); got != want {
			t.Errorf("StatusClass(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestMergedRowAggregation(t *testing.T) {
	d := BuildDashboard([]*Snapshot{threePodSnap()}, "", 5*time.Second, podNoun)

	if got, want := rowNames(d.Rows), []string{"billing", "exports", "orders"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v (sorted by name)", got, want)
	}

	orders := rowByName(t, d.Rows, "orders")
	if orders.Status != "active" || orders.Class != "active" {
		t.Errorf("orders status/class = %q/%q, want active/active (Running+Idle agree on class)", orders.Status, orders.Class)
	}
	if orders.Concurrency != "8 ×3 pods" {
		t.Errorf("orders concurrency = %q", orders.Concurrency)
	}
	if orders.Busy != 10 || orders.Idle != 14 {
		t.Errorf("orders busy/idle = %d/%d, want 10/14 (summed)", orders.Busy, orders.Idle)
	}
	if orders.PendingMax != 140 || orders.AbsentOn != 0 {
		t.Errorf("orders pending/absent = %d/%d, want 140/0", orders.PendingMax, orders.AbsentOn)
	}
	if want := "p0: Running\np1: Idle\np2: Running"; orders.StatusTip != want {
		t.Errorf("orders StatusTip = %q, want %q", orders.StatusTip, want)
	}
	if want := "p0: 80\np1: 140\np2: 20"; orders.PendingTip != want {
		t.Errorf("orders PendingTip = %q, want %q", orders.PendingTip, want)
	}

	billing := rowByName(t, d.Rows, "billing")
	if billing.Status != "mixed" || billing.Class != "mixed" || billing.Concurrency != "mixed 2–3" {
		t.Errorf("billing = %q/%q/%q, want mixed/mixed/mixed 2–3", billing.Status, billing.Class, billing.Concurrency)
	}
	if billing.AbsentOn != 1 {
		t.Errorf("billing AbsentOn = %d, want 1 (healthy p2 lacks it)", billing.AbsentOn)
	}
	if billing.Busy != 1 || billing.PendingMax != 9 {
		t.Errorf("billing busy/pending = %d/%d, want 1/9", billing.Busy, billing.PendingMax)
	}

	exports := rowByName(t, d.Rows, "exports")
	if exports.Status != "Running" || exports.Class != "active" {
		t.Errorf("exports status/class = %q/%q, want raw Running/active when all agree", exports.Status, exports.Class)
	}

	// emails: only p0 reports it; p1 is healthy without it; p2 is errored and
	// must contribute nothing (not even its stale workers).
	emails := snap(atTick(0),
		okPod("p0", map[string]*Sample{"emails": smp("Stopped", 8, 0, 8, 3)}),
		okPod("p1", map[string]*Sample{}),
		PodSnapshot{Key: "p2", Label: "p2", Err: "timeout",
			Workers: map[string]*Sample{"emails": smp("Running", 8, 5, 3, 99)}},
	)
	row := rowByName(t, BuildDashboard([]*Snapshot{emails}, "", 5*time.Second, podNoun).Rows, "emails")
	if row.AbsentOn != 1 {
		t.Errorf("emails AbsentOn = %d, want 1", row.AbsentOn)
	}
	if row.Concurrency != "8" || row.Status != "Stopped" || row.Class != "stopped" || row.Busy != 0 || row.PendingMax != 3 {
		t.Errorf("emails = %+v: errored p2 leaked into the row", row)
	}
}

func TestPodFilterKeepsOnePod(t *testing.T) {
	d := BuildDashboard([]*Snapshot{threePodSnap()}, "p1", 5*time.Second, podNoun)

	if got, want := rowNames(d.Rows), []string{"billing", "exports", "orders"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	orders := rowByName(t, d.Rows, "orders")
	if orders.Busy != 0 || orders.Concurrency != "8" || orders.Status != "Idle" || orders.PendingMax != 140 || orders.AbsentOn != 0 {
		t.Errorf("orders on p1 = %+v", orders)
	}
	if billing := rowByName(t, d.Rows, "billing"); billing.Status != "Running" || billing.Concurrency != "3" {
		t.Errorf("billing on p1 = %+v", billing)
	}

	// p2 has no billing, so the filtered view must not list it.
	d = BuildDashboard([]*Snapshot{threePodSnap()}, "p2", 5*time.Second, podNoun)
	if got, want := rowNames(d.Rows), []string{"exports", "orders"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows for p2 = %v, want %v", got, want)
	}

	if d = BuildDashboard([]*Snapshot{threePodSnap()}, "nope", 5*time.Second, podNoun); len(d.Rows) != 0 {
		t.Errorf("unknown pod filter produced rows %v", rowNames(d.Rows))
	}
}

func TestSeriesGapsAndPartial(t *testing.T) {
	iv := 5 * time.Second
	s1 := snap(atTick(0),
		okPod("p0", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 2, 0)}),
		okPod("p1", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 2, 0)}),
		okPod("p2", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 2, 0)}),
	)
	s2 := snap(atTick(1),
		okPod("p0", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 4, 0)}),
		okPod("p1", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 6, 0)}),
		okPod("p2", map[string]*Sample{"orders": smp("Running", 8, 0, 8, 0)}), // RateOK false
	)
	s3 := snap(atTick(2),
		okPod("p0", map[string]*Sample{"orders": smp("Running", 8, 0, 8, 0)}),
		okPod("p1", map[string]*Sample{"orders": smp("Running", 8, 0, 8, 0)}),
		okPod("p2", map[string]*Sample{"orders": smp("Running", 8, 0, 8, 0)}),
	)
	snaps := []*Snapshot{s1, s2, s3}

	row := rowByName(t, BuildDashboard(snaps, "", iv, podNoun).Rows, "orders")
	if len(row.Spark) != 3 {
		t.Fatalf("spark has %d points, want 3", len(row.Spark))
	}
	p := row.Spark[0]
	if p.V != 6 || p.Gap || p.Partial || p.Title != stamp(s1)+" · 6.0/s · 3/3 pods" {
		t.Errorf("point 0 = %+v", p)
	}
	p = row.Spark[1]
	if p.V != 10 || p.Gap || !p.Partial || p.Title != stamp(s2)+" · 10.0/s · 2/3 pods" {
		t.Errorf("point 1 = %+v, want V 10 partial 2/3", p)
	}
	p = row.Spark[2]
	if p.V != 0 || !p.Gap || p.Partial || p.Title != stamp(s3)+" · no data" {
		t.Errorf("point 2 = %+v, want a gap", p)
	}

	// A worker absent from a snapshot is a gap there, not a zero.
	other := snap(atTick(3), okPod("p0", map[string]*Sample{"billing": smp("Running", 1, 0, 1, 0)}))
	row = rowByName(t, BuildDashboard([]*Snapshot{s1, other}, "", iv, podNoun).Rows, "billing")
	if len(row.Spark) != 2 || !row.Spark[0].Gap || !row.Spark[1].Gap {
		t.Errorf("billing spark = %+v, want two gaps", row.Spark)
	}
}

// Nothing is load-balanced as far as the views go: units that share a Via add
// up like any others.
func TestNoLoadBalancedExclusion(t *testing.T) {
	iv := 5 * time.Second
	balanced := func(key string, completed float64) PodSnapshot {
		p := okPod(key, map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), completed, 0)})
		p.Via, p.Instance, p.Balanced = "http://lb", key[1:], true
		return p
	}
	s := snap(atTick(0), balanced("#a", 4), balanced("#b", 6))
	d := BuildDashboard([]*Snapshot{s}, "", iv, podNoun)
	if p := rowByName(t, d.Rows, "orders").Spark[0]; p.V != 10 || p.Partial || p.Gap || p.Title != stamp(s)+" · 10.0/s · 2/2 pods" {
		t.Errorf("row point = %+v, want solid 10.0/s from 2/2 pods", p)
	}
	if d.KPIs.CompletedPerSec != 10 {
		t.Errorf("KPI completed/s = %v, want 10 (both units counted)", d.KPIs.CompletedPerSec)
	}
	// Each unit stands alone when filtered to.
	if p := rowByName(t, BuildDashboard([]*Snapshot{s}, "#b", iv, podNoun).Rows, "orders").Spark[0]; p.V != 6 || p.Title != stamp(s)+" · 6.0/s · 1/1 pods" {
		t.Errorf("filtered point = %+v, want 6.0/s from 1/1 pods", p)
	}
}

// An errored pod is expected to contribute (its data is unknown), so the
// point is hollow k/N instead of a solid dip.
func TestSeriesPartialCountsErroredPods(t *testing.T) {
	iv := 5 * time.Second
	ws := func(completed float64) map[string]*Sample {
		return map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), completed, 0.5)}
	}
	errored := func(endpoint string) PodSnapshot {
		// Carries a rated sample to prove an errored pod adds nothing to the sum.
		return PodSnapshot{Key: endpoint, Label: endpoint, Err: "timeout", Workers: ws(99)}
	}

	// (a) p2 errored: 2 of 3 expected pods contribute, in the worker series,
	// the detail series and the KPI series alike.
	down := snap(atTick(0), okPod("p0", ws(4)), okPod("p1", ws(6)), errored("p2"))
	d := BuildDashboard([]*Snapshot{down}, "", iv, podNoun)
	check := func(what string, p Point, v float64, title string) {
		t.Helper()
		if p.V != v || !p.Partial || p.Gap || p.Title != title {
			t.Errorf("%s = %+v, want V %v partial %q", what, p, v, title)
		}
	}
	check("row", rowByName(t, d.Rows, "orders").Spark[0], 10, stamp(down)+" · 10.0/s · 2/3 pods")
	check("kpi completed", d.KPIs.Completed[0], 10, stamp(down)+" · 10.0/s · 2/3 pods")
	check("kpi failed", d.KPIs.Failed[0], 60, stamp(down)+" · 60.0/min · 2/3 pods")
	det, _ := BuildDetail([]*Snapshot{down}, "", "orders", iv, podNoun)
	check("detail", det.Completed1h[0], 10, stamp(down)+" · 10.0/s · 2/3 pods")

	// Errored pods count in N even when no healthy pod reports the worker, but
	// with nobody contributing the point is a gap.
	allDown := snap(atTick(0), errored("p0"), okPod("p1", map[string]*Sample{"billing": smp("Running", 1, 0, 1, 0)}))
	if p := rowByName(t, BuildDashboard([]*Snapshot{allDown}, "", iv, podNoun).Rows, "billing").Spark[0]; !p.Gap {
		t.Errorf("billing point = %+v, want a gap (nobody contributed)", p)
	}

	solid := func(what string, p Point, v float64, title string) {
		t.Helper()
		if p.V != v || p.Partial || p.Gap || p.Title != title {
			t.Errorf("%s = %+v, want V %v solid %q", what, p, v, title)
		}
	}

	// (b) a healthy pod lacking the worker is not expected to report it...
	other := map[string]*Sample{"billing": rated(smp("Running", 2, 0, 2, 0), 1, 0)}
	lacking := snap(atTick(0), okPod("p0", ws(4)), okPod("p1", ws(6)), okPod("p2", other))
	d = BuildDashboard([]*Snapshot{lacking}, "", iv, podNoun)
	solid("row for orders", rowByName(t, d.Rows, "orders").Spark[0], 10, stamp(lacking)+" · 10.0/s · 2/2 pods")
	solid("row for billing", rowByName(t, d.Rows, "billing").Spark[0], 1, stamp(lacking)+" · 1.0/s · 1/1 pods")
	// ...while the fleet series expects every healthy pod, and p2 does contribute.
	solid("kpi", d.KPIs.Completed[0], 11, stamp(lacking)+" · 11.0/s · 3/3 pods")
	det, _ = BuildDetail([]*Snapshot{lacking}, "", "orders", iv, podNoun)
	solid("detail", det.Completed1h[0], 10, stamp(lacking)+" · 10.0/s · 2/2 pods")
}

func TestKPIsPendingIsMaxPerPodSummedOverWorkers(t *testing.T) {
	mk := func(n int) *Snapshot {
		return snap(atTick(n),
			okPod("p0", map[string]*Sample{
				"orders":  rated(smp("Running", 8, 6, 2, 80), 4, 0.5),
				"billing": rated(smp("Paused", 2, 0, 2, 24), 1, 0.25),
			}),
			okPod("p1", map[string]*Sample{
				"orders":  rated(smp("Idle", 8, 0, 8, 140), 6, 0.5),
				"billing": rated(smp("Running", 3, 1, 2, 10), 1, 0.25),
			}),
			okPod("p2", map[string]*Sample{
				"orders":  rated(smp("Running", 8, 4, 4, 20), 2, 0.5),
				"billing": rated(smp("Stopped", 2, 0, 2, 0), 1, 0.25),
			}),
		)
	}

	d := BuildDashboard([]*Snapshot{mk(0)}, "", 5*time.Second, podNoun)
	k := d.KPIs
	if k.PendingMax != 164 {
		t.Errorf("PendingMax = %d, want 140+24 = 164 (max per pod, summed over workers)", k.PendingMax)
	}
	if k.CompletedPerSec != 15 {
		t.Errorf("CompletedPerSec = %v, want 15", k.CompletedPerSec)
	}
	if k.FailedPerMin != 135 {
		t.Errorf("FailedPerMin = %v, want 2.25/s*60 = 135", k.FailedPerMin)
	}
	wantCounts := map[string]int{"active": 4, "paused": 1, "stopped": 1}
	if !reflect.DeepEqual(k.StatusCounts, wantCounts) {
		t.Errorf("StatusCounts = %v, want %v (pod×worker instances)", k.StatusCounts, wantCounts)
	}
	if len(k.Completed) != 1 || len(k.Failed) != 1 || len(k.Pending) != 1 {
		t.Fatalf("series lengths = %d/%d/%d, want 1/1/1", len(k.Completed), len(k.Failed), len(k.Pending))
	}
	if want := stamp(mk(0)) + " · 15.0/s · 3/3 pods"; k.Completed[0].Title != want || k.Completed[0].V != 15 {
		t.Errorf("completed point = %+v, want title %q", k.Completed[0], want)
	}
	if want := stamp(mk(0)) + " · 135.0/min · 3/3 pods"; k.Failed[0].Title != want || k.Failed[0].V != 135 {
		t.Errorf("failed point = %+v, want title %q", k.Failed[0], want)
	}
	if want := stamp(mk(0)) + " · 164 pending (max/pod)"; k.Pending[0].Title != want || k.Pending[0].V != 164 || k.Pending[0].Gap {
		t.Errorf("pending point = %+v, want title %q", k.Pending[0], want)
	}

	// Series cover the trendWindow before the newest snapshot, by time: a
	// shorter history gives all of it...
	var snaps []*Snapshot
	for n := range 8 {
		snaps = append(snaps, mk(n))
	}
	d = BuildDashboard(snaps, "", 5*time.Second, podNoun)
	if len(d.KPIs.Completed) != 8 || len(d.KPIs.Pending) != 8 || len(d.Rows[0].Spark) != 8 {
		t.Errorf("series lengths %d/%d/%d, want 8 (the history is shorter than the window)",
			len(d.KPIs.Completed), len(d.KPIs.Pending), len(d.Rows[0].Spark))
	}
	// ...and a longer one is cut 10 minutes before the newest, that point
	// included: polls 2 minutes apart keep 6 of 8.
	var sparse []*Snapshot
	for n := range 8 {
		sparse = append(sparse, mk(24*n))
	}
	d = BuildDashboard(sparse, "", 2*time.Minute, podNoun)
	if len(d.KPIs.Completed) != 6 || len(d.KPIs.Pending) != 6 || len(d.Rows[0].Spark) != 6 {
		t.Fatalf("series lengths %d/%d/%d, want 6", len(d.KPIs.Completed), len(d.KPIs.Pending), len(d.Rows[0].Spark))
	}
	if !strings.HasPrefix(d.KPIs.Completed[0].Title, stamp(sparse[2])) || !strings.HasPrefix(d.KPIs.Completed[5].Title, stamp(sparse[7])) {
		t.Errorf("window is not the newest 10 minutes: first %q last %q", d.KPIs.Completed[0].Title, d.KPIs.Completed[5].Title)
	}
}

// TestNewestByTime: the trend window is a span of time, not a count of
// snapshots, so it holds the same minutes whatever the poll interval was.
func TestNewestByTime(t *testing.T) {
	at := func(d time.Duration) *Snapshot { return &Snapshot{At: viewT0.Add(d)} }
	m := time.Minute
	snaps := []*Snapshot{at(-20 * m), at(-9 * m), at(-5 * m), at(0)}

	if got := newest(snaps, 10*m); !reflect.DeepEqual(got, snaps[1:]) {
		t.Errorf("newest 10m = %v, want the last 3", got)
	}
	if got := newest(snaps, 25*m); !reflect.DeepEqual(got, snaps) {
		t.Errorf("newest 25m = %v, want all 4", got)
	}
	edge := []*Snapshot{at(-11 * m), at(-10 * m), at(0)}
	if got := newest(edge, 10*m); !reflect.DeepEqual(got, edge[1:]) {
		t.Errorf("newest 10m = %v, want the one exactly 10m back kept and the one before it dropped", got)
	}
	if got := newest(snaps, 0); !reflect.DeepEqual(got, snaps[3:]) {
		t.Errorf("newest 0 = %v, want the latest alone (at least one)", got)
	}
	if got := newest(snaps[:1], 10*m); !reflect.DeepEqual(got, snaps[:1]) {
		t.Errorf("newest of one snapshot = %v, want it", got)
	}
	if got := newest(nil, 10*m); len(got) != 0 {
		t.Errorf("newest of none = %v, want none", got)
	}
}

// TestFleetRatesSkipWorkerlessPods: a healthy pod running no workers has no
// rate to give, so it must not make every KPI point partial.
func TestFleetRatesSkipWorkerlessPods(t *testing.T) {
	s := snap(atTick(0),
		okPod("http://a", map[string]*Sample{"w": rated(smp("Running", 1, 0, 0, 0), 2, 0.5)}),
		okPod("http://b", map[string]*Sample{"w": rated(smp("Running", 1, 0, 0, 0), 3, 0.5)}),
		okPod("http://c", map[string]*Sample{}),
	)
	k := BuildDashboard([]*Snapshot{s}, "", 5*time.Second, podNoun).KPIs
	if p := k.Completed[0]; p.Partial || p.Gap || p.Title != stamp(s)+" · 5.0/s · 2/2 pods" {
		t.Errorf("completed point = %+v, want solid 5.0/s from 2/2 pods", p)
	}
	if p := k.Failed[0]; p.Partial || p.Title != stamp(s)+" · 60.0/min · 2/2 pods" {
		t.Errorf("failed point = %+v, want solid 60.0/min from 2/2 pods", p)
	}
}

func TestKPIGapsWithoutRates(t *testing.T) {
	// First snapshot: no sample has a rate yet.
	s := snap(atTick(0), okPod("p0", map[string]*Sample{"orders": smp("Running", 8, 0, 8, 12)}))
	d := BuildDashboard([]*Snapshot{s}, "", 5*time.Second, podNoun)
	if p := d.KPIs.Completed[0]; !p.Gap || p.V != 0 {
		t.Errorf("completed point = %+v, want gap", p)
	}
	if p := d.KPIs.Failed[0]; !p.Gap {
		t.Errorf("failed point = %+v, want gap", p)
	}
	if d.KPIs.CompletedPerSec != 0 || d.KPIs.FailedPerMin != 0 {
		t.Errorf("scalars = %v/%v, want 0/0 without rates", d.KPIs.CompletedPerSec, d.KPIs.FailedPerMin)
	}
	if p := d.KPIs.Pending[0]; p.Gap || p.V != 12 {
		t.Errorf("pending point = %+v, want 12 (pending needs no rate)", p)
	}

	// Every pod errored: pending has nothing to report either.
	down := snap(atTick(1), PodSnapshot{Key: "p0", Label: "p0", Err: "refused"})
	d = BuildDashboard([]*Snapshot{down}, "", 5*time.Second, podNoun)
	if p := d.KPIs.Pending[0]; !p.Gap || p.Title != stamp(down)+" · no data" {
		t.Errorf("pending point on errored fleet = %+v, want gap", p)
	}
}

func TestHealthDots(t *testing.T) {
	bad := func(ep string) PodSnapshot { return PodSnapshot{Key: ep, Err: "down"} }
	cases := []struct {
		name string
		s    *Snapshot
		want HealthView
	}{
		{"all", snap(atTick(0), okPod("p0", nil), okPod("p1", nil), okPod("p2", nil)), HealthView{3, 3, "●"}},
		{"some", snap(atTick(0), okPod("p0", nil), okPod("p1", nil), bad("p2")), HealthView{2, 3, "◐"}},
		{"none", snap(atTick(0), bad("p0"), bad("p1"), bad("p2")), HealthView{0, 3, "○"}},
		{"no pods", snap(atTick(0)), HealthView{0, 0, "○"}},
		{"nil", nil, HealthView{0, 0, "○"}},
	}
	for _, tc := range cases {
		if got := Health(tc.s); got != tc.want {
			t.Errorf("%s: Health = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestBalancerNote(t *testing.T) {
	s := &Snapshot{Behind: map[string]int{
		"http://10.0.0.2:8090/varmq": 2, // deliberately out of order: endpoints are sorted
		"http://10.0.0.1:8090/varmq": 3,
	}}
	if got, want := BalancerNote(s), "3 instances behind 10.0.0.1:8090 · 2 instances behind 10.0.0.2:8090"; got != want {
		t.Errorf("BalancerNote = %q, want %q", got, want)
	}
	// One reads in the singular.
	one := &Snapshot{Behind: map[string]int{"http://10.0.0.3:8090/varmq": 1}}
	if got, want := BalancerNote(one), "1 instance behind 10.0.0.3:8090"; got != want {
		t.Errorf("BalancerNote of one = %q, want %q", got, want)
	}
	mixed := &Snapshot{Behind: map[string]int{"http://lb-b:8090/varmq": 4, "http://lb-a:8090/varmq": 1}}
	if got, want := BalancerNote(mixed), "1 instance behind lb-a:8090 · 4 instances behind lb-b:8090"; got != want {
		t.Errorf("BalancerNote = %q, want %q", got, want)
	}
	for name, s := range map[string]*Snapshot{
		"nil":    nil,
		"no map": snap(atTick(0), okPod("p0", nil)),
		"empty":  {Behind: map[string]int{}},
	} {
		if got := BalancerNote(s); got != "" {
			t.Errorf("%s: BalancerNote = %q, want empty", name, got)
		}
	}
}

func TestTip(t *testing.T) {
	for _, tc := range []struct {
		unit PodSnapshot
		want string
	}{
		{PodSnapshot{Instance: "a", Via: "http://x"}, "a via http://x"},
		{PodSnapshot{Via: "http://x"}, "http://x"},
		{PodSnapshot{Key: "#a"}, "#a"},
		{PodSnapshot{Key: "#a", Instance: "a"}, "#a"}, // an instance without an address says nothing more
	} {
		if got := tc.unit.Tip(); got != tc.want {
			t.Errorf("Tip of %+v = %q, want %q", tc.unit, got, tc.want)
		}
	}
}

// notReached is a healthy balanced unit that lists the workers in unreached
// but got no answer for them, on top of the workers it did answer for.
func notReached(key string, workers map[string]*Sample, unreached ...string) PodSnapshot {
	p := okPod(key, workers)
	p.Via, p.Instance, p.Balanced, p.Unreached = "http://lb", key[1:], true, unreached
	return p
}

// A worker a healthy unit lists but did not answer for is neither reporting
// nor absent on it.
func TestUnreachedIsNotAbsent(t *testing.T) {
	iv := 5 * time.Second
	s := snap(atTick(0),
		okPod("u1", map[string]*Sample{"w": rated(smp("Running", 4, 1, 3, 2), 3, 0)}),
		notReached("#u2", nil, "w"),
	)

	row := rowByName(t, BuildDashboard([]*Snapshot{s}, "", iv, podNoun).Rows, "w")
	if row.AbsentOn != 0 {
		t.Errorf("AbsentOn = %d, want 0 (u2 lists w)", row.AbsentOn)
	}
	if want := "u1: Running\n#u2: not reached"; row.StatusTip != want {
		t.Errorf("StatusTip = %q, want %q", row.StatusTip, want)
	}
	if row.Status != "Running" || row.Concurrency != "4" || row.Busy != 1 || row.PendingMax != 2 {
		t.Errorf("row = %+v, want the values of u1 alone", row)
	}

	g := BuildGrid(s)
	if want := (GridCell{Present: true, Status: "Running", Class: "active", Busy: 1, Concurrency: 4, Pending: 2}); g.Rows[0].Cells[0] != want {
		t.Errorf("u1 cell = %+v, want %+v", g.Rows[0].Cells[0], want)
	}
	if c := g.Rows[0].Cells[1]; !c.Unreached || c.Present {
		t.Errorf("u2 cell = %+v, want Unreached and not Present", c)
	}

	// The rate point counts u2 as expected, not contributing.
	p := WorkerRates([]*Snapshot{s}, "", "w", completedRate, podNoun)[0]
	if p.V != 3 || !p.Partial || p.Gap || p.Title != stamp(s)+" · 3.0/s · 1/2 pods" {
		t.Errorf("rate point = %+v, want partial 3.0/s from 1/2 pods", p)
	}

	// Alone, with nothing contributing, the point is a gap.
	only := snap(atTick(0), notReached("#u2", nil, "w"))
	if p := WorkerRates([]*Snapshot{only}, "", "w", completedRate, podNoun)[0]; !p.Gap || p.Title != stamp(only)+" · no data" {
		t.Errorf("rate point of an unreached-only worker = %+v, want a gap", p)
	}
	// An unreached name on another unit does not make a lacking unit expected.
	lacking := snap(atTick(0), okPod("u1", map[string]*Sample{"w": rated(smp("Running", 4, 1, 3, 2), 3, 0)}), okPod("u3", map[string]*Sample{}))
	if p := WorkerRates([]*Snapshot{lacking}, "", "w", completedRate, podNoun)[0]; p.Partial || p.Title != stamp(lacking)+" · 3.0/s · 1/1 pods" {
		t.Errorf("rate point with a lacking unit = %+v, want solid 1/1", p)
	}
}

// With no unit answering for a worker its row has nothing to merge: it says so
// instead of failing on an empty fold, and the detail page has no such worker.
func TestUnreachedByEveryUnitStillHasARow(t *testing.T) {
	s := snap(atTick(0), notReached("#u1", map[string]*Sample{"other": smp("Idle", 2, 0, 2, 0)}, "w"), notReached("#u2", nil, "w"))
	d := BuildDashboard([]*Snapshot{s}, "", 5*time.Second, podNoun)
	row := rowByName(t, d.Rows, "w")
	if row.Status != "not reached" || row.Class != "unknown" || row.Concurrency != "—" || row.AbsentOn != 0 || !row.Spark[0].Gap ||
		!row.Unknown || row.PendingTip != "" {
		t.Errorf("row = %+v, want a not reached row without values", row)
	}
	if other := rowByName(t, d.Rows, "other"); other.Unknown {
		t.Errorf("other = %+v, want its values known", other)
	}
	if want := "#u1: not reached\n#u2: not reached"; row.StatusTip != want {
		t.Errorf("StatusTip = %q, want %q", row.StatusTip, want)
	}
	if got := rowByName(t, d.Rows, "other"); got.AbsentOn != 1 {
		t.Errorf("other AbsentOn = %d, want 1 (u2 lacks it, u1 answered)", got.AbsentOn)
	}
	if _, ok := BuildDetail([]*Snapshot{s}, "", "w", 5*time.Second, podNoun); ok {
		t.Error("BuildDetail reported a worker no unit answered for")
	}
}

// In the fleet series a unit with unreached workers is expected but contributes
// only its valid rates and is never counted as contributing, so the point is
// hollow; it is a gap only when nothing contributed and no rate was summed.
func TestFleetRatesUnreachedMakesPartial(t *testing.T) {
	iv := 5 * time.Second
	s := snap(atTick(0),
		notReached("#u1", map[string]*Sample{"w": rated(smp("Running", 1, 0, 1, 0), 2, 0.5)}, "x"),
		okPod("u2", map[string]*Sample{"w": rated(smp("Running", 1, 0, 1, 0), 3, 0.5)}),
	)
	k := BuildDashboard([]*Snapshot{s}, "", iv, podNoun).KPIs
	if p := k.Completed[0]; p.V != 5 || p.Gap || !p.Partial || p.Title != stamp(s)+" · 5.0/s · 1/2 pods" {
		t.Errorf("completed point = %+v, want hollow 5.0/s from 1/2 pods", p)
	}
	if p := k.Failed[0]; p.V != 60 || p.Gap || !p.Partial || p.Title != stamp(s)+" · 60.0/min · 1/2 pods" {
		t.Errorf("failed point = %+v, want hollow 60.0/min from 1/2 pods", p)
	}

	// A rate was summed although no unit counts as contributing: still a point.
	one := snap(atTick(0), notReached("#u1", map[string]*Sample{"w": rated(smp("Running", 1, 0, 1, 0), 2, 0)}, "x"))
	if p := fleetRates([]*Snapshot{one}, "", completedRate, 1, "/s", podNoun)[0]; p.Gap || !p.Partial || p.V != 2 || p.Title != stamp(one)+" · 2.0/s · 0/1 pods" {
		t.Errorf("point = %+v, want hollow 2.0/s from 0/1 pods", p)
	}
	// Nothing rated and nothing contributing: a gap.
	none := snap(atTick(0), notReached("#u1", map[string]*Sample{"w": smp("Running", 1, 0, 1, 0)}, "x"), notReached("#u2", nil, "w"))
	if p := fleetRates([]*Snapshot{none}, "", completedRate, 1, "/s", podNoun)[0]; !p.Gap {
		t.Errorf("point = %+v, want a gap", p)
	}
}

func TestBuildGridAlignsCells(t *testing.T) {
	s := snap(atTick(0),
		okPod("p0", map[string]*Sample{
			"orders":  smp("Running", 8, 6, 2, 80),
			"billing": smp("Paused", 2, 0, 2, 5),
		}),
		okPod("p1", map[string]*Sample{
			"orders": smp("Weird", 8, 0, 8, 140),
		}),
		okPod("p2", map[string]*Sample{ // healthy but has no billing
			"orders": smp("Stopping", 4, 1, 3, 7),
		}),
	)
	g := BuildGrid(s)
	if len(g.Pods) != 3 || g.Pods[2] != (PodColumn{Key: "p2", Label: "p2", Tip: "p2"}) {
		t.Fatalf("pods = %+v", g.Pods)
	}
	if len(g.Rows) != 2 || g.Rows[0].Name != "billing" || g.Rows[1].Name != "orders" {
		t.Fatalf("rows = %+v, want billing, orders", g.Rows)
	}
	for _, r := range g.Rows {
		if len(r.Cells) != len(g.Pods) {
			t.Fatalf("row %q has %d cells for %d pods", r.Name, len(r.Cells), len(g.Pods))
		}
	}

	billing := g.Rows[0].Cells
	if want := (GridCell{Present: true, Status: "Paused", Class: "paused", Busy: 0, Concurrency: 2, Pending: 5}); billing[0] != want {
		t.Errorf("billing/p0 = %+v, want %+v", billing[0], want)
	}
	if billing[1].Present || billing[2].Present {
		t.Errorf("billing cells on p1/p2 = %+v/%+v, want absent", billing[1], billing[2])
	}
	orders := g.Rows[1].Cells
	if want := (GridCell{Present: true, Status: "Running", Class: "active", Busy: 6, Concurrency: 8, Pending: 80}); orders[0] != want {
		t.Errorf("orders/p0 = %+v, want %+v", orders[0], want)
	}
	if orders[1].Class != "unknown" || orders[1].Status != "Weird" {
		t.Errorf("orders/p1 = %+v, want Weird/unknown", orders[1])
	}
	if want := (GridCell{Present: true, Status: "Stopping", Class: "transitional", Busy: 1, Concurrency: 4, Pending: 7}); orders[2] != want {
		t.Errorf("orders/p2 = %+v, want %+v", orders[2], want)
	}

	// An errored pod keeps its column (with the error) but shows no cells.
	s = snap(atTick(0),
		okPod("p0", map[string]*Sample{"orders": smp("Running", 8, 6, 2, 80)}),
		PodSnapshot{Key: "p1", Via: "http://b", Instance: "inst-b", Label: "pod-b", Err: "connection refused",
			Workers: map[string]*Sample{"orders": smp("Running", 8, 3, 5, 1)}},
		okPod("p2", map[string]*Sample{"orders": smp("Running", 8, 4, 4, 20)}),
	)
	g = BuildGrid(s)
	if want := (PodColumn{Key: "p1", Label: "pod-b", Err: "connection refused", Tip: "inst-b via http://b"}); g.Pods[1] != want {
		t.Errorf("errored column = %+v, want %+v", g.Pods[1], want)
	}
	cells := g.Rows[0].Cells
	if !cells[0].Present || cells[1].Present || !cells[2].Present {
		t.Errorf("cells = %+v, want present/absent/present", cells)
	}
}

func TestBuildDetail(t *testing.T) {
	counters := func(sm *Sample, submitted, completed, ok, failed uint64) *Sample {
		sm.Metrics.Submitted, sm.Metrics.Completed, sm.Metrics.Successful, sm.Metrics.Failed = submitted, completed, ok, failed
		return sm
	}
	iv := 5 * time.Second

	s := snap(atTick(0),
		okPod("p0", map[string]*Sample{"orders": counters(smp("Running", 8, 0, 8, 0), 10, 8, 6, 2)}),
		okPod("p1", map[string]*Sample{"orders": counters(smp("Idle", 8, 0, 8, 0), 1, 1, 1, 0)}),
		okPod("p2", map[string]*Sample{"billing": smp("Running", 2, 0, 2, 0)}),
	)
	d, ok := BuildDetail([]*Snapshot{s}, "", "orders", iv, podNoun)
	if !ok {
		t.Fatal("orders reported absent")
	}
	if d.Name != "orders" || d.Status != "active" || d.Class != "active" {
		t.Errorf("name/status/class = %q/%q/%q", d.Name, d.Status, d.Class)
	}
	if want := "p0: Running\np1: Idle"; d.StatusTip != want {
		t.Errorf("StatusTip = %q, want %q", d.StatusTip, want)
	}
	if d.Submitted != 11 || d.Completed != 9 || d.Successful != 7 || d.Failed != 2 {
		t.Errorf("counters = %d/%d/%d/%d, want 11/9/7/2 (summed over reporting pods)", d.Submitted, d.Completed, d.Successful, d.Failed)
	}
	if d.SuccessRate != "77.8%" {
		t.Errorf("SuccessRate = %q, want 77.8%% (7/9)", d.SuccessRate)
	}
	if len(d.Pods) != 2 || d.Pods[0].Key != "p0" || d.Pods[1].Key != "p1" {
		t.Errorf("Pods = %+v, want only the pods reporting orders", d.Pods)
	}

	// 6 ok / 2 failed on one pod.
	one := snap(atTick(0), okPod("p0", map[string]*Sample{"orders": counters(smp("Running", 8, 0, 8, 0), 8, 8, 6, 2)}))
	if d, _ = BuildDetail([]*Snapshot{one}, "", "orders", iv, podNoun); d.SuccessRate != "75.0%" {
		t.Errorf("SuccessRate = %q, want 75.0%%", d.SuccessRate)
	}
	// 0/0 has no rate.
	zero := snap(atTick(0), okPod("p0", map[string]*Sample{"orders": smp("Running", 8, 0, 8, 0)}))
	if d, _ = BuildDetail([]*Snapshot{zero}, "", "orders", iv, podNoun); d.SuccessRate != "—" {
		t.Errorf("SuccessRate = %q, want —", d.SuccessRate)
	}

	// Slider bounds: start = max current per-pod concurrency, max = max(64, 2*start).
	for _, tc := range []struct{ start, wantMax int }{{8, 64}, {40, 80}, {32, 64}, {33, 66}} {
		cs := snap(atTick(0),
			okPod("p0", map[string]*Sample{"orders": smp("Running", 1, 0, 1, 0)}),
			okPod("p1", map[string]*Sample{"orders": smp("Running", tc.start, 0, tc.start, 0)}),
		)
		d, _ = BuildDetail([]*Snapshot{cs}, "", "orders", iv, podNoun)
		if d.ConcurrencyStart != tc.start || d.ConcurrencyMax != tc.wantMax {
			t.Errorf("start %d: got start/max %d/%d, want %d/%d", tc.start, d.ConcurrencyStart, d.ConcurrencyMax, tc.start, tc.wantMax)
		}
	}

	// Pod filter narrows counters, status and the pod list.
	d, ok = BuildDetail([]*Snapshot{s}, "p1", "orders", iv, podNoun)
	if !ok || d.Status != "Idle" || d.Submitted != 1 || len(d.Pods) != 1 || d.Pods[0].Key != "p1" {
		t.Errorf("p1 detail = %+v ok=%v", d, ok)
	}

	// Absent: unknown worker, filtered pod lacks it, errored pod, no history.
	if _, ok = BuildDetail([]*Snapshot{s}, "", "ghost", iv, podNoun); ok {
		t.Error("unknown worker reported present")
	}
	if _, ok = BuildDetail([]*Snapshot{s}, "p2", "orders", iv, podNoun); ok {
		t.Error("worker reported present on a pod that lacks it")
	}
	bad := snap(atTick(0), PodSnapshot{Key: "p0", Err: "down",
		Workers: map[string]*Sample{"orders": smp("Running", 8, 0, 8, 0)}})
	if _, ok = BuildDetail([]*Snapshot{bad}, "", "orders", iv, podNoun); ok {
		t.Error("worker reported present on an errored pod")
	}
	if _, ok = BuildDetail(nil, "", "orders", iv, podNoun); ok {
		t.Error("worker reported present with no history")
	}
}

func TestBuildDetailSeriesSpanWholeHistory(t *testing.T) {
	// 200 polls at 5s is 16 minutes: more than the 10 minute dashboard window.
	var snaps []*Snapshot
	for n := range 200 {
		snaps = append(snaps, snap(atTick(n),
			okPod("p0", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 2, 0.5)}),
			okPod("p1", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 0), 3, 0.5)}),
		))
	}
	d, ok := BuildDetail(snaps, "", "orders", 5*time.Second, podNoun)
	if !ok {
		t.Fatal("orders reported absent")
	}
	if len(d.Completed1h) != 200 || len(d.Failed1h) != 200 {
		t.Fatalf("series lengths = %d/%d, want 200 (whole history)", len(d.Completed1h), len(d.Failed1h))
	}
	last := snaps[199]
	if p := d.Completed1h[199]; p.V != 5 || p.Title != stamp(last)+" · 5.0/s · 2/2 pods" {
		t.Errorf("completed point = %+v", p)
	}
	if p := d.Failed1h[199]; p.V != 1 || p.Title != stamp(last)+" · 1.0/s · 2/2 pods" {
		t.Errorf("failed point = %+v (detail failed series is per second)", p)
	}

	if dash := BuildDashboard(snaps, "", 5*time.Second, podNoun); len(dash.Rows[0].Spark) != 121 {
		t.Errorf("dashboard spark = %d points, want 121 (10 min back from the newest, both ends)", len(dash.Rows[0].Spark))
	}
}

func TestEmptyInputs(t *testing.T) {
	for name, snaps := range map[string][]*Snapshot{
		"nil":      nil,
		"empty":    {},
		"no pods":  {snap(atTick(0))},
		"no wkrs":  {snap(atTick(0), okPod("p0", nil), okPod("p1", map[string]*Sample{}))},
		"all down": {snap(atTick(0), PodSnapshot{Key: "p0", Err: "down"})},
	} {
		for _, pod := range []string{"", "p0"} {
			d := BuildDashboard(snaps, pod, 5*time.Second, podNoun)
			if len(d.Rows) != 0 {
				t.Errorf("%s/%q: rows = %v, want none", name, pod, rowNames(d.Rows))
			}
			if d.KPIs.StatusCounts == nil || len(d.KPIs.StatusCounts) != 0 || d.KPIs.PendingMax != 0 {
				t.Errorf("%s/%q: KPIs = %+v", name, pod, d.KPIs)
			}
			if _, ok := BuildDetail(snaps, pod, "orders", 5*time.Second, podNoun); ok {
				t.Errorf("%s/%q: BuildDetail reported a worker", name, pod)
			}
		}
		if len(snaps) > 0 {
			latest := snaps[len(snaps)-1]
			if g := BuildGrid(latest); len(g.Rows) != 0 {
				t.Errorf("%s: grid rows = %+v", name, g.Rows)
			}
			Health(latest)
			BalancerNote(latest)
		}
	}

	if g := BuildGrid(nil); len(g.Pods) != 0 || len(g.Rows) != 0 {
		t.Errorf("BuildGrid(nil) = %+v", g)
	}
	if h := Health(nil); h != (HealthView{Dot: "○"}) {
		t.Errorf("Health(nil) = %+v", h)
	}
	if w := BalancerNote(nil); w != "" {
		t.Errorf("BalancerNote(nil) = %q", w)
	}
}

func TestRowIDIsSafe(t *testing.T) {
	name := "a/b c\"<x>"
	s := snap(atTick(0), okPod("p0", map[string]*Sample{name: smp("Running", 1, 0, 1, 0)}))
	rows := BuildDashboard([]*Snapshot{s}, "", 5*time.Second, podNoun).Rows
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rowNames(rows))
	}
	if rows[0].Name != name {
		t.Errorf("Name = %q, want the raw name", rows[0].Name)
	}
	if !regexp.MustCompile(`^w-[0-9a-f]+$`).MatchString(rows[0].ID) {
		t.Errorf("ID = %q, want ^w-[0-9a-f]+$", rows[0].ID)
	}
	if want := "w-" + fmt.Sprintf("%x", name); rows[0].ID != want {
		t.Errorf("ID = %q, want %q", rows[0].ID, want)
	}
}

// Snapshots and their samples are shared and read concurrently, so no builder
// may write to them.
func TestBuildersDoNotMutateSnapshots(t *testing.T) {
	fixture := func() []*Snapshot {
		unit := okPod("#c", map[string]*Sample{"orders": rated(smp("Weird", 8, 4, 4, 20), 2, 0.5)})
		unit.Via, unit.Instance, unit.Balanced, unit.Unreached = "http://lb", "c", true, []string{"billing"}
		first := snap(atTick(0),
			okPod("p1", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 1), 3, 0)}),
			okPod("p0", map[string]*Sample{"orders": rated(smp("Idle", 4, 0, 4, 2), 1, 1), "billing": smp("Paused", 1, 0, 1, 0)}),
			unit,
			PodSnapshot{Key: "p3", Err: "down"},
		)
		first.Behind = map[string]int{"http://lb": 1}
		return []*Snapshot{
			first,
			snap(atTick(1),
				okPod("p1", map[string]*Sample{"orders": rated(smp("Running", 8, 0, 8, 1), 3, 0)}),
			),
		}
	}
	snaps, pristine := fixture(), fixture()

	BuildDashboard(snaps, "", 5*time.Second, podNoun)
	BuildDashboard(snaps, "p1", 5*time.Second, podNoun)
	BuildGrid(snaps[0])
	BuildDetail(snaps, "", "orders", 5*time.Second, podNoun)
	Health(snaps[0])
	BalancerNote(snaps[0])

	if !reflect.DeepEqual(snaps, pristine) {
		t.Error("a view builder modified its input snapshots")
	}
}

// TestFilterFollowsLabel: a view filtered to a label charts that unit in
// every snapshot, though its key changed between them (its process
// restarted behind a balancer under a new instance ID).
func TestFilterFollowsLabel(t *testing.T) {
	iv := 5 * time.Second
	unit := func(key string, completed float64) PodSnapshot {
		return PodSnapshot{Key: key, Via: "http://lb", Instance: key[1:], Label: "instance-2", Balanced: true,
			Workers: map[string]*Sample{"orders": rated(smp("Running", 1, 0, 1, 0), completed, 0)}}
	}
	other := okPod("http://c", map[string]*Sample{"orders": rated(smp("Running", 1, 0, 1, 0), 100, 0)})
	snaps := []*Snapshot{snap(atTick(0), unit("#old", 3), other), snap(atTick(1), unit("#new", 4), other)}

	pts := WorkerRates(snaps, "instance-2", "orders", completedRate, instanceNoun)
	if len(pts) != 2 || pts[0].Gap || pts[0].V != 3 || pts[1].V != 4 {
		t.Errorf("instance-2's points = %+v, want 3 then 4: the unit's own, across its key change", pts)
	}
	if got := PodsOf(snaps[1], "instance-2"); len(got) != 1 || got[0].Key != "#new" {
		t.Errorf("PodsOf(instance-2) = %+v, want the unit labelled so", got)
	}
	if got := PodsOf(snaps[1], "#new"); got != nil {
		t.Errorf("PodsOf(a key) = %+v, want none: units are named by label", got)
	}
	if _, ok := BuildDetail(snaps, "instance-2", "orders", iv, instanceNoun); !ok {
		t.Error("BuildDetail(instance-2) finds no orders")
	}
}

// TestStale: a server's newest snapshot is stale once two polls were
// missed and 2s more passed, not a moment before; no snapshot is not stale.
func TestStale(t *testing.T) {
	const interval = 5 * time.Second
	latest := &Snapshot{At: viewT0}
	limit := viewT0.Add(2*interval + 2*time.Second)
	for _, c := range []struct {
		latest *Snapshot
		now    time.Time
		want   bool
	}{
		{latest, viewT0, false},
		{latest, limit, false},
		{latest, limit.Add(time.Millisecond), true},
		{latest, limit.Add(time.Hour), true},
		{nil, limit.Add(time.Hour), false},
	} {
		if got := Stale(c.latest, interval, c.now); got != c.want {
			t.Errorf("Stale(at %v, %v) = %v, want %v", c.latest != nil, c.now.Sub(viewT0), got, c.want)
		}
	}
}
