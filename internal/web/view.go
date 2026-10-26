package web

import (
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file turns snapshots into the view models the templates render. All
// aggregation rules live here so templates stay dumb. Every function is pure:
// snapshots and their samples are shared and read concurrently, so nothing here
// writes to them.
//
// Terms used throughout (a "pod" is a unit, one varmq instance; the pages call
// it by the server's Noun, "pod" on Kubernetes and "instance" elsewhere):
//   - healthy pod: PodSnapshot.Err == "". Errored pods contribute nothing, not
//     even workers they may still carry.
//   - reporting pod (for a worker): healthy and has that worker.
//   - unreached pod (for a worker): healthy and lists the worker but got no
//     answer for it (PodSnapshot.Unreached). It is neither reporting nor
//     absent: it adds no values and is shown as "not reached".
//   - expected pod (for a rate point, "k/N pods"): a pod that should contribute
//     a rate. Errored pods are expected too (their data is unknown, so they
//     count in N and contribute 0), and so are unreached ones, which makes the
//     point partial instead of plotting a solid dip.

const (
	// trendWindow is how far back row and KPI sparklines reach.
	trendWindow = 10 * time.Minute
	// concurrencyFloor is the smallest upper bound of the concurrency slider.
	concurrencyFloor = 64
	// tipSep separates per-pod lines in tooltips (title attributes render \n
	// as a line break).
	tipSep = "\n"
)

// Point is one chart sample.
type Point struct {
	V       float64
	Gap     bool   // no data for this snapshot: draw a break, not zero
	Partial bool   // fewer pods contributed than were expected to: draw hollow
	Title   string // hover text, e.g. "15:04:05 · 42.0/s · 2/3 instances"
}

// HealthView summarises pod health for the header.
type HealthView struct {
	Healthy, Total int
	Dot            string // ● all healthy, ◐ some, ○ none
}

// KPIView is the card row above the dashboard table.
type KPIView struct {
	CompletedPerSec, FailedPerMin float64
	PendingMax                    int            // Σ over workers of max-per-pod pending
	StatusCounts                  map[string]int // status class → pod×worker instances
	Completed, Failed, Pending    []Point        // last 10 min; Failed is per minute
}

// RowView is one worker in the dashboard table.
type RowView struct {
	Name, ID                 string // ID = "w-" + hex(Name): safe as a DOM id
	Status, Class, StatusTip string // StatusTip: one "label: Status" line per pod
	Concurrency              string // "{n} ×{p} {many}", "{n}", "mixed {min}–{max}" or "—"
	Busy, Idle, PendingMax   int
	Unknown                  bool   // no pod answered for the worker: Busy, Idle and PendingMax are not known
	PendingTip               string // one "label: n" line per pod
	AbsentOn                 int    // healthy pods that lack this worker
	Spark                    []Point
}

// DashboardView is screen A.
type DashboardView struct {
	KPIs KPIView
	Rows []RowView // sorted by Name
}

// PodColumn is one pod column of the grid, also the pod list of the detail
// view. Tip is the hover text of Label.
type PodColumn struct{ Key, Label, Err, Tip string }

// GridCell is one worker on one pod. Unreached: the pod lists the worker but
// did not answer for it this poll, which is not the same as lacking it.
type GridCell struct {
	Present, Unreached         bool
	Status, Class              string
	Busy, Concurrency, Pending int
}

// GridRow is one worker across every pod column.
type GridRow struct {
	Name  string
	Cells []GridCell // aligned with GridView.Pods
}

// GridView is screen B.
type GridView struct {
	Pods []PodColumn
	Rows []GridRow // sorted by Name
}

// DetailView is screen C.
type DetailView struct {
	Name, Status, Class, StatusTip           string
	Submitted, Completed, Successful, Failed uint64
	SuccessRate                              string      // "99.5%" or "—"
	ConcurrencyStart, ConcurrencyMax         int         // start = max current per-pod; max = max(64, 2*start)
	Completed1h, Failed1h                    []Point     // per second
	Pods                                     []PodColumn // pods reporting this worker
}

// StatusClass maps a varmq worker status to its colour class. Anything varmq
// might add later is "unknown".
func StatusClass(status string) string {
	switch status {
	case "Idle", "Running":
		return "active"
	case "Initiated", "Pausing", "Stopping":
		return "transitional"
	case "Paused":
		return "paused"
	case "Stopped":
		return "stopped"
	}
	return "unknown"
}

// Health counts healthy pods in the latest snapshot. nil (no poll yet) is
// zero pods.
func Health(latest *Snapshot) HealthView {
	h := HealthView{Dot: "○"}
	if latest == nil {
		return h
	}
	h.Total = len(latest.Pods)
	for _, p := range latest.Pods {
		if p.Err == "" {
			h.Healthy++
		}
	}
	switch {
	case h.Healthy == 0:
	case h.Healthy == h.Total:
		h.Dot = "●"
	default:
		h.Dot = "◐"
	}
	return h
}

// BalancerNote says how many instances answered behind each balancer endpoint
// in the newest poll, endpoints sorted, and is "" when none did. It counts
// varmq instances, so its noun does not follow the server's.
func BalancerNote(latest *Snapshot) string {
	if latest == nil {
		return ""
	}
	var parts []string
	for _, ep := range slices.Sorted(maps.Keys(latest.Behind)) {
		n, word := latest.Behind[ep], "instances"
		if n == 1 {
			word = "instance"
		}
		parts = append(parts, fmt.Sprintf("%d %s behind %s", n, word, hostPort(ep)))
	}
	return strings.Join(parts, " · ")
}

// BuildDashboard aggregates the newest snapshot into one row per worker and the
// KPI cards, with sparklines over the 10 minutes up to the newest of snaps
// (oldest first). pod "" merges every pod; otherwise only the one with that key
// is considered. The interval is not needed: the window is a span of time. n
// words the labels and tooltips.
func BuildDashboard(snaps []*Snapshot, pod string, _ time.Duration, n Noun) DashboardView {
	v := DashboardView{KPIs: KPIView{StatusCounts: map[string]int{}}}
	if len(snaps) == 0 {
		return v
	}
	pods := podsOf(snaps[len(snaps)-1], pod)
	window := newest(snaps, trendWindow)

	for _, name := range workerNames(pods) {
		m := mergeWorker(pods, name)
		v.Rows = append(v.Rows, RowView{
			Name:        name,
			ID:          "w-" + hex.EncodeToString([]byte(name)),
			Status:      m.status,
			Class:       m.class,
			StatusTip:   strings.Join(m.statusTips, tipSep),
			Concurrency: concurrencyLabel(m.concurrency, n),
			Busy:        m.busy,
			Idle:        m.idle,
			PendingMax:  m.pendingMax,
			Unknown:     m.reporting == 0,
			PendingTip:  strings.Join(m.pendingTips, tipSep),
			AbsentOn:    m.absent,
			Spark:       workerRates(window, pod, name, completedRate, n),
		})
	}

	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		for _, sm := range p.Workers {
			v.KPIs.StatusCounts[StatusClass(sm.Status)]++
		}
	}
	k := &v.KPIs
	k.PendingMax, _ = peakPending(pods)
	k.Completed = fleetRates(window, pod, completedRate, 1, "/s", n)
	k.Failed = fleetRates(window, pod, failedRate, 60, "/min", n)
	k.Pending = pendingSeries(window, pod, n)
	k.CompletedPerSec = k.Completed[len(k.Completed)-1].V
	k.FailedPerMin = k.Failed[len(k.Failed)-1].V
	return v
}

// BuildGrid lays the latest snapshot out as workers × pods. Every pod keeps its
// column (errored ones with their error); cells of an errored pod are absent,
// and a worker a pod lists but did not answer for is an unreached cell.
func BuildGrid(latest *Snapshot) GridView {
	if latest == nil {
		return GridView{}
	}
	g := GridView{Pods: make([]PodColumn, len(latest.Pods))}
	for i, p := range latest.Pods {
		g.Pods[i] = PodColumn{Key: p.Key, Label: p.Label, Err: p.Err, Tip: p.Tip()}
	}
	for _, name := range workerNames(latest.Pods) {
		cells := make([]GridCell, len(latest.Pods))
		for i, p := range latest.Pods {
			sm := p.Workers[name]
			if p.Err != "" {
				continue
			}
			if sm == nil {
				cells[i].Unreached = p.unreached(name)
				continue
			}
			cells[i] = GridCell{
				Present:     true,
				Status:      sm.Status,
				Class:       StatusClass(sm.Status),
				Busy:        sm.NumProcessing,
				Concurrency: sm.Concurrency,
				Pending:     sm.NumPending,
			}
		}
		g.Rows = append(g.Rows, GridRow{Name: name, Cells: cells})
	}
	return g
}

// BuildDetail aggregates one worker across the pods reporting it in the newest
// snapshot (counters are summed) and charts its rates over the whole history.
// It reports false when no healthy, selected pod has the worker. The detail
// series always span all of snaps, so the interval is not needed. n words the
// tooltips.
func BuildDetail(snaps []*Snapshot, pod, worker string, _ time.Duration, n Noun) (DetailView, bool) {
	if len(snaps) == 0 {
		return DetailView{}, false
	}
	pods := podsOf(snaps[len(snaps)-1], pod)
	m := mergeWorker(pods, worker)
	if m.reporting == 0 {
		return DetailView{}, false
	}

	d := DetailView{
		Name:             worker,
		Status:           m.status,
		Class:            m.class,
		StatusTip:        strings.Join(m.statusTips, tipSep),
		ConcurrencyStart: slices.Max(m.concurrency),
		Completed1h:      workerRates(snaps, pod, worker, completedRate, n),
		Failed1h:         workerRates(snaps, pod, worker, failedRate, n),
	}
	d.ConcurrencyMax = max(concurrencyFloor, 2*d.ConcurrencyStart)
	for _, p := range pods {
		sm := p.Workers[worker]
		if p.Err != "" || sm == nil {
			continue
		}
		d.Submitted += sm.Metrics.Submitted
		d.Completed += sm.Metrics.Completed
		d.Successful += sm.Metrics.Successful
		d.Failed += sm.Metrics.Failed
		d.Pods = append(d.Pods, PodColumn{Key: p.Key, Label: p.Label, Err: p.Err, Tip: p.Tip()})
	}
	d.SuccessRate = "—"
	if total := d.Successful + d.Failed; total > 0 {
		d.SuccessRate = fmt.Sprintf("%.1f%%", float64(d.Successful)/float64(total)*100)
	}
	return d, true
}

// podsOf returns the pods of s a view covers: all of them, or only the one with
// key pod. The result aliases s.Pods and must not be modified.
func podsOf(s *Snapshot, pod string) []PodSnapshot {
	if s == nil {
		return nil
	}
	if pod == "" {
		return s.Pods
	}
	for i := range s.Pods {
		if s.Pods[i].Key == pod {
			return s.Pods[i : i+1]
		}
	}
	return nil
}

// unreached reports whether p lists the worker but got no answer for it.
func (p PodSnapshot) unreached(name string) bool { return slices.Contains(p.Unreached, name) }

// workerNames is the sorted union of worker names over the healthy pods,
// unreached ones included.
func workerNames(pods []PodSnapshot) []string {
	names := make(map[string]struct{})
	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		for name := range p.Workers {
			names[name] = struct{}{}
		}
		for _, name := range p.Unreached {
			names[name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// merged is one worker folded across the pods that report it.
type merged struct {
	status, class string
	statusTips    []string
	concurrency   []int // per reporting pod
	busy, idle    int   // summed over reporting pods
	pendingMax    int   // max over reporting pods, never summed
	pendingTips   []string
	reporting     int // pods reporting the worker
	absent        int // healthy pods lacking it, not counting unreached ones
	unreached     int // healthy pods listing it without an answer
}

// mergeWorker folds name across the healthy pods. status and class are only
// meaningful when reporting > 0. An unreached pod adds only its tip.
func mergeWorker(pods []PodSnapshot, name string) merged {
	var m merged
	var statuses []string
	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		sm := p.Workers[name]
		switch {
		case sm == nil && p.unreached(name):
			m.unreached++
			m.statusTips = append(m.statusTips, p.Label+": not reached")
			continue
		case sm == nil:
			m.absent++
			continue
		}
		m.reporting++
		statuses = append(statuses, sm.Status)
		m.statusTips = append(m.statusTips, p.Label+": "+sm.Status)
		m.concurrency = append(m.concurrency, sm.Concurrency)
		m.busy += sm.NumProcessing
		m.idle += sm.NumIdleWorkers
		m.pendingMax = max(m.pendingMax, sm.NumPending)
		m.pendingTips = append(m.pendingTips, p.Label+": "+strconv.Itoa(sm.NumPending))
	}
	switch {
	case m.reporting > 0:
		m.status, m.class = mergeStatus(statuses)
	case m.unreached > 0:
		m.status = "not reached"
		m.class = StatusClass(m.status)
	}
	return m
}

// mergeStatus is the raw status when every pod agrees, else the class name when
// the classes agree, else "mixed" (which is also its class).
func mergeStatus(statuses []string) (status, class string) {
	first := statuses[0]
	class = StatusClass(first)
	if !slices.ContainsFunc(statuses, func(s string) bool { return s != first }) {
		return first, class
	}
	if slices.ContainsFunc(statuses, func(s string) bool { return StatusClass(s) != class }) {
		return "mixed", "mixed"
	}
	return class, class
}

// concurrencyLabel shows the per-pod concurrency, never a sum, and "—" when no
// pod has any.
func concurrencyLabel(perPod []int, n Noun) string {
	if len(perPod) == 0 {
		return "—"
	}
	lo, hi := slices.Min(perPod), slices.Max(perPod)
	switch {
	case lo != hi:
		return fmt.Sprintf("mixed %d–%d", lo, hi)
	case len(perPod) > 1:
		return fmt.Sprintf("%d ×%d %s", lo, len(perPod), n.Many)
	}
	return strconv.Itoa(lo)
}

// peakPending sums, over workers, the highest per-pod pending count, and
// reports how many healthy pods it looked at.
func peakPending(pods []PodSnapshot) (total, healthy int) {
	peak := make(map[string]int)
	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		healthy++
		for name, sm := range p.Workers {
			peak[name] = max(peak[name], sm.NumPending)
		}
	}
	for _, n := range peak {
		total += n
	}
	return total, healthy
}

func completedRate(s *Sample) float64 { return s.CompletedRate }
func failedRate(s *Sample) float64    { return s.FailedRate }

// newest returns the snapshots taken within span of the latest of snaps
// (oldest first, at least one), however many polls that took.
func newest(snaps []*Snapshot, span time.Duration) []*Snapshot {
	if len(snaps) == 0 {
		return snaps
	}
	since := snaps[len(snaps)-1].At.Add(-span)
	return snaps[slices.IndexFunc(snaps, func(s *Snapshot) bool { return !s.At.Before(since) }):]
}

// workerRates charts one worker's rate (pick) per snapshot: the sum over the
// pods with a valid rate. Expected pods (see the file comment) are the healthy
// ones reporting the worker or listing it unreached, plus every errored one; a
// healthy pod lacking the worker is not expected. With no contributing pod the
// point is a gap.
func workerRates(snaps []*Snapshot, pod, name string, pick func(*Sample) float64, n Noun) []Point {
	out := make([]Point, len(snaps))
	for i, s := range snaps {
		var sum float64
		var contributing, expected int
		for _, p := range podsOf(s, pod) {
			if p.Err != "" {
				expected++
				continue
			}
			sm := p.Workers[name]
			if sm == nil {
				if p.unreached(name) {
					expected++
				}
				continue
			}
			expected++
			if !sm.RateOK {
				continue
			}
			contributing++
			sum += pick(sm)
		}
		if contributing == 0 {
			out[i] = noDataPoint(s)
			continue
		}
		out[i] = ratePoint(s, sum, contributing, expected, 1, "/s", n)
	}
	return out
}

// fleetRates charts a rate summed over every worker per snapshot, scaled by
// scale (60 for /min). Every pod is expected, errored or healthy, except a
// healthy one whose list is empty, which has no rate to give. A pod adds the
// valid rates of its workers but contributes only when it has one and none of
// its workers is unreached, so the point is partial then. It is a gap only when
// nothing contributed and no rate was summed.
func fleetRates(snaps []*Snapshot, pod string, pick func(*Sample) float64, scale float64, unit string, n Noun) []Point {
	out := make([]Point, len(snaps))
	for i, s := range snaps {
		var sum float64
		var contributing, expected int
		var summed bool
		for _, p := range podsOf(s, pod) {
			if p.Err != "" {
				expected++
				continue
			}
			if len(p.Workers) == 0 && len(p.Unreached) == 0 {
				continue
			}
			expected++
			var got bool
			for _, sm := range p.Workers {
				if sm.RateOK {
					sum += pick(sm)
					got = true
				}
			}
			summed = summed || got
			if got && len(p.Unreached) == 0 {
				contributing++
			}
		}
		if contributing == 0 && !summed {
			out[i] = noDataPoint(s)
			continue
		}
		out[i] = ratePoint(s, sum, contributing, expected, scale, unit, n)
	}
	return out
}

// pendingSeries charts the Σ-over-workers of max-per-pod pending per snapshot.
// A snapshot with no healthy pod has no data.
func pendingSeries(snaps []*Snapshot, pod string, n Noun) []Point {
	out := make([]Point, len(snaps))
	for i, s := range snaps {
		total, healthy := peakPending(podsOf(s, pod))
		if healthy == 0 {
			out[i] = noDataPoint(s)
			continue
		}
		out[i] = Point{V: float64(total), Title: fmt.Sprintf("%s · %d pending (max/%s)", pointTime(s), total, n.One)}
	}
	return out
}

// ratePoint builds one rate point from contributing of the expected pods; it
// is partial when fewer contributed than were expected.
func ratePoint(s *Snapshot, sum float64, contributing, expected int, scale float64, unit string, n Noun) Point {
	v := sum * scale
	return Point{
		V:       v,
		Partial: contributing < expected,
		Title:   fmt.Sprintf("%s · %.1f%s · %d/%d %s", pointTime(s), v, unit, contributing, expected, n.Many),
	}
}

// noDataPoint is the gap of a snapshot that has nothing to chart.
func noDataPoint(s *Snapshot) Point {
	return Point{Gap: true, Title: pointTime(s) + " · no data"}
}

// pointTime is the clock time a point's tooltip starts with.
func pointTime(s *Snapshot) string { return s.At.Local().Format("15:04:05") }
