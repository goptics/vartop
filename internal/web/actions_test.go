package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
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
func patches(f *fakeVarmq) []string {
	var out []string
	for _, c := range f.calls() {
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
func noPatches(t *testing.T, fakes ...*fakeVarmq) {
	t.Helper()
	for _, f := range fakes {
		if got := patches(f); len(got) > 0 {
			t.Errorf("fake %s got %q, want no PATCH", f.Instance, got)
		}
	}
}

// wantPatches fails the test unless f received exactly want, in any order.
func wantPatches(t *testing.T, f *fakeVarmq, want ...string) {
	t.Helper()
	got := patches(f)
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("fake %s got %q, want %q", f.Instance, got, want)
	}
}

// setActionStatus makes every PATCH on f's worker answer status.
func setActionStatus(f *fakeVarmq, worker string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	for _, want := range []string{`title="` + aSrv.URL + `">a</span>`, `title="` + bSrv.URL + `">b</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("results lack %q", want)
		}
	}
}

func TestActionOnePod(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	resp, body := post(t, srv, "/workers/orders/actions/resume", multi("pod", aSrv.URL), true)
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
		done func(fakeWorker) bool
	}{
		{"/workers/orders/actions/stop", multi(), func(w fakeWorker) bool { return w.Status == "Stopped" }},
		{"/workers/orders/concurrency", multi("n", "9"), func(w fakeWorker) bool { return w.Concurrency == 9 }},
		{"/bulk/pause", multi("names", "orders", "confirm", "1"), func(w fakeWorker) bool { return w.Status == "Paused" }},
	} {
		t.Run(strings.TrimPrefix(c.path, "/"), func(t *testing.T) {
			t.Parallel()
			fa, fb, aSrv, bSrv := twoFakes(t)
			srv, _ := newTestServer(t, aSrv, bSrv)
			fb.mu.Lock()
			fb.Delay = time.Second // b answers slowly; it drops the request if vartop gives up on it
			fb.mu.Unlock()

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
			waitUntil(t, 3*time.Second, "b's PATCH under way", func() bool { return len(patches(fb)) == 1 })
			cancel() // the browser aborts
			if err := <-sent; err == nil {
				t.Fatal("the request completed before it was aborted; the test proves nothing")
			}

			for _, f := range []*fakeVarmq{fa, fb} {
				waitUntil(t, 3*time.Second, "orders changed on "+f.Instance, func() bool {
					w, _ := f.worker("orders")
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
		{"/workers/orders/actions/pause", multi("pod", "http://gone.invalid")},
		{"/workers/orders/concurrency", multi("n", "4", "pod", "http://gone.invalid")},
		{"/bulk/restart", multi("names", "orders", "confirm", "1")},
		{"/bulk/stop", multi("names", "orders", "confirm", "1")},
		{"/bulk/pause", multi("names", "orders", "pod", "http://gone.invalid")},
		{"/bulk/pause", multi("names", "orders", "pod", "http://gone.invalid", "confirm", "1")},
		{"/bulk/pause", multi("confirm", "1")},
		{"/bulk/pause", multi("names", "", "confirm", "1")},
	} {
		resp, body := post(t, srv, c.path, c.form, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s %v = %d, want 400", c.path, c.form, resp.StatusCode)
		}
		// htmx swaps the body into #errors as HTML: nothing posted is echoed.
		if strings.Contains(body, "<b>") || strings.Contains(body, "gone.invalid") {
			t.Errorf("POST %s: 400 body echoes the request: %q", c.path, body)
		}
	}
	noPatches(t, fa, fb)
}

func TestPOSTWithoutHXRequest(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, m := newTestServer(t, aSrv, bSrv)

	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/workers/orders/actions/pause", multi()},
		{"/workers/orders/concurrency", multi("n", "9")},
		{"/bulk/pause", multi("names", "orders", "confirm", "1")},
		{"/server", multi("name", "b")},
	} {
		if resp, _ := post(t, srv, c.path, c.form, false); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without HX-Request = %d, want 403", c.path, resp.StatusCode)
		}
	}
	noPatches(t, fa, fb)
	if p := m.Pending(); p != "" {
		t.Errorf("a refused POST /server started switching to %q", p)
	}
}

func TestCrossOriginPOST(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, m := newTestServer(t, aSrv, bSrv)

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
			{"/server", multi("name", "b")},
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
	if p := m.Pending(); p != "" {
		t.Errorf("a cross-origin POST /server started switching to %q", p)
	}
}

func TestGETOnMutatingRoute(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	for _, path := range []string{
		"/workers/orders/actions/pause?server=multi",
		"/workers/orders/concurrency?server=multi&n=3",
		"/bulk/pause?server=multi&names=orders&confirm=1",
		"/server?name=b",
	} {
		for _, hx := range []bool{false, true} {
			if status, _ := fetch(t, srv, path, hx); status != http.StatusMethodNotAllowed {
				t.Errorf("GET %s (hx %v) = %d, want 405", path, hx, status)
			}
		}
	}
	noPatches(t, fa, fb)
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

func TestStaleServerPOST(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, m := newTestServer(t, aSrv, bSrv)

	stale := func(server, banner string) {
		t.Helper()
		for _, c := range []struct {
			path string
			form url.Values
		}{
			{"/workers/orders/actions/pause", url.Values{}},
			{"/workers/orders/concurrency", url.Values{"n": {"9"}}},
			{"/bulk/pause", url.Values{"names": {"orders"}}},
			{"/bulk/pause", url.Values{"names": {"orders"}, "confirm": {"1"}}},
		} {
			if server != "" {
				c.form.Set("server", server)
			}
			resp, body := post(t, srv, c.path, c.form, true)
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("POST %s server=%q = %d, want 409", c.path, server, resp.StatusCode)
			}
			// htmx would swap a 409 into #errors; the headers send it to
			// #banner instead, which the body must not duplicate.
			if resp.Header.Get("HX-Retarget") != "#banner" || resp.Header.Get("HX-Reswap") != "innerHTML" {
				t.Errorf("POST %s: HX-Retarget %q HX-Reswap %q, want #banner innerHTML", c.path, resp.Header.Get("HX-Retarget"), resp.Header.Get("HX-Reswap"))
			}
			if body != `<p class="banner">`+banner+`</p>` {
				t.Errorf("POST %s server=%q body = %q, want the %q banner alone", c.path, server, body, banner)
			}
		}
	}

	stale("other", "Server changed to multi — reload")
	stale("", "Server changed to multi — reload")
	stale("b", "Server changed to multi — reload")
	noPatches(t, fa, fb)

	// The real case: another tab switched to b while this one still shows
	// multi. Nothing may reach multi's pods, which are still reachable.
	if err := m.Switch("b"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "b active", func() bool { key, _ := m.Active(); return key == "b" })
	stale("multi", "Server changed to b — reload")
	noPatches(t, fa, fb)
}

// TestPOSTBindsConfigKey: a POST names its server by configuration key,
// which config.Server.Name need not equal.
func TestPOSTBindsConfigKey(t *testing.T) {
	f, fSrv := newFakeVarmq(t, "a", map[string]*fakeWorker{"orders": {Status: "Running", Concurrency: 1}})
	m := startManager(t, &config.Config{Servers: map[string]*config.Server{
		"prod-eu": {Name: "Production EU", Interval: 1, Endpoints: []string{fSrv.URL}},
	}}, "prod-eu")
	srv := serve(t, m)

	if resp, _ := post(t, srv, "/workers/orders/actions/pause", url.Values{"server": {"Production EU"}}, true); resp.StatusCode != http.StatusConflict {
		t.Errorf("server=<config.Server.Name> = %d, want 409: names are keys", resp.StatusCode)
	}
	noPatches(t, f)
	if resp, _ := post(t, srv, "/workers/orders/actions/pause", url.Values{"server": {"prod-eu"}}, true); resp.StatusCode != http.StatusOK {
		t.Errorf("server=prod-eu = %d, want 200", resp.StatusCode)
	}
	wantPatches(t, f, "PATCH /workers/orders/actions/pause")
}

func TestStaleServerPOSTWithoutSession(t *testing.T) {
	m, _ := newGatedManager(t, "dev")
	srv := serve(t, m)
	if err := m.Switch("dev"); err != nil { // its build stays blocked
		t.Fatal(err)
	}

	resp, body := post(t, srv, "/workers/orders/actions/pause", url.Values{"server": {"dev"}}, true)
	if resp.StatusCode != http.StatusConflict || body != `<p class="banner">Not connected — reload</p>` {
		t.Errorf("POST while nothing is active = %d %q, want 409 and the not-connected banner", resp.StatusCode, body)
	}
}

func TestActionEscapesName(t *testing.T) {
	f, fSrv := newFakeVarmq(t, "a", map[string]*fakeWorker{"a/b c": {Status: "Running", Concurrency: 1}})
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
	fa, aSrv := newFakeVarmq(t, "a", map[string]*fakeWorker{"orders": {Status: "Running", Concurrency: 8}})
	fb, bSrv := newFakeVarmq(t, "b", map[string]*fakeWorker{"orders": {Status: "Running", Concurrency: 4}})
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

func TestBulkConfirmThenExecute(t *testing.T) {
	fa, fb, aSrv, bSrv := twoFakes(t)
	srv, _ := newTestServer(t, aSrv, bSrv)

	form := multi("names", "orders", "names", "emails", "names", "orders")
	resp, body := post(t, srv, "/bulk/pause", form, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm step = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		"Confirm pause on multi",
		`<th scope="row" class="name">emails</th>`,
		`<th scope="row" class="name">orders</th>`,
		`<th title="` + aSrv.URL + `">a</th>`,
		`<th title="` + bSrv.URL + `">b</th>`,
		`<input type="hidden" name="confirm" value="1">`,
		`<input type="hidden" name="server" value="multi">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm panel lacks %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, `<td>pause</td>`); n != 4 {
		t.Errorf("confirm panel has %d cells, want 2 names × 2 pods", n)
	}
	noPatches(t, fa, fb)

	form.Set("confirm", "1")
	resp, body = post(t, srv, "/bulk/pause", form, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("execute step = %d, want 200", resp.StatusCode)
	}
	wantPatches(t, fa, "PATCH /workers/orders/actions/pause", "PATCH /workers/emails/actions/pause")
	// emails is not on b: varmq's 404 is a failure there.
	wantPatches(t, fb, "PATCH /workers/orders/actions/pause", "PATCH /workers/emails/actions/pause")
	if n := strings.Count(resultsOf(body), "<li "); n != 4 {
		t.Errorf("%d results, want one per name × pod:\n%s", n, body)
	}
	for _, want := range []string{`<span class="worker">orders</span>`, `<span class="worker">emails</span>`} {
		if strings.Count(body, want) != 2 {
			t.Errorf("results should name each worker on both pods: %q", want)
		}
	}

	t.Run("one pod", func(t *testing.T) {
		before := len(patches(fb))
		_, body := post(t, srv, "/bulk/resume", multi("names", "orders", "pod", aSrv.URL), true)
		if strings.Count(body, "<td>resume</td>") != 1 || !strings.Contains(body, `<input type="hidden" name="pod" value="`+aSrv.URL+`">`) {
			t.Errorf("pod-scoped confirm should list one pod and carry it:\n%s", body)
		}
		post(t, srv, "/bulk/resume", multi("names", "orders", "pod", aSrv.URL, "confirm", "1"), true)
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

// TestNoRefreshForGonePod: an action response never fills #live with other
// pods' data under a pod the newest snapshot lacks.
func TestNoRefreshForGonePod(t *testing.T) {
	h := NewHistory(time.Second)
	h.Append(snap(atTick(0), okPod("http://b", map[string]*Sample{"orders": smp("Running", 1, 0, 0, 0)})))
	s := &Session{History: h}
	for _, from := range []string{"/", "/workers/orders"} {
		req := httptest.NewRequest(http.MethodPost, "/workers/orders/actions/pause", nil)
		req.Header.Set("HX-Current-URL", "http://127.0.0.1"+from)
		if set, _, ok := livePage(req, "multi", s, "http://a", "orders"); ok {
			t.Errorf("from %s: livePage refreshes %s for a gone pod", from, set)
		}
		if _, _, ok := livePage(req, "multi", s, "http://b", "orders"); !ok {
			t.Errorf("from %s: livePage refuses a reported pod", from)
		}
	}
}

func TestServerSwitchRedirects(t *testing.T) {
	_, fSrv := newFakeVarmq(t, "a", map[string]*fakeWorker{"orders": {Status: "Running", Concurrency: 1}})
	m := startManager(t, &config.Config{Servers: map[string]*config.Server{
		"multi":   {Name: "multi", Interval: 1, Endpoints: []string{fSrv.URL}},
		"a b&c":   {Name: "a b&c", Interval: 1, Endpoints: []string{"http://b.invalid"}},
		"staging": {Name: "staging", Interval: 1, Endpoints: []string{"http://s.invalid"}},
	}}, "multi")
	srv := serve(t, m)

	for _, name := range []string{"", "nope", "Multi"} {
		resp, body := post(t, srv, "/server", multi("name", name), true)
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("HX-Redirect") != "" {
			t.Errorf("POST /server name=%q = %d (HX-Redirect %q), want 400 without redirect", name, resp.StatusCode, resp.Header.Get("HX-Redirect"))
		}
		if name != "" && strings.Contains(body, name) {
			t.Errorf("POST /server name=%q: 400 body echoes the name", name)
		}
	}
	if p := m.Pending(); p != "" {
		t.Fatalf("a rejected POST /server started switching to %q", p)
	}

	resp, _ := post(t, srv, "/server", multi("name", "a b&c"), true)
	if got := resp.Header.Get("HX-Redirect"); resp.StatusCode/100 != 2 || got != "/?server=a+b%26c" {
		t.Errorf("POST /server name=%q = %d, HX-Redirect %q, want 2xx and /?server=a+b%%26c", "a b&c", resp.StatusCode, got)
	}
	waitUntil(t, 3*time.Second, "a b&c active", func() bool { key, _ := m.Active(); return key == "a b&c" })

	// R22: switching never checks the tab's server; this tab still says
	// multi, and a Retry for a failed server posts that server.
	for _, server := range []string{"multi", "staging", ""} {
		resp, _ := post(t, srv, "/server", url.Values{"server": {server}, "name": {"staging"}}, true)
		if got := resp.Header.Get("HX-Redirect"); got != "/?server=staging" {
			t.Errorf("POST /server name=staging server=%q: HX-Redirect %q, want /?server=staging", server, got)
		}
	}
}
