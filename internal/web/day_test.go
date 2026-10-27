package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
)

// ratedSmp is a running "orders" sample with completed/failed rates.
func ratedSmp(completed, failed float64) *fleet.Sample {
	s := smp("Running", 4, 1, 3, 0)
	s.CompletedRate, s.FailedRate, s.RateOK = completed, failed, true
	return s
}

// ordersAt is a snapshot at at of instance-1 and instance-2 (a and b),
// each running orders at completed/failed.
func ordersAt(at time.Time, a, b string, completed, failed float64) *fleet.Snapshot {
	return snap(at,
		labelled(okPod(a, map[string]*fleet.Sample{"orders": ratedSmp(completed, failed)}), "instance-1"),
		labelled(okPod(b, map[string]*fleet.Sample{"orders": ratedSmp(completed, failed)}), "instance-2"))
}

// figures is the Completed and the Failed chart of a worker page.
func figures(t *testing.T, body string) (completed, failed string) {
	t.Helper()
	parts := strings.Split(body, `<figure class="chart`)
	if len(parts) != 3 {
		t.Fatalf("want two charts, got %d:\n%s", len(parts)-1, body)
	}
	cut := func(s string) string { s, _, _ = strings.Cut(s, "</figure>"); return s }
	return cut(parts[1]), cut(parts[2])
}

// axisLabels is the x-axis labels of a chart.
func axisLabels(fig string) []string {
	_, axis, _ := strings.Cut(fig, `class="axis-x"`)
	axis, _, _ = strings.Cut(axis, "</div>")
	var out []string
	for _, m := range regexp.MustCompile(`>(\d\d:\d\d)<`).FindAllStringSubmatch(axis, -1) {
		out = append(out, m[1])
	}
	return out
}

// dayFixture serves a server whose orders ran at 9/s completed, 1/s
// failed (per instance) 20 hours ago, and at 1/s, 0 failed, in the last
// minute.
func dayFixture(t *testing.T) *fixture {
	t.Helper()
	_, _, aSrv, bSrv := twoFakes(t)
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 10, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}})
	f.claim()
	now := time.Now()
	f.write("multi", ordersAt(now.Add(-20*time.Hour), aSrv.URL, bSrv.URL, 9, 1))
	f.write("multi", ordersAt(now.Add(-30*time.Second), aSrv.URL, bSrv.URL, 1, 0))
	f.write("multi", ordersAt(now, aSrv.URL, bSrv.URL, 1, 0))
	return f
}

// TestDayChartSwitch: each chart of a worker page has its own 1h | 24h
// switch. With completed=24h the Completed chart spans the day — its axis
// every 6 hours, a point per 5 minutes with its average and peak, the
// day's highest poll in its caption — while the Failed chart keeps its
// hour; each switch links to the page with only its own chart changed.
func TestDayChartSwitch(t *testing.T) {
	f := dayFixture(t)
	_, body := fetch(t, f.srv, "/workers/orders?server=multi&completed=24h", false)
	completed, failed := figures(t, body)

	if !strings.Contains(completed, `<span class="peak">peak 18/s</span>`) || !strings.Contains(completed, " · avg 18.0/s · peak 18.0/s · 2/2 instances") {
		t.Errorf("the 24h Completed chart lacks the day's peak (18/s, two instances at 9) or its buckets' tooltips:\n%s", completed)
	}
	labels := axisLabels(completed)
	if len(labels) < 4 || slices.ContainsFunc(labels, func(l string) bool { return !slices.Contains([]string{"00:00", "06:00", "12:00", "18:00"}, l) }) {
		t.Errorf("24h axis labels = %v, want the clock every 6 hours", labels)
	}
	if strings.Contains(failed, "avg ") || !strings.Contains(failed, `<span class="peak">peak 0/s</span>`) {
		t.Errorf("the Failed chart should still show its hour:\n%s", failed)
	}

	link := func(fig, label string) (href string, current bool) {
		t.Helper()
		m := regexp.MustCompile(`<a href="([^"]*)"[^>]*?( aria-current="true")?[^>]*>` + label + `</a>`).FindStringSubmatch(fig)
		if m == nil {
			t.Fatalf("no %s switch in:\n%s", label, fig)
		}
		return html.UnescapeString(m[1]), m[2] != ""
	}
	if href, cur := link(completed, "24h"); !cur || href != "/workers/orders?completed=24h&server=multi" {
		t.Errorf("Completed 24h = %q current %v, want the current choice", href, cur)
	}
	if href, cur := link(completed, "1h"); cur || href != "/workers/orders?server=multi" {
		t.Errorf("Completed 1h = %q current %v, want a link dropping completed", href, cur)
	}
	if href, cur := link(failed, "24h"); cur || href != "/workers/orders?completed=24h&failed=24h&server=multi" {
		t.Errorf("Failed 24h = %q current %v, want a link keeping completed=24h", href, cur)
	}
	if href, cur := link(failed, "1h"); !cur || href != "/workers/orders?completed=24h&server=multi" {
		t.Errorf("Failed 1h = %q current %v, want the current choice", href, cur)
	}

	// The 2s refresh and the instance picker keep each chart's choice.
	if !strings.Contains(body, `id="poller" hidden hx-get="`+html.EscapeString("/workers/orders?completed=24h&server=multi")+`"`) {
		t.Errorf("the poller loses completed=24h:\n%s", body)
	}
	if !strings.Contains(body, `<input type="hidden" name="completed" value="24h">`) {
		t.Errorf("the instance picker loses completed=24h:\n%s", body)
	}

	// One instance's day: that unit's rates alone.
	_, body = fetch(t, f.srv, "/workers/orders?server=multi&instance=instance-2&failed=24h", false)
	if _, failed = figures(t, body); !strings.Contains(failed, `<span class="peak">peak 1/s</span>`) || !strings.Contains(failed, "· 1/1 instances") {
		t.Errorf("instance-2's 24h Failed chart = %s, want its own peak of 1/s", failed)
	}
}

// TestDayChartDefaultsToHour: without the parameters both charts show the
// hour, as before, with 1h the current choice.
func TestDayChartDefaultsToHour(t *testing.T) {
	f := dayFixture(t)
	_, body := fetch(t, f.srv, "/workers/orders?server=multi", false)
	completed, failed := figures(t, body)
	for _, fig := range []string{completed, failed} {
		if strings.Contains(fig, "avg ") || !regexp.MustCompile(`aria-current="true"[^>]*>1h</a>`).MatchString(fig) {
			t.Errorf("a default chart should show the hour, 1h current:\n%s", fig)
		}
	}
	if !strings.Contains(completed, `<span class="peak">peak 2/s</span>`) {
		t.Errorf("the hour's peak should be 2/s:\n%s", completed)
	}
}

// TestDayChartTopsUp: the day is read from the file once, then topped up
// with what the collector stores after.
func TestDayChartTopsUp(t *testing.T) {
	f := dayFixture(t)
	_, body := fetch(t, f.srv, "/workers/orders?server=multi&completed=24h", false)
	if c, _ := figures(t, body); !strings.Contains(c, "peak 18/s") {
		t.Fatalf("first read: want peak 18/s:\n%s", c)
	}
	f.write("multi", ordersAt(time.Now(), f.cfg.Servers["multi"].Endpoints[0], f.cfg.Servers["multi"].Endpoints[1], 25, 0))
	_, body = fetch(t, f.srv, "/workers/orders?server=multi&completed=24h", true)
	if c, _ := figures(t, body); !strings.Contains(c, "peak 50/s") {
		t.Errorf("after a new snapshot at 25/s per instance: want peak 50/s:\n%s", c)
	}
}

// TestActionKeepsDayChart: an action from a page showing a 24h chart
// refreshes that page's charts as they are, the day included.
func TestActionKeepsDayChart(t *testing.T) {
	f := dayFixture(t)
	req := postRequest(t, f.srv, "/workers/orders/actions/pause", url.Values{"server": {"multi"}})
	req.Header.Set("HX-Current-URL", f.srv.URL+"/workers/orders?server=multi&completed=24h")
	resp, body := send(t, f.srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", resp.StatusCode, body)
	}
	_, live, _ := strings.Cut(body, `hx-swap-oob="outerHTML"`)
	if c, _ := figures(t, live); !strings.Contains(c, "peak 18/s") || !strings.Contains(c, "avg ") {
		t.Errorf("the refreshed Completed chart lost its day:\n%s", c)
	}
}
