package web

import (
	"bytes"
	"html/template"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// The nouns of a Kubernetes server and of every other. Fixtures that name
// no server pick one.
var (
	podNoun      = fleet.Noun{One: "pod", Many: "pods", Article: "a", Param: "pod"}
	instanceNoun = fleet.Noun{One: "instance", Many: "instances", Article: "an", Param: "instance"}
)

func TestPageScope(t *testing.T) {
	for _, tc := range []struct {
		p    Page
		want string
	}{
		{Page{Noun: podNoun}, "all pods"},
		{Page{Noun: instanceNoun}, "all instances"},
		{Page{Noun: instanceNoun, Pod: "instance-2"}, "instance-2"},
		{Page{Noun: podNoun, Pod: "web-0"}, "web-0"},
	} {
		if got := tc.p.Scope(); got != tc.want {
			t.Errorf("Scope() of %+v = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// run executes the named entry template of page set `set`.
func run(t *testing.T, set, entry string, data any) string {
	t.Helper()
	ts, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	var buf bytes.Buffer
	if err := ts[set].ExecuteTemplate(&buf, entry, data); err != nil {
		t.Fatalf("execute %s/%s: %v", set, entry, err)
	}
	return buf.String()
}

// coords parses a "x,y x,y" polyline string.
func coords(t *testing.T, s string) [][2]float64 {
	t.Helper()
	var out [][2]float64
	for _, c := range strings.Fields(s) {
		xs, ys, ok := strings.Cut(c, ",")
		if !ok {
			t.Fatalf("bad coordinate %q in %q", c, s)
		}
		x, errx := strconv.ParseFloat(xs, 64)
		y, erry := strconv.ParseFloat(ys, 64)
		if errx != nil || erry != nil {
			t.Fatalf("bad coordinate %q in %q", c, s)
		}
		out = append(out, [2]float64{x, y})
	}
	return out
}

// ptAt is a point of value v taken at at.
func ptAt(at time.Time, v float64) fleet.Point { return fleet.Point{At: at, V: v, Title: "t"} }

// series is n points of value v, every interval from start.
func series(start time.Time, interval time.Duration, n int, v float64) []fleet.Point {
	pts := make([]fleet.Point, n)
	for i := range pts {
		pts[i] = ptAt(start.Add(time.Duration(i)*interval), v)
	}
	return pts
}

// The 1h charts have a time axis at every 10 minutes of the clock in the
// hour shown, and a scale of 0, half the peak and the peak, each with its
// gridline; positions are percentages of the chart's box (pad included),
// so labels set in HTML line up with the SVG at any width.
func TestHistoryAxes(t *testing.T) {
	end := time.Date(2026, 10, 27, 13, 27, 30, 0, time.Local)
	pts := series(end.Add(-25*time.Minute), 5*time.Second, 301, 10) // 13:02:30 … 13:27:30
	pts[150].V = 40

	h := history(pts, time.Hour, 5*time.Second, 720, 120)

	var xs []string
	for _, tk := range h.XTicks {
		xs = append(xs, tk.Label)
	}
	// The hour shown runs 12:27:30 … 13:27:30: ticks before the data are kept,
	// the axis is the hour, not the stretch with points.
	if want := []string{"12:30", "12:40", "12:50", "13:00", "13:10", "13:20"}; !slices.Equal(xs, want) {
		t.Errorf("x ticks = %q, want %q", xs, want)
	}
	// 13:20 is 52.5 of the 60 minutes in: x 630 of 720, in a box padded by
	// sparkPad on each side.
	if last := h.XTicks[len(h.XTicks)-1]; last.Pct != "87.2" || last.Grid != "630.0" {
		t.Errorf("13:20 tick at %s%% (grid x %s), want 87.2%% (630.0)", last.Pct, last.Grid)
	}
	var ys []string
	for _, tk := range h.YTicks {
		ys = append(ys, tk.Label)
	}
	if want := []string{"0", "20", "40"}; !slices.Equal(ys, want) {
		t.Errorf("y ticks = %q, want %q", ys, want)
	}
	// The peak's gridline is the top of the plot, 0's the bottom; the labels
	// sit at the same heights of the padded box.
	if top, bottom := h.YTicks[2], h.YTicks[0]; top.Grid != "0.0" || bottom.Grid != "120.0" {
		t.Errorf("y gridlines at %s (peak) and %s (0), want 0.0 and 120.0", top.Grid, bottom.Grid)
	}
	var yPct []string
	for _, tk := range h.YTicks {
		yPct = append(yPct, tk.Pct)
	}
	if want := []string{"97.6", "50.0", "2.4"}; !slices.Equal(yPct, want) {
		t.Errorf("y label positions = %q%%, want %q%%", yPct, want)
	}
	// The chart itself is the sparkline's: same lines, same hover targets.
	if s := sparkline(pts, time.Hour, 5*time.Second, 720, 120); !slices.Equal(h.Lines, s.Lines) || len(h.Hits) != len(s.Hits) {
		t.Error("history draws another chart than sparkline")
	}

	t.Run("a flat zero series has one tick", func(t *testing.T) {
		h := history(series(end.Add(-time.Minute), 5*time.Second, 13, 0), time.Hour, 5*time.Second, 720, 120)
		if len(h.YTicks) != 1 || h.YTicks[0].Label != "0" {
			t.Errorf("y ticks = %+v, want just 0", h.YTicks)
		}
	})
	t.Run("no data, no axis", func(t *testing.T) {
		h := history(nil, time.Hour, 5*time.Second, 720, 120)
		if len(h.XTicks) != 0 || len(h.YTicks) != 0 {
			t.Errorf("ticks = %+v %+v, want none", h.XTicks, h.YTicks)
		}
	})
	t.Run("a window on the boundaries has both", func(t *testing.T) {
		end := time.Date(2026, 10, 27, 13, 30, 0, 0, time.Local)
		h := history(series(end.Add(-time.Hour), time.Minute, 61, 1), time.Hour, time.Minute, 720, 120)
		if n := len(h.XTicks); n != 7 || h.XTicks[0].Pct != "0.4" || h.XTicks[n-1].Pct != "99.6" {
			t.Errorf("x ticks = %+v, want 7 from 0.4%% to 99.6%%", h.XTicks)
		}
	})
	// Small rates (a failure now and then) are the Failed chart's usual
	// scale: its three labels must still be three values.
	t.Run("a small peak keeps its labels apart", func(t *testing.T) {
		for _, top := range []float64{0.0333, 0.0667, 0.1, 0.3} {
			pts := series(end.Add(-time.Minute), 5*time.Second, 13, 0)
			pts[6].V = top
			var ys []string
			for _, tk := range history(pts, time.Hour, 5*time.Second, 720, 120).YTicks {
				ys = append(ys, tk.Label)
			}
			if len(ys) != 3 || ys[0] == ys[1] || ys[1] == ys[2] || ys[2] == "0" {
				t.Errorf("peak %v: y ticks = %q, want 0, half the peak and the peak, all different", top, ys)
			}
		}
	})
	t.Run("labels", func(t *testing.T) {
		for v, want := range map[float64]string{
			0: "0", 0.0167: "0.017", 0.0333: "0.033", 0.05: "0.05", 0.1: "0.1", 0.15: "0.15", 0.26: "0.26", 0.5: "0.5", 0.75: "0.75",
			3.5: "3.5", 12: "12", 34.9: "34.9", 150.4: "150", 2500: "2500",
		} {
			if got := tickLabel(v); got != want {
				t.Errorf("tickLabel(%v) = %q, want %q", v, got, want)
			}
		}
	})
	// Ticks follow the local clock, in zones off the hour by a part of it
	// and through the hour a fall-back repeats.
	t.Run("zones", func(t *testing.T) {
		local := time.Local
		t.Cleanup(func() { time.Local = local })
		for _, tc := range []struct {
			zone   string
			newest time.Time
			want   []string
		}{
			{"Asia/Kathmandu", time.Date(2026, 10, 27, 7, 42, 30, 0, time.UTC), []string{"12:30", "12:40", "12:50", "13:00", "13:10", "13:20"}},
			{"America/New_York", time.Date(2026, 11, 1, 7, 27, 30, 0, time.UTC), []string{"01:30", "01:40", "01:50", "02:00", "02:10", "02:20"}},
			{"Europe/Berlin", time.Date(2026, 10, 25, 1, 27, 30, 0, time.UTC), []string{"02:30", "02:40", "02:50", "02:00", "02:10", "02:20"}},
		} {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("no zone data: %v", err)
			}
			time.Local = loc
			h := history(series(tc.newest.Add(-time.Hour), time.Minute, 61, 1), time.Hour, time.Minute, 720, 120)
			var xs []string
			for _, tk := range h.XTicks {
				xs = append(xs, tk.Label)
				if g, _ := strconv.ParseFloat(tk.Grid, 64); g < 0 || g > 720 {
					t.Errorf("%s: tick %s at x %s, outside the plot", tc.zone, tk.Label, tk.Grid)
				}
			}
			if !slices.Equal(xs, tc.want) {
				t.Errorf("%s: x ticks = %q, want %q", tc.zone, xs, tc.want)
			}
		}
	})
}

func TestPolylines(t *testing.T) {
	const iv = 5 * time.Second
	t0 := viewT0
	gapAt := func(at time.Time) fleet.Point { return fleet.Point{At: at, Gap: true, Title: "no data"} }
	t.Run("a gap splits runs", func(t *testing.T) {
		runs := polylines([]fleet.Point{ptAt(t0, 1), ptAt(t0.Add(iv), 2), gapAt(t0.Add(2 * iv)), ptAt(t0.Add(3*iv), 4)}, 3*iv, iv, 30, 10)
		if len(runs) != 2 {
			t.Fatalf("runs = %q, want 2", runs)
		}
		first, second := coords(t, runs[0]), coords(t, runs[1])
		if len(first) != 2 || len(second) != 1 {
			t.Fatalf("run lengths = %d, %d, want 2, 1 (%q)", len(first), len(second), runs)
		}
		// x follows the point's time over the window: 0, 10, (gap at 20), 30.
		if first[0][0] != 0 || first[1][0] != 10 || second[0][0] != 30 {
			t.Errorf("x = %v %v, want 0 10 30", first, second)
		}
		// y is scaled to the series maximum (4): the max sits at the top, 2 at the middle.
		if second[0][1] != 0 || first[1][1] != 5 {
			t.Errorf("y = %v %v, want max at 0 and 2 at 5", first, second)
		}
	})
	t.Run("empty draws nothing", func(t *testing.T) {
		if runs := polylines(nil, time.Minute, iv, 30, 10); len(runs) != 0 {
			t.Errorf("runs = %q, want none", runs)
		}
		if runs := polylines([]fleet.Point{gapAt(t0), gapAt(t0.Add(iv))}, time.Minute, iv, 30, 10); len(runs) != 0 {
			t.Errorf("all-gap runs = %q, want none", runs)
		}
	})
	t.Run("one point is one coordinate, at the newest end", func(t *testing.T) {
		runs := polylines([]fleet.Point{ptAt(t0, 3)}, time.Minute, iv, 30, 10)
		if len(runs) != 1 || len(coords(t, runs[0])) != 1 || coords(t, runs[0])[0][0] != 30 {
			t.Errorf("runs = %q, want one run of one coordinate at x 30", runs)
		}
	})
	t.Run("all zero is a flat line at the bottom", func(t *testing.T) {
		runs := polylines(series(t0, iv, 3, 0), time.Minute, iv, 30, 10)
		if len(runs) != 1 {
			t.Fatalf("runs = %q, want 1", runs)
		}
		for _, c := range coords(t, runs[0]) {
			if c[1] != 10 {
				t.Errorf("y = %v, want 10 (h) for every point", c[1])
			}
		}
	})
}

// TestSparklinePlacesByTime (R39): a web chart places each point at its
// time over the chart's fixed window, up to the newest point: a stretch
// with no snapshots — no collector ran — is blank space of its width, the
// line broken where points are more than two intervals apart (a late poll
// is not a break). Points older than the window are not drawn, nor counted
// in its peak.
func TestSparklinePlacesByTime(t *testing.T) {
	const iv = 5 * time.Second
	t0 := viewT0
	var pts []fleet.Point
	pts = append(pts, series(t0.Add(-3*time.Minute), iv, 36, 99)...) // older than the window: not drawn
	pts = append(pts, series(t0, iv, 25, 1)...)                      // 0–2 min
	pts = append(pts, ptAt(t0.Add(2*time.Minute+9*time.Second), 2))  // a late poll, 9s after
	pts = append(pts, series(t0.Add(7*time.Minute), iv, 37, 3)...)   // 7–10 min: after 5 minutes with none
	v := sparkline(pts, fleet.TrendWindow, iv, 120, 20)

	if len(v.Lines) != 2 {
		t.Fatalf("lines = %d, want 2, broken at the 5 minutes with no point:\n%q", len(v.Lines), v.Lines)
	}
	first, second := coords(t, v.Lines[0]), coords(t, v.Lines[1])
	if len(first) != 26 || len(second) != 37 {
		t.Errorf("run lengths = %d, %d, want 26 (the late poll joined) and 37", len(first), len(second))
	}
	// 12 units a minute: the first run from 0 to 2m9s, the second from 7 min to 10.
	if first[0][0] != 0 || first[len(first)-1][0] != 25.8 || second[0][0] != 84 || second[len(second)-1][0] != 120 {
		t.Errorf("runs span x %v–%v and %v–%v, want 0–25.8 and 84–120", first[0][0], first[len(first)-1][0], second[0][0], second[len(second)-1][0])
	}
	if v.Peak != 3 {
		t.Errorf("peak = %v, want 3: the points older than the window are left out", v.Peak)
	}
	for _, h := range v.Hits {
		if x, _ := strconv.ParseFloat(h.X, 64); x < -1 {
			t.Errorf("a hover target at x %v, before the window", x)
		}
	}
}

func TestTemplatesParse(t *testing.T) {
	ts, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"dashboard", "grid", "detail", "results", "confirm"} {
		if ts[k] == nil {
			t.Errorf("missing template set %q", k)
		}
	}
}

func sampleRows() []fleet.RowView {
	return []fleet.RowView{
		{
			Name: "orders", ID: "w-6f7264657273", Status: "Running", Class: "active",
			StatusTip: "a: Running\nb: Running", Concurrency: "4 ×2 pods", Busy: 3, Idle: 5,
			PendingMax: 7, PendingTip: "a: 7\nb: 2",
			Spark: []fleet.Point{ptAt(atTick(0), 1), ptAt(atTick(1), 2), {At: atTick(2), V: 3, Partial: true, Title: "half"}},
		},
		{
			Name: "<script>x</script>", ID: "w-3c7363", Status: "mixed", Class: "mixed",
			Concurrency: "mixed 3–5", AbsentOn: 2,
		},
	}
}

func dashboardPage() Page {
	return Page{
		Title: "vartop", Server: "dev", SelfURL: "/?server=dev", IntervalSec: 5,
		Noun:    podNoun,
		Servers: []string{"dev", "prod"},
		Pods:    []PodOption{{Label: "a"}, {Label: "b"}},
		Health:  fleet.HealthView{Healthy: 2, Total: 2, Dot: "●"},
		Dashboard: &fleet.DashboardView{
			KPIs: fleet.KPIView{
				CompletedPerSec: 12.5, FailedPerMin: 3, PendingMax: 9,
				StatusCounts: map[string]int{"active": 3, "paused": 1},
				Completed:    []fleet.Point{ptAt(atTick(0), 1), ptAt(atTick(1), 12.5)}, Failed: []fleet.Point{ptAt(atTick(0), 0), ptAt(atTick(1), 3)}, Pending: []fleet.Point{ptAt(atTick(1), 9)},
			},
			Rows: sampleRows(),
		},
	}
}

func TestDashboardRenders(t *testing.T) {
	body := run(t, "dashboard", "layout", dashboardPage())

	for _, want := range []string{
		"Pending (max/pod)",
		`hx-trigger="every 5s"`,
		`id="live"`,
		`hx-select="#live"`,
		`hx-sync="#page:drop"`,
		`&lt;script&gt;`,
		`name="htmx-config"`,
		`/static/htmx-2.0.4.min.js`,
		`/static/app.css`,
		"4 ×2 pods",
		"mixed 3–5",
		"absent on 2 pods",
		"●", "2/2 pods",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	if strings.Contains(body, "http-equiv") {
		t.Error("a connected page must not auto-reload")
	}
	if strings.Contains(body, "<script>x") {
		t.Error("worker name was not escaped")
	}
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("<script occurrences = %d, want exactly the htmx include", n)
	}
	for _, bad := range []string{"onclick=", "onchange=", "oninput="} {
		if strings.Contains(body, bad) {
			t.Errorf("inline handler %q in page", bad)
		}
	}
}

func TestDashboardRowsAndKPIs(t *testing.T) {
	body := run(t, "dashboard", "layout", dashboardPage())

	for _, want := range []string{
		`id="sel-w-6f7264657273"`,           // checkbox keeps its identity across polls...
		`hx-preserve`,                       // ...and htmx keeps its state
		`href="/workers/orders?server=dev"`, // worker link carries server and pod
		`class="status active"`,
		`class="status mixed"`,
		"title=\"a: Running\nb: Running\"",
		`hx-post="/bulk/pause"`,
		`hx-post="/bulk/resume"`,
		`<form class="pick" action="/" method="get" hx-get="/" hx-trigger="change" hx-target="body" hx-push-url="true" hx-sync="#page:replace">`,
		`<select name="server">`,
		"All pods (merged)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	// Hollow circle for the partial point, rate-less row gets no svg of its own.
	if !strings.Contains(body, `class="dot partial"`) {
		t.Error("partial point not drawn hollow")
	}
	if !strings.Contains(body, "<title>half</title>") {
		t.Error("point title missing")
	}
	// Picking a server loads its page at once, as picking a unit does: no
	// button to press after the choice.
	_, picker, _ := strings.Cut(body, `<form class="pick" action="/" method="get"`)
	picker, _, _ = strings.Cut(picker, "</form>")
	if strings.Contains(picker, "<button") {
		t.Errorf("the server picker should apply on change, with no button:\n%s", picker)
	}
	// Bulk checkboxes belong to the bulk form that sits outside #live.
	if !strings.Contains(body, `form="bulk"`) {
		t.Error("checkbox is not tied to the bulk form")
	}
}

// A row no unit answered for shows no numbers: unknown is not zero.
func TestUnknownRowShowsNoValues(t *testing.T) {
	p := dashboardPage()
	p.Dashboard.Rows = []fleet.RowView{{Name: "w", ID: "w-77", Status: "not reached", Class: "unknown", Concurrency: "—", Unknown: true}}

	body := run(t, "dashboard", "layout", p)

	if want := `<td>—</td>
    <td class="num">—</td>
    <td class="num">—</td>
    <td class="num">—</td>`; !strings.Contains(body, want) {
		t.Errorf("row lacks %q:\n%s", want, body)
	}
}

func TestKPIShowsDashWhenLastPointIsGap(t *testing.T) {
	p := dashboardPage()
	p.Dashboard.KPIs.CompletedPerSec = 99.9
	p.Dashboard.KPIs.Completed = []fleet.Point{ptAt(atTick(0), 1), {At: atTick(1), Gap: true, Title: "no data"}}
	p.Dashboard.KPIs.Failed = nil
	body := run(t, "dashboard", "layout", p)
	if strings.Contains(body, "99.9") {
		t.Error("KPI shows a number although the newest point is a gap")
	}
	if !strings.Contains(body, "—") {
		t.Error("KPI does not show —")
	}

	p = dashboardPage()
	body = run(t, "dashboard", "layout", p)
	if !strings.Contains(body, "12.5") {
		t.Error("KPI hides a valid number")
	}
}

func TestEveryFormCarriesServer(t *testing.T) {
	for set, page := range map[string]Page{
		"dashboard": dashboardPage(),
		"detail":    detailPage(),
		"grid":      gridPage(),
	} {
		body := run(t, set, "layout", page)
		forms := strings.Split(body, "<form")[1:]
		if len(forms) == 0 {
			t.Fatalf("%s: no forms rendered", set)
		}
		for i, f := range forms {
			f, _, _ = strings.Cut(f, "</form>")
			// The server picker's select is the server.
			if !strings.Contains(f, `<input type="hidden" name="server" value="dev">`) && !strings.Contains(f, `<select name="server">`) {
				t.Errorf("%s: form %d lacks the server:\n%s", set, i, f)
			}
		}
	}
}

func TestEmptyPagesRender(t *testing.T) {
	t.Run("dashboard without workers", func(t *testing.T) {
		p := dashboardPage()
		p.Dashboard = &fleet.DashboardView{KPIs: fleet.KPIView{StatusCounts: map[string]int{}}}
		body := run(t, "dashboard", "layout", p)
		if !strings.Contains(body, "No workers") {
			t.Error("empty dashboard has no empty-state copy")
		}
		if !strings.Contains(body, `id="live"`) {
			t.Error("#live must exist even when empty")
		}
	})
	t.Run("grid without pods", func(t *testing.T) {
		p := dashboardPage()
		p.Dashboard, p.Grid = nil, &fleet.GridView{}
		body := run(t, "grid", "layout", p)
		if !strings.Contains(body, "No pods") {
			t.Error("empty grid has no empty-state copy")
		}
	})
	t.Run("waiting", func(t *testing.T) {
		body := run(t, "dashboard", "layout", Page{Title: "vartop", Server: "dev", Waiting: "Waiting for the first snapshot of dev…", SelfURL: "/?server=dev", IntervalSec: 5})
		if !strings.Contains(body, `<p class="waiting" role="status">Waiting for the first snapshot of dev…</p>`) {
			t.Error("waiting copy missing")
		}
		if !strings.Contains(body, `<meta http-equiv="refresh" content="2">`) {
			t.Error("waiting page does not reload itself")
		}
		if strings.Contains(body, `hx-select="#live"`) {
			t.Error("waiting page polls #live only, so the header would never update")
		}
	})
	t.Run("banner", func(t *testing.T) {
		p := Page{Title: "vartop", Server: "dev", Banner: "instance-9 is not an instance of dev", IntervalSec: 5, SelfURL: "/?server=dev"}
		body := run(t, "dashboard", "layout", p)
		if !strings.Contains(body, `id="banner"`) || !strings.Contains(body, "instance-9 is not an instance of dev") {
			t.Error("banner missing")
		}
		if !strings.Contains(body, `hx-select-oob="#banner,#status"`) {
			t.Error("poller cannot deliver the banner")
		}
		if !strings.Contains(body, `id="live"`) {
			t.Error("#live must exist so the poll swap has something to replace")
		}
	})
	t.Run("not configured", func(t *testing.T) {
		p := Page{Title: "vartop", Server: "gone", Servers: []string{"dev"}, Unconfigured: true, Banner: "Server gone is not configured", SelfURL: "/?server=gone"}
		body := run(t, "dashboard", "layout", p)
		for _, want := range []string{`<option value="gone" selected disabled>gone</option>`, `<option value="dev">dev</option>`, "Server gone is not configured"} {
			if !strings.Contains(body, want) {
				t.Errorf("not-configured page lacks %q:\n%s", want, body)
			}
		}
	})
	t.Run("balancer note", func(t *testing.T) {
		p := dashboardPage()
		if body := run(t, "dashboard", "layout", p); strings.Contains(body, `class="note"`) {
			t.Error("a note is rendered although there is none")
		}
		p.Note = "3 instances behind lb:8090"
		if body := run(t, "dashboard", "layout", p); !strings.Contains(body, `<p class="note" role="note">3 instances behind lb:8090</p>`) {
			t.Error("balancer note missing")
		}
	})
}

func gridPage() Page {
	p := dashboardPage()
	p.Dashboard = nil
	p.Grid = &fleet.GridView{
		Pods: []fleet.PodColumn{{Key: "http://a:3000", Label: "a"}, {Key: "http://b:3000", Label: "b", Err: "connection refused"}},
		Rows: []fleet.GridRow{{Name: "orders", Cells: []fleet.GridCell{
			{Present: true, Status: "Running", Class: "active", Busy: 2, Concurrency: 4, Pending: 1},
			{},
		}}},
	}
	return p
}

func TestGridRenders(t *testing.T) {
	body := run(t, "grid", "layout", gridPage())
	for _, want := range []string{
		"connection refused", `class="down"`,
		`class="status active"`, "2/4", `href="/workers/orders?server=dev"`,
		`id="live"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	if strings.Contains(body, "not reached") {
		t.Error("a cell says not reached although none is")
	}

	p := gridPage()
	p.Grid.Rows[0].Cells[1] = fleet.GridCell{Unreached: true}
	body = run(t, "grid", "layout", p)
	if !strings.Contains(body, `<span class="na">not reached</span>`) {
		t.Errorf("unreached cell not rendered:\n%s", body)
	}
}

// Hover text of a unit's label is its Tip, never its key.
func TestTipsAreTitles(t *testing.T) {
	const tip = "inst-a via http://a:3000"
	for name, body := range map[string]string{
		"select option": run(t, "dashboard", "layout", Page{Server: "dev", SelfURL: "/", Pods: []PodOption{{Label: "instance-1", Tip: tip}}}),
		"grid column": run(t, "grid", "layout", Page{Server: "dev", Grid: &fleet.GridView{
			Pods: []fleet.PodColumn{{Key: "#a", Label: "instance-1", Tip: tip}},
			Rows: []fleet.GridRow{{Name: "w", Cells: []fleet.GridCell{{}}}},
		}}),
		"detail pods": run(t, "detail", "layout", Page{Server: "dev", Detail: &fleet.DetailView{Name: "w", Pods: []fleet.PodColumn{{Key: "#a", Label: "instance-1", Tip: tip}}}}),
		"confirm":     run(t, "confirm", "confirm", ConfirmView{Action: "pause", Pods: []fleet.PodColumn{{Key: "#a", Label: "instance-1", Tip: tip}}}),
		"result":      run(t, "results", "results", ResultsView{Results: []fleet.ActionResult{{Pod: "instance-1", Key: "#a", Tip: tip, Outcome: "ok"}}, Noun: podNoun}),
	} {
		if !strings.Contains(body, `title="`+tip+`">instance-1<`) {
			t.Errorf("%s: label lacks the tip as title:\n%s", name, body)
		}
	}
}

func detailPage() Page {
	p := dashboardPage()
	p.Dashboard = nil
	p.Detail = &fleet.DetailView{
		Name: "a/b c", Status: "Running", Class: "active", StatusTip: "a: Running",
		Submitted: 10, Completed: 8, Successful: 7, Failed: 1, SuccessRate: "87.5%",
		ConcurrencyStart: 4, ConcurrencyMax: 64,
		Completed1h: []fleet.Point{ptAt(atTick(0), 1), ptAt(atTick(1), 2)}, Failed1h: []fleet.Point{ptAt(atTick(0), 0), ptAt(atTick(1), 0)},
		Pods: []fleet.PodColumn{{Key: "http://a:3000", Label: "a"}},
	}
	return p
}

func TestDetailRenders(t *testing.T) {
	p := detailPage()
	body := run(t, "detail", "layout", p)
	for _, want := range []string{
		"87.5%",
		// The 1h charts have axes, gridlines and the crosshair the hover
		// script moves; the script is a static file, not inline.
		`<div class="history">`,
		`class="axis-y"`,
		`class="axis-x"`,
		`<line class="grid"`,
		`<line class="cross"`,
		`<script src="/static/detail.js" defer></script>`,
		`hx-post="/workers/a%2Fb%20c/actions/pause"`,
		`hx-post="/workers/a%2Fb%20c/actions/resume"`,
		`hx-post="/workers/a%2Fb%20c/actions/restart"`,
		`hx-post="/workers/a%2Fb%20c/actions/stop"`,
		`hx-confirm="Stop a/b c on all pods (dev)?"`,
		`hx-confirm="Restart a/b c on all pods (dev)?"`,
		`hx-post="/workers/a%2Fb%20c/concurrency"`,
		`type="range"`, `max="64"`, `min="1"`, `name="n"`, `type="number"`,
		`hx-trigger="change"`,
		`hx-sync="#page:replace"`,
		`id="results"`,
		`name="pod" value=""`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}

	// Scope names the selected pod, in the confirm text and the pod field.
	p.Pod = "a"
	body = run(t, "detail", "layout", p)
	for _, want := range []string{`hx-confirm="Stop a/b c on a (dev)?"`, `name="pod" value="a"`, `href="/?pod=a&amp;server=dev"`} {
		if !strings.Contains(body, want) {
			t.Errorf("pod-scoped body lacks %q", want)
		}
	}
}

func TestLiveRegionHoldsNoControls(t *testing.T) {
	// Controls inside #live would be wiped by every poll; the oob fragment
	// that action responses carry must therefore hold none of them.
	for set, page := range map[string]Page{"detail": detailPage(), "dashboard": dashboardPage()} {
		oob := run(t, set, "live_oob", page)
		if !strings.Contains(oob, `id="live"`) || !strings.Contains(oob, `hx-swap-oob="outerHTML"`) {
			t.Errorf("%s: live_oob is not an out-of-band #live:\n%s", set, oob)
		}
		for _, control := range []string{"/actions/", "/concurrency", "/bulk/", `type="range"`, "<form"} {
			if strings.Contains(oob, control) {
				t.Errorf("%s: #live contains control %q", set, control)
			}
		}
	}
	if oob := run(t, "dashboard", "live_oob", dashboardPage()); !strings.Contains(oob, `hx-preserve`) {
		t.Error("oob refresh would lose bulk selection: checkbox lacks hx-preserve")
	}
}

func TestResultsRender(t *testing.T) {
	body := run(t, "results", "results", ResultsView{Noun: podNoun, Results: []fleet.ActionResult{
		{Pod: "a", Key: "http://a:3000", Outcome: "ok"},
		{Pod: "b", Key: "http://b:3000", Outcome: "no change", Msg: "worker is not running"},
		{Pod: "c", Key: "http://c:3000", Outcome: "failed", Msg: "<b>boom</b>"},
	}})
	for _, want := range []string{"a", "ok", "no change (worker is not running)", "failed (&lt;b&gt;boom&lt;/b&gt;)"} {
		if !strings.Contains(body, want) {
			t.Errorf("results lack %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "<li"); n != 3 {
		t.Errorf("<li count = %d, want one per result", n)
	}
	if strings.Contains(body, "<b>boom") {
		t.Error("result message not escaped")
	}
}

func TestConfirmRenders(t *testing.T) {
	body := run(t, "confirm", "confirm", ConfirmView{
		Action: "pause", Server: "dev", Pod: "", Noun: podNoun,
		Pods: []fleet.PodColumn{{Key: "http://a:3000", Label: "pod-a"}, {Key: "http://b:3000", Label: "pod-b", Err: "<i>down</i>"}},
		Rows: []ConfirmRow{{Name: "orders", Absent: []bool{true, false}}, {Name: "a&b", Absent: []bool{false, false}}},
	})
	for _, want := range []string{
		`hx-post="/bulk/pause"`,
		`<input type="hidden" name="server" value="dev">`,
		`<input type="hidden" name="pod" value="">`,
		`<input type="hidden" name="confirm" value="1">`,
		`<input type="hidden" name="names" value="orders">`,
		`<input type="hidden" name="names" value="a&amp;b">`,
		"pod-a", "pod-b", "orders", "a&amp;b", "dev",
		`<small class="err">&lt;i&gt;down&lt;/i&gt;</small>`,
		`<th scope="row" class="name">orders</th><td><span class="na">not on this pod</span></td><td>pause</td>`,
		`<th scope="row" class="name">a&amp;b</th><td>pause</td><td>pause</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm lacks %q:\n%s", want, body)
		}
	}
}

// TestCopyFollowsNoun renders every page set with each noun: the words of the
// units follow the Noun the Page, ConfirmView or ResultsView carries, and no
// other noun's.
func TestCopyFollowsNoun(t *testing.T) {
	for _, n := range []fleet.Noun{podNoun, instanceNoun} {
		other := podNoun
		if n == podNoun {
			other = instanceNoun
		}
		one, many := n.One, n.Many
		check := func(t *testing.T, name, body string, want ...string) {
			t.Helper()
			for _, w := range want {
				if !strings.Contains(body, w) {
					t.Errorf("%s lacks %q:\n%s", name, w, body)
				}
			}
			// Only the words that name a unit: the pod form field and class names stay.
			text := visibleText(body)
			if strings.Contains(strings.ToLower(text), other.One) {
				t.Errorf("%s says %q on a %s page:\n%s", name, other.One, one, text)
			}
		}

		t.Run(one, func(t *testing.T) {
			p := dashboardPage()
			p.Noun = n
			p.Dashboard.Rows[0].Concurrency = "4 ×2 " + many // fixture copy: a view model words this itself
			check(t, "dashboard", run(t, "dashboard", "layout", p),
				"<label>"+n.Cap()+` <select name="`+n.Param+`">`,
				"All "+many+" (merged)",
				">"+n.Cap()+" grid</a>",
				`title="healthy / total `+many+`"`,
				"● 2/2 "+many+"</span>",
				"Pending (max/"+one+")",
				"Concurrency (per "+one+")",
				"absent on 2 "+many+"<",
				`<p class="sub">`+one+" × worker</p>",
			)
			if got := strings.Count(run(t, "dashboard", "layout", p), "Pending (max/"+one+")"); got != 2 {
				t.Errorf("Pending (max/%s) appears %d times, want in the card and the column header", one, got)
			}

			p = detailPage()
			p.Noun = n
			check(t, "detail", run(t, "detail", "layout", p),
				`<p class="pods">`+n.CapMany()+" reporting: ",
				"per "+one+", on all "+many+"<",
				`aria-label="Concurrency per `+one+`"`,
				`aria-label="Concurrency per `+one+`, exact"`,
				`hx-confirm="Stop a/b c on all `+many+` (dev)?"`,
			)

			p = gridPage()
			p.Noun = n
			check(t, "grid", run(t, "grid", "layout", p), ">"+n.Cap()+" grid</a>")
			p.Grid = &fleet.GridView{}
			check(t, "empty grid", run(t, "grid", "layout", p), "No "+many+" to show.")

			check(t, "confirm", run(t, "confirm", "confirm", ConfirmView{
				Action: "pause", Server: "dev", Noun: n,
				Pods: []fleet.PodColumn{{Key: "#a", Label: "a"}},
				Rows: []ConfirmRow{{Name: "orders", Absent: []bool{true}}},
			}), "pause each worker on each "+one+" below:", `<span class="na">not on this `+one+`</span>`)

			check(t, "no results", run(t, "results", "results", ResultsView{Noun: n}),
				`<p class="empty">No `+many+" were targeted.</p>")
		})
	}
}

func TestStaticAssetsEmbedded(t *testing.T) {
	js, err := assets.ReadFile("static/htmx-2.0.4.min.js")
	if err != nil || len(js) == 0 {
		t.Fatalf("htmx not embedded: %v", err)
	}
	if !regexp.MustCompile(`version:"2\.0\.4"`).Match(js) {
		t.Error("vendored htmx is not 2.0.4")
	}
	css, err := assets.ReadFile("static/app.css")
	if err != nil || len(css) == 0 {
		t.Fatalf("app.css not embedded: %v", err)
	}
	// The chart hover script: it follows the pointer over a 1h chart and
	// reads the hover target's text.
	detail, err := assets.ReadFile("static/detail.js")
	if err != nil {
		t.Fatalf("detail.js not embedded: %v", err)
	}
	for _, want := range []string{"pointermove", ".history", `addEventListener("input"`, ".controls"} {
		if !strings.Contains(string(detail), want) {
			t.Errorf("detail.js lacks %q: not the charts' hover readout and the concurrency inputs kept in step", want)
		}
	}
	// The primary colour is varmq's logo teal, which the native controls
	// (the concurrency slider, checkboxes) take too.
	for _, want := range []string{"#1A1B26", "#C0CAF5", "prefers-color-scheme: light", ".mixed", "--accent: #44BF9E", "accent-color: var(--accent)"} {
		if !strings.Contains(strings.ToUpper(string(css)), strings.ToUpper(want)) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

func TestSparklineHitTargetsAreBounded(t *testing.T) {
	// An hour of one-second points in a 120-wide box must not emit 3600
	// hover targets.
	pts := make([]fleet.Point, 3600)
	for i := range pts {
		pts[i] = ptAt(viewT0.Add(time.Duration(i)*time.Second), float64(i%7))
	}
	v := sparkline(pts, time.Hour, time.Second, 120, 26)
	if len(v.Hits) == 0 || len(v.Hits) > 130 {
		t.Errorf("hits = %d, want 1..130", len(v.Hits))
	}
	// At the default density, 5s polls over 10 minutes, every point keeps
	// its own target.
	if n := len(sparkline(series(viewT0, 5*time.Second, 121, 1), fleet.TrendWindow, 5*time.Second, 120, 26).Hits); n != 121 {
		t.Errorf("hits for 121 points = %d, want 121", n)
	}
}

func TestPathEscapeFunc(t *testing.T) {
	tmpl := template.Must(template.New("x").Funcs(funcs).Parse(`{{pathEscape .}}`))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, "a/b c"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a%2Fb%20c" {
		t.Errorf("pathEscape = %q", buf.String())
	}
}

// cssRule returns the declarations of the rule with exactly this selector.
func cssRule(t *testing.T, css, selector string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("app.css has no rule %q", selector)
	}
	return m[1]
}

func TestCSSPartialPointsAreHollowEverywhere(t *testing.T) {
	b, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	// ".failed .spark .dot" (0,3,0) outranks ".spark .partial" (0,2,0), so the
	// failed chart needs its own hollow fill or partial points turn solid red.
	for _, sel := range []string{".spark .partial", ".failed .spark .partial"} {
		if rule := cssRule(t, css, sel); !strings.Contains(rule, "fill: var(--panel)") {
			t.Errorf("%s does not restore the hollow fill: %s", sel, rule)
		}
	}
}

func TestCSSLeavesCopyVerbatim(t *testing.T) {
	// Mandated UI copy such as "Pending (max/pod)" must render as written.
	b, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "text-transform: uppercase") {
		t.Error("app.css uppercases text; headers must render exactly as templated")
	}
}

// TestControlsFollowBalancedScope: one balanced unit gets the note instead of
// controls; a scope holding one gets every control but Restart.
func TestControlsFollowBalancedScope(t *testing.T) {
	note := `<p class="note">` + template.HTMLEscapeString(oneUnitCopy) + `</p>`

	p := detailPage()
	p.ScopeBalanced = true
	body := run(t, "detail", "layout", p)
	if strings.Contains(body, "/actions/restart") || !strings.Contains(body, "/actions/stop") || !strings.Contains(body, "/concurrency") {
		t.Errorf("detail over a balanced scope should have every control but Restart:\n%s", body)
	}

	p.OneBalanced = true
	body = run(t, "detail", "layout", p)
	if !strings.Contains(body, note) || strings.Contains(body, "<form class=\"conc\"") || strings.Contains(body, "/actions/") {
		t.Errorf("detail of one balanced unit should have the note and no controls:\n%s", body)
	}

	d := dashboardPage()
	d.ScopeBalanced, d.OneBalanced = true, true
	body = run(t, "dashboard", "layout", d)
	if !strings.Contains(body, note) || strings.Contains(body, `id="bulk"`) {
		t.Errorf("dashboard of one balanced unit should have the note and no bulk form:\n%s", body)
	}
}

// TestStaleHidesCurrentValues: a stale page shows no current value, only
// "—", and keeps its history drawn.
func TestStaleHidesCurrentValues(t *testing.T) {
	const stale = "No new data for 1m0s — vartop web is not collecting."
	for set, p := range map[string]Page{"dashboard": dashboardPage(), "detail": detailPage(), "grid": gridPage()} {
		p.Note = "3 instances behind lb:8080"
		fresh := run(t, set, "layout", p)
		p.Stale = stale
		body := run(t, set, "layout", p)
		// Current state beyond the values: the balancers' note, a unit's
		// error and down mark, the units reporting the worker.
		for _, current := range []string{"3 instances behind lb:8080", `class="down"`, "connection refused", ">a</span>"} {
			if strings.Contains(fresh, current) && strings.Contains(body, current) {
				t.Errorf("%s: a stale page shows the current %q", set, current)
			}
		}
		if set == "detail" && !strings.Contains(body, `<p class="pods">Pods reporting: —</p>`) {
			t.Errorf("detail: a stale page lacks %q:\n%s", "Pods reporting: —", body)
		}
		_, banner, _ := strings.Cut(body, `<div id="banner" role="status">`)
		banner, _, _ = strings.Cut(banner, "</div>")
		if want := `<p class="banner stale">` + stale + `</p>`; !strings.Contains(banner, want) {
			t.Errorf("%s: banner lacks %q:\n%s", set, want, banner)
		}
		_, live, _ := strings.Cut(body, `<div id="live">`)
		for _, current := range []string{"12.5", ">9<", "87.5%", ">10<", `class="status active"`, "2/4", "4 ×2 pods"} {
			if strings.Contains(live, current) {
				t.Errorf("%s: a stale page shows the current value %q", set, current)
			}
		}
		if set != "grid" && !strings.Contains(live, "<polyline") {
			t.Errorf("%s: a stale page lost its history", set)
		}
	}
}

// TestFeedCacheEvicts: a server's feed is kept per poll interval and
// closed once no request read it for feedIdle.
func TestFeedCacheEvicts(t *testing.T) {
	c := newFeedCache(t.TempDir())
	t.Cleanup(c.close)
	var closed []*store.Feed
	c.closeFeed = func(f *store.Feed) error {
		closed = append(closed, f)
		return f.Close()
	}
	t0 := time.Now()
	a := c.get("a", time.Second, t0)
	if c.get("a", time.Second, t0.Add(time.Minute)) != a {
		t.Error("a second read of a got a new feed")
	}
	if c.get("a", 2*time.Second, t0.Add(time.Minute)) == a {
		t.Error("a's new interval kept its old feed")
	}
	a2 := c.get("a", 2*time.Second, t0.Add(time.Minute))
	c.get("b", time.Second, t0.Add(time.Minute+feedIdle+time.Second))
	if len(c.feeds) != 1 {
		t.Errorf("%d feeds after a's went idle, want b's alone", len(c.feeds))
	}
	if len(closed) != 2 || !slices.Contains(closed, a) || !slices.Contains(closed, a2) {
		t.Errorf("closed %d feeds, want a's two", len(closed))
	}
	if c.get("a", time.Second, t0.Add(time.Minute+feedIdle+time.Second)) == a {
		t.Error("a's idle feed was kept")
	}
}
