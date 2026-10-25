package web

import (
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goptics/vartop/internal/config"
)

// This file turns snapshots into the view models the templates render. All
// aggregation rules live here so templates stay dumb. Every function is pure:
// snapshots and their samples are shared and read concurrently, so nothing here
// writes to them.
//
// Terms used throughout:
//   - healthy pod: PodSnapshot.Err == "". Errored pods contribute nothing, not
//     even workers they may still carry.
//   - load-balanced pod: LoadBalanced > 1. One endpoint answered by several
//     instances, so its per-worker rates are meaningless and are excluded.
//   - reporting pod (for a worker): healthy and has that worker.

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
	Partial bool   // fewer pods contributed than reported the worker: draw hollow
	Title   string // hover text, e.g. "15:04:05 · 42.0/s · 2/3 pods"
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
	Concurrency              string // "{n} ×{p} pods", "{n}" or "mixed {min}–{max}"
	Busy, Idle, PendingMax   int
	PendingTip               string // one "label: n" line per pod
	AbsentOn                 int    // healthy pods that lack this worker
	RateNA                   bool   // every pod reporting this worker is load-balanced
	Spark                    []Point
}

// DashboardView is screen A.
type DashboardView struct {
	KPIs KPIView
	Rows []RowView // sorted by Name
}

// PodColumn is one pod column of the grid, also the pod list of the detail view.
type PodColumn struct{ Endpoint, Label, Err string }

// GridCell is one worker on one pod.
type GridCell struct {
	Present                    bool
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

// LBWarning explains, when any pod is a load-balanced endpoint, that each
// refresh shows only one instance. It mirrors the TUI's loadBalancedWarning
// (internal/tui/dashboard.go) and returns "" when no pod is load-balanced.
func LBWarning(latest *Snapshot) string {
	if latest == nil {
		return ""
	}
	lb := make(map[string]int)
	for _, p := range latest.Pods {
		if p.LoadBalanced > 1 {
			lb[p.Endpoint] = p.LoadBalanced
		}
	}
	if len(lb) == 0 {
		return ""
	}
	var parts []string
	for _, ep := range slices.Sorted(maps.Keys(lb)) {
		parts = append(parts, fmt.Sprintf("%s (%d instances seen)", config.ShortOrigin(ep), lb[ep]))
	}
	return fmt.Sprintf("⚠️  Load-balanced endpoint: %s. Each refresh shows one instance; use kubernetes or dns discovery to see all.", strings.Join(parts, ", "))
}

// BuildDashboard aggregates the newest snapshot into one row per worker and the
// KPI cards, with sparklines over the newest 10 minutes of snaps (oldest first).
// pod "" merges every pod; otherwise only that endpoint is considered.
func BuildDashboard(snaps []*Snapshot, pod string, interval time.Duration) DashboardView {
	v := DashboardView{KPIs: KPIView{StatusCounts: map[string]int{}}}
	if len(snaps) == 0 {
		return v
	}
	pods := podsOf(snaps[len(snaps)-1], pod)
	window := newest(snaps, trendWindow, interval)

	for _, name := range workerNames(pods) {
		m := mergeWorker(pods, name)
		v.Rows = append(v.Rows, RowView{
			Name:        name,
			ID:          "w-" + hex.EncodeToString([]byte(name)),
			Status:      m.status,
			Class:       m.class,
			StatusTip:   strings.Join(m.statusTips, tipSep),
			Concurrency: concurrencyLabel(m.concurrency),
			Busy:        m.busy,
			Idle:        m.idle,
			PendingMax:  m.pendingMax,
			PendingTip:  strings.Join(m.pendingTips, tipSep),
			AbsentOn:    m.absent,
			RateNA:      m.allLB,
			Spark:       workerRates(window, pod, name, completedRate),
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
	k.Completed = fleetRates(window, pod, completedRate, 1, "/s")
	k.Failed = fleetRates(window, pod, failedRate, 60, "/min")
	k.Pending = pendingSeries(window, pod)
	k.CompletedPerSec = k.Completed[len(k.Completed)-1].V
	k.FailedPerMin = k.Failed[len(k.Failed)-1].V
	return v
}

// BuildGrid lays the latest snapshot out as workers × pods. Every pod keeps its
// column (errored ones with their error); cells of an errored pod are absent.
func BuildGrid(latest *Snapshot) GridView {
	if latest == nil {
		return GridView{}
	}
	g := GridView{Pods: make([]PodColumn, len(latest.Pods))}
	for i, p := range latest.Pods {
		g.Pods[i] = PodColumn{Endpoint: p.Endpoint, Label: p.Label, Err: p.Err}
	}
	for _, name := range workerNames(latest.Pods) {
		cells := make([]GridCell, len(latest.Pods))
		for i, p := range latest.Pods {
			sm := p.Workers[name]
			if p.Err != "" || sm == nil {
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
// series always span all of snaps, so the interval is not needed.
func BuildDetail(snaps []*Snapshot, pod, worker string, _ time.Duration) (DetailView, bool) {
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
		Completed1h:      workerRates(snaps, pod, worker, completedRate),
		Failed1h:         workerRates(snaps, pod, worker, failedRate),
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
		d.Pods = append(d.Pods, PodColumn{Endpoint: p.Endpoint, Label: p.Label, Err: p.Err})
	}
	d.SuccessRate = "—"
	if total := d.Successful + d.Failed; total > 0 {
		d.SuccessRate = fmt.Sprintf("%.1f%%", float64(d.Successful)/float64(total)*100)
	}
	return d, true
}

// podsOf returns the pods of s a view covers: all of them, or only the one with
// endpoint pod. The result aliases s.Pods and must not be modified.
func podsOf(s *Snapshot, pod string) []PodSnapshot {
	if s == nil {
		return nil
	}
	if pod == "" {
		return s.Pods
	}
	for i := range s.Pods {
		if s.Pods[i].Endpoint == pod {
			return s.Pods[i : i+1]
		}
	}
	return nil
}

// workerNames is the sorted union of worker names over the healthy pods.
func workerNames(pods []PodSnapshot) []string {
	names := make(map[string]struct{})
	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		for name := range p.Workers {
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
	reporting     int  // pods reporting the worker
	absent        int  // healthy pods lacking it
	allLB         bool // every reporting pod is load-balanced
}

// mergeWorker folds name across the healthy pods. status and class are only
// meaningful when reporting > 0.
func mergeWorker(pods []PodSnapshot, name string) merged {
	m := merged{allLB: true}
	var statuses []string
	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		sm := p.Workers[name]
		if sm == nil {
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
		m.allLB = m.allLB && p.LoadBalanced > 1
	}
	if m.reporting > 0 {
		m.status, m.class = mergeStatus(statuses)
	} else {
		m.allLB = false
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

// concurrencyLabel shows the per-pod concurrency, never a sum.
func concurrencyLabel(perPod []int) string {
	lo, hi := slices.Min(perPod), slices.Max(perPod)
	switch {
	case lo != hi:
		return fmt.Sprintf("mixed %d–%d", lo, hi)
	case len(perPod) > 1:
		return fmt.Sprintf("%d ×%d pods", lo, len(perPod))
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

// newest returns the snapshots within span: the last int(span/interval),
// at least one. A non-positive interval keeps everything.
func newest(snaps []*Snapshot, span, interval time.Duration) []*Snapshot {
	n := len(snaps)
	if interval > 0 {
		n = min(n, max(1, int(span/interval)))
	}
	return snaps[len(snaps)-n:]
}

// workerRates charts one worker's rate (pick) per snapshot: the sum over
// reporting pods that are neither load-balanced nor missing a valid rate.
func workerRates(snaps []*Snapshot, pod, name string, pick func(*Sample) float64) []Point {
	out := make([]Point, len(snaps))
	for i, s := range snaps {
		var sum float64
		var contributing, reporting int
		for _, p := range podsOf(s, pod) {
			sm := p.Workers[name]
			if p.Err != "" || sm == nil {
				continue
			}
			reporting++
			if p.LoadBalanced > 1 || !sm.RateOK {
				continue
			}
			contributing++
			sum += pick(sm)
		}
		out[i] = ratePoint(s, sum, contributing, reporting, 1, "/s")
	}
	return out
}

// fleetRates charts a rate summed over every worker per snapshot, scaled by
// scale (60 for /min). A pod "reports" when it is healthy and has workers, and
// "contributes" when it is not load-balanced and has a worker with a valid rate.
func fleetRates(snaps []*Snapshot, pod string, pick func(*Sample) float64, scale float64, unit string) []Point {
	out := make([]Point, len(snaps))
	for i, s := range snaps {
		var sum float64
		var contributing, reporting int
		for _, p := range podsOf(s, pod) {
			if p.Err != "" || len(p.Workers) == 0 {
				continue
			}
			reporting++
			if p.LoadBalanced > 1 {
				continue
			}
			var got bool
			for _, sm := range p.Workers {
				if sm.RateOK {
					sum += pick(sm)
					got = true
				}
			}
			if got {
				contributing++
			}
		}
		out[i] = ratePoint(s, sum, contributing, reporting, scale, unit)
	}
	return out
}

// pendingSeries charts the Σ-over-workers of max-per-pod pending per snapshot.
// A snapshot with no healthy pod has no data.
func pendingSeries(snaps []*Snapshot, pod string) []Point {
	out := make([]Point, len(snaps))
	for i, s := range snaps {
		total, healthy := peakPending(podsOf(s, pod))
		if healthy == 0 {
			out[i] = Point{Gap: true, Title: pointTime(s) + " · no data"}
			continue
		}
		out[i] = Point{V: float64(total), Title: fmt.Sprintf("%s · %d pending (max/pod)", pointTime(s), total)}
	}
	return out
}

// ratePoint builds one rate point. With no contributing pod it is a gap; with
// fewer contributing than reporting pods it is partial.
func ratePoint(s *Snapshot, sum float64, contributing, reporting int, scale float64, unit string) Point {
	if contributing == 0 {
		return Point{Gap: true, Title: pointTime(s) + " · no data"}
	}
	v := sum * scale
	return Point{
		V:       v,
		Partial: contributing < reporting,
		Title:   fmt.Sprintf("%s · %.1f%s · %d/%d pods", pointTime(s), v, unit, contributing, reporting),
	}
}

// pointTime is the clock time a point's tooltip starts with.
func pointTime(s *Snapshot) string { return s.At.Local().Format("15:04:05") }
