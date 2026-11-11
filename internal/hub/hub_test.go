package hub

import (
	"context"
	"errors"
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
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

const (
	tokA = "token-a-0123456789abcdef"
	tokB = "token-b-0123456789abcdef"
	// slowBeat keeps a beating fake connected under -race; only the ping
	// and missed-beat tests use short heartbeats.
	slowBeat = time.Second
)

// hubConfig is a config of hub servers, name → token.
func hubConfig(servers map[string]string) *config.Config {
	cfg := &config.Config{Servers: make(map[string]*config.Server)}
	for name, tok := range servers {
		cfg.Servers[name] = &config.Server{Name: name, Hub: true, Token: tok}
	}
	return cfg
}

// newTestHub serves a Router with heartbeat hb and at most maxInstances
// per server, syncing servers; both close when the test ends.
func newTestHub(t *testing.T, hb time.Duration, maxInstances int, servers map[string]string) (*Router, string) {
	t.Helper()
	r := newRouter(hb, maxInstances, nil)
	r.Sync(hubConfig(servers))
	ts := httptest.NewUnstartedServer(r)
	ts.Config = NewServer(r, nil, nil)
	ts.Start()
	t.Cleanup(func() {
		r.Close()
		ts.Close()
	})
	return r, ts.URL
}

// send does method url with token ("" none) and returns the status.
func send(t *testing.T, method, u, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func streamURL(base, instance, protocol string) string {
	q := url.Values{}
	if instance != "" {
		q.Set("instance", instance)
	}
	if protocol != "" {
		q.Set("protocol", protocol)
	}
	return base + "/hub/stream?" + q.Encode()
}

// get sends GET path to instance id through rt.
func get(ctx context.Context, rt http.RoundTripper, id, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint(id)+path, nil)
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(req)
}

func TestStreamAuth(t *testing.T) {
	_, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	good := streamURL(base, "i1", "1")
	for _, c := range []struct{ name, url, auth string }{
		{"no header", good, ""},
		{"wrong token", good, "Bearer " + tokB},
		{"basic scheme", good, "Basic " + tokA},
		{"bearer empty", good, "Bearer "},
		{"token in query only", good + "&token=" + tokA, ""},
		{"wrong token, bad params", streamURL(base, "", "9"), "Bearer nope"},
	} {
		if got := send(t, http.MethodGet, c.url, c.auth); got != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", c.name, got)
		}
	}
	for _, path := range []string{"/hub/reply/abc?instance=i1", "/hub/beat?instance=i1"} {
		if got := send(t, http.MethodPost, base+path, "Bearer "+tokB); got != http.StatusUnauthorized {
			t.Errorf("POST %s with a wrong token: status %d, want 401", path, got)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, good, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 without WWW-Authenticate")
	}
}

func TestStreamBadParams(t *testing.T) {
	_, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	for _, c := range []struct{ name, url string }{
		{"no instance", streamURL(base, "", "1")},
		{"instance of 129 bytes", streamURL(base, strings.Repeat("x", 129), "1")},
		{"no protocol", streamURL(base, "i1", "")},
		{"protocol 2", streamURL(base, "i1", "2")},
	} {
		if got := send(t, http.MethodGet, c.url, "Bearer "+tokA); got != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.name, got)
		}
	}
	if _, code := fleettest.DialHub(t, base, tokA, strings.Repeat("x", 128), fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Errorf("instance of 128 bytes: status %d, want 200", code)
	}
}

func TestStreamDuplicate409(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA, "b": tokB})
	if _, code := fleettest.DialHub(t, base, tokA, "dup", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Fatalf("first stream: %d", code)
	}
	if _, code := fleettest.DialHub(t, base, tokA, "dup", fleettest.HubInstanceOptions{}); code != http.StatusConflict {
		t.Errorf("second stream of the same ID: %d, want 409", code)
	}
	// IDs are per server: the same ID on another server is no duplicate.
	if _, code := fleettest.DialHub(t, base, tokB, "dup", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Errorf("same ID on another server: %d, want 200", code)
	}
	if got := r.Source(tokA).Endpoints(); len(got) != 1 {
		t.Errorf("server a endpoints = %v, want one", got)
	}
}

func TestStreamHelloFirst(t *testing.T) {
	_, base := newTestHub(t, 1500*time.Millisecond, 0, map[string]string{"a": tokA})
	inst, code := fleettest.DialHub(t, base, tokA, "i1", fleettest.HubInstanceOptions{})
	if code != http.StatusOK {
		t.Fatal(code)
	}
	if want := `{"type":"hello","protocol":1,"heartbeat_ms":1500}`; inst.FirstLine() != want {
		t.Errorf("first line = %s, want %s", inst.FirstLine(), want)
	}
	// The production constructor keeps heartbeat_ms in the wire range.
	for _, c := range []struct{ in, want time.Duration }{
		{0, DefaultHeartbeat}, {time.Millisecond, MinHeartbeat}, {time.Hour, MaxHeartbeat}, {7 * time.Second, 7 * time.Second},
	} {
		if got := NewRouter(Options{Heartbeat: c.in}).heartbeat; got != c.want {
			t.Errorf("NewRouter(Heartbeat %v).heartbeat = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPingCadence(t *testing.T) {
	const hb = 40 * time.Millisecond
	_, base := newTestHub(t, hb, 0, map[string]string{"a": tokA})
	inst, _ := fleettest.DialHub(t, base, tokA, "i1", fleettest.HubInstanceOptions{})
	fleettest.WaitUntil(t, 5*time.Second, "6 pings", func() bool { return len(inst.Pings()) >= 6 })
	p := inst.Pings()
	mean := p[len(p)-1].Sub(p[0]) / time.Duration(len(p)-1)
	if mean < hb/2 {
		t.Errorf("mean gap between pings %v, want about %v", mean, hb)
	}
}

func TestRoundTrip(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	rt := r.Transport(tokA)
	workers := map[string]*fleettest.Worker{"a b/c": {Status: "Running", Concurrency: 2}}
	for _, id := range []string{"plain", "with/slash", "50%", "..", "spa ce?#", "\xff\xfe", strings.Repeat("z", 128)} {
		// The fake varmq says it is "liar": the hub names the instance by
		// its stream, so one endpoint is always one instance.
		fv, srv := fleettest.NewVarmq(t, "liar", workers)
		fleettest.DialHub(t, base, tokA, id, fleettest.HubInstanceOptions{Handler: srv.Config.Handler})
		c := api.NewClientVia(Endpoint(id), "", rt)
		list, instance, err := c.ListWorkersFresh(context.Background())
		if err != nil || len(list) != 1 || instance != id {
			t.Errorf("%q: ListWorkersFresh = %v, %q, %v; want one worker from %q", id, list, instance, err, id)
			continue
		}
		if _, err := c.GetWorkerCtx(context.Background(), url.PathEscape("a b/c")); err != nil {
			t.Errorf("%q: GetWorker: %v", id, err)
		}
		if err := c.ActionCtx(context.Background(), url.PathEscape("a b/c"), "pause"); err != nil {
			t.Errorf("%q: Action: %v", id, err)
		}
		want := []string{"GET /workers", "GET /workers/a%20b%2Fc", "PATCH /workers/a%20b%2Fc/actions/pause"}
		if got := fv.Calls(); !slices.Equal(got, want) {
			t.Errorf("%q: varmq received %v, want %v", id, got, want)
		}
	}

	// Status, Content-Type and body pass; other headers do not.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /odd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Location", "http://evil.example/")
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set(api.InstanceHeader, "liar")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "moved?"+r.URL.RawQuery)
	})
	fleettest.DialHub(t, base, tokA, "odd", fleettest.HubInstanceOptions{Handler: mux})
	resp, err := (&http.Client{Transport: rt}).Get(Endpoint("odd") + "/odd?x=1&y=%20")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || string(body) != "moved?x=1&y=%20" {
		t.Errorf("reply = %d %q, want 302 %q (no redirect followed)", resp.StatusCode, body, "moved?x=1&y=%20")
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Get(api.InstanceHeader); got != "odd" {
		t.Errorf("%s = %q, want the stream's ID", api.InstanceHeader, got)
	}
	for _, h := range []string{"Location", "Set-Cookie"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("reply passed %s: %q", h, v)
		}
	}

	// An error status comes back as an *api.APIError.
	_, srv := fleettest.NewVarmq(t, "e", nil)
	fleettest.DialHub(t, base, tokA, "err", fleettest.HubInstanceOptions{Handler: srv.Config.Handler})
	_, err = api.NewClientVia(Endpoint("err"), "", rt).GetWorkerCtx(context.Background(), "missing")
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("GetWorker of a missing worker = %v, want an APIError 404", err)
	}
}

// held starts a RoundTrip of GET /workers to inst in the background and
// returns its frame and result.
func held(t *testing.T, ctx context.Context, rt http.RoundTripper, inst *fleettest.HubInstance) (fleettest.HubFrame, <-chan error) {
	t.Helper()
	res := make(chan error, 1)
	go func() {
		resp, err := get(ctx, rt, inst.ID, "/workers")
		if err == nil {
			resp.Body.Close()
		}
		res <- err
	}()
	select {
	case f := <-inst.Requests():
		return f, res
	case <-time.After(5 * time.Second):
		t.Fatal("no request frame within 5s")
	}
	return fleettest.HubFrame{}, nil
}

func wait(t *testing.T, res <-chan error) error {
	t.Helper()
	select {
	case err := <-res:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("RoundTrip still waiting after 5s")
	}
	return nil
}

func TestReplyUnknownID404(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	b, _ := fleettest.DialHub(t, base, tokA, "b", fleettest.HubInstanceOptions{})
	f, res := held(t, context.Background(), r.Transport(tokA), a)
	if f.Method != http.MethodGet || f.Path != "/workers" || !validRequestID(f.ID) || len(f.ID) < 22 {
		t.Errorf("frame = %+v", f)
	}
	ok := http.Header{"Content-Type": {"application/json"}}
	for _, c := range []struct {
		name     string
		inst, id string
	}{
		{"another instance's request", b.ID, f.ID},
		{"unknown id", a.ID, "nope"},
		{"invalid id", a.ID, "a.b"},
		{"instance not connected", "ghost", f.ID},
	} {
		if code, err := b.ReplyAs(c.inst, c.id, 200, ok, strings.NewReader("[]"), 2); err != nil || code != http.StatusNotFound {
			t.Errorf("%s: %d, %v; want 404", c.name, code, err)
		}
	}
	if code, err := a.Reply(f.ID, 200, ok, []byte("[]")); err != nil || code != http.StatusNoContent {
		t.Fatalf("reply: %d, %v; want 204", code, err)
	}
	if err := wait(t, res); err != nil {
		t.Errorf("RoundTrip: %v", err)
	}
	if code, _ := a.Reply(f.ID, 200, ok, []byte("[]")); code != http.StatusNotFound {
		t.Errorf("second reply: %d, want 404", code)
	}
}

func TestReplyBadStatus(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	f, res := held(t, context.Background(), r.Transport(tokA), a)
	if code, _ := a.Reply(f.ID, 99, nil, nil); code != http.StatusBadRequest {
		t.Errorf("X-Hub-Status 99: %d, want 400", code)
	}
	if err := wait(t, res); err == nil || !strings.Contains(err.Error(), "X-Hub-Status") {
		t.Errorf("RoundTrip = %v, want an X-Hub-Status error", err)
	}
}

// zeros reads n zero bytes.
type zeros struct{ n int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	k := min(int64(len(p)), z.n)
	clear(p[:k])
	z.n -= k
	return int(k), nil
}

func TestReplyOversize413(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	for _, c := range []struct {
		name string
		n    int64 // declared length; -1 chunked
	}{{"declared", MaxReply + 1}, {"chunked", -1}} {
		f, res := held(t, context.Background(), r.Transport(tokA), a)
		// 100-continue: a declared size is refused before the body is sent.
		code, err := a.ReplyAs(a.ID, f.ID, 200, http.Header{"Expect": {"100-continue"}}, &zeros{MaxReply + 1}, c.n)
		if err != nil || code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: %d, %v; want 413", c.name, code, err)
		}
		if err := wait(t, res); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("%s: RoundTrip = %v, want a too-large error", c.name, err)
		}
	}
	// Exactly MaxReply is delivered.
	f, res := held(t, context.Background(), r.Transport(tokA), a)
	if code, err := a.ReplyAs(a.ID, f.ID, 200, nil, &zeros{MaxReply}, MaxReply); err != nil || code != http.StatusNoContent {
		t.Errorf("reply of MaxReply bytes: %d, %v; want 204", code, err)
	}
	if err := wait(t, res); err != nil {
		t.Errorf("RoundTrip of MaxReply bytes: %v", err)
	}
}

func TestCallerContextCancel(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	f, res := held(t, ctx, r.Transport(tokA), a)
	cancel()
	if err := wait(t, res); !errors.Is(err, context.Canceled) {
		t.Errorf("RoundTrip = %v, want context.Canceled", err)
	}
	if code, _ := a.Reply(f.ID, 200, nil, nil); code != http.StatusNotFound {
		t.Errorf("reply after the caller left: %d, want 404", code)
	}
}

func TestDisconnectFailsWaiters(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	_, res := held(t, context.Background(), r.Transport(tokA), a)
	a.Close()
	if err := wait(t, res); !errors.Is(err, ErrDisconnected) {
		t.Errorf("RoundTrip = %v, want ErrDisconnected", err)
	}
	fleettest.WaitUntil(t, 5*time.Second, "instance gone from discovery", func() bool {
		return len(r.Source(tokA).Endpoints()) == 0
	})
	if _, err := get(context.Background(), r.Transport(tokA), "a", "/workers"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("RoundTrip after disconnect = %v, want ErrNotConnected", err)
	}
}

func TestMissedBeatsDrop(t *testing.T) {
	r, base := newTestHub(t, 30*time.Millisecond, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "silent", fleettest.HubInstanceOptions{NoBeat: true})
	select {
	case <-a.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("instance that never beats still connected after 5s")
	}
	if got := r.Source(tokA).Endpoints(); len(got) != 0 {
		t.Errorf("endpoints = %v, want none", got)
	}
}

func TestBeatingInstanceStays(t *testing.T) {
	r, base := newTestHub(t, 200*time.Millisecond, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "alive", fleettest.HubInstanceOptions{})
	select {
	case <-a.Done():
		t.Fatal("beating instance dropped")
	case <-time.After(1500 * time.Millisecond):
	}
	if got := r.Source(tokA).Endpoints(); len(got) != 1 {
		t.Errorf("endpoints = %v, want the instance", got)
	}
}

func TestByeRemovesAtOnce(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	if code, err := a.Beat(false); err != nil || code != http.StatusNoContent {
		t.Fatalf("beat: %d, %v", code, err)
	}
	if code, err := a.Beat(true); err != nil || code != http.StatusNoContent {
		t.Fatalf("bye: %d, %v", code, err)
	}
	if got := r.Source(tokA).Endpoints(); len(got) != 0 {
		t.Errorf("endpoints right after bye = %v, want none", got)
	}
	select {
	case <-a.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open 5s after bye")
	}
	if code, _ := a.Beat(false); code != http.StatusNotFound {
		t.Errorf("beat after bye: %d, want 404", code)
	}
}

func TestTokenRouting(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA, "b": tokB})
	_, srvA := fleettest.NewVarmq(t, "x", map[string]*fleettest.Worker{"on-a": {Status: "Running"}})
	_, srvB := fleettest.NewVarmq(t, "x", map[string]*fleettest.Worker{"on-b": {Status: "Running"}})
	fleettest.DialHub(t, base, tokA, "same", fleettest.HubInstanceOptions{Handler: srvA.Config.Handler})
	fleettest.DialHub(t, base, tokB, "same", fleettest.HubInstanceOptions{Handler: srvB.Config.Handler})
	for tok, want := range map[string]string{tokA: "on-a", tokB: "on-b"} {
		list, err := api.NewClientVia(Endpoint("same"), "", r.Transport(tok)).ListWorkersCtx(context.Background())
		if err != nil || len(list) != 1 || list[0].Name != want {
			t.Errorf("list through %s's hub = %v, %v; want %s", tok[:7], list, err, want)
		}
	}
	if got := r.Servers(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Servers = %v", got)
	}

	// A held request on a cannot be answered through b, even naming the
	// same instance ID.
	r2, base2 := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA, "b": tokB})
	ha, _ := fleettest.DialHub(t, base2, tokA, "same", fleettest.HubInstanceOptions{})
	hb, _ := fleettest.DialHub(t, base2, tokB, "same", fleettest.HubInstanceOptions{})
	f, res := held(t, context.Background(), r2.Transport(tokA), ha)
	if code, _ := hb.Reply(f.ID, 200, nil, []byte("[]")); code != http.StatusNotFound {
		t.Errorf("reply through server b: %d, want 404", code)
	}
	if code, _ := ha.Reply(f.ID, 200, nil, []byte("[]")); code != http.StatusNoContent {
		t.Errorf("reply through server a: %d, want 204", code)
	}
	if err := wait(t, res); err != nil {
		t.Error(err)
	}
}

func TestInFlightCap(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	rt := r.Transport(tokA)
	var wg sync.WaitGroup
	errs := make(chan error, MaxInFlight+1)
	for range MaxInFlight + 1 {
		wg.Go(func() {
			resp, err := get(context.Background(), rt, "a", "/workers")
			if err == nil {
				resp.Body.Close()
			}
			errs <- err
		})
	}
	var frames []fleettest.HubFrame
	for len(frames) < MaxInFlight {
		select {
		case f := <-a.Requests():
			frames = append(frames, f)
		case <-time.After(5 * time.Second):
			t.Fatalf("%d frames within 5s, want %d", len(frames), MaxInFlight)
		}
	}
	select {
	case f := <-a.Requests():
		t.Fatalf("frame %d sent while %d are in flight: %+v", MaxInFlight+1, MaxInFlight, f)
	case <-time.After(200 * time.Millisecond):
	}
	if code, _ := a.Reply(frames[0].ID, 200, nil, []byte("[]")); code != http.StatusNoContent {
		t.Fatal(code)
	}
	select {
	case f := <-a.Requests():
		frames = append(frames, f)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting caller got no slot after a reply")
	}
	for _, f := range frames[1:] {
		if code, _ := a.Reply(f.ID, 200, nil, []byte("[]")); code != http.StatusNoContent {
			t.Errorf("reply: %d", code)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestNotConnected(t *testing.T) {
	r, _ := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	start := time.Now()
	if _, err := get(context.Background(), r.Transport(tokA), "ghost", "/workers"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("unknown instance: %v, want ErrNotConnected", err)
	}
	if _, err := get(context.Background(), r.Transport("not-a-served-token"), "ghost", "/workers"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("unserved token: %v, want ErrNotConnected", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("failing took %v: a disconnected instance must fail at once", d)
	}
	var nilRouter *Router
	if _, err := get(context.Background(), nilRouter.Transport(tokA), "x", "/workers"); !errors.Is(err, ErrHubOff) {
		t.Errorf("nil router: %v, want ErrHubOff", err)
	}
	hubSrv := &config.Server{Name: "h", Hub: true, Token: tokA}
	if _, err := nilRouter.BuildPool(hubSrv); !errors.Is(err, ErrHubOff) {
		t.Errorf("nil router BuildPool: %v, want ErrHubOff", err)
	}
	if _, err := api.PoolFor(hubSrv); !errors.Is(err, api.ErrHubServer) {
		t.Errorf("api.PoolFor(hub) = %v, want ErrHubServer", err)
	}
	if p, err := nilRouter.BuildPool(&config.Server{Name: "s", Endpoints: []string{"http://x"}}); err != nil || len(p.Clients()) != 1 {
		t.Errorf("BuildPool of a static server: %v", err)
	}
	// A hub endpoint never reaches the network through a default client.
	if err := api.NewClient(Endpoint("x"), tokA).GetHealth(); err == nil || !strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Errorf("default client on a hub endpoint: %v, want unsupported protocol scheme", err)
	}
	for _, u := range []string{"http://hub.invalid/x/workers", "hub://other/x/workers", "hub://hub.invalid/", "hub://hub.invalid/%zz/w"} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if req == nil {
			continue
		}
		if _, err := r.Transport(tokA).RoundTrip(req); err == nil || errors.Is(err, ErrNotConnected) {
			t.Errorf("%s: %v, want a not-a-hub-endpoint error", u, err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, Endpoint("x")+"/workers", nil)
	if _, err := r.Transport(tokA).RoundTrip(req); err == nil || !strings.Contains(err.Error(), "method") {
		t.Errorf("POST: %v, want a method error", err)
	}
}

func TestSyncFollowsConfig(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA, "b": tokB})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	b, _ := fleettest.DialHub(t, base, tokB, "b", fleettest.HubInstanceOptions{})

	// a's token changes, b is removed: both lose their instances.
	const tokA2 = "token-a2-0123456789abcdef"
	r.Sync(hubConfig(map[string]string{"a": tokA2}))
	for _, inst := range []*fleettest.HubInstance{a, b} {
		select {
		case <-inst.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s still connected 5s after its server changed", inst)
		}
	}
	if _, code := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{}); code != http.StatusUnauthorized {
		t.Errorf("old token: %d, want 401", code)
	}
	if _, code := fleettest.DialHub(t, base, tokA2, "a", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Errorf("new token: %d, want 200", code)
	}
	if got := r.Servers(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("Servers = %v, want [a]", got)
	}

	// A server whose token is unchanged keeps its instances; Close drops them.
	r.Sync(hubConfig(map[string]string{"a": tokA2, "c": tokB}))
	if got := r.Source(tokA2).Endpoints(); len(got) != 1 {
		t.Errorf("endpoints of a after an unrelated change = %v", got)
	}
	r.Close()
	if _, code := fleettest.DialHub(t, base, tokA2, "late", fleettest.HubInstanceOptions{}); code != http.StatusUnauthorized {
		t.Errorf("after Close: %d, want 401", code)
	}
}

func TestMaxInstances(t *testing.T) {
	_, base := newTestHub(t, slowBeat, 2, map[string]string{"a": tokA})
	for _, id := range []string{"1", "2"} {
		if _, code := fleettest.DialHub(t, base, tokA, id, fleettest.HubInstanceOptions{}); code != http.StatusOK {
			t.Fatalf("instance %s: %d", id, code)
		}
	}
	if _, code := fleettest.DialHub(t, base, tokA, "3", fleettest.HubInstanceOptions{}); code != http.StatusServiceUnavailable {
		t.Errorf("instance past the cap: %d, want 503", code)
	}
}

func TestEndpointSplit(t *testing.T) {
	for _, id := range []string{"a", "a/b", "%", "..", "?#", "\xff", strings.Repeat("q", 128)} {
		u, err := url.Parse(Endpoint(id) + "/workers/x%2Fy?state=pending")
		if err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		got, path, ok := split(u)
		if !ok || got != id || path != "/workers/x%2Fy?state=pending" {
			t.Errorf("split(Endpoint(%q)) = %q, %q, %v", id, got, path, ok)
		}
	}
}

// TestServeTLS: Serve with a certificate speaks TLS and HTTP/2 to an
// instance, carries a round trip, and on shutdown drops the stream and
// returns.
func TestServeTLS(t *testing.T) {
	tmp := httptest.NewUnstartedServer(http.NotFoundHandler())
	tmp.EnableHTTP2 = true
	tmp.StartTLS()
	cert, client := tmp.TLS.Certificates[0], tmp.Client() // trusts cert, attempts HTTP/2
	tmp.Close()

	r := newRouter(slowBeat, 0, nil)
	r.Sync(hubConfig(map[string]string{"a": tokA}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, r, &cert, 5*time.Second, nil) }()
	base := "https://" + ln.Addr().String()

	req, _ := http.NewRequest(http.MethodGet, streamURL(base, "probe", "1"), nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
		t.Errorf("stream over TLS = %s %d, want HTTP/2 200", resp.Proto, resp.StatusCode)
	}
	resp.Body.Close()

	_, v := fleettest.NewVarmq(t, "x", map[string]*fleettest.Worker{"w": {Status: "Running"}})
	inst, code := fleettest.DialHub(t, base, tokA, "tls", fleettest.HubInstanceOptions{Handler: v.Config.Handler, Client: client})
	if code != http.StatusOK {
		t.Fatalf("dial over TLS: %d", code)
	}
	if list, err := api.NewClientVia(Endpoint("tls"), "", r.Transport(tokA)).ListWorkersCtx(context.Background()); err != nil || len(list) != 1 {
		t.Errorf("round trip over TLS: %v, %v", list, err)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve = %v, want nil after shutdown", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Serve still running 6s after its context ended")
	}
	select {
	case <-inst.Done():
	case <-time.After(5 * time.Second):
		t.Error("stream still open after shutdown")
	}
}
