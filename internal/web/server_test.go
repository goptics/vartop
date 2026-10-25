package web

import (
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// testToken is the token of every newTestServer server; no response may carry it.
const testToken = "s3cr3t-tok"

// newTestServer serves the web UI over a Manager whose active server, "multi",
// polls the given fake varmqs every second with testToken. A second
// configured server, "b", points at an unreachable endpoint, for switch
// tests. It returns once the first snapshot is in. The web server and the
// Manager are closed when the test ends, before fakes created earlier.
func newTestServer(t *testing.T, fakes ...*httptest.Server) (*httptest.Server, *Manager) {
	t.Helper()
	endpoints := make([]string, len(fakes))
	for i, f := range fakes {
		endpoints[i] = f.URL
	}
	cfg := &config.Config{Servers: map[string]*config.Server{
		"multi": {Name: "multi", Interval: 1, Endpoints: endpoints, Token: testToken},
		"b":     {Name: "b", Interval: 1, Endpoints: []string{"http://b.invalid"}, Token: testToken},
	}}
	m := startManager(t, cfg, "multi")
	return serve(t, m), m
}

// startManager returns a Manager over cfg (pools from api.PoolFor) with the
// server keyed name active and its first snapshot in. It is closed when the
// test ends.
func startManager(t *testing.T, cfg *config.Config, name string) *Manager {
	t.Helper()
	m := NewManager(cfg, api.PoolFor)
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Switch(name); err != nil {
		t.Fatalf("Switch(%s): %v", name, err)
	}
	waitUntil(t, 3*time.Second, "first snapshot", func() bool {
		s := m.Current()
		return s != nil && s.History.Latest() != nil
	})
	return m
}

// serve starts the web UI for m on a listener from Listen, allowing the
// hosts AllowedHosts gives for it. It is closed when the test ends.
func serve(t *testing.T, m *Manager) *httptest.Server {
	t.Helper()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	h, err := NewHandler(m, AllowedHosts(ln))
	if err != nil {
		_ = ln.Close()
		t.Fatalf("NewHandler: %v", err)
	}
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// twoFakes starts two fake pods: orders runs on both, emails only on a.
func twoFakes(t *testing.T) (fa, fb *fakeVarmq, aSrv, bSrv *httptest.Server) {
	t.Helper()
	fa, aSrv = newFakeVarmq(t, "a", map[string]*fakeWorker{
		"orders": {Status: "Running", Concurrency: 4, Busy: 1},
		"emails": {Status: "Paused", Concurrency: 2},
	})
	fb, bSrv = newFakeVarmq(t, "b", map[string]*fakeWorker{
		"orders": {Status: "Running", Concurrency: 4, Busy: 2},
	})
	return fa, fb, aSrv, bSrv
}

// send performs req against srv and returns the response and its body.
func send(t *testing.T, srv *httptest.Server, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", req.Method, req.URL, err)
	}
	return resp, string(body)
}

// fetch GETs path from srv, as htmx does when hx is set, and returns the
// status and body.
func fetch(t *testing.T, srv *httptest.Server, path string, hx bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	resp, body := send(t, srv, req)
	return resp.StatusCode, body
}

func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

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

	for _, c := range []struct {
		path string
		want []string
	}{
		{"/", []string{"orders", "emails", `hx-get="/?server=multi"`, "<title>vartop · multi</title>"}},
		{"/grid", []string{"orders", "emails", `hx-get="/grid?server=multi"`, "<title>Pod grid · multi</title>"}},
		{"/workers/orders", []string{"orders", "Pods reporting:", `hx-get="/workers/orders?server=multi"`, "<title>orders · multi</title>"}},
	} {
		status, body := fetch(t, srv, c.path, false)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", c.path, status)
			continue
		}
		for _, want := range append(c.want, "2/2 pods", aSrv.URL, bSrv.URL) {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s lacks %q", c.path, want)
			}
		}
	}

	t.Run("pod filter", func(t *testing.T) {
		_, body := fetch(t, srv, "/?pod="+url.QueryEscape(bSrv.URL), false)
		if !strings.Contains(body, ">orders<") || strings.Contains(body, ">emails<") {
			t.Error("dashboard filtered to b should show orders and not emails")
		}
		// A pod never seen (a bad bookmark) shows why, not the merged view.
		_, body = fetch(t, srv, "/?pod="+url.QueryEscape("http://gone.invalid"), false)
		if !strings.Contains(body, "Pod http://gone.invalid is no longer reported — reload") || strings.Contains(body, `<tr id="w-`) {
			t.Error("an unknown pod should show the gone-pod banner and no rows")
		}
	})

	// The grid shows every pod, so it offers no pod select.
	t.Run("pod select", func(t *testing.T) {
		for path, want := range map[string]bool{"/": true, "/workers/orders": true, "/grid": false, "/grid?server=other": false} {
			if _, body := fetch(t, srv, path, false); strings.Contains(body, `<select name="pod">`) != want {
				t.Errorf("GET %s: pod select shown = %v, want %v", path, !want, want)
			}
		}
	})

	t.Run("unknown worker", func(t *testing.T) {
		for _, path := range []string{"/workers/nope", "/workers/emails?pod=" + url.QueryEscape(bSrv.URL)} {
			status, body := fetch(t, srv, path, false)
			if status != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, status)
			}
			if !strings.Contains(body, "not found") || strings.Contains(body, "Retry") {
				t.Errorf("GET %s should say not found, without a Retry form", path)
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

// mutableSource is a discovery.Source whose endpoints a test changes.
type mutableSource struct {
	mu  sync.Mutex
	eps []string
}

func (s *mutableSource) Endpoints() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.eps)
}

func (s *mutableSource) set(eps ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eps = eps
}

func (*mutableSource) Close() error { return nil }

// TestVanishedPodShowsBanner: a page filtered to a pod the newest snapshot
// lacks says so and shows no data, never another pod's under its name.
func TestVanishedPodShowsBanner(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	src := &mutableSource{eps: []string{aSrv.URL, bSrv.URL}}
	cfg := &config.Config{Servers: map[string]*config.Server{
		"multi": {Name: "multi", Interval: 1, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}}
	m := NewManager(cfg, func(*config.Server) (*api.Pool, error) { return api.NewPool(src, ""), nil })
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Switch("multi"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "first snapshot", func() bool {
		s := m.Current()
		return s != nil && s.History.Latest() != nil
	})
	srv := serve(t, m)

	dash := "/?server=multi&pod=" + url.QueryEscape(aSrv.URL)
	detail := "/workers/orders?server=multi&pod=" + url.QueryEscape(aSrv.URL)
	if _, body := fetch(t, srv, dash, false); !strings.Contains(body, ">emails<") {
		t.Fatal("dashboard filtered to a lacks emails before a leaves")
	}

	src.set(bSrv.URL) // a leaves discovery
	waitUntil(t, 3*time.Second, "a snapshot without a", func() bool { return len(m.Current().History.Latest().Pods) == 1 })

	// Labelled from the last snapshot that had it.
	banner := `<p class="banner">Pod a is no longer reported — reload</p>`
	for _, path := range []string{dash, detail} {
		for _, hx := range []bool{true, false} { // a poll, a full load
			status, body := fetch(t, srv, path, hx)
			if status != http.StatusOK || !strings.Contains(body, banner) {
				t.Errorf("GET %s (hx %v) = %d, want 200 with %q", path, hx, status, banner)
			}
			for _, data := range []string{">orders<", `<tr id="w-`, "Pods reporting", `id="bulk"`, `class="controls"`, "No workers"} {
				if strings.Contains(body, data) {
					t.Errorf("GET %s (hx %v) shows %q", path, hx, data)
				}
			}
		}
	}
	// The full page polls without the pod.
	if _, body := fetch(t, srv, dash, false); !strings.Contains(body, `hx-get="/?server=multi"`) {
		t.Error("full load of a gone pod still polls it")
	}
}

// TestStatusShowsUpdateTime: #status, which every poll refreshes, shows the
// newest snapshot's time, so a page whose polls stopped is visibly stale.
func TestStatusShowsUpdateTime(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, m := newTestServer(t, aSrv, bSrv)
	h := m.Current().History

	for _, path := range []string{"/", "/grid", "/workers/orders"} {
		before := stamp(h.Latest())
		_, body := fetch(t, srv, path, true)
		after := stamp(h.Latest()) // a tick may land mid-request
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
	_, fSrv := newFakeVarmq(t, "a", map[string]*fakeWorker{"a/b c": {Status: "Running", Concurrency: 1}})
	srv, _ := newTestServer(t, fSrv)

	status, body := fetch(t, srv, "/workers/a%2Fb%20c", false)
	if status != http.StatusOK {
		t.Fatalf("GET /workers/a%%2Fb%%20c = %d, want 200", status)
	}
	// The poller and the pod select must reach the same worker again.
	for _, want := range []string{`<h2>a/b c</h2>`, `hx-get="/workers/a%2Fb%20c?server=multi"`, `hx-get="/workers/a%2Fb%20c"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
}

func TestStaleServerParamShowsBanner(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, path := range []string{"/?server=other", "/grid?server=other", "/workers/orders?server=other", "/?server=b"} {
		for _, hx := range []bool{false, true} {
			status, body := fetch(t, srv, path, hx)
			if status != http.StatusOK {
				t.Errorf("GET %s (hx %v) = %d, want 200", path, hx, status)
			}
			if !strings.Contains(body, "Server changed to multi — reload") {
				t.Errorf("GET %s (hx %v) lacks the banner", path, hx)
			}
			// The URL itself may name a worker; rendered data never shows.
			for _, data := range []string{">orders<", ">emails<", "Pods reporting", `<tr id="w-`} {
				if strings.Contains(body, data) {
					t.Errorf("GET %s (hx %v) shows worker data %q", path, hx, data)
				}
			}
			// #status is refreshed by every poll, so it must still be right.
			if !strings.Contains(body, "● 2/2 pods") {
				t.Errorf("GET %s (hx %v) lacks the active server's health", path, hx)
			}
		}
	}

	// The stale page keeps polling its own URL, and its links lead to the
	// active server.
	_, body := fetch(t, srv, "/?server=other", false)
	for _, want := range []string{`hx-get="/?server=other"`, `href="/grid?server=multi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stale page lacks %q", want)
		}
	}

	_, body = fetch(t, srv, "/?server=multi", false)
	if strings.Contains(body, "Server changed") || !strings.Contains(body, "orders") {
		t.Error("GET /?server=multi should show multi's data without the banner")
	}
}

// TestServerIdentityIsConfigKey: pages name servers by configuration key,
// which config.Server.Name need not equal.
func TestServerIdentityIsConfigKey(t *testing.T) {
	_, fSrv := newFakeVarmq(t, "a", map[string]*fakeWorker{"orders": {Status: "Running", Concurrency: 1}})
	m := startManager(t, &config.Config{Servers: map[string]*config.Server{
		"prod-eu": {Name: "Production EU", Interval: 1, Endpoints: []string{fSrv.URL}},
		"dev":     {Name: "dev", Interval: 1, Endpoints: []string{"http://dev.invalid"}},
	}}, "prod-eu")
	srv := serve(t, m)

	for _, path := range []string{"/?server=prod-eu", "/"} {
		_, body := fetch(t, srv, path, false)
		if strings.Contains(body, "Server changed") || !strings.Contains(body, ">orders<") {
			t.Errorf("GET %s should show prod-eu's data without the banner", path)
		}
		for _, want := range []string{`<option value="prod-eu" selected>`, `hx-get="/?server=prod-eu"`, `name="server" value="prod-eu"`} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s lacks %q", path, want)
			}
		}
	}
	_, body := fetch(t, srv, "/?server="+url.QueryEscape("Production EU"), false)
	if !strings.Contains(body, "Server changed to prod-eu — reload") {
		t.Error("?server=<config.Server.Name> should be stale: names are keys")
	}
}

func TestGETNeverCallsVarmq(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, m := newTestServer(t, aSrv, bSrv)
	h := m.Current().History
	calls := func() int { return len(fa.calls()) + len(fb.calls()) }
	const perTick = (1 + 2) + (1 + 1) // a list and a get per worker, on each fake

	ticksBefore, callsBefore := len(h.Snapshots()), calls()
	for range 20 {
		for _, path := range []string{"/", "/grid", "/workers/orders"} {
			if status, _ := fetch(t, srv, path, false); status != http.StatusOK {
				t.Fatalf("GET %s = %d", path, status)
			}
		}
	}
	ticks, grew := len(h.Snapshots())-ticksBefore, calls()-callsBefore

	// Polls finished in the window plus one in flight account for every call.
	if limit := (ticks + 1) * perTick; grew > limit {
		t.Errorf("varmq got %d calls during 60 page loads and %d ticks, want at most %d", grew, ticks, limit)
	}
}

func TestTokenNeverInResponses(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // refuses connections: its pod renders an error
	srv, _ := newTestServer(t, aSrv, bSrv, dead)

	_, grid := fetch(t, srv, "/grid", false)
	if !strings.Contains(grid, `<small class="err">`) {
		t.Fatal("the dead pod's error is not rendered; the test would miss a token in it")
	}

	for _, path := range []string{
		"/", "/grid", "/workers/orders", "/workers/nope", "/?server=other",
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

func TestConnectingAndErrorStates(t *testing.T) {
	m, g := newGatedManager(t, "dev", "prod")
	srv := serve(t, m)
	connecting := func(name string) string {
		return "Connecting to " + ConnectionLabel(m.cfg.Servers[name]) + "…"
	}
	retry := func(name string) string { return `<input type="hidden" name="name" value="` + name + `">` }

	// The first build is in flight.
	if err := m.Switch("dev"); err != nil {
		t.Fatal(err)
	}
	status, body := fetch(t, srv, "/", false)
	if status != http.StatusOK || !strings.Contains(body, `<p class="connecting" role="status">`+connecting("dev")+"</p>") {
		t.Errorf("GET / while connecting = %d, want 200 with %q", status, connecting("dev"))
	}
	if strings.Contains(body, `hx-trigger="every`) || strings.Contains(body, "Retry") {
		t.Error("connecting page polls or offers Retry")
	}
	// A poll only swaps #live, #banner and #status: it gets the message as
	// its banner, with a 200.
	status, body = fetch(t, srv, "/", true)
	if status != http.StatusOK || !strings.Contains(body, `<p class="banner">`+connecting("dev")+"</p>") {
		t.Errorf("htmx GET / while connecting = %d, want 200 with the banner %q", status, connecting("dev"))
	}
	if !strings.Contains(body, "○ 0/0 pods") {
		t.Error("htmx connecting response has no health in #status")
	}

	// The first build fails: its error, and Retry for dev.
	g.release("dev", errors.New("dial tcp: connection refused"))
	waitUntil(t, 3*time.Second, "build error", func() bool { return m.Err() != nil })
	for _, path := range []string{"/", "/grid", "/workers/orders"} {
		status, body := fetch(t, srv, path, false)
		if status != http.StatusOK {
			t.Errorf("GET %s after a failed build = %d, want 200", path, status)
		}
		for _, want := range []string{`<p class="err">dial tcp: connection refused</p>`, "Retry", retry("dev")} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s after a failed build lacks %q", path, want)
			}
		}
	}
	status, body = fetch(t, srv, "/", true)
	if status != http.StatusOK || !strings.Contains(body, `<p class="banner">dial tcp: connection refused</p>`) {
		t.Errorf("htmx GET / after a failed build = %d, want 200 with the error as banner", status)
	}

	// Retry succeeds: dev's (empty) dashboard.
	activate(t, m, g, "dev")
	waitUntil(t, 3*time.Second, "dev's first snapshot", func() bool { return m.Current().History.Latest() != nil })
	_, body = fetch(t, srv, "/", false)
	if strings.Contains(body, "Retry") || strings.Contains(body, "Connecting") || !strings.Contains(body, "No workers") {
		t.Error("GET / with dev active should show dev's dashboard")
	}

	// Switching to prod: a tab already on prod waits for it, dev's tabs keep
	// dev's data.
	if err := m.Switch("prod"); err != nil {
		t.Fatal(err)
	}
	if _, body := fetch(t, srv, "/?server=prod", false); !strings.Contains(body, connecting("prod")) {
		t.Errorf("GET /?server=prod while prod builds lacks %q", connecting("prod"))
	}
	for _, path := range []string{"/", "/?server=dev"} {
		if _, body := fetch(t, srv, path, false); strings.Contains(body, "Connecting") || !strings.Contains(body, "No workers") {
			t.Errorf("GET %s while prod builds should show dev's dashboard", path)
		}
	}

	// The switch fails: dev keeps polling; prod's tab shows the error and
	// retries prod.
	g.release("prod", errors.New("prod unreachable"))
	waitUntil(t, 3*time.Second, "prod's build error", func() bool { return m.Err() != nil })
	_, body = fetch(t, srv, "/?server=prod", false)
	for _, want := range []string{"prod unreachable", "Retry", retry("prod")} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /?server=prod after its build failed lacks %q", want)
		}
	}
	if strings.Contains(body, `hx-trigger="every`) {
		t.Error("error page polls: every poll would repeat the error as a banner")
	}
	if _, body := fetch(t, srv, "/?server=dev", false); strings.Contains(body, "prod unreachable") || !strings.Contains(body, "No workers") {
		t.Error("GET /?server=dev should still show dev's dashboard")
	}
}
