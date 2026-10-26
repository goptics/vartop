package web

import (
	"bytes"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/goptics/vartop/internal/config"
)

// The nouns of a Kubernetes server and of every other. Fixtures that name
// no server pick one.
var (
	podNoun      = Noun{One: "pod", Many: "pods", Article: "a"}
	instanceNoun = Noun{One: "instance", Many: "instances", Article: "an"}
)

func TestNounFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *config.Server
		want Noun
	}{
		{"kubernetes", &config.Server{Kubernetes: &config.KubernetesRef{Context: "prod", Namespace: "jobs", Service: "varmq"}}, podNoun},
		{"static endpoints", &config.Server{Endpoints: []string{"http://a:1", "http://b:1"}}, instanceNoun},
		{"dns", &config.Server{DNS: &config.DNSRef{Host: "varmq-headless", Port: 8080}}, instanceNoun},
		{"nil", nil, instanceNoun},
	} {
		if got := NounFor(tc.srv); got != tc.want {
			t.Errorf("%s: NounFor = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		n         Noun
		cap, many string
	}{
		{podNoun, "Pod", "Pods"},
		{instanceNoun, "Instance", "Instances"},
		{Noun{}, "", ""}, // the zero Noun a bare Page carries must not panic
	} {
		if got := tc.n.Cap(); got != tc.cap {
			t.Errorf("%+v.Cap() = %q, want %q", tc.n, got, tc.cap)
		}
		if got := tc.n.CapMany(); got != tc.many {
			t.Errorf("%+v.CapMany() = %q, want %q", tc.n, got, tc.many)
		}
	}
}

func TestPageScope(t *testing.T) {
	opts := []PodOption{{Key: "#a", Label: "instance-1"}, {Key: "#b", Label: "instance-2"}}
	for _, tc := range []struct {
		p    Page
		want string
	}{
		{Page{Noun: podNoun}, "all pods"},
		{Page{Noun: instanceNoun}, "all instances"},
		{Page{Noun: instanceNoun, Pod: "#b", Pods: opts}, "instance-2"},
		{Page{Noun: instanceNoun, Pod: "#gone", Pods: opts}, "#gone"},
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

func pt(v float64) Point { return Point{V: v, Title: "t"} }

var gap = Point{Gap: true, Title: "no data"}

func TestPolylines(t *testing.T) {
	t.Run("a gap splits runs", func(t *testing.T) {
		runs := polylines([]Point{pt(1), pt(2), gap, pt(4)}, 30, 10)
		if len(runs) != 2 {
			t.Fatalf("runs = %q, want 2", runs)
		}
		first, second := coords(t, runs[0]), coords(t, runs[1])
		if len(first) != 2 || len(second) != 1 {
			t.Fatalf("run lengths = %d, %d, want 2, 1 (%q)", len(first), len(second), runs)
		}
		// x follows the point's index over the whole series: 0, 10, (gap at 20), 30.
		if first[0][0] != 0 || first[1][0] != 10 || second[0][0] != 30 {
			t.Errorf("x = %v %v, want 0 10 30", first, second)
		}
		// y is scaled to the series maximum (4): the max sits at the top, 2 at the middle.
		if second[0][1] != 0 || first[1][1] != 5 {
			t.Errorf("y = %v %v, want max at 0 and 2 at 5", first, second)
		}
	})
	t.Run("empty draws nothing", func(t *testing.T) {
		if runs := polylines(nil, 30, 10); len(runs) != 0 {
			t.Errorf("runs = %q, want none", runs)
		}
		if runs := polylines([]Point{gap, gap}, 30, 10); len(runs) != 0 {
			t.Errorf("all-gap runs = %q, want none", runs)
		}
	})
	t.Run("one point is one coordinate", func(t *testing.T) {
		runs := polylines([]Point{pt(3)}, 30, 10)
		if len(runs) != 1 || len(coords(t, runs[0])) != 1 {
			t.Errorf("runs = %q, want one run of one coordinate", runs)
		}
	})
	t.Run("all zero is a flat line at the bottom", func(t *testing.T) {
		runs := polylines([]Point{pt(0), pt(0), pt(0)}, 30, 10)
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

func sampleRows() []RowView {
	return []RowView{
		{
			Name: "orders", ID: "w-6f7264657273", Status: "Running", Class: "active",
			StatusTip: "a: Running\nb: Running", Concurrency: "4 ×2 pods", Busy: 3, Idle: 5,
			PendingMax: 7, PendingTip: "a: 7\nb: 2",
			Spark: []Point{pt(1), pt(2), {V: 3, Partial: true, Title: "half"}},
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
		Pods:    []PodOption{{Key: "http://a:3000", Label: "a"}, {Key: "http://b:3000", Label: "b"}},
		Health:  HealthView{Healthy: 2, Total: 2, Dot: "●"},
		Dashboard: &DashboardView{
			KPIs: KPIView{
				CompletedPerSec: 12.5, FailedPerMin: 3, PendingMax: 9,
				StatusCounts: map[string]int{"active": 3, "paused": 1},
				Completed:    []Point{pt(1), pt(12.5)}, Failed: []Point{pt(0), pt(3)}, Pending: []Point{pt(9)},
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
		`hx-post="/server"`,
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
	// Bulk checkboxes belong to the bulk form that sits outside #live.
	if !strings.Contains(body, `form="bulk"`) {
		t.Error("checkbox is not tied to the bulk form")
	}
}

func TestKPIShowsDashWhenLastPointIsGap(t *testing.T) {
	p := dashboardPage()
	p.Dashboard.KPIs.CompletedPerSec = 99.9
	p.Dashboard.KPIs.Completed = []Point{pt(1), gap}
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
			if !strings.Contains(f, `<input type="hidden" name="server" value="dev">`) {
				t.Errorf("%s: form %d lacks hidden server:\n%s", set, i, f)
			}
		}
	}
}

func TestEmptyPagesRender(t *testing.T) {
	t.Run("dashboard without workers", func(t *testing.T) {
		p := dashboardPage()
		p.Dashboard = &DashboardView{KPIs: KPIView{StatusCounts: map[string]int{}}}
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
		p.Dashboard, p.Grid = nil, &GridView{}
		body := run(t, "grid", "layout", p)
		if !strings.Contains(body, "No pods") {
			t.Error("empty grid has no empty-state copy")
		}
	})
	t.Run("connecting", func(t *testing.T) {
		body := run(t, "dashboard", "layout", Page{Title: "vartop", Connecting: "Connecting to dev…", SelfURL: "/"})
		if !strings.Contains(body, "Connecting to dev…") {
			t.Error("connecting copy missing")
		}
		if strings.Contains(body, "every 0s") {
			t.Error("poller with a zero interval")
		}
		if !strings.Contains(body, `<meta http-equiv="refresh" content="2">`) {
			t.Error("connecting page does not reload itself")
		}
		if strings.Contains(body, `hx-select="#live"`) {
			t.Error("connecting page polls #live only, so the header would never update")
		}
	})
	t.Run("startup error offers retry", func(t *testing.T) {
		body := run(t, "dashboard", "layout", Page{Title: "vartop", Server: "dev", Err: "dial tcp: refused"})
		for _, want := range []string{"dial tcp: refused", "Retry", `hx-post="/server"`, `name="name" value="dev"`} {
			if !strings.Contains(body, want) {
				t.Errorf("body lacks %q", want)
			}
		}
	})
	t.Run("stale banner", func(t *testing.T) {
		p := Page{Title: "vartop", Server: "old", Banner: "Server changed to new — reload", IntervalSec: 5, SelfURL: "/?server=old"}
		body := run(t, "dashboard", "layout", p)
		if !strings.Contains(body, `id="banner"`) || !strings.Contains(body, "Server changed to new — reload") {
			t.Error("banner missing")
		}
		if !strings.Contains(body, `hx-select-oob="#banner,#status"`) {
			t.Error("poller cannot deliver the banner to a stale tab")
		}
		if !strings.Contains(body, `id="live"`) {
			t.Error("#live must exist so the poll swap has something to replace")
		}
		if strings.Contains(body, "Retry") {
			t.Error("banner page offers Retry")
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
	p.Grid = &GridView{
		Pods: []PodColumn{{Key: "http://a:3000", Label: "a"}, {Key: "http://b:3000", Label: "b", Err: "connection refused"}},
		Rows: []GridRow{{Name: "orders", Cells: []GridCell{
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
	p.Grid.Rows[0].Cells[1] = GridCell{Unreached: true}
	body = run(t, "grid", "layout", p)
	if !strings.Contains(body, `<span class="na">not reached</span>`) {
		t.Errorf("unreached cell not rendered:\n%s", body)
	}
}

// Hover text of a unit's label is its Tip, never its key.
func TestTipsAreTitles(t *testing.T) {
	const tip = "inst-a via http://a:3000"
	for name, body := range map[string]string{
		"select option": run(t, "dashboard", "layout", Page{Server: "dev", SelfURL: "/", Pods: []PodOption{{Key: "#a", Label: "instance-1", Tip: tip}}}),
		"grid column": run(t, "grid", "layout", Page{Server: "dev", Grid: &GridView{
			Pods: []PodColumn{{Key: "#a", Label: "instance-1", Tip: tip}},
			Rows: []GridRow{{Name: "w", Cells: []GridCell{{}}}},
		}}),
		"detail pods": run(t, "detail", "layout", Page{Server: "dev", Detail: &DetailView{Name: "w", Pods: []PodColumn{{Key: "#a", Label: "instance-1", Tip: tip}}}}),
		"confirm":     run(t, "confirm", "confirm", ConfirmView{Action: "pause", Pods: []PodColumn{{Key: "#a", Label: "instance-1", Tip: tip}}}),
		"result":      run(t, "results", "results", ResultsView{Results: []ActionResult{{Pod: "instance-1", Key: "#a", Tip: tip, Outcome: "ok"}}, Noun: podNoun}),
	} {
		if !strings.Contains(body, `title="`+tip+`">instance-1<`) {
			t.Errorf("%s: label lacks the tip as title:\n%s", name, body)
		}
	}
}

func detailPage() Page {
	p := dashboardPage()
	p.Dashboard = nil
	p.Detail = &DetailView{
		Name: "a/b c", Status: "Running", Class: "active", StatusTip: "a: Running",
		Submitted: 10, Completed: 8, Successful: 7, Failed: 1, SuccessRate: "87.5%",
		ConcurrencyStart: 4, ConcurrencyMax: 64,
		Completed1h: []Point{pt(1), pt(2)}, Failed1h: []Point{pt(0), pt(0)},
		Pods: []PodColumn{{Key: "http://a:3000", Label: "a"}},
	}
	return p
}

func TestDetailRenders(t *testing.T) {
	p := detailPage()
	body := run(t, "detail", "layout", p)
	for _, want := range []string{
		"87.5%",
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
	p.Pod = "http://a:3000"
	body = run(t, "detail", "layout", p)
	for _, want := range []string{`hx-confirm="Stop a/b c on a (dev)?"`, `name="pod" value="http://a:3000"`} {
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
	body := run(t, "results", "results", ResultsView{Noun: podNoun, Results: []ActionResult{
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
		Pods: []PodColumn{{Key: "http://a:3000", Label: "pod-a"}, {Key: "http://b:3000", Label: "pod-b", Err: "<i>down</i>"}},
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
	for _, n := range []Noun{podNoun, instanceNoun} {
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
				"<label>"+n.Cap()+` <select name="pod">`,
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
			p.Grid = &GridView{}
			check(t, "empty grid", run(t, "grid", "layout", p), "No "+many+" to show.")

			check(t, "confirm", run(t, "confirm", "confirm", ConfirmView{
				Action: "pause", Server: "dev", Noun: n,
				Pods: []PodColumn{{Key: "#a", Label: "a"}},
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
	for _, want := range []string{"#1A1B26", "#C0CAF5", "prefers-color-scheme: light", ".mixed"} {
		if !strings.Contains(strings.ToUpper(string(css)), strings.ToUpper(want)) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

func TestSparklineHitTargetsAreBounded(t *testing.T) {
	// 3600 one-second points in a 120-wide box must not emit 3600 hover targets.
	pts := make([]Point, 3600)
	for i := range pts {
		pts[i] = pt(float64(i % 7))
	}
	v := sparkline(pts, 120, 26)
	if len(v.Hits) == 0 || len(v.Hits) > 130 {
		t.Errorf("hits = %d, want 1..130", len(v.Hits))
	}
	// At the default density every point keeps its own target.
	if n := len(sparkline(pts[:120], 120, 26).Hits); n != 120 {
		t.Errorf("hits for 120 points = %d, want 120", n)
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
