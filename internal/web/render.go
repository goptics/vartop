package web

import (
	"embed"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// assets holds the HTML templates and the static files (vendored htmx, CSS).
// Handlers serve static/ at /static/.
//
//go:embed templates static
var assets embed.FS

// PodOption is one entry of the unit select. Tip is the hover text of
// Label.
type PodOption struct{ Label, Tip string }

// Page is everything a full page needs. Handlers fill it; templates never
// compute anything the view models do not already carry.
type Page struct {
	// Server is the key of the server the page shows, Pod the label of
	// the unit it is filtered to ("" for every unit), named in the URL by
	// Noun.Param. SelfURL is the page's path with its server and unit, as
	// the poller asks for it.
	Title, Server, Pod, SelfURL string
	Noun                        fleet.Noun // what the server's units are called
	Servers                     []string   // every configured server, sorted
	Pods                        []PodOption
	IntervalSec                 int // the server's poll interval; 0: no poller
	Health                      fleet.HealthView
	Updated                     string // clock time of the newest snapshot, "" before the first

	// ScopeBalanced: what the page's actions reach holds a unit behind a
	// load balancer, so there is no Restart. OneBalanced: it is one such
	// unit, so there are no controls at all, only a note saying why.
	ScopeBalanced, OneBalanced bool

	// Note is the quiet line about the balancers behind the server (see
	// BalancerNote). Banner is a notice about what this tab shows: its
	// server is not configured or failing, its unit is gone, unknown or
	// down, or its worker is not found. Waiting is the whole "Waiting for
	// the first snapshot of {server}…" message of a full page (htmx
	// reloads instead, see reload). Stale says how old the newest
	// snapshot is, and why: the page then shows no current value — health,
	// the balancers' note, the units' errors and the units reporting a
	// worker included — only history.
	Note, Banner, Waiting, Stale string

	// Unconfigured: Server is not in the config; the server select shows
	// it chosen.
	Unconfigured bool

	// Gone is Pod when the newest snapshot has no unit so labelled (see
	// podGone): the unit select shows it chosen, and the banner links to
	// the merged view.
	Gone string

	// reload: an htmx request got a waiting page, which htmx would swap
	// in only part of; the response has the browser load it whole instead
	// (HX-Redirect to SelfURL).
	reload bool

	// At most one is set; none while waiting or when the server or unit
	// cannot be shown.
	Dashboard *fleet.DashboardView
	Grid      *fleet.GridView
	Detail    *fleet.DetailView
}

// Interval is the server's poll interval; 0 with no server.
func (p Page) Interval() time.Duration { return time.Duration(p.IntervalSec) * time.Second }

// Scope names what an action on this page touches: the selected unit's
// label, or "all pods" ("all instances" off Kubernetes).
func (p Page) Scope() string {
	if p.Pod == "" {
		return "all " + p.Noun.Many
	}
	return p.Pod
}

// ResultsView is what the "results" template renders: the outcome of every
// unit an action reached. Noun words the notice for an action that reached none.
type ResultsView struct {
	Results []fleet.ActionResult
	Noun    fleet.Noun
}

// ConfirmView is the bulk-action confirmation panel: Rows (workers) × Pods
// on Server, scoped to the unit labelled Pod ("" for every unit). A down
// unit's column carries its Err.
type ConfirmView struct {
	Action, Server, Pod string
	Noun                fleet.Noun
	Pods                []fleet.PodColumn
	Rows                []ConfirmRow
}

// ConfirmRow is one worker of the confirmation panel. Absent is aligned with
// ConfirmView.Pods: true where the pod's newest snapshot is healthy and
// lacks the worker (a pod that lists it without an answer does not lack it).
// A down or not yet polled pod is never marked: the action is sent to every
// pod in scope either way.
type ConfirmRow struct {
	Name   string
	Absent []bool
}

// funcs are the template functions of every set.
var funcs = template.FuncMap{
	"pathEscape":    url.PathEscape,
	"pageURL":       pageURL,
	"workerURL":     workerURL,
	"pathOf":        pathOf,
	"sparkline":     sparkline,
	"history":       history,
	"tickLabel":     tickLabel,
	"noData":        noData,
	"trendWindow":   func() time.Duration { return fleet.TrendWindow },
	"historyWindow": func() time.Duration { return fleet.HistoryWindow },
	"statusClasses": func() []string { return []string{"active", "transitional", "paused", "stopped", "unknown"} },
	"oneBalanced":   func() string { return fleet.OneBalancedMsg },
}

// parseTemplates parses one template set per page, keyed "dashboard", "grid",
// "detail", "results" and "confirm" (layout.html + <key>.html). Entry points:
//
//   - dashboard, grid, detail: ExecuteTemplate(w, "layout", Page) renders the
//     full document. The dashboard and detail sets also define "live_oob"
//     (Page): the #live region as an out-of-band swap, for action responses.
//   - results: ExecuteTemplate(w, "results", ResultsView).
//   - confirm: ExecuteTemplate(w, "confirm", ConfirmView).
func parseTemplates() (map[string]*template.Template, error) {
	sets := make(map[string]*template.Template)
	for _, name := range []string{"dashboard", "grid", "detail", "results", "confirm"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", name, err)
		}
		sets[name] = t
	}
	return sets, nil
}

// pageURL is path plus the server the page is bound to and the label of
// its unit, under param (the server's Noun.Param); empty ones are left
// out.
func pageURL(path, server, param, label string) string {
	q := url.Values{}
	if server != "" {
		q.Set("server", server)
	}
	if label != "" {
		q.Set(param, label)
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// workerURL is the detail page of a worker; the name is path-escaped.
func workerURL(name, server, param, label string) string {
	return pageURL("/workers/"+url.PathEscape(name), server, param, label)
}

// pathOf drops the query from a request URI ("" is the root).
func pathOf(uri string) string {
	path, _, _ := strings.Cut(uri, "?")
	if path == "" {
		return "/"
	}
	return path
}

// noData reports whether a series has nothing to show as its current value:
// it is empty, or its newest point is a gap.
func noData(pts []fleet.Point) bool {
	return len(pts) == 0 || pts[len(pts)-1].Gap
}

// peak is the largest value of the non-gap points (0 if none).
func peak(pts []fleet.Point) float64 {
	var top float64
	for _, p := range pts {
		if !p.Gap && p.V > top {
			top = p.V
		}
	}
	return top
}

// minHit is the narrowest hover target, in sparkline units. Denser series
// share targets so one hour at a 1s interval does not emit thousands of
// elements per poll; at the default 5s interval every point keeps its own.
const minHit = 1.0

// sparkPad is the viewBox margin that keeps strokes and dots at the edges of
// the plot from being clipped.
const sparkPad = 3

// placed is a point positioned inside a w×h plot.
type placed struct {
	x, y float64
	p    fleet.Point
}

// chartArea is a w×h plot of the window up to the newest of a series'
// points, which it places by time (fleet.Point.At): a
// stretch with no snapshot is blank space of its width, not squeezed away
// by the points around it. interval is the series' poll interval: points
// more than two apart are not joined. 0 joins every pair, gaps aside.
type chartArea struct {
	start         time.Time
	window        time.Duration
	interval      time.Duration
	width, height float64
}

// newChartArea is the area pts are drawn in, and the points of pts it
// shows: those taken within window of the newest, oldest first.
func newChartArea(pts []fleet.Point, window, interval time.Duration, w, h float64) (chartArea, []fleet.Point) {
	a := chartArea{window: window, interval: interval, width: w, height: h}
	if len(pts) == 0 {
		return a, nil
	}
	a.start = pts[len(pts)-1].At.Add(-window)
	i := slices.IndexFunc(pts, func(p fleet.Point) bool { return !p.At.Before(a.start) })
	if i < 0 { // a window below zero: nothing in it
		return a, nil
	}
	return a, pts[i:]
}

// xAt is the x of a point taken at at: its time into the window, scaled to
// the width, the newest on the right edge.
func (a chartArea) xAt(at time.Time) float64 {
	if a.window <= 0 {
		return a.width
	}
	return float64(at.Sub(a.start)) / float64(a.window) * a.width
}

// apart reports whether two points taken at prev and at are too far apart
// to be joined: more than two intervals, so polls missed, not one late.
func (a chartArea) apart(prev, at time.Time) bool {
	return a.interval > 0 && at.Sub(prev) > 2*a.interval
}

// yAt scales v into [0, h] against top, 0 at the bottom (y == h). A series
// whose maximum is 0 is a flat line at the bottom.
func yAt(v, top, h float64) float64 {
	if top <= 0 || v <= 0 {
		return h
	}
	return h - min(v, top)/top*h
}

// runsOf positions pts, the points a shows, scaled to top, and splits
// them where a point is a gap or the points are apart.
func (a chartArea) runsOf(pts []fleet.Point, top float64) [][]placed {
	var runs [][]placed
	var cur []placed
	for _, p := range pts {
		if p.Gap || cur != nil && a.apart(cur[len(cur)-1].p.At, p.At) {
			if cur != nil {
				runs = append(runs, cur)
				cur = nil
			}
			if p.Gap {
				continue
			}
		}
		cur = append(cur, placed{a.xAt(p.At), yAt(p.V, top, a.height), p})
	}
	if cur != nil {
		runs = append(runs, cur)
	}
	return runs
}

// hitWidth is the width of a hover target: a poll's width, where the
// interval is known, but never under minHit.
func (a chartArea) hitWidth(n int) float64 {
	switch {
	case a.interval > 0 && a.window > 0:
		return max(minHit, a.width*float64(a.interval)/float64(a.window))
	case n > 1:
		return max(minHit, a.width/float64(n-1))
	}
	return max(minHit, a.width)
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

func joinRun(run []placed) string {
	coords := make([]string, len(run))
	for i, p := range run {
		coords[i] = num(p.x) + "," + num(p.y)
	}
	return strings.Join(coords, " ")
}

// polylines returns one "x,y x,y" string per run of pts (see runsOf) in a
// w×h plot of window, y scaled to the maximum of the points shown.
func polylines(pts []fleet.Point, window, interval time.Duration, w, h float64) []string {
	return sparkline(pts, window, interval, w, h).Lines
}

// sparkView is what the "spark" template draws. Peak is the largest value
// it draws, for the caption.
type sparkView struct {
	ViewBox       string
	Width, Height string
	Top, HitH     string // y and height of the hover targets
	Lines         []string
	Dots          []sparkDot
	Hits          []sparkHit
	Peak          float64
}

// sparkDot is a circle: a partial point (hollow) or a solid point that has no
// neighbour to be joined to by a line.
type sparkDot struct{ X, Y, Class string }

// sparkHit is an invisible column over one point (or a few, see minHit) whose
// <title> is the hover text. Gaps get one too, so "no data" can be hovered.
type sparkHit struct{ X, W, Title string }

// sparkline lays pts, a series polled every interval, out in a w×h plot of
// the window up to its newest point, each at its time (see chartArea): a
// polyline per run, hollow circles for partial points, dots for isolated
// ones, and hover targets on top. Points older than the window are not
// drawn, nor counted in its peak.
func sparkline(pts []fleet.Point, window, interval time.Duration, w, h float64) sparkView {
	a, pts := newChartArea(pts, window, interval, w, h)
	v := sparkView{
		ViewBox: fmt.Sprintf("%d %d %s %s", -sparkPad, -sparkPad, num(w+2*sparkPad), num(h+2*sparkPad)),
		Width:   num(w + 2*sparkPad),
		Height:  num(h + 2*sparkPad),
		Top:     strconv.Itoa(-sparkPad),
		HitH:    num(h + 2*sparkPad),
		Peak:    peak(pts),
	}
	for _, run := range a.runsOf(pts, v.Peak) {
		v.Lines = append(v.Lines, joinRun(run))
		for _, p := range run {
			switch {
			case p.p.Partial:
				v.Dots = append(v.Dots, sparkDot{num(p.x), num(p.y), "dot partial"})
			case len(run) == 1:
				v.Dots = append(v.Dots, sparkDot{num(p.x), num(p.y), "dot"})
			}
		}
	}

	// A target per column of the plot holding points, gaps included,
	// titled by the middle one of them.
	col := a.hitWidth(len(pts))
	for lo := 0; lo < len(pts); {
		c := math.Round(a.xAt(pts[lo].At) / col)
		hi := lo + 1
		for hi < len(pts) && math.Round(a.xAt(pts[hi].At)/col) == c {
			hi++
		}
		v.Hits = append(v.Hits, sparkHit{X: num(c*col - col/2), W: num(col), Title: pts[lo+(hi-lo-1)/2].Title})
		lo = hi
	}
	return v
}

// historyTick is the step of a 1h chart's time axis, on the clock.
const historyTick = 10 * time.Minute

// axisTick is a labelled tick of a chart axis. Pct is where it sits along
// the axis, in percent of the chart's box (sparkPad included), for its HTML
// label; Grid is its coordinate in the plot, for its SVG gridline.
type axisTick struct{ Label, Pct, Grid string }

// historyView is what the "history" template draws: the sparkline of a
// series with a time axis, a value scale and their gridlines. PlotW and
// PlotH are the plot's size, the gridlines' and the crosshair's extent.
type historyView struct {
	sparkView
	XTicks, YTicks []axisTick
	PlotW, PlotH   string
}

// history is sparkline with axes: a tick at every historyTick of the clock
// in the window up to the newest point, and 0, half the peak and the peak
// (0 alone for a flat zero series). A series with nothing to draw has no
// axes.
func history(pts []fleet.Point, window, interval time.Duration, w, h float64) historyView {
	v := historyView{sparkView: sparkline(pts, window, interval, w, h), PlotW: num(w), PlotH: num(h)}
	a, shown := newChartArea(pts, window, interval, w, h)
	if len(shown) == 0 {
		return v
	}
	inBox := func(c, size float64) string { return num((c + sparkPad) / (size + 2*sparkPad) * 100) }

	// Ticks on the local clock, so a zone off the hour by a part of it
	// still gets :00, :10, …: the start floored in its own zone offset, not
	// rebuilt from its wall clock, which a fall-back hour makes ambiguous.
	// Every offset change is a multiple of historyTick, so later ticks stay
	// on the clock too.
	_, off := a.start.Local().Zone()
	shift := time.Duration(off) * time.Second
	t := a.start.Add(shift).Truncate(historyTick).Add(-shift)
	if t.Before(a.start) {
		t = t.Add(historyTick)
	}
	for end := a.start.Add(window); !t.After(end); t = t.Add(historyTick) {
		x := a.xAt(t)
		v.XTicks = append(v.XTicks, axisTick{Label: t.Local().Format("15:04"), Pct: inBox(x, w), Grid: num(x)})
	}

	scale := []float64{0}
	if v.Peak > 0 {
		scale = append(scale, v.Peak/2, v.Peak)
	}
	for _, val := range scale {
		y := yAt(val, v.Peak, h)
		v.YTicks = append(v.YTicks, axisTick{Label: tickLabel(val), Pct: inBox(y, h), Grid: num(y)})
	}
	return v
}

// tickLabel is a scale value as short as reads well: whole from 100, one
// decimal from 1, and two significant digits below, so that the small
// rates a Failed chart is mostly made of (a failure now and then) do not
// all round to 0. Trailing zeros are dropped.
func tickLabel(v float64) string {
	decimals := 1
	switch {
	case v >= 100:
		decimals = 0
	case v > 0 && v < 1:
		decimals = 1 - int(math.Floor(math.Log10(v)))
	}
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
