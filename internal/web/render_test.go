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

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
)

// The instance noun every server has, and a "pod" one that fixtures use to
// check that pages word themselves by the Noun they are given.
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
	// A day's axis has the clock every 6 hours, on the local clock across
	// a daylight-saving change too, and in a zone off the hour.
	t.Run("day", func(t *testing.T) {
		local := time.Local
		t.Cleanup(func() { time.Local = local })
		for _, tc := range []struct {
			zone   string
			newest time.Time
			want   []string
		}{
			{"America/New_York", time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC), []string{"12:00", "18:00", "00:00", "06:00"}},
			{"Asia/Kathmandu", time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC), []string{"12:00", "18:00", "00:00", "06:00"}},
		} {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("no zone data: %v", err)
			}
			time.Local = loc
			h := history(series(tc.newest.Add(-24*time.Hour), 5*time.Minute, 289, 1), 24*time.Hour, 5*time.Minute, 720, 120)
			var xs []string
			for _, tk := range h.XTicks {
				xs = append(xs, tk.Label)
			}
			if !slices.Equal(xs, tc.want) {
				t.Errorf("%s: day ticks = %q, want %q", tc.zone, xs, tc.want)
			}
		}
	})
	// Longer spans: local midnights, every day for a week (across New
	// York's fall-back), every 5th day (counted from 1970) for 30 days.
	t.Run("days", func(t *testing.T) {
		local := time.Local
		t.Cleanup(func() { time.Local = local })
		for _, tc := range []struct {
			zone   string
			newest time.Time
			span   time.Duration
			want   []string
		}{
			{"America/New_York", time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC), 7 * 24 * time.Hour,
				[]string{"Thu 29", "Fri 30", "Sat 31", "Sun 1", "Mon 2", "Tue 3", "Wed 4"}},
			{"Asia/Kathmandu", time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC), 30 * 24 * time.Hour,
				[]string{"29 Sep", "4 Oct", "9 Oct", "14 Oct", "19 Oct", "24 Oct"}},
			// Clocks that change at midnight: Santiago springs from 00:00 to
			// 01:00 on Sun 6 Sep, Havana falls back from 01:00 to 00:00 on
			// Sun 1 Nov. Each day still gets one tick.
			{"America/Santiago", time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC), 7 * 24 * time.Hour,
				[]string{"Thu 3", "Fri 4", "Sat 5", "Sun 6", "Mon 7", "Tue 8", "Wed 9"}},
			{"America/Havana", time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC), 7 * 24 * time.Hour,
				[]string{"Thu 29", "Fri 30", "Sat 31", "Sun 1", "Mon 2", "Tue 3", "Wed 4"}},
		} {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("no zone data: %v", err)
			}
			time.Local = loc
			step := fleet.StepFor(tc.span)
			h := history(series(tc.newest.Add(-tc.span), step, int(tc.span/step)+1, 1), tc.span, step, 720, 120)
			var xs []string
			for _, tk := range h.XTicks {
				xs = append(xs, tk.Label)
			}
			if !slices.Equal(xs, tc.want) {
				t.Errorf("%s %v: ticks = %q, want %q", tc.zone, tc.span, xs, tc.want)
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
	for _, k := range []string{"dashboard", "grid", "detail", "queues", "queue", "workflows", "workflow", "results", "confirm"} {
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
			StatusTip: "a: Running\nb: Paused", Concurrency: "mixed 3–5", AbsentOn: 2,
		},
	}
}

func dashboardPage() Page {
	return Page{
		Title: "vartop", Server: "dev", SelfURL: "/?server=dev", IntervalSec: 5,
		Noun:    podNoun,
		Servers: []string{"dev", "prod"},
		Pods:    []PodOption{{Label: "a"}, {Label: "b"}},
		Health:  fleet.HealthView{Healthy: 2, Total: 2},
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
		`class="health h-ok"`, "2/2 pods",
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
		"<h1>Dashboard</h1>",
		`data-label="Busy"`, // the phone layout's cell labels
		">mixed</span><small class=\"split\">a: Running\nb: Paused</small>", // a mixed status's breakdown is text, not only a title
		`hx-post="/bulk/pause"`,
		`hx-post="/bulk/resume"`,
		`<form class="pick" action="/" method="get" hx-get="/" hx-trigger="change" hx-target="body" hx-push-url="true" hx-sync="#page:replace">`,
		`<select name="server" id="pick-server">`, // ids let htmx keep focus across swaps
		`<a id="link-w-6f7264657273" href=`,
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

	if want := `">—</td>
    <td class="num" data-label="Busy">—</td>
    <td class="num" data-label="Idle">—</td>
    <td class="num" data-label="Pending (max/pod)">—</td>`; !strings.Contains(body, want) {
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
	wf := workflowView(fleet.WorkflowGroup{WorkflowRecord: fleettest.SampleWorkflows(time.Now().UnixMilli())[1].Record()}, "dev", time.Now())
	for set, page := range map[string]Page{
		"dashboard": dashboardPage(),
		"detail":    detailPage(),
		"grid":      gridPage(),
		"workflow":  {Server: "dev", OwnPoll: true, Workflow: wf},
	} {
		body := run(t, set, "layout", page)
		forms := strings.Split(body, "<form")[1:]
		if len(forms) == 0 {
			t.Fatalf("%s: no forms rendered", set)
		}
		for i, f := range forms {
			f, _, _ = strings.Cut(f, "</form>")
			// The server picker's select is the server.
			if !strings.Contains(f, `<input type="hidden" name="server" value="dev">`) && !strings.Contains(f, `<select name="server"`) {
				t.Errorf("%s: form %d lacks the server:\n%s", set, i, f)
			}
		}
	}
	// the cancel confirmation's form too
	body := run(t, "confirm", "workflow_confirm", WorkflowConfirmView{Server: "dev", ID: "w", At: 1, Noun: instanceNoun})
	if !strings.Contains(body, `<input type="hidden" name="server" value="dev">`) {
		t.Errorf("workflow_confirm lacks the server:\n%s", body)
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
		`class="down"`,
		`<small class="err">unreachable</small></th>`,                     // the header stays short...
		`<li><b>b</b> : <span class="err">connection refused</span></li>`, // ...the error is below
		`<h1>Pod grid</h1>`,
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
		`hx-trigger="change delay:600ms"`, // keys fire change per step: one write per pause
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
				"<label>"+n.Cap()+` <select name="`+n.Param+`"`,
				"All "+many+" (merged)",
				">"+n.Cap()+" grid</a>",
				`title="healthy / total `+many+`"`,
				">2/2 "+many+"</span>",
				"Pending (max/"+one+")",
				"Concurrency (per "+one+")",
				"absent on 2 "+many+"<",
				`<p class="sub">`+one+" × worker</p>",
			)
			body := run(t, "dashboard", "layout", p)
			if got := strings.Count(body, "Pending (max/"+one+")") - strings.Count(body, `data-label="Pending (max/`+one+`)"`); got != 2 {
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
	// The graph's script: pan and zoom, trace, panel, kept across swaps.
	wf, err := assets.ReadFile("static/workflow.js")
	if err != nil {
		t.Fatalf("workflow.js not embedded: %v", err)
	}
	for _, want := range []string{".dag-port", "htmx:afterSwap", "task-detail-", "ctrlKey", "Escape", "data-zoom", "dataset.taken", "CSS.escape(", ".wf-panel .body a[href="} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("workflow.js lacks %q", want)
		}
	}
	// The primary colour is varmq's logo teal, which the native controls
	// (the concurrency slider, checkboxes) take too.
	for _, want := range []string{"#0D0F14", "#E3E6ED", "prefers-color-scheme: light", ".mixed", "--accent: #44BF9E", "accent-color: var(--accent)"} {
		if !strings.Contains(strings.ToUpper(string(css)), strings.ToUpper(want)) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

// One unit down for good makes every merged point partial: only the ends of
// a partial stretch get a circle, so the line stays visible.
func TestSparklineMarksPartialStretchEnds(t *testing.T) {
	pts := series(viewT0, 5*time.Second, 10, 1)
	for i := range pts {
		pts[i].Partial = i >= 2 && i <= 7 || i == 9
	}
	v := sparkline(pts, fleet.TrendWindow, 5*time.Second, 120, 20)
	var got []string
	for _, d := range v.Dots {
		got = append(got, d.X)
	}
	line := coords(t, v.Lines[0])
	want := []string{num(line[2][0]), num(line[7][0]), num(line[9][0])}
	if !slices.Equal(got, want) {
		t.Errorf("partial dots at x %v, want %v: the stretch's ends and the lone one", got, want)
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

func TestOnset(t *testing.T) {
	at := time.Date(2026, 11, 5, 15, 4, 0, 0, time.Local)
	p := func(min int, v float64) fleet.Point {
		return fleet.Point{At: at.Add(time.Duration(min) * time.Minute), V: v}
	}
	for _, c := range []struct {
		pts  []fleet.Point
		want string
	}{
		{nil, ""},
		{[]fleet.Point{p(0, 1), p(1, 0)}, ""},
		{[]fleet.Point{p(0, 0), p(1, 2), p(2, 1)}, "since 15:05"},
		{[]fleet.Point{p(0, 1), {Gap: true}, p(2, 1)}, "since 15:06"},
		{[]fleet.Point{p(0, 1), p(1, 1)}, "for 1+ min"},                    // run fills the series: start not seen
		{[]fleet.Point{p(0, 1), p(10, 1)}, "for 10+ min"},                  // a full window
		{[]fleet.Point{p(0, 1), {At: at.Add(20 * time.Second), V: 1}}, ""}, // just started: too short to say
	} {
		if got := onset(c.pts); got != c.want {
			t.Errorf("onset(%v) = %q, want %q", c.pts, got, c.want)
		}
	}
}

// layers is the layer of each task of l.
func layers(l dagLayout) []int { return l.layer[:l.n] }

// rank is the order of task ids within their layers, top to bottom.
func rank(l dagLayout, ly int) []int {
	var out []int
	for _, id := range l.order[ly] {
		if id < l.n {
			out = append(out, id)
		}
	}
	return out
}

// A task's layer is the longest path to it from a root; an edge spanning
// layers runs through a dummy slot in each layer between.
func TestLayoutDAGLayers(t *testing.T) {
	// diamond: 0 -> 1, 2 -> 3
	l, _ := layoutDAG(4, [][]int{nil, {0}, {0}, {1, 2}})
	if got := layers(l); !slices.Equal(got, []int{0, 1, 1, 2}) {
		t.Errorf("diamond layers = %v", got)
	}
	if len(l.chains) != 4 || len(l.layer) != 4 {
		t.Errorf("diamond: %d edges, %d ids, want 4 and no dummy", len(l.chains), len(l.layer))
	}
	// 0 -> 1 -> 2 -> 3, and 0 -> 3: the long edge passes layers 1 and 2
	l, _ = layoutDAG(4, [][]int{nil, {0}, {1}, {2, 0}})
	if got := layers(l); !slices.Equal(got, []int{0, 1, 2, 3}) {
		t.Errorf("chain layers = %v", got)
	}
	long := l.chains[len(l.chains)-1]
	if len(long) != 4 || long[0] != 0 || long[3] != 3 || long[1] < 4 || long[2] < 4 {
		t.Fatalf("long edge chain = %v, want 0, two dummies, 3", long)
	}
	if d := l.path(long); strings.Count(d, " L") != 2 || strings.Count(d, " C") != 3 {
		t.Errorf("long edge path %q: want a straight run through each dummy, a curve in each gap", d)
	}
	// a dummy slot holds its column: no task box overlaps it
	for _, id := range long[1:3] {
		for _, other := range l.order[l.layer[id]] {
			if other < l.n && abs(l.y[other]-l.y[id]) < (dagNodeH+dagDummyH)/2 {
				t.Errorf("dummy %d at y %v overlaps task %d at %v", id, l.y[id], other, l.y[other])
			}
		}
	}
	if l.width != 2*dagPad+4*dagNodeW+3*dagGapX {
		t.Errorf("width = %v", l.width)
	}
}

func abs(f float64) float64 { return max(f, -f) }

// Barycenter sweeps uncross edges: children follow their parents' order.
// Ties (no neighbours) keep the producer's order.
func TestLayoutDAGOrders(t *testing.T) {
	// 0 and 1 roots; 2 under 1, 3 under 0: 3 belongs above 2
	l, _ := layoutDAG(4, [][]int{nil, nil, {1}, {0}})
	if got := rank(l, 1); !slices.Equal(got, []int{3, 2}) {
		t.Errorf("layer 1 order = %v, want [3 2] (no crossing)", got)
	}
	if l.y[3] >= l.y[2] {
		t.Errorf("y: 3 at %v, 2 at %v", l.y[3], l.y[2])
	}
	l, _ = layoutDAG(3, [][]int{nil, nil, nil})
	if got := rank(l, 0); !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("independent tasks reordered: %v", got)
	}
	// a lone task in a layer sits level with the middle of the tallest
	l, _ = layoutDAG(4, [][]int{nil, {0}, {0}, {1, 2}})
	if l.y[0] != l.y[3] || l.y[0] != (l.y[1]+l.y[2])/2 {
		t.Errorf("diamond not centred: %v", l.y)
	}
}

// varmq refuses cyclic plans; a bad record still lays out: deps out of
// range, on itself or closing a cycle get no edge, and nothing loops.
func TestLayoutDAGGuards(t *testing.T) {
	l, _ := layoutDAG(3, [][]int{{1}, {0}, {2, 7, -1}})
	if len(l.chains) != 1 {
		t.Errorf("chains = %v, want one edge of the cycle", l.chains)
	}
	for _, c := range l.chains {
		if l.layer[c[0]] >= l.layer[c[len(c)-1]] {
			t.Errorf("edge %v does not go forward", c)
		}
	}
	if l, _ := layoutDAG(0, nil); len(l.order) != 0 || l.height != 2*dagPad {
		t.Errorf("empty layout = %+v", l)
	}
	if l, _ := layoutDAG(2, nil); len(l.order) != 1 || len(l.chains) != 0 {
		t.Errorf("no deps given = %+v", l)
	}
	// a huge record: no graph, not a huge layout
	if _, ok := layoutDAG(dagMaxSlots+1, nil); ok {
		t.Error("more tasks than slots laid out")
	}
	fan := make([][]int, 200) // a chain of 100, each of the next 100 also on task 0: long edges
	for i := 1; i < 100; i++ {
		fan[i] = []int{i - 1}
	}
	for i := 100; i < 200; i++ {
		fan[i] = []int{0, 99}
	}
	if _, ok := layoutDAG(200, fan); ok {
		t.Error("~10000 dummies laid out")
	}
	// a dense fan-in: no dummy, but r*r edges, each a path on the page
	if _, ok := layoutDAG(2*40, fanIn(40)); !ok {
		t.Error("40x40 fan-in (1600 edges) not laid out")
	}
	if _, ok := layoutDAG(2*100, fanIn(100)); ok {
		t.Error("100x100 fan-in (10000 edges) laid out")
	}
}

// fanIn is r roots, then r tasks each on every root.
func fanIn(r int) [][]int {
	deps := make([][]int, 2*r)
	for i := r; i < 2*r; i++ {
		for p := range r {
			deps[i] = append(deps[i], p)
		}
	}
	return deps
}

// Graphs near the cap: a chain, one wide layer, a dense fan-in, long
// edges. Layout must stay cheap enough to run on every poll.
func BenchmarkLayoutDAG(b *testing.B) {
	chain := make([][]int, dagMaxSlots/2)
	for i := 1; i < len(chain); i++ {
		chain[i] = []int{i - 1}
	}
	wide := make([][]int, dagMaxSlots/2)
	for i := 1; i < len(wide); i++ {
		wide[i] = []int{0}
	}
	long := make([][]int, 100) // a chain of 50, the next 50 each on its head and tail
	for i := 1; i < 50; i++ {
		long[i] = []int{i - 1}
	}
	for i := 50; i < 100; i++ {
		long[i] = []int{0, 49}
	}
	for name, deps := range map[string][][]int{"chain": chain, "wide": wide, "fan-in": fanIn(48), "long-edges": long} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				if _, ok := layoutDAG(len(deps), deps); !ok {
					b.Fatal("over the cap")
				}
			}
		})
	}
}

// The workflow page model: a node per task, an edge per dep, dashed into an
// always task, animated into a task in flight while the workflow runs.
func TestWorkflowView(t *testing.T) {
	wfs := fleettest.SampleWorkflows(time.Now().UnixMilli())
	run := workflowView(fleet.WorkflowGroup{WorkflowRecord: wfs[1].Record(), On: []string{"a"}}, "dev", time.Now())
	if run.Ctl.Final || len(run.Graph.Nodes) != 7 || len(run.Graph.Edges) != 8 || !run.Running || run.Progress.Live != 2 || run.Progress.Ended() != 3 {
		t.Fatalf("running view: final %v, %d nodes, %d edges, progress %+v", run.Ctl.Final, len(run.Graph.Nodes), len(run.Graph.Edges), run.Progress)
	}
	var always, live int
	for _, e := range run.Graph.Edges {
		if e.Always {
			always++
			if run.Tasks[e.To].Name != "report" {
				t.Errorf("dashed edge into %s", run.Tasks[e.To].Name)
			}
		}
		if e.Live {
			live++
		}
	}
	if always != 2 || live != 2 {
		t.Errorf("always edges %d, live edges %d; want 2 and 2", always, live)
	}
	if tv := run.Tasks[2]; !tv.Live || tv.QueueShort != "audit" || tv.QueueURL != "/queues/redisq:shop:fifo:%7Baudit%7D?server=dev" || tv.Label != "audit-log, pending, after extract" {
		t.Errorf("audit-log = %+v", tv)
	}
	saga := workflowView(fleet.WorkflowGroup{WorkflowRecord: wfs[2].Record()}, "dev", time.Now())
	if !saga.Ctl.Final || saga.Tasks[0].Undo == nil || saga.Tasks[0].Undo.State.Class != "completed" || saga.Duration != "4.2s" {
		t.Errorf("saga = %+v", saga)
	}
	for _, tv := range saga.Tasks {
		if tv.Live {
			t.Errorf("task %s live in an ended workflow", tv.Name)
		}
	}
}

func TestWorkflowWords(t *testing.T) {
	for in, want := range map[string]StateView{
		"compensation_failed": {"compensation failed", "compensation-failed"},
		"running":             {"running", "running"},
		"paused":              {"paused", "unknown"}, // a state a later varmq adds: as it is
		"":                    {"unknown", "unknown"},
		"<b>":                 {"<b>", "unknown"},
	} {
		if got := stateView(in); got != want {
			t.Errorf("stateView(%q) = %+v, want %+v", in, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		400 * time.Millisecond: "0.4s", 38 * time.Second: "38s", 95 * time.Second: "1m 35s",
		2*time.Hour + 5*time.Minute: "2h 05m", 76 * time.Hour: "3d 4h", -time.Second: "—",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
	for id, want := range map[string]string{"redisq:shop:fifo:{orders}": "orders", "memory:a:reports": "reports", "plain": "plain"} {
		if got := queueShort(id); got != want {
			t.Errorf("queueShort(%q) = %q", id, got)
		}
	}
	for n, want := range map[int]string{812: "812 B", 9320: "9.1 KiB", 1_204_311: "1.1 MiB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
	if got := cut("transform-refunds-and-more", 20); got != "transform-refunds-a…" || cut("short", 20) != "short" {
		t.Errorf("cut = %q", got)
	}
}

// The workflow page: the graph is an SVG of focusable task links, each to
// its table row; #live polls itself while the workflow runs and not once
// it ended; the cancel buttons go with it.
func TestWorkflowRenders(t *testing.T) {
	wfs := fleettest.SampleWorkflows(time.Now().UnixMilli())
	page := func(i int) Page {
		v := workflowView(fleet.WorkflowGroup{WorkflowRecord: wfs[i].Record(), On: []string{"a", "b"}}, "dev", time.Now())
		p := Page{Title: "wf", Server: "dev", SelfURL: "/workflows/x?server=dev", OwnPoll: true, Workflow: v}
		if !v.Ctl.Final {
			p.IntervalSec = 5
		}
		return p
	}
	body := run(t, "workflow", "layout", page(1))
	for _, want := range []string{
		`<div id="live" hx-disinherit="*" hx-get="/workflows/x?server=dev" hx-trigger="every 5s" hx-select="#live"`,
		`<a class="node t-pending live" href="#task-2" data-i="2" aria-label="audit-log, pending, after extract"`,
		`<rect class="halo"`, `<path class="flow"`, `<path class="edge always"`, `marker-end="url(#dag-arrow)"`,
		`<tr id="task-6">`, `<div id="task-detail-6">`, `<span class="chip">always</span>`,
		`hx-post="/workflows/` + fleettest.RunningID + `/actions/cancel">Cancel workflow</button>`,
		`name="undo" value="false"`, `<input type="hidden" name="at" value="`, `<dt>On cancel</dt><dd>undo jobs run</dd>`,
		`<div id="results" aria-live="polite" hx-preserve="true"></div>`,
		`<script src="/static/workflow.js" defer></script>`,
		`aria-current="page"><svg class="ico" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false"><circle cx="3.5"`, // the Workflows nav entry
		// the hooks workflow.js queries
		`class="dagbox"`, `<div class="dag-tools" hidden>`, `data-zoom="fit"`, `<div class="dag-port" tabindex="0"`,
		`<svg class="dag"`, `data-w="`, `data-h="`, `<g class="view">`, `data-from="`, `data-to="`,
		`<aside class="wf-panel" hidden`, `<button type="button" class="close"`, `<div class="body"></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("running workflow page lacks %q", want)
		}
	}
	if strings.Contains(body, `id="poller"`) {
		t.Error("the layout poller polls too: #live polls itself")
	}

	body = run(t, "workflow", "layout", page(2))
	for _, want := range []string{`<g class="undo t-completed"`, "carrier API: 503", `<span class="wst s-compensated">compensated</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("compensated page lacks %q", want)
		}
	}
	for _, not := range []string{"hx-trigger", "Cancel workflow", "refresh every", `class="flow"`, `class="halo"`} {
		if strings.Contains(body, not) {
			t.Errorf("ended workflow page has %q", not)
		}
	}
	// no undo on cancel: one button
	if body := run(t, "workflow", "wfctl", WorkflowCtl{Server: "dev", ID: "w", At: 1}); !strings.Contains(body, "Cancel workflow") || strings.Contains(body, "without undo") {
		t.Errorf("wfctl without undo_on_cancel:\n%s", body)
	}
	rec := wfs[1].Record()
	rec.UndoOnCancel = false
	v := workflowView(fleet.WorkflowGroup{WorkflowRecord: rec, On: []string{"a"}}, "dev", time.Now())
	body = run(t, "workflow", "layout", Page{Server: "dev", OwnPoll: true, Workflow: v})
	if !strings.Contains(body, `<dt>On cancel</dt><dd>no undo jobs</dd>`) || !strings.Contains(body, "Cancel workflow") || strings.Contains(body, "without undo") {
		t.Errorf("running page without undo_on_cancel:\n%s", body)
	}

	// over the edge cap: no graph, a note, every task in the table
	var tasks []contract.TaskRecord
	for i, deps := range fanIn(100) {
		tr := contract.TaskRecord{Name: "t" + strconv.Itoa(i), State: contract.JobWaiting}
		for _, d := range deps {
			tr.Deps = append(tr.Deps, "t"+strconv.Itoa(d))
		}
		tasks = append(tasks, tr)
	}
	big := workflowView(fleet.WorkflowGroup{WorkflowRecord: contract.WorkflowRecord{ID: "big", State: contract.WorkflowRunning, CreatedAt: 1, Tasks: tasks}}, "dev", time.Now())
	body = run(t, "workflow", "layout", Page{Server: "dev", OwnPoll: true, Workflow: big})
	if !strings.Contains(body, "200 tasks: too many edges to draw a graph") || !strings.Contains(body, `<tr id="task-199">`) || strings.Contains(body, `<svg class="dag"`) {
		t.Errorf("over-cap workflow: no note, a graph, or tasks missing")
	}
}

// A producer names tasks, queues, reasons and the workflow: every one is
// text on the page, never markup (the panel copies #task-detail-N with
// innerHTML, so the server's escaping is all there is).
func TestWorkflowRendersHostileFields(t *testing.T) {
	const evil = `<script>alert(1)</script><img src=x onerror=y>`
	task := func(name string, deps ...string) contract.TaskRecord {
		return contract.TaskRecord{Name: name, State: contract.JobState(evil), QueueID: evil, JobID: evil, Reason: evil, Deps: deps}
	}
	a, b := task(evil+"a"), task(evil+"b", evil+"a")
	b.Undo = &contract.TaskRecord{Name: evil, State: contract.JobFailed, QueueID: evil, JobID: evil, Reason: evil}
	rec := contract.WorkflowRecord{ID: evil, State: contract.WorkflowState(evil), CreatedAt: 1, Tasks: []contract.TaskRecord{a, b}}
	v := workflowView(fleet.WorkflowGroup{WorkflowRecord: rec, On: []string{evil}}, "dev", time.Now())
	v.Others, v.Ambiguous, v.Failed = []WorkflowChoice{{URL: "/x", Created: evil, State: evil, On: []string{evil}}}, []string{evil}, []string{evil}
	for name, body := range map[string]string{
		"page":    run(t, "workflow", "layout", Page{Title: evil, Server: "dev", OwnPoll: true, Banner: evil, Workflow: v}),
		"confirm": run(t, "confirm", "workflow_confirm", WorkflowConfirmView{Server: "dev", ID: evil, On: []string{evil}, Noun: instanceNoun}),
		"list": run(t, "workflows", "layout", Page{Server: "dev", WorkflowList: &WorkflowsView{Read: true, Rows: []WorkflowRow{
			{ID: evil, URL: workflowURL(evil, "dev", 0), State: stateView(evil), Created: evil, Duration: evil, On: []string{evil}}}}}),
	} {
		if !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<img") || strings.Contains(body, "<script>alert") {
			t.Errorf("%s: a field rendered as markup", name)
		}
		if n, want := strings.Count(body, "<script"), map[string]int{"page": 2, "confirm": 0, "list": 1}[name]; n != want {
			t.Errorf("%s: %d <script, want %d (htmx, workflow.js)", name, n, want)
		}
	}

	// an ID with '/', '?' and '#': one path segment in every link and post
	const odd, esc = "a/b?c#d", "/workflows/a%2Fb%3Fc%23d"
	for name, c := range map[string]struct{ body, want string }{
		"list":    {run(t, "workflows", "layout", Page{Server: "dev", WorkflowList: &WorkflowsView{Read: true, Rows: []WorkflowRow{{ID: odd, URL: workflowURL(odd, "dev", 0)}}}}), `href="` + esc + `?server=dev"`},
		"wfctl":   {run(t, "workflow", "wfctl", WorkflowCtl{Server: "dev", ID: odd, At: 1}), `hx-post="` + esc + `/actions/cancel"`},
		"confirm": {run(t, "confirm", "workflow_confirm", WorkflowConfirmView{Server: "dev", ID: odd, Noun: instanceNoun}), `hx-post="` + esc + `/actions/cancel"`},
	} {
		if !strings.Contains(c.body, c.want) {
			t.Errorf("%s with ID %q lacks %s:\n%s", name, odd, c.want, c.body)
		}
	}
}

// Two tasks of one name: a dep on it is the first one's edge, as in the
// table, not the last's.
func TestWorkflowViewDuplicateName(t *testing.T) {
	rec := contract.WorkflowRecord{ID: "w", State: contract.WorkflowRunning, Tasks: []contract.TaskRecord{
		{Name: "a", State: contract.JobCompleted}, {Name: "a", State: contract.JobCompleted}, {Name: "b", State: contract.JobWaiting, Deps: []string{"a"}},
	}}
	v := workflowView(fleet.WorkflowGroup{WorkflowRecord: rec}, "dev", time.Now())
	if len(v.Graph.Edges) != 1 || v.Graph.Edges[0].From != 0 || v.Graph.Edges[0].To != 2 {
		t.Errorf("edges = %+v, want 0 -> 2", v.Graph.Edges)
	}
}
