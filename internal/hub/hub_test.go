package hub

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	return newTestHubWith(t, hb, maxInstances, servers, nil)
}

// newTestHubWith is newTestHub with its hub-wide limits set by tune first.
func newTestHubWith(t *testing.T, hb time.Duration, maxInstances int, servers map[string]string, tune func(*limits)) (*Router, string) {
	t.Helper()
	r := newRouter(hb, maxInstances, nil)
	if tune != nil {
		tune(r.lim)
	}
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
		{"instance not UTF-8", streamURL(base, "\xff\xfe", "1")},
		{"instance with a newline", streamURL(base, "a\nb", "1")},
		{"instance with NUL", streamURL(base, "a\x00", "1")},
		{"instance with DEL", streamURL(base, "a\x7f", "1")},
		{"instance with ESC", streamURL(base, "x\x1b[31m", "1")},
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
	s := inst.Session()
	if want := `{"type":"hello","protocol":1,"heartbeat_ms":1500,"session":"` + s + `"}`; inst.FirstLine() != want {
		t.Errorf("first line = %s, want %s", inst.FirstLine(), want)
	}
	if len(s) < 22 || len(s) > 64 || strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		t.Errorf("session %q: want 22 to 64 bytes of [A-Za-z0-9_-]", s)
	}
	other, _ := fleettest.DialHub(t, base, tokA, "i2", fleettest.HubInstanceOptions{})
	if other.Session() == s {
		t.Error("two streams got the same session")
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
	for _, id := range []string{"plain", "with/slash", "50%", "..", "spa ce?#", "ünï cødé", strings.Repeat("z", 128)} {
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
	if f.Method != http.MethodGet || f.Path != "/workers" || len(f.ID) < 22 {
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
	// Errors name the instance, never the internal hub URL.
	_, _, err := api.NewClientVia(Endpoint("ghost"), "", r.Transport(tokA)).ListWorkersFresh(context.Background())
	if err == nil || !errors.Is(err, ErrNotConnected) || !strings.Contains(err.Error(), `"ghost"`) || strings.Contains(err.Error(), "hub.invalid") || strings.Contains(err.Error(), "hub://") {
		t.Errorf("unit error = %v, want ErrNotConnected naming the instance and no hub URL", err)
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
	if got := served(r); !slices.Equal(got, []string{"a"}) {
		t.Errorf("served = %v, want [a]", got)
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
	for _, id := range []string{"a", "a/b", "%", "..", "?#", "é", strings.Repeat("q", 128)} {
		u, err := url.Parse(Endpoint(id) + "/workers/x%2Fy?state=pending")
		if err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		got, path, ok := split(u)
		if !ok || got != id || path != "/workers/x%2Fy?state=pending" {
			t.Errorf("split(Endpoint(%q)) = %q, %q, %v", id, got, path, ok)
		}
		// The pages show the instance, not the internal URL.
		if got := config.ShortOrigin(Endpoint(id)); got != id {
			t.Errorf("config.ShortOrigin(Endpoint(%q)) = %q, want the ID", id, got)
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

	const timeout = 300 * time.Millisecond
	r := newRouter(slowBeat, 0, nil)
	r.lim.reqTimeout = timeout // an HTTP/2 stream must outlive it too
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
	req, _ = http.NewRequest(http.MethodGet, streamURL(base, "body", "1"), io.MultiReader(strings.NewReader("x")))
	req.Header.Set("Authorization", "Bearer "+tokA)
	if resp, err = client.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("stream with a body over TLS = %s %d, want HTTP/2 400", resp.Proto, resp.StatusCode)
	}

	_, v := fleettest.NewVarmq(t, "x", map[string]*fleettest.Worker{"w": {Status: "Running"}})
	inst, code := fleettest.DialHub(t, base, tokA, "tls", fleettest.HubInstanceOptions{Handler: v.Config.Handler, Client: client})
	if code != http.StatusOK {
		t.Fatalf("dial over TLS: %d", code)
	}
	select {
	case <-inst.Done():
		t.Fatal("the request timeout cut an HTTP/2 stream")
	case <-time.After(4 * timeout):
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

// served is the names of the hub servers r serves, sorted.
func served(r *Router) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, h := range r.hubs {
		names = append(names, h.label())
	}
	slices.Sort(names)
	return names
}

// post sends POST base+path with tokA and session ("" none), and returns
// the status.
func post(t *testing.T, base, path, session string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokA)
	if session != "" {
		req.Header.Set(SessionHeader, session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestSessionBinding: beats, byes and replies belong to the stream's
// session. A holder of the token cannot evict a connected instance, beat
// for it, or answer its requests.
func TestSessionBinding(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	victim, _ := fleettest.DialHub(t, base, tokA, "victim", fleettest.HubInstanceOptions{})
	for _, session := range []string{"", "wrong-session-0123456789", victim.Session() + "x"} {
		for _, path := range []string{"/hub/beat?instance=victim&bye=1", "/hub/beat?instance=victim"} {
			if code := post(t, base, path, session); code != http.StatusNotFound {
				t.Errorf("POST %s with session %q: %d, want 404", path, session, code)
			}
		}
	}
	select {
	case <-victim.Done():
		t.Fatal("a bye without the session closed the victim's stream")
	case <-time.After(100 * time.Millisecond):
	}
	// A request to the victim cannot be answered without its session.
	f, res := held(t, context.Background(), r.Transport(tokA), victim)
	if code := post(t, base, "/hub/reply/"+f.ID+"?instance=victim", ""); code != http.StatusNotFound {
		t.Errorf("reply without the session: %d, want 404", code)
	}
	if code, _ := victim.Reply(f.ID, 200, nil, []byte("[]")); code != http.StatusNoContent {
		t.Errorf("reply with the session: %d, want 204", code)
	}
	if err := wait(t, res); err != nil {
		t.Error(err)
	}
	// The session of an old stream is dead after a redial.
	again, code := victim.Redial(t)
	if code != http.StatusOK {
		t.Fatalf("redial: %d", code)
	}
	if code := post(t, base, "/hub/beat?instance=victim&bye=1", victim.Session()); code != http.StatusNotFound {
		t.Errorf("bye with the old stream's session: %d, want 404", code)
	}
	if got := r.Source(tokA).Endpoints(); len(got) != 1 {
		t.Errorf("endpoints = %v, want the redialed victim", got)
	}
	_ = again
}

// TestListedAfterFirstBeat: an instance is listed, for discovery and for
// requests, from its session's first beat; its stream's opening starts
// the liveness clock only.
func TestListedAfterFirstBeat(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "quiet", fleettest.HubInstanceOptions{NoBeat: true})
	if got := r.Source(tokA).Endpoints(); len(got) != 0 {
		t.Errorf("endpoints before the first beat = %v, want none", got)
	}
	if _, err := get(context.Background(), r.Transport(tokA), "quiet", "/workers"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("RoundTrip before the first beat = %v, want ErrNotConnected", err)
	}
	if _, code := fleettest.DialHub(t, base, tokA, "quiet", fleettest.HubInstanceOptions{}); code != http.StatusConflict {
		t.Errorf("same ID before the first beat: %d, want 409", code)
	}
	if code, _ := a.Beat(false); code != http.StatusNoContent {
		t.Fatalf("first beat: %d", code)
	}
	if got := r.Source(tokA).Endpoints(); !slices.Equal(got, []string{Endpoint("quiet")}) {
		t.Errorf("endpoints after the first beat = %v", got)
	}
}

// TestByeBeforeRedialFreesID: an instance that says bye before it redials
// gets its ID back at once, with no 409 window.
func TestByeBeforeRedialFreesID(t *testing.T) {
	_, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "re", fleettest.HubInstanceOptions{})
	for i := range 5 {
		next, code := a.Redial(t)
		if code != http.StatusOK {
			t.Fatalf("redial %d after bye: %d, want 200", i, code)
		}
		a = next
	}
}

// silent opens id's stream over raw TCP, reads the hello, then says
// nothing more and never closes: a half-open peer.
func silent(t *testing.T, base, id string) net.Conn {
	t.Helper()
	u, _ := url.Parse(base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /hub/stream?instance=%s&protocol=1 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", id, tokA)
	br := bufio.NewReader(conn)
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(l, `"hello"`) {
			return conn
		}
	}
}

// TestReview409Bound: the old stream of an ID is held by a peer gone
// without a FIN, and the instance redials without a bye. The hub never
// replaces the live stream (409), but frees the ID within 3 to 4
// heartbeats of the stream's last beat.
func TestReview409Bound(t *testing.T) {
	const hb = 200 * time.Millisecond
	_, base := newTestHub(t, hb, 0, map[string]string{"a": tokA})
	silent(t, base, "dup")
	start := time.Now()
	for {
		_, code := fleettest.DialHub(t, base, tokA, "dup", fleettest.HubInstanceOptions{})
		if code == http.StatusOK {
			break
		}
		if code != http.StatusConflict {
			t.Fatalf("redial: %d", code)
		}
		if time.Since(start) > 20*hb {
			t.Fatal("still 409 after 20 heartbeats")
		}
		time.Sleep(hb / 10)
	}
	if d := time.Since(start); d > 5*hb {
		t.Errorf("ID freed after %v, want within 4 heartbeats", d)
	}
}

// TestPreAuthReadDeadline: a client that sends headers and then stalls
// its body, with or without a token, loses its connection at the request
// timeout. An authenticated stream outlives it.
func TestPreAuthReadDeadline(t *testing.T) {
	const timeout = 300 * time.Millisecond
	_, base := newTestHubWith(t, 200*time.Millisecond, 0, map[string]string{"a": tokA}, func(l *limits) { l.reqTimeout = timeout })
	addr := strings.TrimPrefix(base, "http://")
	for _, c := range []struct{ name, req string }{
		{"no token, beat", "POST /hub/beat?instance=x HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n"},
		{"no token, reply", "POST /hub/reply/abc?instance=x HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n"},
		{"no token, unknown path", "POST /nope HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n"},
		{"no token, chunked", "POST /hub/beat HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n"},
		{"token, beat", "POST /hub/beat?instance=x HTTP/1.1\r\nHost: h\r\nAuthorization: Bearer " + tokA + "\r\nContent-Length: 100\r\n\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			start := time.Now()
			if _, err := io.WriteString(conn, c.req); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err = io.Copy(io.Discard, conn)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("connection still held after 10s")
			}
			if d := time.Since(start); d > timeout+2*time.Second {
				t.Errorf("connection held %v, want about %v", d, timeout)
			}
		})
	}
	// Without a token the 401 comes at once, not after the unread body.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	io.WriteString(conn, "POST /hub/beat HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	status, _ := bufio.NewReader(conn).ReadString('\n')
	if d := time.Since(start); !strings.HasPrefix(status, "HTTP/1.1 401") || d > timeout/2 {
		t.Errorf("tokenless stalled POST: %q after %v, want a 401 at once", status, d)
	}
	t.Run("stream", func(t *testing.T) {
		in, code := fleettest.DialHub(t, base, tokA, "long", fleettest.HubInstanceOptions{})
		if code != http.StatusOK {
			t.Fatal(code)
		}
		select {
		case <-in.Done():
			t.Fatal("the request timeout cut an authenticated stream")
		case <-time.After(6 * timeout):
		}
	})
}

// TestCapListener: the hub listener holds at most its cap of connections;
// one past it is closed at once, and a closed one frees its slot.
func TestCapListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var max atomic.Int64
	max.Store(2)
	ln := &capListener{Listener: inner, max: &max}
	defer ln.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	dial()
	dial()
	var held []net.Conn
	for range 2 {
		select {
		case c := <-accepted:
			held = append(held, c)
		case <-time.After(5 * time.Second):
			t.Fatal("not accepted")
		}
	}
	over := dial()
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("connection past the cap: read %v, want it closed at once", err)
	}
	held[0].Close()
	held[0].Close() // a second Close frees nothing more
	dial()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("a freed slot was not reused")
	}
	if n := ln.open(); n != 2 {
		t.Errorf("open count = %d, want 2", n)
	}
	// Serve caps its listener: an instance at the instance limit still
	// beats over HTTP/1.1 (stream plus a beat connection each).
	r := newRouter(slowBeat, 2, nil)
	r.Sync(hubConfig(map[string]string{"a": tokA}))
	hln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, hln, r, nil, time.Second, nil)
	hbase := "http://" + hln.Addr().String()
	for _, id := range []string{"1", "2"} {
		in, code := fleettest.DialHub(t, hbase, tokA, id, fleettest.HubInstanceOptions{})
		if code != http.StatusOK {
			t.Fatalf("instance %s: %d", id, code)
		}
		if code, err := in.Beat(false); err != nil || code != http.StatusNoContent {
			t.Errorf("beat of instance %s at the limit: %d, %v", id, code, err)
		}
	}
}

// gate reads n bytes, then blocks until release (or never, when nil).
type gate struct {
	n       int
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (g *gate) Read(p []byte) (int, error) {
	if g.n > 0 {
		k := min(len(p), g.n)
		clear(p[:k])
		g.n -= k
		return k, nil
	}
	g.once.Do(func() { close(g.started) })
	<-g.release
	return 0, io.EOF
}

// TestStalledReplyAborted: a reply whose body stalls keeps its caller's
// slot while it is read, is aborted once the caller leaves, and is cut at
// the request timeout when the caller waits on.
func TestStalledReplyAborted(t *testing.T) {
	r, base := newTestHubWith(t, slowBeat, 0, map[string]string{"a": tokA}, func(l *limits) { l.reqTimeout = 30 * time.Second })
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	c := r.hub(keyOf(tokA)).get("a")
	ctx, cancel := context.WithCancel(context.Background())
	f, res := held(t, ctx, r.Transport(tokA), a)
	g := &gate{n: 1000, release: make(chan struct{}), started: make(chan struct{})}
	defer close(g.release)
	posted := make(chan int, 1)
	go func() {
		code, _ := a.ReplyAs(a.ID, f.ID, 200, nil, g, 2000)
		posted <- code
	}()
	<-g.started
	time.Sleep(50 * time.Millisecond)
	if n := len(c.slots); n != 1 {
		t.Errorf("slots taken while the reply is read = %d, want 1", n)
	}
	cancel()
	if err := wait(t, res); !errors.Is(err, context.Canceled) {
		t.Errorf("RoundTrip = %v, want context.Canceled", err)
	}
	select {
	case code := <-posted:
		if code != http.StatusNotFound && code != 0 {
			t.Errorf("aborted reply: %d, want 404 or a closed connection", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reply body still read 5s after its caller left")
	}
	fleettest.WaitUntil(t, 5*time.Second, "the slot freed", func() bool { return len(c.slots) == 0 })
	if used := r.hub(keyOf(tokA)).replyUsed.Load(); used != 0 {
		t.Errorf("reply budget in use after the handler returned = %d, want 0", used)
	}
}

// TestReplyReadTimeout: a reply body that stalls while its caller waits is
// cut at the request timeout; the caller gets an error.
func TestReplyReadTimeout(t *testing.T) {
	r, base := newTestHubWith(t, slowBeat, 0, map[string]string{"a": tokA}, func(l *limits) { l.reqTimeout = 300 * time.Millisecond })
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	f, res := held(t, context.Background(), r.Transport(tokA), a)
	g := &gate{n: 10, release: make(chan struct{}), started: make(chan struct{})}
	defer close(g.release)
	go a.ReplyAs(a.ID, f.ID, 200, nil, g, 100)
	if err := wait(t, res); err == nil || !strings.Contains(err.Error(), "read reply") {
		t.Errorf("RoundTrip = %v, want a read error", err)
	}
}

// TestReplyBudget503: reply bytes a hub server buffers at once are
// bounded; a reply past the budget gets 503 and its caller an error, and
// the budget comes back when a reply is done.
func TestReplyBudget503(t *testing.T) {
	r, base := newTestHubWith(t, slowBeat, 0, map[string]string{"a": tokA}, func(l *limits) { l.replyMax = 1000 })
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	rt := r.Transport(tokA)

	f, res := held(t, context.Background(), rt, a)
	if code, _ := a.Reply(f.ID, 200, nil, make([]byte, 1001)); code != http.StatusServiceUnavailable {
		t.Errorf("reply past the budget: %d, want 503", code)
	}
	if err := wait(t, res); err == nil || !strings.Contains(err.Error(), "reply memory") {
		t.Errorf("RoundTrip = %v, want a reply memory error", err)
	}

	// One stalled reply holds 800; a second of 300 does not fit.
	f1, res1 := held(t, context.Background(), rt, a)
	g := &gate{n: 10, release: make(chan struct{}), started: make(chan struct{})}
	go a.ReplyAs(a.ID, f1.ID, 200, nil, g, 800)
	<-g.started
	f2, res2 := held(t, context.Background(), rt, a)
	if code, _ := a.Reply(f2.ID, 200, nil, make([]byte, 300)); code != http.StatusServiceUnavailable {
		t.Errorf("reply while the budget is held: %d, want 503", code)
	}
	if err := wait(t, res2); err == nil {
		t.Error("RoundTrip past the budget succeeded")
	}
	close(g.release)
	_ = wait(t, res1) // short body: an error, the budget freed
	fleettest.WaitUntil(t, 5*time.Second, "the budget freed", func() bool { return r.hub(keyOf(tokA)).replyUsed.Load() == 0 })
	f3, res3 := held(t, context.Background(), rt, a)
	if code, _ := a.Reply(f3.ID, 200, nil, make([]byte, 300)); code != http.StatusNoContent {
		t.Errorf("reply after the budget came back: %d, want 204", code)
	}
	if err := wait(t, res3); err != nil {
		t.Error(err)
	}
}

// TestJoinRate503: new streams of a hub server are rate limited; a
// refused one gets 503, a 409 costs nothing, and the bucket refills.
func TestJoinRate503(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	_, base := newTestHubWith(t, slowBeat, 0, map[string]string{"a": tokA}, func(l *limits) {
		l.joinBurst, l.joinRate, l.now = 2, 1, clock
	})
	for _, id := range []string{"1", "2"} {
		if _, code := fleettest.DialHub(t, base, tokA, id, fleettest.HubInstanceOptions{}); code != http.StatusOK {
			t.Fatalf("instance %s within the burst: %d", id, code)
		}
	}
	if _, code := fleettest.DialHub(t, base, tokA, "1", fleettest.HubInstanceOptions{}); code != http.StatusConflict {
		t.Errorf("duplicate: %d, want 409", code)
	}
	if _, code := fleettest.DialHub(t, base, tokA, "3", fleettest.HubInstanceOptions{}); code != http.StatusServiceUnavailable {
		t.Errorf("instance past the burst: %d, want 503", code)
	}
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()
	if _, code := fleettest.DialHub(t, base, tokA, "3", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Errorf("instance a second later: %d, want 200", code)
	}
	if _, code := fleettest.DialHub(t, base, tokA, "4", fleettest.HubInstanceOptions{}); code != http.StatusServiceUnavailable {
		t.Errorf("second instance in that second: %d, want 503", code)
	}
}

// TestSyncRename: a hub server renamed with the same token keeps its Hub
// and its instances, under the new name.
func TestSyncRename(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	before := r.hub(keyOf(tokA))
	r.Sync(hubConfig(map[string]string{"renamed": tokA}))
	if r.hub(keyOf(tokA)) != before {
		t.Error("a rename made a new Hub")
	}
	if got := served(r); !slices.Equal(got, []string{"renamed"}) {
		t.Errorf("served = %v, want [renamed]", got)
	}
	select {
	case <-a.Done():
		t.Fatal("a rename dropped the instance")
	case <-time.After(100 * time.Millisecond):
	}
	if got := r.Source(tokA).Endpoints(); len(got) != 1 {
		t.Errorf("endpoints after a rename = %v", got)
	}
}

// fromListener makes each connection inner accepts come from the next
// address of from (a "host:port").
type fromListener struct {
	net.Listener
	from chan string
}

type fromConn struct {
	net.Conn
	addr net.Addr
}

func (c fromConn) RemoteAddr() net.Addr { return c.addr }

func (l fromListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	addr, err := net.ResolveTCPAddr("tcp", <-l.from)
	if err != nil {
		c.Close()
		return nil, err
	}
	return fromConn{c, addr}, nil
}

// TestCapPerIP: the hub listener holds at most perIP tokenless connections
// of one source IP: one more closes the IP's oldest. Other IPs are not
// touched, an authenticated one no longer counts, and a closed one frees
// its slot and its place of its Hub's share.
func TestCapPerIP(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	from := make(chan string, 1)
	var max atomic.Int64
	max.Store(100)
	ln := &capListener{Listener: fromListener{inner, from}, max: &max, perIP: 2, share: 1}
	defer ln.Close()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	// dial connects as ip and returns the accepted connection.
	dial := func(ip string) net.Conn {
		t.Helper()
		from <- ip + ":1"
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		select {
		case sc := <-accepted:
			return sc
		case <-time.After(5 * time.Second):
			t.Fatalf("connection of %s not accepted", ip)
			return nil
		}
	}
	// count is the open connections, the tokenless ones of each IP and
	// the places taken of the Hubs' shares.
	count := func() (int64, map[string]int, int) {
		ln.mu.Lock()
		defer ln.mu.Unlock()
		ips := make(map[string]int)
		for ip, q := range ln.byIP {
			ips[ip] = q.Len()
		}
		return ln.n, ips, len(ln.byHub)
	}
	a1 := dial("10.0.0.1")
	a2 := dial("10.0.0.1")
	a3 := dial("10.0.0.1") // past the limit: the oldest, a1, is closed
	if _, err := a1.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("the IP's oldest connection: read %v, want it closed", err)
	}
	b := dial("10.0.0.2")
	h1, h2 := new(Hub), new(Hub)
	if !ln.reserve(h1) {
		t.Fatal("no place of an empty share")
	}
	ln.settle(a2.(*capConn), h1, true) // authenticated: no longer counted for its IP
	if ln.reserve(h1) {
		t.Error("a place past the share of 1")
	}
	if !ln.reserve(h2) {
		t.Error("another Hub's full share took h2's place")
	}
	ln.settle(a3.(*capConn), h2, false) // not accepted: given back
	a4 := dial("10.0.0.1")
	if n, ips, hubs := count(); n != 4 || !maps.Equal(ips, map[string]int{"10.0.0.1": 2, "10.0.0.2": 1}) || hubs != 1 {
		t.Errorf("open = %d, tokenless per IP %v, %d Hubs; want 4, a3 and a4 of 10.0.0.1, b of 10.0.0.2, h1's", n, ips, hubs)
	}
	for _, c := range []net.Conn{a1, a2, a3, a4, b} {
		c.Close()
	}
	if n, ips, hubs := count(); n != 0 || len(ips) != 0 || hubs != 0 {
		t.Errorf("after closing all: open = %d, tokenless per IP %v, %d Hubs; want none", n, ips, hubs)
	}
	if r := NewRouter(Options{MaxConnsPerIP: 5}); r.perIP != 5 {
		t.Errorf("Options.MaxConnsPerIP 5: perIP = %d", r.perIP)
	}
}

// TestUnauthedConnClosed: a connection that carried no request accepted
// for a live session is closed at the request timeout, idle HTTP/2 and
// HTTP/1.1 keep-alive alike; one that carried an accepted stream lives on.
func TestUnauthedConnClosed(t *testing.T) {
	tmp := httptest.NewUnstartedServer(http.NotFoundHandler())
	tmp.EnableHTTP2 = true
	tmp.StartTLS()
	cert := tmp.TLS.Certificates[0]
	pool := tmp.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	tmp.Close()
	const timeout = 300 * time.Millisecond
	r := newRouter(slowBeat, 0, nil)
	r.lim.reqTimeout = timeout
	r.Sync(hubConfig(map[string]string{"a": tokA}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, r, &cert, time.Second, nil) }()
	defer func() { cancel(); <-served }()

	dial := func(proto, req string) *tls.Conn {
		t.Helper()
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, NextProtos: []string{proto}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := io.WriteString(c, req); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// closedWithin: c is closed by the server within d.
	closedWithin := func(c net.Conn, d time.Duration) bool {
		_ = c.SetReadDeadline(time.Now().Add(d))
		_, err := io.Copy(io.Discard, c)
		var ne net.Error
		return !errors.As(err, &ne) || !ne.Timeout()
	}
	h2 := dial("h2", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00")
	h1 := dial("http/1.1", "GET /nope HTTP/1.1\r\nHost: h\r\n\r\n")
	authed := dial("http/1.1", "GET /hub/stream?instance=x&protocol=1 HTTP/1.1\r\nHost: h\r\nAuthorization: Bearer "+tokA+"\r\n\r\n")
	if !closedWithin(h2, timeout+3*time.Second) {
		t.Error("an idle tokenless HTTP/2 connection outlived the request timeout")
	}
	if !closedWithin(h1, timeout+3*time.Second) {
		t.Error("a tokenless HTTP/1.1 keep-alive connection outlived the request timeout")
	}
	if closedWithin(authed, 4*timeout) {
		t.Error("a connection that carried an accepted stream was closed at the request timeout")
	}
}

// TestConnCapFollowsSync: the listener's cap is connsPerInstance per
// instance of every hub server served, plus the headroom, and follows Sync.
func TestConnCapFollowsSync(t *testing.T) {
	r := newRouter(slowBeat, 3, nil)
	for _, c := range []struct {
		servers map[string]string
		want    int64
	}{
		{nil, 10*3 + connHeadroom},
		{map[string]string{"a": tokA, "b": tokB}, 10*3*2 + connHeadroom},
		{map[string]string{"a": tokA}, 10*3 + connHeadroom},
	} {
		r.Sync(hubConfig(c.servers))
		if got := r.connCap.Load(); got != c.want {
			t.Errorf("cap with %d servers = %d, want %d", len(c.servers), got, c.want)
		}
	}
}

// TestReplyBudgetPerServer: chunked replies reserve the budget as their
// bytes arrive, and each hub server has its own budget: 16 stalled
// one-byte chunked replies of server a fill a's budget, so a's next reply
// gets 503 while b's is delivered.
func TestReplyBudgetPerServer(t *testing.T) {
	r, base := newTestHubWith(t, slowBeat, 0, map[string]string{"a": tokA, "b": tokB}, func(l *limits) { l.replyMax = 16 * replyStep })
	rtA := r.Transport(tokA)
	var gates []*gate
	defer func() {
		for _, g := range gates {
			close(g.release)
		}
	}()
	var a *fleettest.HubInstance
	for i := range 2 {
		var code int
		if a, code = fleettest.DialHub(t, base, tokA, fmt.Sprintf("a%d", i), fleettest.HubInstanceOptions{}); code != http.StatusOK {
			t.Fatal(code)
		}
		for range MaxInFlight {
			f, _ := held(t, context.Background(), rtA, a)
			g := &gate{n: 1, release: make(chan struct{}), started: make(chan struct{})}
			gates = append(gates, g)
			go a.ReplyAs(a.ID, f.ID, 200, nil, g, -1) // chunked
			<-g.started
		}
	}
	fleettest.WaitUntil(t, 5*time.Second, "a's replies read", func() bool { return r.hub(keyOf(tokA)).replyUsed.Load() == 16*replyStep })
	a2, _ := fleettest.DialHub(t, base, tokA, "a2", fleettest.HubInstanceOptions{})
	b, _ := fleettest.DialHub(t, base, tokB, "b", fleettest.HubInstanceOptions{})
	for _, c := range []struct {
		in   *fleettest.HubInstance
		rt   http.RoundTripper
		want int
	}{{a2, rtA, http.StatusServiceUnavailable}, {b, r.Transport(tokB), http.StatusNoContent}} {
		f, res := held(t, context.Background(), c.rt, c.in)
		if code, _ := c.in.Reply(f.ID, 200, nil, []byte(`[]`)); code != c.want {
			t.Errorf("reply of %s while a's chunked replies fill a's budget: %d, want %d", c.in.ID, code, c.want)
		}
		if err := wait(t, res); (err == nil) != (c.want == http.StatusNoContent) {
			t.Errorf("RoundTrip to %s: %v", c.in.ID, err)
		}
	}
}

// TestStreamBody400: a stream request with a body, sized or chunked, gets
// 400: the hub never reads a stream's body.
func TestStreamBody400(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	for _, body := range []io.Reader{strings.NewReader("x"), io.MultiReader(strings.NewReader("x"))} {
		req, _ := http.NewRequest(http.MethodGet, streamURL(base, "body", "1"), body)
		req.Header.Set("Authorization", "Bearer "+tokA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("stream with a body (length %d, 0: chunked): %d, want 400", req.ContentLength, resp.StatusCode)
		}
	}
	if r.hub(keyOf(tokA)).get("body") != nil {
		t.Error("a stream with a body was registered")
	}
}

// logBuf is a log sink safe for concurrent writes.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestPerIPCountsTokenless: the per-IP limit counts tokenless connections
// only, and at the limit the IP's oldest one is closed for the new one:
// instances behind one address are not cut, and a tokenless client of
// their address cannot lock them out.
func TestPerIPCountsTokenless(t *testing.T) {
	var logs logBuf
	r := newRouter(slowBeat, 0, nil)
	r.perIP = 3
	r.Sync(hubConfig(map[string]string{"a": tokA}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, r, nil, time.Second, slog.New(slog.NewTextHandler(&logs, nil))) }()
	defer func() { cancel(); <-served }()
	base := "http://" + ln.Addr().String()

	a, code := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	if code != http.StatusOK {
		t.Fatal(code)
	}
	fleettest.WaitUntil(t, 5*time.Second, "a listed", func() bool { return len(r.Source(tokA).Endpoints()) == 1 })
	if code, err := a.Beat(false); code != http.StatusNoContent {
		t.Fatalf("beat: %d, %v", code, err)
	}
	idle := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	// closed: the server closed c within d; else it still holds it.
	closed := func(c net.Conn, d time.Duration) bool {
		_ = c.SetReadDeadline(time.Now().Add(d))
		_, err := c.Read(make([]byte, 1))
		return !errors.Is(err, os.ErrDeadlineExceeded)
	}
	// a's stream and beat connections leave the IP's 3 tokenless slots.
	held := []net.Conn{idle(), idle(), idle()}
	for i, c := range held {
		if closed(c, 200*time.Millisecond) {
			t.Fatalf("tokenless connection %d closed with a's connections open: they counted", i)
		}
	}
	// A fourth closes the oldest.
	fourth := idle()
	if !closed(held[0], 5*time.Second) {
		t.Error("the oldest tokenless connection was not closed for a new one")
	}
	if closed(fourth, 200*time.Millisecond) {
		t.Error("a tokenless connection at the IP's limit was refused")
	}
	// An instance of the same address still gets in, and a stays.
	if _, code := fleettest.DialHub(t, base, tokA, "b", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Errorf("instance b of a full address: %d, want 200", code)
	}
	if code, err := a.Beat(false); code != http.StatusNoContent {
		t.Errorf("beat of a after the evictions: %d, %v", code, err)
	}
	if out := logs.String(); !strings.Contains(out, "127.0.0.1") || strings.Contains(out, tokA) {
		t.Errorf("logs: want the eviction with its IP, never the token:\n%s", out)
	}
}

// serveHub serves r with Serve on a new loopback listener until the test
// ends, and returns its address.
func serveHub(t *testing.T, r *Router) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, r, nil, time.Second, nil) }()
	t.Cleanup(func() { cancel(); <-served })
	return ln.Addr().String()
}

// rawPost sends one request on a new connection to addr and returns the
// connection and the status, 0 when no reply came.
func rawPost(t *testing.T, addr, req string) (net.Conn, int) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, req); err != nil {
		return c, 0
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return c, 0
	}
	resp.Body.Close()
	_ = c.SetReadDeadline(time.Time{})
	return c, resp.StatusCode
}

// beatReq is a beat of instance with token and session.
func beatReq(instance, token, session string) string {
	return fmt.Sprintf("POST /hub/beat?instance=%s HTTP/1.1\r\nHost: h\r\nAuthorization: Bearer %s\r\n%s: %s\r\nContent-Length: 0\r\n\r\n", instance, token, SessionHeader, session)
}

// TestValidTokenNotLive: a valid token alone does not authenticate a
// connection; a request of no live session (a beat of an unknown
// instance, a duplicate stream) leaves it under the request timeout.
func TestValidTokenNotLive(t *testing.T) {
	const timeout = 300 * time.Millisecond
	r := newRouter(slowBeat, 0, nil)
	r.lim.reqTimeout = timeout
	r.Sync(hubConfig(map[string]string{"a": tokA}))
	addr := serveHub(t, r)
	if _, code := fleettest.DialHub(t, "http://"+addr, tokA, "dup", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Fatal(code)
	}
	for _, c := range []struct {
		name, req string
		want      int
	}{
		{"beat of nobody", beatReq("nobody", tokA, "wrong-session-0123456789"), http.StatusNotFound},
		{"duplicate stream", "GET /hub/stream?instance=dup&protocol=1 HTTP/1.1\r\nHost: h\r\nAuthorization: Bearer " + tokA + "\r\n\r\n", http.StatusConflict},
	} {
		conn, code := rawPost(t, addr, c.req)
		if code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout + 2*time.Second))
		_, err := io.Copy(io.Discard, conn)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%s: the connection outlived the request timeout", c.name)
		}
	}
}

// TestServerConnShare: each hub server holds at most connsPerInstance ×
// maxInstances authenticated connections: a holder of server a's token
// cannot fill the listener and lock out server b. A connection
// authenticated for a may still carry b's requests (a reverse proxy).
func TestServerConnShare(t *testing.T) {
	r := newRouter(slowBeat, 1, nil) // a share of 10 each; the cap 276
	r.Sync(hubConfig(map[string]string{"a": tokA, "b": tokB}))
	addr := serveHub(t, r)
	base := "http://" + addr
	a, code := fleettest.DialHub(t, base, tokA, "a1", fleettest.HubInstanceOptions{})
	if code != http.StatusOK {
		t.Fatal(code)
	}
	fleettest.WaitUntil(t, 5*time.Second, "a1 listed", func() bool { return len(r.Source(tokA).Endpoints()) == 1 })
	var first net.Conn
	held := 0
	for range 400 {
		c, code := rawPost(t, addr, beatReq("a1", tokA, a.Session()))
		if code != http.StatusNoContent {
			break
		}
		if first == nil {
			first = c
		}
		held++
	}
	if held > connsPerInstance {
		t.Errorf("a held %d more authenticated connections, want at most its share of %d", held, connsPerInstance)
	}
	b, code := fleettest.DialHub(t, base, tokB, "b1", fleettest.HubInstanceOptions{})
	if code != http.StatusOK {
		t.Fatalf("b's stream while a holds its share: %d, want 200", code)
	}
	fleettest.WaitUntil(t, 5*time.Second, "b1 listed", func() bool { return len(r.Source(tokB).Endpoints()) == 1 })
	if first == nil {
		t.Fatal("no connection of a authenticated")
	}
	io.WriteString(first, beatReq("b1", tokB, b.Session()))
	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if resp, err := http.ReadResponse(bufio.NewReader(first), nil); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Errorf("b's beat on a connection of a: %v, %v; want 204", resp, err)
	}
}

// TestChunkedReplyMemory: a chunked reply of MaxReply bytes allocates
// about its size; a doubling buffer allocated 4 times it and held twice.
func TestChunkedReplyMemory(t *testing.T) {
	r, base := newTestHub(t, slowBeat, 0, map[string]string{"a": tokA})
	a, _ := fleettest.DialHub(t, base, tokA, "a", fleettest.HubInstanceOptions{})
	f, res := held(t, context.Background(), r.Transport(tokA), a)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	code, err := a.ReplyAs(a.ID, f.ID, 200, nil, &zeros{MaxReply}, -1)
	runtime.ReadMemStats(&after)
	if err != nil || code != http.StatusNoContent {
		t.Fatalf("chunked reply of MaxReply bytes: %d, %v; want 204", code, err)
	}
	if err := wait(t, res); err != nil {
		t.Fatal(err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > MaxReply*3/2 {
		t.Errorf("a chunked reply of %d bytes allocated %d", MaxReply, got)
	}
}
