package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// postRequest is a POST of form to path on srv as htmx sends it from the
// page: a form body, "HX-Request: true" and a same-origin Sec-Fetch-Site.
func postRequest(t *testing.T, srv *httptest.Server, path string, form url.Values) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

// post submits form to path on srv, as htmx does when hx is set, and
// returns the response and its body.
func post(t *testing.T, srv *httptest.Server, path string, form url.Values, hx bool) (*http.Response, string) {
	t.Helper()
	req := postRequest(t, srv, path, form)
	if !hx {
		req.Header.Del("HX-Request")
	}
	return send(t, srv, req)
}

// patches returns the PATCH requests f has received, leaving out the
// poller's GETs.
func patches(f *fleettest.Varmq) []string {
	var out []string
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "PATCH ") {
			out = append(out, c)
		}
	}
	return out
}

// resultsOf is the part of an action response before its out-of-band #live.
func resultsOf(body string) string {
	results, _, _ := strings.Cut(body, `<div id="live"`)
	return results
}

// noPatches fails the test if any of fakes received a PATCH.
func noPatches(t *testing.T, fakes ...*fleettest.Varmq) {
	t.Helper()
	for _, f := range fakes {
		if got := patches(f); len(got) > 0 {
			t.Errorf("fake %s got %q, want no PATCH", f.Instance, got)
		}
	}
}

// wantPatches fails the test unless f received exactly want, in any order.
func wantPatches(t *testing.T, f *fleettest.Varmq, want ...string) {
	t.Helper()
	got := patches(f)
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("fake %s got %q, want %q", f.Instance, got, want)
	}
}

// setActionStatus makes every PATCH on f's worker answer status.
func setActionStatus(f *fleettest.Varmq, worker string, status int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Workers[worker].ActionStatus = status
}

func multi(kv ...string) url.Values {
	form := url.Values{"server": {"multi"}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Add(kv[i], kv[i+1])
	}
	return form
}

func TestActionAllPods(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	resp, body := post(t, srv, "/workers/orders/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	wantPatches(t, fa, "PATCH /workers/orders/actions/pause")
	wantPatches(t, fb, "PATCH /workers/orders/actions/pause")
	if n := strings.Count(resultsOf(body), `<li class="ok">`); n != 2 {
		t.Errorf("%d ok results, want 2:\n%s", n, body)
	}
	// One line per pod, labelled as the pages label it.
	labels := fleettest.StaticLabels(aSrv.URL, bSrv.URL)
	for _, want := range []string{
		`title="a via ` + aSrv.URL + `">` + labels[aSrv.URL] + `</span>`,
		`title="b via ` + bSrv.URL + `">` + labels[bSrv.URL] + `</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("results lack %q", want)
		}
	}
}

func TestActionOnePod(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	resp, body := post(t, srv, "/workers/orders/actions/resume", multi("instance", fleettest.StaticLabels(aSrv.URL, bSrv.URL)[aSrv.URL]), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	wantPatches(t, fa, "PATCH /workers/orders/actions/resume")
	noPatches(t, fb)
	if n := strings.Count(resultsOf(body), "<li "); n != 1 {
		t.Errorf("%d results, want 1 for one pod", n)
	}
}

func TestAction409IsNoChange(t *testing.T) {
	_, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)
	setActionStatus(fb, "orders", http.StatusConflict)

	resp, body := post(t, srv, "/workers/orders/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `<span class="outcome">no change (worker is not running)</span>`) {
		t.Errorf("body lacks the 409 as no change:\n%s", body)
	}
	if r := resultsOf(body); strings.Count(r, `<li class="ok">`) != 1 || strings.Count(r, `<li class="nochange">`) != 1 || strings.Contains(r, `<li class="failed">`) {
		t.Errorf("want one ok and one no change, nothing failed:\n%s", body)
	}
}

// TestActionOnPodLackingWorker: a worker action fans out to every pod in
// scope; one that lacks the worker (varmq's 404) has nothing to change.
func TestActionOnPodLackingWorker(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	_, body := post(t, srv, "/workers/emails/actions/resume", multi(), true)
	if r := resultsOf(body); strings.Count(r, `<li class="ok">`) != 1 || !strings.Contains(r, `<li class="nochange">`) || !strings.Contains(r, "no change (not on this instance)") {
		t.Errorf("want ok on a and no change (not on this instance) on b:\n%s", r)
	}
}

func TestActionPartialFailureIs200(t *testing.T) {
	fa, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)
	setActionStatus(fa, "orders", http.StatusInternalServerError)

	resp, body := post(t, srv, "/workers/orders/actions/stop", multi(), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on partial failure", resp.StatusCode)
	}
	if r := resultsOf(body); strings.Count(r, `<li class="failed">`) != 1 || strings.Count(r, `<li class="ok">`) != 1 {
		t.Errorf("want one failed and one ok:\n%s", body)
	}
	if !strings.Contains(body, "failed (API error (status 500): Internal Server Error)") {
		t.Errorf("failure lacks varmq's error:\n%s", body)
	}
}

func TestActionResponsesNeverCarryToken(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // refuses connections: its result is a transport error
	srv, _ := newTestServer(t, aSrv, bSrv, dead)

	for _, c := range []struct {
		path   string
		form   url.Values
		failed bool // the dead pod's error is rendered
	}{
		{"/workers/orders/actions/pause", multi(), true},
		{"/workers/orders/concurrency", multi("n", "7"), true},
		{"/bulk/pause", multi("names", "orders"), false},
		{"/bulk/pause", multi("names", "orders", "confirm", "1"), true},
		{"/workers/orders/actions/pause", url.Values{"server": {"other"}}, false},
	} {
		resp, body := post(t, srv, c.path, c.form, true)
		if c.failed && !strings.Contains(body, `<li class="failed">`) {
			t.Errorf("POST %s %v: the dead pod's failure is not rendered; the test would miss a token in it", c.path, c.form)
		}
		if strings.Contains(body, testToken) {
			t.Errorf("POST %s %v: body contains the token", c.path, c.form)
		}
		for k, vs := range resp.Header {
			if strings.Contains(strings.Join(vs, " "), testToken) {
				t.Errorf("POST %s: header %s contains the token", c.path, k)
			}
		}
	}
}

// TestMutationsFinishWhenBrowserAborts: htmx aborts an action request when
// another control fires (hx-sync replace), which closes the connection. The
// PATCHes already under way must still complete on every pod, or an action
// would be half-applied with no results shown.
func TestMutationsFinishWhenBrowserAborts(t *testing.T) {
	for _, c := range []struct {
		path string
		form url.Values
		done func(fleettest.Worker) bool
	}{
		{"/workers/orders/actions/stop", multi(), func(w fleettest.Worker) bool { return w.Status == "Stopped" }},
		{"/workers/orders/concurrency", multi("n", "9"), func(w fleettest.Worker) bool { return w.Concurrency == 9 }},
		{"/bulk/pause", multi("names", "orders", "confirm", "1"), func(w fleettest.Worker) bool { return w.Status == "Paused" }},
	} {
		t.Run(strings.TrimPrefix(c.path, "/"), func(t *testing.T) {
			t.Parallel()
			fa, fb, aSrv, bSrv := twoFakes(t)
			srv, _ := newTestServer(t, aSrv, bSrv)
			fb.Mu.Lock()
			fb.Delay = time.Second // b answers slowly; it drops the request if vartop gives up on it
			fb.Mu.Unlock()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := postRequest(t, srv, c.path, c.form).WithContext(ctx)
			sent := make(chan error, 1)
			go func() {
				resp, err := srv.Client().Do(req)
				if err == nil {
					resp.Body.Close()
				}
				sent <- err
			}()
			fleettest.WaitUntil(t, 3*time.Second, "b's PATCH under way", func() bool { return len(patches(fb)) == 1 })
			cancel() // the browser aborts
			if err := <-sent; err == nil {
				t.Fatal("the request completed before it was aborted; the test proves nothing")
			}

			for _, f := range []*fleettest.Varmq{fa, fb} {
				fleettest.WaitUntil(t, 3*time.Second, "orders changed on "+f.Instance, func() bool {
					w, _ := f.Worker("orders")
					return c.done(w)
				})
			}
		})
	}
}

func TestActionRejectsUnknown(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/orders/actions/start", multi()},
		{"/workers/orders/actions/%3Cb%3Ex", multi()},
		{"/workers/orders/concurrency", multi("n", "0", "instance", "<b>x")},
		{"/bulk/restart", multi("names", "orders", "confirm", "1")},
		{"/bulk/stop", multi("names", "orders", "confirm", "1")},
		{"/bulk/pause", multi("confirm", "1")},
		{"/bulk/pause", multi("names", "", "confirm", "1")},
	} {
		resp, body := post(t, srv, c.path, c.form, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s %v = %d, want 400", c.path, c.form, resp.StatusCode)
		}
		// htmx swaps the body into #errors as HTML: nothing posted is echoed.
		if strings.Contains(body, "<b>") {
			t.Errorf("POST %s: 400 body echoes the request: %q", c.path, body)
		}
	}
	noPatches(t, fa, fb)
}

func TestPOSTWithoutHXRequest(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, f := newTestServer(t, aSrv, bSrv)

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/orders/actions/pause", multi()},
		{"/workers/orders/concurrency", multi("n", "9")},
		{"/bulk/pause", multi("names", "orders", "confirm", "1")},
		{"/daemon/start", multi()},
	} {
		if resp, _ := post(t, srv, c.path, c.form, false); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without HX-Request = %d, want 403", c.path, resp.StatusCode)
		}
	}
	noPatches(t, fa, fb)
	if n := f.started(); n != 0 {
		t.Errorf("a refused POST /daemon/start started the daemon %d times", n)
	}
}

func TestCrossOriginPOST(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, f := newTestServer(t, aSrv, bSrv)

	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Sec-Fetch-Site": "", "Origin": "http://evil.example"},
	} {
		for _, c := range []struct {
			path string
			form url.Values
		}{
			{"/workers/orders/actions/pause", multi()},
			{"/workers/orders/concurrency", multi("n", "9")},
			{"/bulk/pause", multi("names", "orders", "confirm", "1")},
			{"/daemon/start", multi()},
		} {
			req := postRequest(t, srv, c.path, c.form)
			for k, v := range headers {
				if v == "" {
					req.Header.Del(k)
				} else {
					req.Header.Set(k, v)
				}
			}
			if resp, _ := send(t, srv, req); resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST %s with %v = %d, want 403", c.path, headers, resp.StatusCode)
			}
		}
	}
	noPatches(t, fa, fb)
	if n := f.started(); n != 0 {
		t.Errorf("a cross-origin POST /daemon/start started the daemon %d times", n)
	}
}

func TestGETOnMutatingRoute(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, f := newTestServer(t, aSrv, bSrv)

	for _, path := range []string{
		"/workers/orders/actions/pause?server=multi",
		"/workers/orders/concurrency?server=multi&n=3",
		"/bulk/pause?server=multi&names=orders&confirm=1",
		"/daemon/start?server=multi",
	} {
		for _, hx := range []bool{false, true} {
			if status, _ := fetch(t, srv, path, hx); status != http.StatusMethodNotAllowed {
				t.Errorf("GET %s (hx %v) = %d, want 405", path, hx, status)
			}
		}
	}
	noPatches(t, fa, fb)
	if n := f.started(); n != 0 {
		t.Errorf("GET /daemon/start started the daemon %d times", n)
	}
}

// TestNonPOSTMethodsNeedHTMX: the HX-Request guard covers every method that
// can change state, not only POST.
func TestNonPOSTMethodsNeedHTMX(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, hx := range []bool{false, true} {
			req, err := http.NewRequest(method, srv.URL+"/workers/orders/actions/pause", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := http.StatusForbidden
			if hx {
				req.Header.Set("HX-Request", "true")
				want = http.StatusMethodNotAllowed
			}
			if resp, _ := send(t, srv, req); resp.StatusCode != want {
				t.Errorf("%s (hx %v) = %d, want %d", method, hx, resp.StatusCode, want)
			}
		}
	}
}

// TestPOSTBindsConfigKey: a POST names its server by configuration key,
// which config.Server.Name need not equal.
func TestPOSTBindsConfigKey(t *testing.T) {
	f, fSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 1}})
	fx := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"prod-eu": {Interval: 1, Endpoints: []string{fSrv.URL}},
	}})
	fx.cfg.Servers["prod-eu"].Name = "Production EU"
	fx.startDaemon()
	fx.waitFirst("prod-eu")
	srv := fx.srv

	if resp, body := post(t, srv, "/workers/orders/actions/pause", url.Values{"server": {"Production EU"}}, true); resp.StatusCode != http.StatusConflict || body != `<p class="banner">Server Production EU is not configured</p>` {
		t.Errorf("server=<config.Server.Name> = %d %q, want 409, not configured: names are keys", resp.StatusCode, body)
	}
	noPatches(t, f)
	if resp, _ := post(t, srv, "/workers/orders/actions/pause", url.Values{"server": {"prod-eu"}}, true); resp.StatusCode != http.StatusOK {
		t.Errorf("server=prod-eu = %d, want 200", resp.StatusCode)
	}
	wantPatches(t, f, "PATCH /workers/orders/actions/pause")
}

// TestPOSTBeforeFirstRefresh: before a server's first snapshot nothing is
// known, not even which endpoints are balancers, so every POST that would
// reach varmq is refused in the banner and sends nothing.
func TestPOSTBeforeFirstRefresh(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 10, Endpoints: []string{lb.URL, srvs[0].URL}},
	}})

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/w/actions/pause", multi()},
		{"/workers/w/actions/restart", multi()},
		{"/workers/w/actions/pause", multi("instance", "instance-1")},
		{"/workers/w/concurrency", multi("n", "3")},
		{"/bulk/pause", multi("names", "w")},
		{"/bulk/pause", multi("names", "w", "confirm", "1")},
	} {
		resp, body := post(t, f.srv, c.path, c.form, true)
		if resp.StatusCode != http.StatusConflict || resp.Header.Get("HX-Retarget") != "#banner" ||
			body != `<p class="banner">Waiting for the first snapshot of multi…</p>` {
			t.Errorf("POST %s %v = %d (HX-Retarget %q) %q, want 409 with the waiting banner",
				c.path, c.form, resp.StatusCode, resp.Header.Get("HX-Retarget"), body)
		}
	}
	noPatches(t, fakes...)
}

func TestActionEscapesName(t *testing.T) {
	f, fSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"a/b c": {Status: "Running", Concurrency: 1}})
	srv, _ := newTestServer(t, fSrv)

	for _, c := range []struct {
		path string
		form url.Values
		want string
	}{
		{"/workers/a%2Fb%20c/actions/pause", multi(), "PATCH /workers/a%2Fb%20c/actions/pause"},
		{"/workers/a%2Fb%20c/concurrency", multi("n", "3"), "PATCH /workers/a%2Fb%20c/config/concurrency/3"},
		{"/bulk/resume", multi("names", "a/b c", "confirm", "1"), "PATCH /workers/a%2Fb%20c/actions/resume"},
	} {
		before := len(patches(f))
		if resp, body := post(t, srv, c.path, c.form, true); resp.StatusCode != http.StatusOK || !strings.Contains(body, `<li class="ok">`) {
			t.Errorf("POST %s = %d, want 200 with ok:\n%s", c.path, resp.StatusCode, body)
		}
		if got := patches(f)[before:]; !slices.Equal(got, []string{c.want}) {
			t.Errorf("POST %s sent %q, want %q", c.path, got, c.want)
		}
	}
}

func TestConcurrencyValidatesAndSkips(t *testing.T) {
	fa, aSrv := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 8}})
	fb, bSrv := fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 4}})
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, n := range []string{"0", "-1", "", "x", "2.5", "8 "} {
		if resp, _ := post(t, srv, "/workers/orders/concurrency", multi("n", n), true); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("n=%q = %d, want 400", n, resp.StatusCode)
		}
	}
	noPatches(t, fa, fb)

	resp, body := post(t, srv, "/workers/orders/concurrency", multi("n", "8"), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("n=8 = %d, want 200", resp.StatusCode)
	}
	noPatches(t, fa)
	wantPatches(t, fb, "PATCH /workers/orders/config/concurrency/8")
	if !strings.Contains(body, `<span class="outcome">no change (already 8)</span>`) || strings.Count(resultsOf(body), `<li class="ok">`) != 1 {
		t.Errorf("want a's skip as no change and b ok:\n%s", body)
	}

	// Every pod already there: nothing is sent.
	before := len(patches(fb))
	if _, body := post(t, srv, "/workers/orders/concurrency", multi("n", "8"), true); strings.Count(body, "no change (already 8)") != 2 {
		t.Errorf("second n=8 should skip both pods:\n%s", body)
	}
	if got := patches(fb)[before:]; len(got) != 0 {
		t.Errorf("second n=8 sent %q", got)
	}
}

// A worker a unit lists but did not answer for is not "not on this instance": the
// action is sent there like to any unit, so the confirm panel must not say
// otherwise.
func TestBulkConfirmUnreachedIsNotAbsent(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	f := startLive(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 3600, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}}, "multi")
	srv := f.srv
	// The first poll ran and the next is an hour away: this snapshot is the latest.
	unit := func(endpoint string, workers map[string]*fleet.Sample, unreached ...string) fleet.PodSnapshot {
		return fleet.PodSnapshot{Key: endpoint, Via: endpoint, Label: endpoint, Workers: workers, Unreached: unreached}
	}
	f.write("multi", snap(time.Now(),
		unit(aSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 0, 4, 0)}),
		unit(bSrv.URL, nil, "orders"),
	))

	_, body := post(t, srv, "/bulk/pause", multi("names", "orders", "names", "emails"), true)

	// emails is on neither unit: both cells say so. orders is on a and
	// unreached on b: both are sent.
	if absent, paused := strings.Count(body, `<span class="na">not on this instance</span>`), strings.Count(body, `<td>pause</td>`); absent != 2 || paused != 2 {
		t.Errorf("confirm panel has %d absent and %d pause cells, want 2 and 2:\n%s", absent, paused, body)
	}
	if _, orders, _ := strings.Cut(body, `<th scope="row" class="name">orders</th>`); !strings.HasPrefix(orders, `<td>pause</td><td>pause</td>`) {
		t.Errorf("orders row should read pause on both units:\n%s", body)
	}
}

func TestBulkConfirmThenExecute(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	form := multi("names", "orders", "names", "emails", "names", "orders")
	resp, body := post(t, srv, "/bulk/pause", form, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm step = %d, want 200", resp.StatusCode)
	}
	labels := fleettest.StaticLabels(aSrv.URL, bSrv.URL)
	for _, want := range []string{
		"Confirm pause on multi",
		`<th scope="row" class="name">emails</th>`,
		`<th scope="row" class="name">orders</th>`,
		`<th title="a via ` + aSrv.URL + `">` + labels[aSrv.URL] + `</th>`,
		`<th title="b via ` + bSrv.URL + `">` + labels[bSrv.URL] + `</th>`,
		`<input type="hidden" name="confirm" value="1">`,
		`<input type="hidden" name="server" value="multi">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm panel lacks %q:\n%s", want, body)
		}
	}
	// emails is not on b: its cell says so; the other three are sent.
	absent := `<td><span class="na">not on this instance</span></td>`
	if n, m := strings.Count(body, `<td>pause</td>`), strings.Count(body, absent); n != 3 || m != 1 {
		t.Errorf("confirm panel has %d pause and %d absent cells, want 3 and 1 (2 names × 2 pods)", n, m)
	}
	emailsRow := `<td>pause</td>` + absent // pods are sorted by endpoint
	if bSrv.URL < aSrv.URL {
		emailsRow = absent + `<td>pause</td>`
	}
	if _, emails, _ := strings.Cut(body, `<th scope="row" class="name">emails</th>`); !strings.HasPrefix(emails, emailsRow) {
		t.Errorf("emails row should read pause on a, absent on b:\n%s", body)
	}
	noPatches(t, fa, fb)

	form.Set("confirm", "1")
	resp, body = post(t, srv, "/bulk/pause", form, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("execute step = %d, want 200", resp.StatusCode)
	}
	wantPatches(t, fa, "PATCH /workers/orders/actions/pause", "PATCH /workers/emails/actions/pause")
	// Bulk still fans out to every pod in scope; varmq's 404 for emails on
	// b is no change, not a failure.
	wantPatches(t, fb, "PATCH /workers/orders/actions/pause", "PATCH /workers/emails/actions/pause")
	if n := strings.Count(resultsOf(body), "<li "); n != 4 {
		t.Errorf("%d results, want one per name × pod:\n%s", n, body)
	}
	if r := resultsOf(body); strings.Count(r, "no change (not on this instance)") != 1 || strings.Contains(r, `<li class="failed">`) {
		t.Errorf("emails on b should read no change (not on this instance), nothing failed:\n%s", r)
	}
	for _, want := range []string{`<span class="worker">orders</span>`, `<span class="worker">emails</span>`} {
		if strings.Count(body, want) != 2 {
			t.Errorf("results should name each worker on both pods: %q", want)
		}
	}

	t.Run("one instance", func(t *testing.T) {
		before := len(patches(fb))
		a := labels[aSrv.URL]
		_, body := post(t, srv, "/bulk/resume", multi("names", "orders", "instance", a), true)
		if strings.Count(body, "<td>resume</td>") != 1 || !strings.Contains(body, `<input type="hidden" name="instance" value="`+a+`">`) {
			t.Errorf("instance-scoped confirm should list one instance and carry it:\n%s", body)
		}
		post(t, srv, "/bulk/resume", multi("names", "orders", "instance", a, "confirm", "1"), true)
		if got := patches(fb)[before:]; len(got) != 0 {
			t.Errorf("pod-scoped bulk reached b: %q", got)
		}
	})
}

// liveOOB is the out-of-band #live region of an action response, "" if none.
func liveOOB(body string) string {
	_, live, ok := strings.Cut(body, `<div id="live" hx-swap-oob="outerHTML">`)
	if !ok {
		return ""
	}
	return live
}

func TestActionResponseReflectsNewState(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)
	paused := `<span class="status paused"` // its title lists each pod's status

	if _, body := fetch(t, srv, "/workers/orders", false); strings.Contains(body, paused) {
		t.Fatal("orders is paused before the action")
	}
	req := postRequest(t, srv, "/workers/orders/actions/pause", multi())
	req.Header.Set("HX-Current-URL", srv.URL+"/workers/orders?server=multi")
	_, body := send(t, srv, req)
	live := liveOOB(body)
	if !strings.Contains(live, "<h2>orders</h2>") || !strings.Contains(live, paused) || !strings.Contains(live, ">Paused</span>") {
		t.Errorf("response's #live should show orders paused:\n%s", body)
	}
	if strings.Index(body, `<ul class="results">`) > strings.Index(body, `id="live"`) {
		t.Error("results should come before the out-of-band #live")
	}
}

// TestActionRefreshesPageItCameFrom: the out-of-band #live is the page named
// by HX-Current-URL; without it, the page that carries the route's controls.
func TestActionRefreshesPageItCameFrom(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, c := range []struct {
		path, from string
		form       url.Values
		want       string // in the oob #live; "" = no oob
	}{
		{"/workers/orders/actions/pause", "", multi(), "<h2>orders</h2>"},
		{"/workers/orders/actions/pause", "/workers/orders?server=multi", multi(), "<h2>orders</h2>"},
		{"/workers/orders/actions/resume", "/workers/emails", multi(), "<h2>emails</h2>"},
		{"/workers/orders/actions/pause", "/?server=multi", multi(), `<table class="workers">`},
		{"/workers/orders/concurrency", "/", multi("n", "5"), `<table class="workers">`},
		{"/bulk/resume", "", multi("names", "orders", "confirm", "1"), `<table class="workers">`},
		{"/bulk/pause", "/?server=multi&pod=x", multi("names", "orders", "confirm", "1"), `<table class="workers">`},
		{"/workers/orders/actions/pause", "/grid?server=multi", multi(), ""},
		{"/workers/orders/actions/pause", "/workers/nope", multi(), ""},
	} {
		req := postRequest(t, srv, c.path, c.form)
		if c.from != "" {
			req.Header.Set("HX-Current-URL", srv.URL+c.from)
		}
		resp, body := send(t, srv, req)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<ul class="results">`) {
			t.Errorf("POST %s from %q = %d, want 200 with results", c.path, c.from, resp.StatusCode)
			continue
		}
		live := liveOOB(body)
		if c.want == "" && live != "" || !strings.Contains(live, c.want) {
			t.Errorf("POST %s from %q: oob #live should contain %q:\n%s", c.path, c.from, c.want, live)
		}
	}

	// The bulk confirm step changes nothing, so it refreshes nothing.
	if _, body := post(t, srv, "/bulk/pause", multi("names", "orders"), true); liveOOB(body) != "" {
		t.Error("the confirm panel carries an oob #live")
	}
}

// TestActionResponsesClearErrors: every action and confirm response empties
// #errors out of band, so an earlier failure does not outlive a success.
func TestActionResponsesClearErrors(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)
	clear := `<div id="errors" hx-swap-oob="innerHTML"></div>`

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/orders/actions/pause", multi()},
		{"/workers/orders/concurrency", multi("n", "6")},
		{"/bulk/resume", multi("names", "orders")},
		{"/bulk/resume", multi("names", "orders", "confirm", "1")},
	} {
		if resp, body := post(t, srv, c.path, c.form, true); resp.StatusCode != http.StatusOK || !strings.Contains(body, clear) {
			t.Errorf("POST %s %v = %d, want 200 with %s:\n%s", c.path, c.form, resp.StatusCode, clear, body)
		}
	}
}

// TestFailedRefreshKeepsResults: once varmq was called, a failing #live
// refresh must not turn the response into a 500 that hides the results.
func TestFailedRefreshKeepsResults(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	tmpl["dashboard"] = template.Must(template.New("broken").Parse(`{{define "live_oob"}}<div id="live">{{.Nope}}</div>{{end}}`))
	h := &handler{tmpl: tmpl}

	rec := httptest.NewRecorder()
	h.fragments(rec,
		fragment{"results", "results", ResultsView{Results: []fleet.ActionResult{{Pod: "a", Key: "http://a:3000", Outcome: "ok"}}, Noun: podNoun}},
		fragment{"dashboard", "live_oob", Page{}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `<li class="ok">`) {
		t.Fatalf("= %d, want 200 with the results:\n%s", rec.Code, body)
	}
	if strings.Contains(body, `<div id="live"`) {
		t.Error("half-rendered #live in the response")
	}
	if !strings.Contains(body, `<div id="errors" hx-swap-oob="innerHTML"><p class="err">could not refresh the page: render dashboard/live_oob: `) {
		t.Errorf("no note in #errors about the failed refresh:\n%s", body)
	}

	// Without results there is nothing to keep.
	rec = httptest.NewRecorder()
	h.fragments(rec, fragment{"dashboard", "live_oob", Page{}})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("failing first fragment = %d, want 500", rec.Code)
	}
}

// TestNoRefreshForGonePod: an action response never fills #live with other
// units' data under a label the newest snapshot lacks.
func TestNoRefreshForGonePod(t *testing.T) {
	snaps := []*fleet.Snapshot{snap(atTick(0), okPod("http://b", map[string]*fleet.Sample{"orders": smp("Running", 1, 0, 0, 0)}))}
	for _, from := range []string{"/", "/workers/orders"} {
		req := httptest.NewRequest(http.MethodPost, "/workers/orders/actions/pause", nil)
		req.Header.Set("HX-Current-URL", "http://127.0.0.1"+from)
		if set, _, ok := livePage(req, "multi", instanceNoun, 5*time.Second, snaps, "http://a", "orders", ""); ok {
			t.Errorf("from %s: livePage refreshes %s for a gone unit", from, set)
		}
		if _, _, ok := livePage(req, "multi", instanceNoun, 5*time.Second, snaps, "http://b", "orders", ""); !ok {
			t.Errorf("from %s: livePage refuses a reported unit", from)
		}
	}
}

// TestLiveRefreshBreaksAtGaps (R39): the #live an action response
// refreshes draws its charts as the page's poll does, by time and broken
// where polls are missing: it knows the server's interval.
func TestLiveRefreshBreaksAtGaps(t *testing.T) {
	var snaps []*fleet.Snapshot
	for _, n := range []int{0, 1, 2, 480, 481, 482} { // 40 minutes with none between
		s := smp("Running", 1, 0, 0, 0)
		s.CompletedRate, s.RateOK = 1, true
		snaps = append(snaps, snap(atTick(n), okPod("http://a", map[string]*fleet.Sample{"orders": s})))
	}
	req := httptest.NewRequest(http.MethodPost, "/workers/orders/actions/pause", nil)
	req.Header.Set("HX-Current-URL", "http://127.0.0.1/workers/orders?server=multi")
	set, p, ok := livePage(req, "multi", instanceNoun, 5*time.Second, snaps, "", "orders", "")
	if !ok || set != "detail" {
		t.Fatalf("livePage = %q, %v; want the detail", set, ok)
	}
	body := run(t, set, "live_oob", p)
	_, chart, _ := strings.Cut(body, `<figure class="chart">`)
	chart, _, _ = strings.Cut(chart, "</figure>")
	if n := strings.Count(chart, "<polyline"); n != 2 {
		t.Errorf("the refreshed chart has %d lines, want 2, broken at the 40 minutes with no poll:\n%s", n, chart)
	}
}

// resultLine is the results line of unit u reading outcome.
func resultLine(u fleet.PodSnapshot, outcome string) string {
	return `<span class="pod" title="` + u.Tip() + `">` + u.Label + `</span> <span class="outcome">` + outcome + `</span>`
}

// wantResults fails the test unless the results of body have exactly one
// line per key of want, in want's order, reading its outcome.
func wantResults(t *testing.T, body string, units map[string]fleet.PodSnapshot, want ...[2]string) {
	t.Helper()
	results := html.UnescapeString(resultsOf(body))
	if n := strings.Count(results, "<li "); n != len(want) {
		t.Errorf("%d results, want %d:\n%s", n, len(want), results)
	}
	last := -1
	for _, w := range want {
		line := html.UnescapeString(resultLine(units[w[0]], w[1]))
		i := strings.Index(results, line)
		switch {
		case i < 0:
			t.Errorf("results lack %q:\n%s", line, results)
		case i < last:
			t.Errorf("result %q is out of order:\n%s", line, results)
		}
		last = i
	}
}

// totalPatches is how many PATCHes fakes received between them.
func totalPatches(fakes ...*fleettest.Varmq) int {
	n := 0
	for _, f := range fakes {
		n += len(patches(f))
	}
	return n
}

// wantWorker fails the test unless cond holds for worker w on each of fakes.
func wantWorker(t *testing.T, what string, cond func(fleettest.Worker) bool, fakes ...*fleettest.Varmq) {
	t.Helper()
	for _, f := range fakes {
		if w, _ := f.Worker("w"); !cond(w) {
			t.Errorf("w on %s: %+v, want %s", f.Instance, w, what)
		}
	}
}

func paused(w fleettest.Worker) bool { return w.Status == "Paused" }

// TestActionMergedReachesEveryInstance: a merged action through a balancer
// reaches every instance behind it, one result each, within the budget.
func TestActionMergedReachesEveryInstance(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	units := fleet.UnitsOf(f.latest("multi"))

	resp, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	wantResults(t, body, units, [2]string{"#a", "ok"}, [2]string{"#b", "ok"}, [2]string{"#c", "ok"})
	for i, key := range []string{"#a", "#b", "#c"} {
		if want := fmt.Sprintf("instance-%d", i+1); units[key].Label != want {
			t.Errorf("unit %s is labelled %q, want %q", key, units[key].Label, want)
		}
	}
	wantWorker(t, "paused", paused, fakes...)
	if n := totalPatches(fakes...); n > budget3 {
		t.Errorf("%d PATCHes, want at most %d", n, budget3)
	}
}

// TestActionBalancedRepeatIsNoChange409: a balanced unit is never skipped
// from the latest sample; varmq's 409 there, for a concurrency it already
// has, is no change.
func TestActionBalancedRepeatIsNoChange409(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	fakes[1].Mu.Lock()
	fakes[1].Workers["w"].Concurrency = 4
	fakes[1].Mu.Unlock()
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	units := fleet.UnitsOf(f.latest("multi"))

	_, body := post(t, srv, "/workers/w/concurrency", multi("n", "4"), true)
	wantResults(t, body, units,
		[2]string{"#a", "ok"}, [2]string{"#b", "no change (" + fleettest.SameConcurrency + ")"}, [2]string{"#c", "ok"})
	wantWorker(t, "concurrency 4", func(w fleettest.Worker) bool { return w.Concurrency == 4 }, fakes[0], fakes[2])
	if len(patches(fakes[1])) == 0 {
		t.Error("b was skipped: a balanced unit's PATCH is always sent")
	}
	if n := totalPatches(fakes...); n > budget3 {
		t.Errorf("%d PATCHes, want at most %d", n, budget3)
	}
}

// TestActionBalancedUnreachableInstance: an instance that left the balancer
// gets a failure naming the attempts, the others their outcome.
func TestActionBalancedUnreachableInstance(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	units := fleet.UnitsOf(f.latest("multi"))
	srvs[2].Close() // c leaves: the balancer answers its turns with a bare 502

	_, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	wantResults(t, body, units, [2]string{"#a", "ok"}, [2]string{"#b", "ok"},
		[2]string{"#c", "failed (not reached through the load balancer after 18 tries)"})
	wantWorker(t, "paused", paused, fakes[0], fakes[1])
	if n := totalPatches(fakes...); n > budget3 {
		t.Errorf("%d PATCHes, want at most %d", n, budget3)
	}
}

// TestActionBalancedErrorIsRetried: an instance answering a 500 through the
// balancer is tried again within the budget; with no other answer from it,
// it fails with varmq's error.
func TestActionBalancedErrorIsRetried(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	units := fleet.UnitsOf(f.latest("multi"))
	setActionStatus(fakes[1], "w", http.StatusInternalServerError)

	_, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	wantResults(t, body, units, [2]string{"#a", "ok"},
		[2]string{"#b", "failed (API error (status 500): Internal Server Error)"}, [2]string{"#c", "ok"})
	if n := len(patches(fakes[1])); n < 2 {
		t.Errorf("b got %d PATCHes, want it tried again after its 500", n)
	}
	wantWorker(t, "paused", paused, fakes[0], fakes[2])
}

// TestConcurrencyMixedScope: an instance listed by its own address and
// behind the balancer too is one unit, set at its address. When the
// balancer hands it the PATCH again, varmq answers that duplicate 409, which
// is no result: every unit reads ok.
//
// The balancer's PATCHes take turns of their own, starting at a, after a
// head start for a's own address, so its first one is a's duplicate. (With
// no head start the balancer can reach a first; a's own PATCH is then the
// duplicate and reads no change.)
func TestConcurrencyMixedScope(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b")
	var gets, sets atomic.Uint64
	lb := fleettest.NewLB(t, srvs, func(r *http.Request) uint64 {
		if r.Method != http.MethodPatch {
			return gets.Add(1) - 1
		}
		time.Sleep(100 * time.Millisecond)
		return sets.Add(1) - 1
	})
	srv, f := serveBalanced(t, []string{lb.URL, srvs[0].URL}, srvs[0].URL, "#b")
	latest := f.latest("multi")
	var want [][2]string
	for _, u := range latest.Pods {
		want = append(want, [2]string{u.Key, "ok"})
	}

	_, body := post(t, srv, "/workers/w/concurrency", multi("n", "4"), true)
	wantResults(t, body, fleet.UnitsOf(latest), want...)
	wantWorker(t, "concurrency 4", func(w fleettest.Worker) bool { return w.Concurrency == 4 }, fakes...)
	set := "PATCH /workers/w/config/concurrency/4"
	wantPatches(t, fakes[0], set, set) // its own, then the balancer's duplicate
	wantPatches(t, fakes[1], set)
}

// TestActionBudgetCountsEveryInstanceBehind: a balancer picks among every
// instance behind it, those that are direct units too, so the attempts an
// action gets through it are sized by all of them, not by the units it
// wants there.
func TestActionBudgetCountsEveryInstanceBehind(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL, srvs[1].URL, srvs[2].URL}, "#a", srvs[1].URL, srvs[2].URL)
	f.waitFor("multi", "3 instances behind the balancer", func(s *fleet.Snapshot) bool { return s.Behind[lb.URL] == 3 })
	units := fleet.UnitsOf(f.latest("multi"))
	srvs[0].Close() // a leaves: the balancer answers its turns with a bare 502

	_, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	results := html.UnescapeString(resultsOf(body))
	if line := html.UnescapeString(resultLine(units["#a"], "failed (not reached through the load balancer after 18 tries)")); !strings.Contains(results, line) {
		t.Errorf("results lack %q:\n%s", line, results)
	}
	wantWorker(t, "paused", paused, fakes[1], fakes[2])
}

// TestActionThroughHangingBalancerHasADeadline: a balancer that holds every
// request costs an action at most fleet.BalancerActionDeadline, not the
// attempt budget times the client timeout, and the instances read not
// reached.
func TestActionThroughHangingBalancerHasADeadline(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb, hang := fleettest.NewHangingLB(t, fleettest.NewRoundRobinLB(t, srvs...))
	srv, _ := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	hang.Store(true)

	start := time.Now()
	resp, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	// The deadline, then at most fleet.SettleWait for the refresh.
	if elapsed := time.Since(start); elapsed > fleet.BalancerActionDeadline+fleet.SettleWait+time.Second {
		t.Errorf("the action took %v, want it cut at %v", elapsed, fleet.BalancerActionDeadline)
	}
	results := resultsOf(body)
	if resp.StatusCode != http.StatusOK || strings.Count(results, "failed (not reached through the load balancer after ") != 3 {
		t.Errorf("status %d, want 200 with every instance not reached:\n%s", resp.StatusCode, results)
	}
	noPatches(t, fakes...)
}

// TestRestartRefusedWithBalancedScope: restart never goes through a balancer.
func TestRestartRefusedWithBalancedScope(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs[0], srvs[1])
	srv, _ := serveBalanced(t, []string{lb.URL, srvs[2].URL}, "#a", "#b", srvs[2].URL)

	resp, body := post(t, srv, "/workers/w/actions/restart", multi(), true)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(html.UnescapeString(body), "restart isn't available through a load balancer") {
		t.Errorf("merged restart = %d %q, want 400 with the refusal", resp.StatusCode, body)
	}
	noPatches(t, fakes...)
}

// oneUnitCopy is the refusal of an action on one balanced unit, and the note
// shown instead of its controls.
const oneUnitCopy = "A load balancer picks which instance answers, so actions here would reach a random one. Use All instances (merged), or list the instances' own addresses in the server config."

// TestOneBalancedUnitRefused: nothing is sent to one balanced unit: the
// balancer would pick the instance.
func TestOneBalancedUnitRefused(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	one := fleet.UnitsOf(f.latest("multi"))["#a"].Label

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/w/actions/pause", multi("instance", one)},
		{"/workers/w/actions/restart", multi("instance", one)},
		{"/workers/w/concurrency", multi("instance", one, "n", "3")},
		{"/bulk/pause", multi("instance", one, "names", "w")},
		{"/bulk/pause", multi("instance", one, "names", "w", "confirm", "1")},
	} {
		resp, body := post(t, srv, c.path, c.form, true)
		if resp.StatusCode != http.StatusBadRequest || html.UnescapeString(body) != oneUnitCopy+"\n" {
			t.Errorf("POST %s instance=%s = %d %q, want 400 with the one-unit refusal", c.path, one, resp.StatusCode, body)
		}
	}
	noPatches(t, fakes...)
}

// TestMixedScope: an instance with its own address is sent the action
// there, once; the instances behind the balancer through it. The confirm
// panel and the results list the units by number, as the pages do.
func TestMixedScope(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs[0], srvs[1])
	srv, f := serveBalanced(t, []string{lb.URL, srvs[2].URL}, "#a", "#b", srvs[2].URL)
	latest := f.latest("multi")
	units := fleet.UnitsOf(latest)
	var head strings.Builder
	var want [][2]string
	for i, u := range latest.Pods {
		if label := fmt.Sprintf("instance-%d", i+1); u.Label != label {
			t.Fatalf("unit %d is %s %s, want the units listed by number", i, u.Key, u.Label)
		}
		head.WriteString(`<th title="` + u.Tip() + `">` + u.Label + `</th>`)
		want = append(want, [2]string{u.Key, "ok"})
	}

	if _, body := post(t, srv, "/bulk/pause", multi("names", "w"), true); !strings.Contains(body, head.String()) {
		t.Errorf("confirm panel lacks the columns %q:\n%s", head.String(), body)
	}
	_, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	wantResults(t, body, units, want...)
	wantPatches(t, fakes[2], "PATCH /workers/w/actions/pause")
	wantWorker(t, "paused", paused, fakes...)
}

// TestDeadBalancerUnit: a balancer endpoint no instance answered is a unit
// that fails with its error; nothing is sent to it, not even directly.
func TestDeadBalancerUnit(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	f := startLive(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 3600, Endpoints: []string{lb.URL}},
	}}, "multi")
	srv := f.srv
	// The first poll ran and the next is an hour away: this snapshot is the latest.
	dead := fleet.PodSnapshot{Key: lb.URL, Via: lb.URL, Label: "instance-1", Balanced: true, Err: "x"}
	f.write("multi", snap(time.Now(), dead))
	units := fleet.UnitsOf(f.latest("multi"))

	if resp, _ := post(t, srv, "/workers/w/actions/restart", multi(), true); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("restart with a dead balancer unit in scope = %d, want 400", resp.StatusCode)
	}
	_, body := post(t, srv, "/bulk/pause", multi("names", "w"), true)
	if want := `<th title="` + lb.URL + `">instance-1<small class="err">x</small></th>`; !strings.Contains(body, want) {
		t.Errorf("confirm panel lacks the dead unit's column %q:\n%s", want, body)
	}
	// Last: its response pokes, and the next snapshot finds the endpoint direct.
	_, body = post(t, srv, "/workers/w/actions/pause", multi(), true)
	wantResults(t, body, units, [2]string{lb.URL, "failed (x)"})
	noPatches(t, fakes...)
}

// TestBulkConfirmColumnsAreUnits: behind a balancer, the confirm panel's
// columns are the instances, and the bulk action reaches each one per worker.
func TestBulkConfirmColumnsAreUnits(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	fakes[0].Mu.Lock()
	fakes[0].Workers["v"] = &fleettest.Worker{Status: "Running", Concurrency: 1}
	fakes[0].Mu.Unlock()
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	srv, f := serveBalanced(t, []string{lb.URL}, "#a", "#b", "#c")
	units := fleet.UnitsOf(f.latest("multi"))

	_, body := post(t, srv, "/bulk/pause", multi("names", "w", "names", "v"), true)
	var head strings.Builder
	head.WriteString(`<th>Worker</th>`)
	for _, key := range []string{"#a", "#b", "#c"} {
		head.WriteString(`<th title="` + units[key].Tip() + `">` + units[key].Label + `</th>`)
	}
	if !strings.Contains(body, head.String()) {
		t.Errorf("confirm panel lacks the columns %q:\n%s", head.String(), body)
	}
	// v is on a only.
	if _, v, _ := strings.Cut(body, `<th scope="row" class="name">v</th>`); !strings.HasPrefix(v, `<td>pause</td><td><span class="na">not on this instance</span></td><td><span class="na">not on this instance</span></td>`) {
		t.Errorf("v's row should read pause on a only:\n%s", body)
	}
	noPatches(t, fakes...)

	_, body = post(t, srv, "/bulk/pause", multi("names", "w", "names", "v", "confirm", "1"), true)
	results := html.UnescapeString(resultsOf(body))
	if n := strings.Count(results, "<li "); n != 6 {
		t.Errorf("%d results, want one per worker × instance:\n%s", n, results)
	}
	for _, c := range []struct{ worker, key, outcome string }{
		{"v", "#a", "ok"}, {"v", "#b", "no change (not on this instance)"}, {"v", "#c", "no change (not on this instance)"},
		{"w", "#a", "ok"}, {"w", "#b", "ok"}, {"w", "#c", "ok"},
	} {
		if line := `<span class="worker">` + c.worker + `</span> on ` + resultLine(units[c.key], c.outcome); !strings.Contains(results, line) {
			t.Errorf("results lack %q:\n%s", line, results)
		}
	}
	wantWorker(t, "paused", paused, fakes...)
	if w, _ := fakes[0].Worker("v"); w.Status != "Paused" {
		t.Errorf("v on a: %+v, want paused", w)
	}
}

// TestActionResponsesUseNoun: an action's results and the page it refreshes
// out of band name the server's units by its noun.
func TestActionResponsesUseNoun(t *testing.T) {
	for _, tc := range []struct {
		name       string
		server     config.Server
		one, other string
		live       map[string]string // page the action came from → copy its #live carries
	}{
		{"static", config.Server{}, "instance", "pod", map[string]string{"/workers/emails": "Instances reporting:", "/": "Pending (max/instance)"}},
		{"kubernetes", kubernetesServer, "pod", "instance", map[string]string{"/workers/emails": "Pods reporting:", "/": "Pending (max/pod)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveUnits(t, tc.server, threeFakes(t)...)
			for from, want := range tc.live {
				// emails lives on one unit; the others answer 404.
				req := postRequest(t, srv, "/workers/emails/actions/resume", multi())
				req.Header.Set("HX-Current-URL", srv.URL+from)
				_, body := send(t, srv, req)
				if want := "no change (not on this " + tc.one + ")"; !strings.Contains(resultsOf(body), want) {
					t.Errorf("from %s: results lack %q:\n%s", from, want, resultsOf(body))
				}
				if !strings.Contains(liveOOB(body), want) {
					t.Errorf("from %s: oob #live lacks %q:\n%s", from, want, liveOOB(body))
				}
				if text := strings.ToLower(visibleText(body)); strings.Contains(text, tc.other) {
					t.Errorf("from %s: response says %q:\n%s", from, tc.other, text)
				}
			}
		})
	}
}

// TestActionRefusedWhenStale: with a stale newest snapshot nothing is sent:
// what the pages show may no longer be the server. With no daemon running
// the refusal is at once; with one, the server is poked and given
// fleet.SettleWait to answer first.
func TestActionRefusedWhenStale(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 10, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}})
	f.claim()
	f.write("multi", snap(time.Now().Add(-60*time.Second),
		labelled(okPod(aSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}), "instance-1"),
		labelled(okPod(bSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 2, 2, 0)}), "instance-2")))
	f.release()

	// About a minute old: the age is rounded to the second when refused.
	down := regexp.MustCompile(`^<p class="banner">No new data for 1m\ds — the vartop daemon is not running\.</p>$`)
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/orders/actions/pause", multi()},
		{"/workers/orders/actions/stop", multi("instance", "instance-1")},
		{"/workers/orders/concurrency", multi("n", "9")},
		{"/bulk/pause", multi("names", "orders")},
		{"/bulk/pause", multi("names", "orders", "confirm", "1")},
	} {
		start := time.Now()
		resp, body := post(t, f.srv, c.path, c.form, true)
		if resp.StatusCode != http.StatusConflict || !down.MatchString(body) {
			t.Errorf("POST %s %v = %d %q, want 409 with %q", c.path, c.form, resp.StatusCode, body, down)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("POST %s took %v: no daemon answers a poke, so nothing is waited for", c.path, took)
		}
	}

	// A daemon holds the lease but does not answer: poked, then refused.
	f.claim()
	before := f.pokes("multi")
	start := time.Now()
	resp, body := post(t, f.srv, "/workers/orders/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusConflict || !strings.HasPrefix(body, `<p class="banner">No new data for 1m`) {
		t.Errorf("POST with a daemon that does not answer = %d %q, want 409 with the stale copy", resp.StatusCode, body)
	}
	if took := time.Since(start); took < fleet.SettleWait/2 || f.pokes("multi") != before+1 {
		t.Errorf("POST took %v with %d pokes, want the server poked once and waited for", took, f.pokes("multi")-before)
	}
	noPatches(t, fa, fb)
}

// TestActionPokesAndShowsResult: after an action the server is poked, and
// the response shows the snapshot taken after the poke: here the only one,
// with the next regular poll an hour away.
func TestActionPokesAndShowsResult(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	f := startLive(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 3600, Endpoints: []string{aSrv.URL, bSrv.URL}},
	}}, "multi")
	first := f.latest("multi")
	before := f.pokes("multi")

	req := postRequest(t, f.srv, "/workers/orders/actions/pause", multi())
	req.Header.Set("HX-Current-URL", f.srv.URL+"/workers/orders?server=multi")
	resp, body := send(t, f.srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d, want 200:\n%s", resp.StatusCode, body)
	}
	if n := f.pokes("multi") - before; n != 1 {
		t.Errorf("the action poked %d times, want once", n)
	}
	if live := liveOOB(body); !strings.Contains(live, ">Paused</span>") {
		t.Errorf("response's #live should show orders paused, from the poked snapshot:\n%s", live)
	}
	if latest := f.latest("multi"); !latest.At.After(first.At) {
		t.Error("no snapshot was taken after the poke")
	}
}

// patchBreaker makes every PATCH a fresh connection sends to its hosts
// panic, on the goroutine that sends it: a stand-in for a reply that
// breaks whatever reads it, on a goroutine an action fans out to. It works
// through http.DefaultTransport's Proxy func, which the api package's fresh
// connections copy when their client is made.
type patchBreaker struct {
	mu    sync.Mutex
	hosts map[string]bool
}

// newPatchBreaker installs a breaker until the test ends. Make it before
// the servers it breaks, so that it is removed after they have stopped.
func newPatchBreaker(t *testing.T) *patchBreaker {
	t.Helper()
	b := &patchBreaker{hosts: make(map[string]bool)}
	tr := http.DefaultTransport.(*http.Transport)
	orig := tr.Proxy
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		b.mu.Lock()
		broken := b.hosts[req.URL.Host]
		b.mu.Unlock()
		if broken && req.Method == http.MethodPatch {
			panic("reply broke")
		}
		return orig(req)
	}
	t.Cleanup(func() { tr.Proxy = orig })
	return b
}

// add breaks the host of the URL u.
func (b *patchBreaker) add(t *testing.T, u string) {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hosts[parsed.Host] = true
}

// TestActionPanicIs500 (carried from task 4): a send that panics on a
// goroutine the action fans out to — here an attempt through a balancer —
// fails that request with a 500, and vartop web serves on.
func TestActionPanicIs500(t *testing.T) {
	b := newPatchBreaker(t)
	fakes, srvs := fleettest.NewInstances(t, "a", "b")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	b.add(t, lb.URL)
	srv, _ := serveBalanced(t, []string{lb.URL}, "#a", "#b")

	resp, body := post(t, srv, "/workers/w/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "internal error: reply broke") {
		t.Errorf("an action whose send panics = %d %q, want 500 naming the panic", resp.StatusCode, body)
	}
	if status, body := fetch(t, srv, "/?server=multi", false); status != http.StatusOK || !strings.Contains(body, ">w<") {
		t.Errorf("GET / after the panic = %d, want the dashboard: the server serves on", status)
	}
	noPatches(t, fakes...)
}

// labelled is u labelled label.
func labelled(u fleet.PodSnapshot, label string) fleet.PodSnapshot {
	u.Label = label
	return u
}

// TestOldFormPOSTSendsNothing (R22): a form from before labels — a tab
// left open across the upgrade — names its unit by key, under pod= on
// every server. A POST takes the unit parameter as it is: a key is no
// label, and the other noun's parameter is no unit of this server; either
// is refused and nothing is sent, where reading it as absent would act on
// every unit. An empty one is the old merged form: it acts on every unit.
func TestOldFormPOSTSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server config.Server
		param  string // this server's
		other  string // the other noun's
		names  string // a unit by the label the server gives it
	}{
		{"static", config.Server{}, "instance", "pod", "instance-1"},
		{"kubernetes", kubernetesServer, "pod", "instance", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			varmqs, fakes := threeVarmqs(t)
			srv := serveUnits(t, tc.server, fakes...)
			for _, unit := range []url.Values{
				{tc.param: {fakes[0].URL}},
				{tc.param: {"#a"}},
				{tc.other: {fakes[0].URL}},
				{tc.other: {"#a"}},
				{tc.other: {tc.names}},
			} {
				for _, c := range []struct {
					path string
					form url.Values
				}{
					{"/workers/orders/actions/pause", multi()},
					{"/workers/orders/concurrency", multi("n", "9")},
					{"/bulk/pause", multi("names", "orders")},
					{"/bulk/pause", multi("names", "orders", "confirm", "1")},
				} {
					for k, v := range unit {
						c.form[k] = v
					}
					resp, body := post(t, srv, c.path, c.form, true)
					if resp.StatusCode != http.StatusConflict || resp.Header.Get("HX-Retarget") != "#banner" {
						t.Errorf("POST %s %v = %d %q, want 409 in the banner", c.path, c.form, resp.StatusCode, body)
					}
				}
			}
			noPatches(t, varmqs...)

			// The old merged form, with the other noun's parameter empty.
			resp, body := post(t, srv, "/workers/orders/actions/pause", multi(tc.other, ""), true)
			if resp.StatusCode != http.StatusOK || strings.Count(resultsOf(body), `<li class="ok">`) != 3 {
				t.Errorf("POST with an empty %s= = %d, want every unit paused:\n%s", tc.other, resp.StatusCode, resultsOf(body))
			}
		})
	}
}

// TestActionResponseShowsStale: an action's refreshed #live, like a page,
// shows no current value once the newest snapshot is stale — here because
// no daemon answers the poke after the action, and the snapshot ages past
// the threshold while the response waits for one.
func TestActionResponseShowsStale(t *testing.T) {
	_, _, aSrv, bSrv := twoFakes(t)
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 2, Endpoints: []string{aSrv.URL, bSrv.URL}}, // stale after 3·2s + 2s = 8s
	}})
	// A daemon runs, as far as the lease says, but answers no poke. It
	// claimed before the snapshot was taken, which is its own (R44); its
	// lease, 7s old, holds for 8s more.
	now := time.Now()
	f.claimAt(now.Add(-7 * time.Second))
	// 6.2s old: fresh to bind the action with 1.8s to spare (for the write
	// and the POST); stale once the response has waited out the whole
	// SettleWait (2s) for a snapshot no daemon takes, 8.2s or more old.
	// The two margins can only sum to SettleWait; the response's is sure,
	// time moving only forward.
	f.write("multi", snap(now.Add(-6200*time.Millisecond),
		labelled(okPod(aSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 1, 3, 0)}), "instance-1"),
		labelled(okPod(bSrv.URL, map[string]*fleet.Sample{"orders": smp("Running", 4, 2, 2, 0)}), "instance-2")))

	req := postRequest(t, f.srv, "/workers/orders/actions/pause", multi())
	req.Header.Set("HX-Current-URL", f.srv.URL+"/?server=multi")
	resp, body := send(t, f.srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d, want 200:\n%s", resp.StatusCode, body)
	}
	if live := liveOOB(body); !strings.Contains(live, `<td class="num">—</td>`) || strings.Contains(live, `<td class="num">3</td>`) {
		t.Errorf("the refreshed #live of a stale server shows current values:\n%s", live)
	}
}
