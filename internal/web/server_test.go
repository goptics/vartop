package web

import (
	"errors"
	"html"
	"html/template"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

func TestListenRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "[::]:0"} {
		ln, err := Listen(addr)
		if err == nil {
			_ = ln.Close()
			t.Errorf("Listen(%q) succeeded, want an error", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("Listen(%q) error %q does not say loopback is required", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		ln, err := Listen(addr)
		if err != nil {
			t.Errorf("Listen(%q): %v", addr, err)
			continue
		}
		if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
			t.Errorf("Listen(%q) bound %v, want a loopback IP", addr, ip)
		}
		_ = ln.Close()
	}
}

func TestListenPortInUse(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	for _, again := range []string{addr, "localhost:" + portOf(t, addr)} {
		ln2, err := Listen(again)
		if err == nil {
			_ = ln2.Close()
			t.Errorf("second Listen(%q) succeeded, want an error", again)
			continue
		}
		if !strings.Contains(err.Error(), again) {
			t.Errorf("error %q does not name %s", err, again)
		}
	}
}

func TestAllowedHosts(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := portOf(t, ln.Addr().String())
	got := AllowedHosts(ln)
	for _, want := range []string{"127.0.0.1:" + p, "localhost:" + p} {
		if !slices.Contains(got, want) {
			t.Errorf("AllowedHosts = %q, missing %s", got, want)
		}
	}
	if slices.Contains(got, "[::1]:"+p) {
		t.Errorf("AllowedHosts = %q lists [::1] for an IPv4 listener", got)
	}

	ln6, err := Listen("[::1]:0")
	if err != nil {
		t.Logf("no IPv6 loopback here, skipping [::1]: %v", err)
		return
	}
	defer ln6.Close()
	p6 := portOf(t, ln6.Addr().String())
	if got := AllowedHosts(ln6); !slices.Contains(got, "[::1]:"+p6) || !slices.Contains(got, "localhost:"+p6) {
		t.Errorf("AllowedHosts(::1) = %q, want [::1]:%s and localhost:%s", got, p6, p6)
	}
}

func TestWrongHostForbidden(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)
	port := portOf(t, srv.Listener.Addr().String())

	for _, c := range []struct {
		method, host, path string
		want               int
	}{
		{"GET", "evil.example:" + port, "/", http.StatusForbidden},
		{"GET", "evil.example:" + port, "/static/app.css", http.StatusForbidden},
		{"GET", "127.0.0.1:1", "/", http.StatusForbidden},
		{"GET", "127.0.0.1", "/", http.StatusForbidden},
		{"POST", "evil.example:" + port, "/", http.StatusForbidden},
		{"GET", "LOCALHOST:" + port, "/", http.StatusOK},
		{"GET", "localhost:" + port, "/grid", http.StatusOK},
		{"GET", "127.0.0.1:" + port, "/static/app.css", http.StatusOK},
	} {
		req, err := http.NewRequest(c.method, srv.URL+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = c.host
		req.Header.Set("HX-Request", "true")
		resp, _ := send(t, srv, req)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s with Host %q = %d, want %d", c.method, c.path, c.host, resp.StatusCode, c.want)
		}
		// No other site may frame the UI and trick clicks onto its controls.
		if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Content-Security-Policy") != "frame-ancestors 'none'" {
			t.Errorf("%s %s with Host %q: response allows framing", c.method, c.path, c.host)
		}
	}
}

// TestPOSTGuards covers the guards in front of every POST route. Nothing
// handles POST /, so a request that gets through them is a 405.
func TestPOSTGuards(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, c := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no HX-Request", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusForbidden},
		{"HX-Request not true", map[string]string{"HX-Request": "1"}, http.StatusForbidden},
		{"cross-site fetch", map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign Origin", map[string]string{"HX-Request": "true", "Origin": "http://evil.example"}, http.StatusForbidden},
		{"same-origin htmx", map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "same-origin"}, http.StatusMethodNotAllowed},
	} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader("server=multi"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if resp, _ := send(t, srv, req); resp.StatusCode != c.want {
			t.Errorf("%s: POST / = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestPagesRender(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)
	labels := fleettest.StaticLabels(aSrv.URL, bSrv.URL)

	for _, c := range []struct {
		path string
		want []string
	}{
		{"/", []string{"orders", "emails", `hx-get="/?server=multi"`, "<title>vartop · multi</title>"}},
		{"/grid", []string{"orders", "emails", `hx-get="/grid?server=multi"`, "<title>Instance grid · multi</title>"}},
		{"/workers/orders", []string{"orders", "Instances reporting:", `hx-get="/workers/orders?server=multi"`, "<title>orders · multi</title>"}},
	} {
		status, body := fetch(t, srv, c.path, false)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", c.path, status)
			continue
		}
		for _, want := range append(c.want, "2/2 instances", aSrv.URL, bSrv.URL) {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s lacks %q", c.path, want)
			}
		}
	}

	t.Run("instance filter", func(t *testing.T) {
		_, body := fetch(t, srv, "/?instance="+labels[bSrv.URL], false)
		if !strings.Contains(body, ">orders<") || strings.Contains(body, ">emails<") {
			t.Error("dashboard filtered to b should show orders and not emails")
		}
		// A label never seen (a bad bookmark) says so, not the merged view.
		_, body = fetch(t, srv, "/?instance=instance-9", false)
		if !strings.Contains(body, "instance-9 is not an instance of multi") || strings.Contains(body, `<tr id="w-`) {
			t.Error("an unknown instance should show the unknown-label banner and no rows")
		}
	})

	// The grid shows every unit, so it offers no unit select.
	t.Run("instance select", func(t *testing.T) {
		for path, want := range map[string]bool{"/": true, "/workers/orders": true, "/grid": false, "/grid?server=b": false} {
			if _, body := fetch(t, srv, path, false); strings.Contains(body, `<select name="instance">`) != want {
				t.Errorf("GET %s: instance select shown = %v, want %v", path, !want, want)
			}
		}
	})

	t.Run("unknown worker", func(t *testing.T) {
		for _, path := range []string{"/workers/nope", "/workers/emails?instance=" + labels[bSrv.URL]} {
			status, body := fetch(t, srv, path, false)
			if status != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, status)
			}
			if !strings.Contains(body, "not found") {
				t.Errorf("GET %s should say not found", path)
			}
			// A poll gets a 200 so htmx swaps the banner in instead of
			// wiping #errors.
			if status, body := fetch(t, srv, path, true); status != http.StatusOK || !strings.Contains(body, `<p class="banner">`) {
				t.Errorf("htmx GET %s = %d, want 200 with the banner", path, status)
			}
		}
		if status, _ := fetch(t, srv, "/nope", false); status != http.StatusNotFound {
			t.Errorf("GET /nope = %d, want 404", status)
		}
	})
}

// TestPagesSayInstanceOnStaticServers: every page of a server whose endpoints
// are listed names its units instances, never pods.
func TestPagesSayInstanceOnStaticServers(t *testing.T) {
	srv := serveUnits(t, config.Server{}, threeFakes(t)...)

	checkPages(t, srv, "pod", map[string][]string{
		"/": {
			"All instances (merged)", ">Instance grid<", "3/3 instances", `title="healthy / total instances"`,
			"Pending (max/instance)", "Concurrency (per instance)", "instance × worker", "4 ×3 instances", "absent on 2 instances",
		},
		"/grid": {">Instance grid<", "<title>Instance grid · multi</title>", "3/3 instances"},
		"/workers/orders": {
			"All instances (merged)", "Instances reporting:", "3/3 instances",
			"per instance, on all instances", `aria-label="Concurrency per instance"`, `aria-label="Concurrency per instance, exact"`,
		},
	})
}

// TestPagesSayPodOnKubernetes: a Kubernetes server reads as it always did,
// except for the subtitle of the status card, which no longer ends in
// "instances".
func TestPagesSayPodOnKubernetes(t *testing.T) {
	srv := serveUnits(t, kubernetesServer, threeFakes(t)...)

	checkPages(t, srv, "instance", map[string][]string{
		"/": {
			"All pods (merged)", ">Pod grid<", "3/3 pods", `title="healthy / total pods"`,
			"Pending (max/pod)", "Concurrency (per pod)", `<p class="sub">pod × worker</p>`, "4 ×3 pods", "absent on 2 pods",
		},
		"/grid": {">Pod grid<", "<title>Pod grid · multi</title>", "3/3 pods"},
		"/workers/orders": {
			"All pods (merged)", "Pods reporting:", "3/3 pods",
			"per pod, on all pods", `aria-label="Concurrency per pod"`, `aria-label="Concurrency per pod, exact"`,
		},
	})
}

// TestMessagesUseNoun: what the handlers say when a request misses names the
// server's units by its noun, with the right article.
func TestMessagesUseNoun(t *testing.T) {
	for _, tc := range []struct {
		name         string
		server       config.Server
		one, article string
	}{
		{"static", config.Server{}, "instance", "an"},
		{"kubernetes", kubernetesServer, "pod", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveUnits(t, tc.server, threeFakes(t)...)
			one := tc.one

			// A form naming a unit the server does not have.
			resp, body := post(t, srv, "/workers/orders/actions/pause", multi(tc.one, "nine"), true)
			if want := "nine is not " + tc.article + " " + one + " of multi"; resp.StatusCode != http.StatusConflict || !strings.Contains(body, want) {
				t.Errorf("unknown unit = %d %q, want 409 with %q", resp.StatusCode, body, want)
			}

			// emails lives on one unit; the others answer 404.
			_, body = post(t, srv, "/workers/emails/actions/resume", multi(), true)
			if want := "no change (not on this " + one + ")"; !strings.Contains(resultsOf(body), want) {
				t.Errorf("404 outcome lacks %q:\n%s", want, resultsOf(body))
			}

			// The confirmation panel.
			_, body = post(t, srv, "/bulk/pause", multi("names", "orders", "names", "emails"), true)
			for _, want := range []string{"pause each worker on each " + one + " below:", `<span class="na">not on this ` + one + `</span>`} {
				if !strings.Contains(body, want) {
					t.Errorf("confirm panel lacks %q:\n%s", want, body)
				}
			}

			// A worker no unit reports, to a full load and to a poll.
			for _, hx := range []bool{false, true} {
				_, body = fetch(t, srv, "/workers/nope", hx)
				if want := "Worker &#34;nope&#34; not found on any " + one; !strings.Contains(body, want) {
					t.Errorf("GET /workers/nope (hx %v) lacks %q:\n%s", hx, want, body)
				}
			}
		})
	}
}

// TestGoneUnitBannerUsesNoun: a label the newest snapshot lacks is gone
// when an older one had it, and unknown when none did; the banner words
// both by the server's noun.
func TestGoneUnitBannerUsesNoun(t *testing.T) {
	both := snap(atTick(0), fleet.PodSnapshot{Key: "#a", Label: "instance-1"}, fleet.PodSnapshot{Key: "#b", Label: "instance-2"})
	left := snap(atTick(1), fleet.PodSnapshot{Key: "#a", Label: "instance-1"})
	for _, tc := range []struct {
		n             fleet.Noun
		label, banner string
	}{
		{instanceNoun, "instance-2", "Instance instance-2 is no longer reported — reload"},
		{podNoun, "instance-2", "Pod instance-2 is no longer reported — reload"},
		{instanceNoun, "instance-9", "instance-9 is not an instance of multi"},
		{podNoun, "web-0", "web-0 is not a pod of multi"},
	} {
		p := Page{Server: "multi", Pod: tc.label, Noun: tc.n}
		if !podGone(&p, []*fleet.Snapshot{both, left}) || p.Banner != tc.banner || p.Gone != tc.label {
			t.Errorf("%s %s: banner %q, gone %q; want %q", tc.n.One, tc.label, p.Banner, p.Gone, tc.banner)
		}
	}
	p := Page{Server: "multi", Pod: "instance-1", Noun: instanceNoun}
	if podGone(&p, []*fleet.Snapshot{both, left}) || p.Banner != "" {
		t.Errorf("a label the newest snapshot has is gone: %q", p.Banner)
	}
}

// TestVanishedPodShowsBanner: a page filtered to a unit the newest snapshot
// lacks says so and shows no data, never another unit's under its label.
func TestVanishedPodShowsBanner(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	src := &mutableSource{eps: []string{aSrv.URL, bSrv.URL}}
	f := unitsFixture(t, config.Server{}, src)
	srv := f.srv
	label := fleettest.StaticLabels(aSrv.URL, bSrv.URL)[aSrv.URL]

	dash := "/?instance=" + label + "&server=multi"
	detail := "/workers/orders?instance=" + label + "&server=multi"
	if _, body := fetch(t, srv, dash, false); !strings.Contains(body, ">emails<") {
		t.Fatal("dashboard filtered to a lacks emails before a leaves")
	}

	src.set(bSrv.URL) // a leaves discovery
	f.waitFor("multi", "a snapshot without a", func(s *fleet.Snapshot) bool { return len(s.Pods) == 1 })

	// Labelled as before, with a way out to the merged view of the same
	// page: the picker shows the gone unit chosen, so picking All instances
	// (merged) there is a change.
	merged := map[string]string{dash: "/?server=multi", detail: "/workers/orders?server=multi"}
	for _, path := range []string{dash, detail} {
		banner := `<p class="banner">Instance ` + label + ` is no longer reported — reload · <a href="` + merged[path] + `">All instances (merged)</a></p>`
		for _, hx := range []bool{true, false} { // a poll, a full load
			status, body := fetch(t, srv, path, hx)
			if status != http.StatusOK || !strings.Contains(body, banner) {
				t.Errorf("GET %s (hx %v) = %d, want 200 with %q", path, hx, status, banner)
			}
			_, picker, _ := strings.Cut(body, `<select name="instance">`)
			picker, _, _ = strings.Cut(picker, `</select>`)
			if gone := `<option value="` + label + `" selected disabled>` + label + `</option>`; strings.Count(picker, " selected") != 1 || !strings.Contains(picker, gone) {
				t.Errorf("GET %s (hx %v): picker %q, want only %q selected", path, hx, picker, gone)
			}
			for _, data := range []string{">orders<", `<tr id="w-`, "Instances reporting", `id="bulk"`, `class="controls"`, "No workers"} {
				if strings.Contains(body, data) {
					t.Errorf("GET %s (hx %v) shows %q", path, hx, data)
				}
			}
		}
	}
	// The full page keeps polling the unit: each poll still shows the
	// banner and nothing else, never a merged fallback (and the unit's data
	// again should it come back).
	_, body := fetch(t, srv, dash, false)
	_, poller, _ := strings.Cut(body, `<div id="poller"`)
	_, pollURL, _ := strings.Cut(poller, `hx-get="`)
	pollURL, _, _ = strings.Cut(pollURL, `"`)
	pollURL = html.UnescapeString(pollURL)
	if !strings.Contains(pollURL, "instance="+label) {
		t.Fatalf("full load of a gone unit polls %q, want its label kept", pollURL)
	}
	_, body = fetch(t, srv, pollURL, true)
	if !strings.Contains(body, "Instance "+label+" is no longer reported — reload") || strings.Contains(body, `<tr id="w-`) || strings.Contains(body, ">orders<") {
		t.Errorf("poll of %s should show the banner and no rows:\n%s", pollURL, body)
	}
}

// TestErroredUnitSaysWhy: a page filtered to a unit that is down says why in
// the banner, not that no workers were reported or that the worker is not
// found.
func TestErroredUnitSaysWhy(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	f := startLive(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 3600, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}}, "multi")
	// The first poll ran and the next is an hour away: this snapshot is the latest.
	down := fleet.PodSnapshot{Key: "#x", Via: aSrv.URL, Instance: "x", Label: "instance-3", Balanced: true, Err: "not reached this refresh · last seen 40s ago"}
	f.write("multi", snap(time.Now(), okPod(aSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 0, 4, 0)}), down))

	banner := `<p class="banner">instance-3: not reached this refresh · last seen 40s ago</p>`
	for _, path := range []string{"/?instance=instance-3", "/workers/orders?instance=instance-3"} {
		for _, hx := range []bool{true, false} { // a poll, a full load
			status, body := fetch(t, f.srv, path, hx)
			if status != http.StatusOK || !strings.Contains(body, banner) {
				t.Errorf("GET %s (hx %v) = %d, want 200 with %q", path, hx, status, banner)
			}
			for _, wrong := range []string{"No workers reported yet.", "not found"} {
				if strings.Contains(body, wrong) {
					t.Errorf("GET %s (hx %v) says %q", path, hx, wrong)
				}
			}
		}
	}
}

// TestStatusShowsUpdateTime: #status, which every poll refreshes, shows the
// newest snapshot's time, so a page whose polls stopped is visibly stale.
func TestStatusShowsUpdateTime(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, f := newTestServer(t, aSrv, bSrv)

	for _, path := range []string{"/", "/grid", "/workers/orders"} {
		before := stamp(f.latest("multi"))
		_, body := fetch(t, srv, path, true)
		after := stamp(f.latest("multi")) // a tick may land mid-request
		_, status, _ := strings.Cut(body, `<div id="status">`)
		status, _, _ = strings.Cut(status, "</div>")
		if !strings.Contains(status, "updated "+before) && !strings.Contains(status, "updated "+after) {
			t.Errorf("GET %s: #status lacks \"updated %s\":\n%s", path, after, status)
		}
	}
}

// TestRenderErrorIsEscaped: htmx inserts a 500 body into #errors as HTML.
func TestRenderErrorIsEscaped(t *testing.T) {
	boom := template.Must(template.New("boom").Funcs(template.FuncMap{
		"fail": func() (string, error) { return "", errors.New("<b>bad</b>") },
	}).Parse(`{{define "layout"}}{{fail}}{{end}}`))
	h := &handler{tmpl: map[string]*template.Template{"dashboard": boom}}

	rec := httptest.NewRecorder()
	h.render(rec, "dashboard", http.StatusOK, Page{})
	if body := rec.Body.String(); rec.Code != http.StatusInternalServerError || strings.Contains(body, "<b>") || !strings.Contains(body, "&lt;b&gt;bad&lt;/b&gt;") {
		t.Errorf("render error = %d %q, want 500 with the error escaped", rec.Code, body)
	}
}

func TestDetailOfEscapedName(t *testing.T) {
	_, fSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"a/b c": {Status: "Running", Concurrency: 1}})
	srv, _ := newTestServer(t, fSrv)

	status, body := fetch(t, srv, "/workers/a%2Fb%20c", false)
	if status != http.StatusOK {
		t.Fatalf("GET /workers/a%%2Fb%%20c = %d, want 200", status)
	}
	// The poller and the unit select must reach the same worker again.
	for _, want := range []string{`<h2>a/b c</h2>`, `hx-get="/workers/a%2Fb%20c?server=multi"`, `hx-get="/workers/a%2Fb%20c"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
}

// TestTwoTabsTwoServers: each tab shows the server its URL names, side by
// side, with no "server changed" notice, and acts on that server only.
func TestTwoTabsTwoServers(t *testing.T) {
	fa, aSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 1}})
	fb, bSrv := fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{"emails": {Status: "Running", Concurrency: 1}})
	f := newFixture(t, &config.Config{DefaultServer: "east", Servers: map[string]*config.Server{
		"east": {Interval: 1, Endpoints: []string{aSrv.URL}},
		"west": {Interval: 1, Endpoints: []string{bSrv.URL}},
	}})
	f.startDaemon()
	f.waitFirst("east")
	f.waitFirst("west")

	for _, c := range []struct{ path, shows, lacks string }{
		{"/?server=east", ">orders<", ">emails<"},
		{"/?server=west", ">emails<", ">orders<"},
		{"/", ">orders<", ">emails<"}, // the default server
		{"/grid?server=west", ">emails<", ">orders<"},
		{"/?server=east", ">orders<", ">emails<"},
	} {
		for _, hx := range []bool{false, true} {
			_, body := fetch(t, f.srv, c.path, hx)
			if !strings.Contains(body, c.shows) || strings.Contains(body, c.lacks) || strings.Contains(body, "Server changed") {
				t.Errorf("GET %s (hx %v) should show %s only, with no notice:\n%s", c.path, hx, c.shows, body)
			}
		}
	}
	_, body := fetch(t, f.srv, "/?server=west", false)
	for _, want := range []string{`<option value="west" selected>`, `<select name="server">`, `name="server" value="west"`} {
		if !strings.Contains(body, want) {
			t.Errorf("west's page lacks %q", want)
		}
	}

	if resp, _ := post(t, f.srv, "/workers/orders/actions/pause", url.Values{"server": {"east"}}, true); resp.StatusCode != http.StatusOK {
		t.Errorf("pause on east = %d, want 200", resp.StatusCode)
	}
	if resp, _ := post(t, f.srv, "/workers/emails/actions/pause", url.Values{"server": {"west"}}, true); resp.StatusCode != http.StatusOK {
		t.Errorf("pause on west = %d, want 200", resp.StatusCode)
	}
	wantPatches(t, fa, "PATCH /workers/orders/actions/pause")
	wantPatches(t, fb, "PATCH /workers/emails/actions/pause")
}

// TestServerIdentityIsConfigKey: pages name servers by configuration key,
// which config.Server.Name need not equal.
func TestServerIdentityIsConfigKey(t *testing.T) {
	_, fSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 1}})
	f := newFixture(t, &config.Config{DefaultServer: "prod-eu", Servers: map[string]*config.Server{
		"prod-eu": {Interval: 1, Endpoints: []string{fSrv.URL}},
		"dev":     {Interval: 1, Endpoints: []string{"http://dev.invalid"}},
	}})
	f.cfg.Servers["prod-eu"].Name = "Production EU"
	f.startDaemon()
	f.waitFirst("prod-eu")

	for _, path := range []string{"/?server=prod-eu", "/"} {
		_, body := fetch(t, f.srv, path, false)
		if !strings.Contains(body, ">orders<") {
			t.Errorf("GET %s should show prod-eu's data", path)
		}
		for _, want := range []string{`<option value="prod-eu" selected>`, `hx-get="/?server=prod-eu"`, `name="server" value="prod-eu"`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s lacks %q", path, want)
			}
		}
	}
	_, body := fetch(t, f.srv, "/?server="+url.QueryEscape("Production EU"), false)
	if !strings.Contains(body, "Server Production EU is not configured") || strings.Contains(body, ">orders<") {
		t.Error("?server=<config.Server.Name> should not be configured: names are keys")
	}
}

// TestInstanceParamResolves: off Kubernetes, ?instance= names a unit by
// the label the pages show; every link and form of the page carries it.
func TestInstanceParamResolves(t *testing.T) {
	varmqs, fakes := threeVarmqs(t)
	f := unitsFixture(t, config.Server{}, &mutableSource{eps: urlsOf(fakes...)})
	labels := fleettest.StaticLabels(urlsOf(fakes...)...)
	var second int // the fake labelled instance-2
	for i, fk := range fakes {
		if labels[fk.URL] == "instance-2" {
			second = i
		}
	}

	_, body := fetch(t, f.srv, "/?server=multi&instance=instance-2", false)
	if !strings.Contains(body, "3/3 instances") {
		t.Errorf("dashboard lacks the server's health:\n%s", body)
	}
	if strings.Contains(body, ">emails<") != (second == 0) {
		t.Errorf("dashboard of instance-2 shows emails = %v, want %v", !(second == 0), second == 0)
	}
	for _, want := range []string{
		`<option value="instance-2" selected`,
		`<input type="hidden" name="instance" value="instance-2">`,
		`href="/workers/orders?instance=instance-2&amp;server=multi"`,
		`href="/grid?instance=instance-2&amp;server=multi"`,
		`hx-get="/?instance=instance-2&amp;server=multi"`,
		`Selected workers on instance-2:`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard of instance-2 lacks %q", want)
		}
	}
	if strings.Contains(body, `name="pod"`) || strings.Contains(body, "pod=") {
		t.Error("an instance page carries a pod parameter")
	}
	_, body = fetch(t, f.srv, "/workers/orders?server=multi&instance=instance-2", false)
	for _, want := range []string{
		`<input type="hidden" name="instance" value="instance-2">`,
		`href="/?instance=instance-2&amp;server=multi"`,
		"Instances reporting: <span",
		`hx-confirm="Stop orders on instance-2 (multi)?"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail of instance-2 lacks %q", want)
		}
	}

	resp, _ := post(t, f.srv, "/workers/orders/actions/pause", multi("instance", "instance-2"), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause on instance-2 = %d, want 200", resp.StatusCode)
	}
	for i, fk := range varmqs {
		if want := map[bool]int{true: 1, false: 0}[i == second]; len(patches(fk)) != want {
			t.Errorf("fake %d (%s) got %q, want %d PATCH", i, labels[fakes[i].URL], patches(fk), want)
		}
	}
}

// TestKubernetesPodParam: on Kubernetes, ?pod= names a pod by its name.
func TestKubernetesPodParam(t *testing.T) {
	varmqs, fakes := threeVarmqs(t) // pods a, b and c: labelled by the instance that answered
	f := unitsFixture(t, kubernetesServer, &mutableSource{eps: urlsOf(fakes...)})

	_, body := fetch(t, f.srv, "/?server=multi&pod=b", false)
	if !strings.Contains(body, ">orders<") || strings.Contains(body, ">emails<") {
		t.Errorf("dashboard of pod b should show orders only:\n%s", body)
	}
	for _, want := range []string{
		`<select name="pod">`,
		`<option value="b" selected`,
		`<input type="hidden" name="pod" value="b">`,
		`href="/workers/orders?pod=b&amp;server=multi"`,
		`Selected workers on b:`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard of pod b lacks %q", want)
		}
	}
	if strings.Contains(body, `name="instance"`) {
		t.Error("a pod page carries an instance parameter")
	}

	if resp, _ := post(t, f.srv, "/workers/orders/actions/pause", multi("pod", "b"), true); resp.StatusCode != http.StatusOK {
		t.Fatalf("pause on pod b = %d, want 200", resp.StatusCode)
	}
	for i, fk := range varmqs {
		if want := map[bool]int{true: 1, false: 0}[i == 1]; len(patches(fk)) != want {
			t.Errorf("fake %s got %q, want %d PATCH", fk.Instance, patches(fk), want)
		}
	}
}

// TestUnknownAndGoneLabels: a label no snapshot of the hour has is not a
// unit of the server; one only older snapshots have is gone. Either way the
// page shows no data, and an action sends nothing.
func TestUnknownAndGoneLabels(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		for _, tc := range []struct {
			server       config.Server
			param, label string
			copy         string
		}{
			{config.Server{}, "instance", "instance-9", "instance-9 is not an instance of multi"},
			{kubernetesServer, "pod", "web-0", "web-0 is not a pod of multi"},
		} {
			fa, fb, aSrv, bSrv := twoFakes(t)
			srv := serveUnits(t, tc.server, aSrv, bSrv)
			for _, hx := range []bool{false, true} {
				status, body := fetch(t, srv, "/?server=multi&"+tc.param+"="+tc.label, hx)
				if status != http.StatusOK || !strings.Contains(body, `<p class="banner">`+tc.copy) || strings.Contains(body, `<tr id="w-`) {
					t.Errorf("GET %s=%s (hx %v) = %d, want 200 with %q and no rows:\n%s", tc.param, tc.label, hx, status, tc.copy, body)
				}
			}
			resp, body := post(t, srv, "/workers/orders/actions/pause", multi(tc.param, tc.label), true)
			if resp.StatusCode != http.StatusConflict || body != `<p class="banner">`+tc.copy+`</p>` {
				t.Errorf("POST %s=%s = %d %q, want 409 with %q", tc.param, tc.label, resp.StatusCode, body, tc.copy)
			}
			noPatches(t, fa, fb)
		}
	})

	t.Run("gone", func(t *testing.T) {
		fa, fb, aSrv, bSrv := twoFakes(t)
		src := &mutableSource{eps: []string{aSrv.URL, bSrv.URL}}
		f := unitsFixture(t, config.Server{}, src)
		label := fleettest.StaticLabels(aSrv.URL, bSrv.URL)[aSrv.URL]
		src.set(bSrv.URL)
		f.waitFor("multi", "a snapshot without a", func(s *fleet.Snapshot) bool { return len(s.Pods) == 1 })

		gone := "Instance " + label + " is no longer reported — reload"
		_, body := fetch(t, f.srv, "/?server=multi&instance="+label, false)
		if !strings.Contains(body, `<p class="banner">`+gone+` · <a href="/?server=multi">All instances (merged)</a>`) {
			t.Errorf("gone unit's page lacks the banner with the merged view:\n%s", body)
		}
		for _, c := range []struct {
			path string
			form url.Values
		}{
			{"/workers/orders/actions/pause", multi("instance", label)},
			{"/workers/orders/concurrency", multi("instance", label, "n", "3")},
			{"/bulk/pause", multi("instance", label, "names", "orders")},
			{"/bulk/pause", multi("instance", label, "names", "orders", "confirm", "1")},
		} {
			resp, body := post(t, f.srv, c.path, c.form, true)
			if resp.StatusCode != http.StatusConflict || body != `<p class="banner">`+gone+`</p>` {
				t.Errorf("POST %s to a gone unit = %d %q, want 409 with %q", c.path, resp.StatusCode, body, gone)
			}
		}
		noPatches(t, fa, fb)
	})
}

// TestOldPodParamFallsBackToMerged: links from before labels — a unit key
// in pod= — show the merged view: off Kubernetes pod= is no parameter at
// all, and on it a value shaped like a key (#… or a URL) is no pod name.
func TestOldPodParamFallsBackToMerged(t *testing.T) {
	for _, tc := range []struct {
		server config.Server
		many   string
	}{
		{config.Server{}, "instances"},
		{kubernetesServer, "pods"},
	} {
		fakes := threeFakes(t)
		srv := serveUnits(t, tc.server, fakes...)
		for _, old := range []string{"%23a", "%23lb-1", url.QueryEscape(fakes[1].URL)} {
			path := "/?server=multi&pod=" + old
			_, body := fetch(t, srv, path, false)
			if !strings.Contains(body, ">emails<") || !strings.Contains(body, ">orders<") || strings.Contains(body, `<p class="banner">`) {
				t.Errorf("GET %s (%s) should show the merged view, no banner:\n%s", path, tc.many, body)
			}
			if !strings.Contains(body, `<option value="" selected>All `+tc.many+` (merged)</option>`) || !strings.Contains(body, `hx-get="/?server=multi"`) {
				t.Errorf("GET %s (%s): the merged view should be chosen and polled", path, tc.many)
			}
		}
	}
}

// TestRemovedServerPage (Review Focus 5): a tab on a server just removed
// from the config says it is not configured — no panic, no data, no
// request to varmq — and offers the servers that are.
func TestRemovedServerPage(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	f := startLive(t, &config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"multi": {Interval: 1, Endpoints: []string{aSrv.URL, bSrv.URL}},
		"other": {Interval: 1, Endpoints: []string{"http://other.invalid"}},
	}}, "multi")
	f.stopDaemon() // so that every call the fakes get from now is the UI's
	calls := len(fa.Calls()) + len(fb.Calls())
	f.setConfig(&config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"other": {Interval: 1, Endpoints: []string{"http://other.invalid"}},
	}})

	const copy = "Server multi is not configured"
	for _, path := range []string{"/?server=multi", "/", "/grid?server=multi", "/workers/orders?server=multi", "/?server=multi&instance=instance-1"} {
		for _, hx := range []bool{false, true} {
			status, body := fetch(t, f.srv, path, hx)
			want := http.StatusNotFound
			if hx {
				want = http.StatusOK // so that htmx swaps the banner in
			}
			if status != want || !strings.Contains(body, copy) {
				t.Errorf("GET %s (hx %v) = %d, want %d with %q:\n%s", path, hx, status, want, copy, body)
			}
			for _, data := range []string{">orders<", `<tr id="w-`, `id="bulk"`, "Instances reporting"} {
				if strings.Contains(body, data) {
					t.Errorf("GET %s (hx %v) shows %q", path, hx, data)
				}
			}
		}
	}
	_, body := fetch(t, f.srv, "/?server=multi", false)
	for _, want := range []string{`<option value="multi" selected disabled>multi</option>`, `<option value="other">other</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker lacks %q:\n%s", want, body)
		}
	}

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/orders/actions/pause", multi()},
		{"/workers/orders/concurrency", multi("n", "3")},
		{"/bulk/pause", multi("names", "orders", "confirm", "1")},
	} {
		resp, body := post(t, f.srv, c.path, c.form, true)
		if resp.StatusCode != http.StatusConflict || body != `<p class="banner">`+copy+`</p>` {
			t.Errorf("POST %s = %d %q, want 409 with %q", c.path, resp.StatusCode, body, copy)
		}
	}
	if now := len(fa.Calls()) + len(fb.Calls()); now != calls {
		t.Errorf("varmq got %d requests about a server no longer configured", now-calls)
	}
}

// TestWaitingForFirstSnapshot: before a server's first snapshot the page
// says so and reloads itself, and an action sends nothing.
func TestWaitingForFirstSnapshot(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 1, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}})
	const copy = "Waiting for the first snapshot of multi…"

	status, body := fetch(t, f.srv, "/?server=multi", false)
	if status != http.StatusOK || !strings.Contains(body, copy) || !strings.Contains(body, `<meta http-equiv="refresh" content="2">`) {
		t.Errorf("GET / = %d, want 200 with %q, reloading itself:\n%s", status, copy, body)
	}
	if strings.Contains(body, `id="poller"`) {
		t.Error("the waiting page polls #live, which would never bring the header")
	}
	// htmx (a poll, the unit picker) would swap in only part of the page:
	// the browser reloads it instead, and gets the page above.
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/?server=multi", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	if resp, _ := send(t, f.srv, req); resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Refresh") != "true" {
		t.Errorf("htmx GET before the first snapshot = %d (HX-Refresh %q), want 200 reloading the page", resp.StatusCode, resp.Header.Get("HX-Refresh"))
	}
	resp, body := post(t, f.srv, "/workers/orders/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusConflict || body != `<p class="banner">`+copy+`</p>` {
		t.Errorf("POST before the first snapshot = %d %q, want 409 with %q", resp.StatusCode, body, copy)
	}
	noPatches(t, fa, fb)
}

// TestStaleBannerAndStart: a server whose newest snapshot is older than
// two intervals and 2s shows how old, and why: the daemon is not running —
// with a button that starts it — or the server's own error. Current values
// read "—"; the history stays.
func TestStaleBannerAndStart(t *testing.T) {
	_, aSrv := fleettest.NewVarmq(t, "a", nil)
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 10, Endpoints: []string{aSrv.URL}},
	}})
	f.claim()
	f.write("multi", snap(time.Now().Add(-60*time.Second), okPod(aSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 2)})))
	f.release() // the daemon stopped

	// About a minute old: the age is rounded to the second when the page
	// is rendered.
	down := regexp.MustCompile(`<p class="banner stale">No new data for 1m\ds — the vartop daemon is not running\.</p>`)
	for _, path := range []string{"/?server=multi", "/grid?server=multi", "/workers/orders?server=multi"} {
		for _, hx := range []bool{false, true} {
			status, body := fetch(t, f.srv, path, hx)
			_, banner, _ := strings.Cut(body, `<div id="banner"`)
			banner, _, _ = strings.Cut(banner, "</div>")
			if status != http.StatusOK || !down.MatchString(banner) || !strings.Contains(banner, `hx-post="/daemon/start"`) || !strings.Contains(banner, ">Start collector</button>") {
				t.Errorf("GET %s (hx %v) = %d, want 200 with %q and the Start collector button in #banner:\n%s", path, hx, status, down, banner)
			}
		}
	}
	_, body := fetch(t, f.srv, "/?server=multi", false)
	if !strings.Contains(body, ">orders<") || !strings.Contains(body, `<td class="num">—</td>`) || strings.Contains(body, `<td class="num">1</td>`) {
		t.Errorf("a stale dashboard should list orders with its current values as —:\n%s", body)
	}
	// Nor the health of the stale snapshot.
	_, status, _ := strings.Cut(body, `<div id="status">`)
	status, _, _ = strings.Cut(status, "</div>")
	if !strings.Contains(status, `<span class="health" title="healthy / total instances">— instances</span>`) || strings.Contains(status, "1/1") {
		t.Errorf("a stale page's #status should show its health as —:\n%s", status)
	}

	// Start collector, from the page it is on: htmx sends the browser
	// there, a full navigation.
	req := postRequest(t, f.srv, "/daemon/start", url.Values{"server": {"multi"}})
	req.Header.Set("HX-Current-URL", f.srv.URL+"/workers/orders?server=multi")
	resp, _ := send(t, f.srv, req)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Redirect") != "/workers/orders?server=multi" || f.started() != 1 {
		t.Errorf("POST /daemon/start = %d, HX-Redirect %q, Start called %d times; want 200 sending htmx back to the page, once",
			resp.StatusCode, resp.Header.Get("HX-Redirect"), f.started())
	}
	// Without htmx (no route lets one through the guards) it is a 303.
	rec := httptest.NewRecorder()
	plain := httptest.NewRequest(http.MethodPost, "/daemon/start", strings.NewReader("server=multi"))
	plain.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.h.startDaemon(rec, plain)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?server=multi" {
		t.Errorf("POST /daemon/start without htmx = %d to %q, want 303 to the dashboard", rec.Code, rec.Header().Get("Location"))
	}

	// A Start that fails says so in the banner, next to the button, which
	// stays.
	_, banner, _ := strings.Cut(body, `<div id="banner"`)
	banner, _, _ = strings.Cut(banner, "</div>")
	if !strings.Contains(banner, `hx-target="#start-error"`) || !strings.Contains(banner, `>Start collector</button> <span id="start-error" class="err" role="alert"></span>`) {
		t.Errorf("the banner lacks the Start button's error slot:\n%s", banner)
	}
	f.mu.Lock()
	f.startErr = errStart
	f.mu.Unlock()
	resp, body = post(t, f.srv, "/daemon/start", url.Values{"server": {"multi"}}, true)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Retarget") != "" || resp.Header.Get("HX-Redirect") != "" || body != errStart.Error() || f.started() != 3 {
		t.Errorf("failed Start = %d (HX-Retarget %q, HX-Redirect %q) %q, want 200 with the error alone, for its slot",
			resp.StatusCode, resp.Header.Get("HX-Retarget"), resp.Header.Get("HX-Redirect"), body)
	}

	// The daemon runs, but the server's collection fails.
	f.claim()
	if err := f.ctl.SetServerErr("multi", "discovery failed: boom — retrying in 5s"); err != nil {
		t.Fatal(err)
	}
	_, body = fetch(t, f.srv, "/?server=multi", false)
	if want := regexp.MustCompile(`No new data for 1m\ds — multi: discovery failed: boom — retrying in 5s<`); !want.MatchString(body) || strings.Contains(body, "Start collector") {
		t.Errorf("stale page of a failing server lacks %q, or offers Start:\n%s", want, body)
	}
}

// TestGETNeverCallsVarmq: pages read the store only; with the daemon
// stopped, no page load reaches varmq.
func TestGETNeverCallsVarmq(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	_, f := newTestServer(t, aSrv, bSrv)
	f.stopDaemon()
	calls := func() int { return len(fa.Calls()) + len(fb.Calls()) }

	before := calls()
	for range 20 {
		for _, path := range []string{"/", "/grid", "/workers/orders", "/?instance=instance-1", "/?server=b", "/?server=nope"} {
			for _, hx := range []bool{false, true} {
				fetch(t, f.srv, path, hx)
			}
		}
	}
	if grew := calls() - before; grew != 0 {
		t.Errorf("varmq got %d calls from page loads", grew)
	}
}

func TestTokenNeverInResponses(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // refuses connections: its unit renders an error
	srv, _ := newTestServer(t, aSrv, bSrv, dead)

	_, grid := fetch(t, srv, "/grid", false)
	if !strings.Contains(grid, `<small class="err">`) {
		t.Fatal("the dead unit's error is not rendered; the test would miss a token in it")
	}

	for _, path := range []string{
		"/", "/grid", "/workers/orders", "/workers/nope", "/?server=other", "/?server=b",
		"/static/app.css", "/static/htmx-2.0.4.min.js", "/static/",
	} {
		for _, hx := range []bool{false, true} {
			req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if hx {
				req.Header.Set("HX-Request", "true")
			}
			resp, body := send(t, srv, req)
			if strings.Contains(body, testToken) {
				t.Errorf("GET %s (hx %v) body contains the token", path, hx)
			}
			for k, vs := range resp.Header {
				if strings.Contains(strings.Join(vs, " "), testToken) {
					t.Errorf("GET %s header %s contains the token", path, k)
				}
			}
		}
	}
}

// TestHandlerRecoversPanic: a handler that panics answers 500, its panic
// logged, and the server serves on.
func TestHandlerRecoversPanic(t *testing.T) {
	var logged []string
	h := recoverPanics(func(format string, args ...any) { logged = append(logged, format) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/boom" {
				panic(&fleet.TickPanic{Value: "<b>reply broke</b>", Stack: []byte("goroutine 9")})
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/boom", nil))
	if body := rec.Body.String(); rec.Code != http.StatusInternalServerError || !strings.Contains(body, "internal error: &lt;b&gt;reply broke&lt;/b&gt;") || strings.Contains(body, "goroutine") {
		t.Errorf("panicking handler = %d %q, want 500 naming the panic, escaped, without its stack", rec.Code, body)
	}
	if len(logged) != 1 {
		t.Errorf("logged %d times, want once", len(logged))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("next request = %d, want it served", rec.Code)
	}
}

// TestFillCarriesNoteAndTips: the header of every page names the balancers
// behind the server and hovers each picker entry with where it was reached.
func TestFillCarriesNoteAndTips(t *testing.T) {
	s := snap(time.Unix(1000, 0),
		fleet.PodSnapshot{Key: "#a", Via: "http://lb:8090/varmq", Instance: "a", Label: "instance-1", Balanced: true},
		fleet.PodSnapshot{Key: "#b", Via: "http://lb:8090/varmq", Instance: "b", Label: "instance-2", Balanced: true},
		fleet.PodSnapshot{Key: "http://c:8090/varmq", Via: "http://c:8090/varmq", Label: "instance-3"},
	)
	s.Behind = map[string]int{"http://lb:8090/varmq": 2}

	var p Page
	fill(&p, []*fleet.Snapshot{s})

	if want := "2 instances behind lb:8090"; p.Note != want {
		t.Errorf("Note = %q, want %q", p.Note, want)
	}
	want := []PodOption{
		{Label: "instance-1", Tip: "a via http://lb:8090/varmq"},
		{Label: "instance-2", Tip: "b via http://lb:8090/varmq"},
		{Label: "instance-3", Tip: "http://c:8090/varmq"},
	}
	if !slices.Equal(p.Pods, want) {
		t.Errorf("Pods = %+v, want %+v", p.Pods, want)
	}
}

// TestControlsBehindBalancer: one balanced unit's pages offer no controls,
// only the note saying why; a scope holding one offers no restart.
func TestControlsBehindBalancer(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	one := fleet.UnitsOf(f.latest("multi"))["#a"].Label
	note := `<p class="note">` + oneUnitCopy + `</p>`

	_, body := fetch(t, srv, "/workers/w?server=multi&instance="+one, false)
	if body = html.UnescapeString(body); !strings.Contains(body, note) {
		t.Errorf("detail of one balanced unit lacks the note %q", note)
	}
	for _, control := range []string{">Pause</button>", ">Restart</button>", "/concurrency"} {
		if strings.Contains(body, control) {
			t.Errorf("detail of one balanced unit has %q", control)
		}
	}

	_, body = fetch(t, srv, "/workers/w?server=multi", false)
	if !strings.Contains(body, ">Pause</button>") || !strings.Contains(body, "/concurrency") || strings.Contains(body, ">Restart</button>") {
		t.Errorf("merged detail behind a balancer should have controls but no Restart:\n%s", body)
	}
	if strings.Contains(html.UnescapeString(body), note) {
		t.Error("merged detail carries the one-unit note")
	}

	_, body = fetch(t, srv, "/?server=multi&instance="+one, false)
	if body = html.UnescapeString(body); !strings.Contains(body, note) || strings.Contains(body, `id="bulk"`) {
		t.Errorf("dashboard of one balanced unit should have the note and no bulk form:\n%s", body)
	}
	if _, body = fetch(t, srv, "/?server=multi", false); !strings.Contains(body, `id="bulk"`) {
		t.Error("merged dashboard behind a balancer lacks the bulk form")
	}

	// Without balancers nothing changes.
	_, _, aSrv, bSrv := twoFakes(t)
	plain, _ := newTestServer(t, aSrv, bSrv)
	label := fleettest.StaticLabels(aSrv.URL, bSrv.URL)[aSrv.URL]
	for _, path := range []string{"/workers/orders?server=multi", "/workers/orders?server=multi&instance=" + label} {
		if _, body := fetch(t, plain, path, false); !strings.Contains(body, ">Restart</button>") {
			t.Errorf("GET %s on a server without balancers lacks Restart", path)
		}
	}
}

// TestFillMarksBalancedScope: a page's scope holds a balanced unit when it
// is merged over one, or is one.
func TestFillMarksBalancedScope(t *testing.T) {
	mixed := []*fleet.Snapshot{snap(time.Unix(1000, 0),
		fleet.PodSnapshot{Key: "#a", Via: "http://lb:1", Instance: "a", Label: "instance-1", Balanced: true},
		fleet.PodSnapshot{Key: "http://c:1", Via: "http://c:1", Instance: "c", Label: "instance-2"},
	)}
	plain := []*fleet.Snapshot{snap(time.Unix(1000, 0), fleet.PodSnapshot{Key: "http://c:1", Via: "http://c:1", Instance: "c", Label: "instance-1"})}

	for _, tc := range []struct {
		snaps         []*fleet.Snapshot
		pod           string
		scope, oneBal bool
	}{
		{mixed, "", true, false},
		{mixed, "instance-1", true, true},
		{mixed, "instance-2", false, false},
		{plain, "", false, false},
	} {
		p := Page{Pod: tc.pod}
		fill(&p, tc.snaps)
		if p.ScopeBalanced != tc.scope || p.OneBalanced != tc.oneBal {
			t.Errorf("pod %q: ScopeBalanced %v OneBalanced %v, want %v %v", tc.pod, p.ScopeBalanced, p.OneBalanced, tc.scope, tc.oneBal)
		}
	}
}
