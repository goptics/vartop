package web

import (
	"embed"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/goptics/vartop/internal/config"
)

// assets holds the HTML templates and the static files (vendored htmx, CSS).
// Handlers serve static/ at /static/.
//
//go:embed templates static
var assets embed.FS

// Noun is what the pages call one unit of a server: "pod" on Kubernetes,
// "instance" on every other server. Article goes before One ("a pod", "an
// instance").
type Noun struct{ One, Many, Article string }

// Cap is One with a capital first letter.
func (n Noun) Cap() string { return capitalize(n.One) }

// CapMany is Many with a capital first letter.
func (n Noun) CapMany() string { return capitalize(n.Many) }

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// NounFor is the noun of srv's units: a Kubernetes server's are pods, any
// other server's (static endpoints, DNS) are instances, and so are nil's.
func NounFor(srv *config.Server) Noun {
	if srv != nil && srv.Kubernetes != nil {
		return Noun{One: "pod", Many: "pods", Article: "a"}
	}
	return Noun{One: "instance", Many: "instances", Article: "an"}
}

// PodOption is one entry of the pod select. Tip is the hover text of Label.
type PodOption struct{ Key, Label, Tip string }

// Page is everything a full page needs. Handlers fill it; templates never
// compute anything the view models do not already carry.
type Page struct {
	Title, Server, Pod, SelfURL string // SelfURL = current path + query, used by the poller
	Noun                        Noun   // what the server's units are called
	Servers                     []string
	Pods                        []PodOption
	IntervalSec                 int
	Health                      HealthView
	Updated                     string // clock time of the newest snapshot, "" before the first

	// ScopeBalanced: what the page's actions reach holds a unit behind a
	// load balancer, so there is no Restart. OneBalanced: it is one such
	// unit, so there are no controls at all, only a note saying why.
	ScopeBalanced, OneBalanced bool

	// Note is the quiet line about the balancers behind the server (see
	// BalancerNote). Banner is a notice about what this tab shows: its server
	// changed, its pod is gone or down, or its worker is not found.
	// Connecting is the whole "Connecting to {label}…" message. Err is a
	// failure to show; with no view model set it also offers a Retry form
	// that posts Server to /server.
	Note, Banner, Connecting, Err string

	// Gone is the label of the pod Pod names when the newest snapshot no
	// longer has it (see podGone): the pod select shows it chosen, and the
	// banner links to the merged view.
	Gone string

	// At most one is set; none while connecting, stale or failed.
	Dashboard *DashboardView
	Grid      *GridView
	Detail    *DetailView
}

// Scope names what an action on this page touches: the selected unit's label,
// or "all pods" ("all instances" off Kubernetes).
func (p Page) Scope() string {
	if p.Pod == "" {
		return "all " + p.Noun.Many
	}
	for _, o := range p.Pods {
		if o.Key == p.Pod {
			return o.Label
		}
	}
	return p.Pod
}

// ActionResult is one pod's outcome of an action. Key is the unit's key and
// Tip the hover text of its label (Pod). Outcome is "ok", "no change" or
// "failed"; Msg, when set, is shown in parentheses after it. Worker names the
// worker when one response covers several (bulk).
type ActionResult struct{ Worker, Pod, Key, Tip, Outcome, Msg string }

// ResultsView is what the "results" template renders: the outcome of every
// unit an action reached. Noun words the notice for an action that reached none.
type ResultsView struct {
	Results []ActionResult
	Noun    Noun
}

// ConfirmView is the bulk-action confirmation panel: Rows (workers) × Pods
// on Server. A down pod's column carries its Err.
type ConfirmView struct {
	Action, Server, Pod string
	Noun                Noun
	Pods                []PodColumn
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
	"noData":        noData,
	"peak":          peak,
	"statusClasses": func() []string { return []string{"active", "transitional", "paused", "stopped", "unknown"} },
	"oneBalanced":   func() string { return oneBalancedMsg },
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

// pageURL is path plus the server and pod the page is bound to; empty ones
// are left out.
func pageURL(path, server, pod string) string {
	q := url.Values{}
	if server != "" {
		q.Set("server", server)
	}
	if pod != "" {
		q.Set("pod", pod)
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// workerURL is the detail page of a worker; the name is path-escaped.
func workerURL(name, server, pod string) string {
	return pageURL("/workers/"+url.PathEscape(name), server, pod)
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
func noData(pts []Point) bool {
	return len(pts) == 0 || pts[len(pts)-1].Gap
}

// peak is the largest value of the non-gap points (0 if none).
func peak(pts []Point) float64 {
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
	p    Point
}

// xAt is the x of the i-th of n points: evenly spread over w, newest on the
// right. A lone point sits in the middle.
func xAt(i, n int, w float64) float64 {
	if n < 2 {
		return w / 2
	}
	return float64(i) * w / float64(n-1)
}

// yAt scales v into [0, h] against top, 0 at the bottom (y == h). A series
// whose maximum is 0 is a flat line at the bottom.
func yAt(v, top, h float64) float64 {
	if top <= 0 || v <= 0 {
		return h
	}
	return h - min(v, top)/top*h
}

// runsOf positions the points of pts in a w×h plot and splits them at gaps.
func runsOf(pts []Point, w, h float64) [][]placed {
	top := peak(pts)
	var runs [][]placed
	var cur []placed
	for i, p := range pts {
		if p.Gap {
			if cur != nil {
				runs = append(runs, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, placed{xAt(i, len(pts), w), yAt(p.V, top, h), p})
	}
	if cur != nil {
		runs = append(runs, cur)
	}
	return runs
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

func joinRun(run []placed) string {
	coords := make([]string, len(run))
	for i, p := range run {
		coords[i] = num(p.x) + "," + num(p.y)
	}
	return strings.Join(coords, " ")
}

// polylines returns one "x,y x,y" string per gap-free run of pts in a w×h
// plot, y scaled to the series maximum.
func polylines(pts []Point, w, h float64) []string {
	var out []string
	for _, run := range runsOf(pts, w, h) {
		out = append(out, joinRun(run))
	}
	return out
}

// sparkView is what the "spark" template draws.
type sparkView struct {
	ViewBox       string
	Width, Height string
	Top, HitH     string // y and height of the hover targets
	Lines         []string
	Dots          []sparkDot
	Hits          []sparkHit
}

// sparkDot is a circle: a partial point (hollow) or a solid point that has no
// neighbour to be joined to by a line.
type sparkDot struct{ X, Y, Class string }

// sparkHit is an invisible column over one point (or a few, see minHit) whose
// <title> is the hover text. Gaps get one too, so "no data" can be hovered.
type sparkHit struct{ X, W, Title string }

// sparkline lays pts out in a w×h plot: a polyline per run, hollow circles
// for partial points, dots for isolated ones, and hover targets on top.
func sparkline(pts []Point, w, h float64) sparkView {
	v := sparkView{
		ViewBox: fmt.Sprintf("%d %d %s %s", -sparkPad, -sparkPad, num(w+2*sparkPad), num(h+2*sparkPad)),
		Width:   num(w + 2*sparkPad),
		Height:  num(h + 2*sparkPad),
		Top:     strconv.Itoa(-sparkPad),
		HitH:    num(h + 2*sparkPad),
	}
	for _, run := range runsOf(pts, w, h) {
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

	n := len(pts)
	step := w
	if n > 1 {
		step = w / float64(n-1)
	}
	stride := 1
	if step < minHit {
		stride = int(math.Ceil(minHit / step))
	}
	for lo := 0; lo < n; lo += stride {
		hi := min(lo+stride, n)
		mid := lo + (hi-lo-1)/2
		v.Hits = append(v.Hits, sparkHit{
			X:     num(xAt(lo, n, w) - step/2),
			W:     num(step * float64(hi-lo)),
			Title: pts[mid].Title,
		})
	}
	return v
}
