package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
)

// recordedRequest captures what a test server saw.
type recordedRequest struct {
	Method      string
	Path        string
	EscapedPath string
	Auth        string
	HasAuth     bool
}

// recordingServer answers every request with the given status, headers and body
// and records each request it receives.
func recordingServer(t *testing.T, status int, header map[string]string, body string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_, has := r.Header["Authorization"]
		reqs = append(reqs, recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			EscapedPath: r.URL.EscapedPath(),
			Auth:        r.Header.Get("Authorization"),
			HasAuth:     has,
		})
		mu.Unlock()
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

func TestGetWorker(t *testing.T) {
	const body = `{"name":"emails","status":"running","concurrency":4,"num_processing":2,"num_pending":7,"num_idle_workers":2,
		"metrics":{"submitted":10,"completed":8,"successful":6,"failed":2}}`

	tests := []struct {
		name         string
		header       map[string]string
		wantInstance string
	}{
		{"with instance header", map[string]string{InstanceHeader: "pod-a"}, "pod-a"},
		{"without instance header", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := recordingServer(t, http.StatusOK, tt.header, body)
			c := NewClient(srv.URL, "")

			w, err := c.GetWorker("emails")
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			reqs := got()
			if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/workers/emails" {
				t.Fatalf("requests = %+v, want one GET /workers/emails", reqs)
			}
			if w.Name != "emails" || w.Status != "running" || w.Concurrency != 4 ||
				w.NumProcessing != 2 || w.NumPending != 7 || w.NumIdleWorkers != 2 {
				t.Errorf("summary decoded wrong: %+v", w.WorkerSummary)
			}
			if w.Metrics.Submitted != 10 || w.Metrics.Completed != 8 || w.Metrics.Successful != 6 || w.Metrics.Failed != 2 {
				t.Errorf("metrics decoded wrong: %+v", w.Metrics)
			}
			if w.Origin != srv.URL {
				t.Errorf("Origin = %q, want %q", w.Origin, srv.URL)
			}
			if w.Instance != tt.wantInstance {
				t.Errorf("Instance = %q, want %q", w.Instance, tt.wantInstance)
			}
			if c.LastInstance() != tt.wantInstance {
				t.Errorf("LastInstance = %q, want %q", c.LastInstance(), tt.wantInstance)
			}
		})
	}
}

func TestGetWorkerInvalidJSON(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, nil, "not json")
	if _, err := NewClient(srv.URL, "").GetWorker("w"); err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

func TestMutatingCalls(t *testing.T) {
	tests := []struct {
		name     string
		call     func(c *Client) error
		wantPath string
	}{
		{"pause", func(c *Client) error { return c.Action("emails", "pause") }, "/workers/emails/actions/pause"},
		{"resume", func(c *Client) error { return c.Action("emails", "resume") }, "/workers/emails/actions/resume"},
		{"restart", func(c *Client) error { return c.Action("jobs", "restart") }, "/workers/jobs/actions/restart"},
		{"tune concurrency", func(c *Client) error { return c.TuneConcurrency("emails", 12) }, "/workers/emails/config/concurrency/12"},
		{"tune concurrency zero", func(c *Client) error { return c.TuneConcurrency("emails", 0) }, "/workers/emails/config/concurrency/0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := recordingServer(t, http.StatusOK, nil, "")
			if err := tt.call(NewClient(srv.URL, "")); err != nil {
				t.Fatalf("call returned %v, want nil", err)
			}
			reqs := got()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1", len(reqs))
			}
			if reqs[0].Method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", reqs[0].Method)
			}
			if reqs[0].Path != tt.wantPath {
				t.Errorf("path = %s, want %s", reqs[0].Path, tt.wantPath)
			}
		})
	}
}

func TestAuthHeader(t *testing.T) {
	calls := map[string]func(c *Client) error{
		"ListWorkers":     func(c *Client) error { _, err := c.ListWorkers(); return err },
		"GetWorker":       func(c *Client) error { _, err := c.GetWorker("w"); return err },
		"Action":          func(c *Client) error { return c.Action("w", "pause") },
		"TuneConcurrency": func(c *Client) error { return c.TuneConcurrency("w", 3) },
		"GetHealth":       func(c *Client) error { return c.GetHealth() },
		// The new-connection variants carry every request through a balancer.
		"ListWorkersFresh":     func(c *Client) error { _, _, err := c.ListWorkersFresh(context.Background()); return err },
		"GetWorkerFresh":       func(c *Client) error { _, _, err := c.GetWorkerFresh(context.Background(), "w"); return err },
		"ActionFresh":          func(c *Client) error { _, err := c.ActionFresh(context.Background(), "w", "pause"); return err },
		"TuneConcurrencyFresh": func(c *Client) error { _, err := c.TuneConcurrencyFresh(context.Background(), "w", 3); return err },
	}

	tests := []struct {
		name     string
		token    string
		wantAuth bool
	}{
		{"token set", "s3cret", true},
		{"token empty", "", false},
	}

	for _, tt := range tests {
		for callName, call := range calls {
			t.Run(tt.name+"/"+callName, func(t *testing.T) {
				// "{}" decodes into WorkerDetails; ListWorkers needs an array.
				body := "{}"
				if callName == "ListWorkers" || callName == "ListWorkersFresh" {
					body = "[]"
				}
				srv, got := recordingServer(t, http.StatusOK, nil, body)
				if err := call(NewClient(srv.URL, tt.token)); err != nil {
					t.Fatalf("call: %v", err)
				}
				reqs := got()
				if len(reqs) != 1 {
					t.Fatalf("got %d requests, want 1", len(reqs))
				}
				if reqs[0].HasAuth != tt.wantAuth {
					t.Fatalf("Authorization present = %v, want %v", reqs[0].HasAuth, tt.wantAuth)
				}
				if tt.wantAuth && reqs[0].Auth != "Bearer "+tt.token {
					t.Errorf("Authorization = %q, want %q", reqs[0].Auth, "Bearer "+tt.token)
				}
			})
		}
	}
}

func TestErrorParsing(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantMsg string
	}{
		{"json error", http.StatusNotFound, `{"error":"worker not found"}`, "worker not found"},
		{"json error 500", http.StatusInternalServerError, `{"error":"boom"}`, "boom"},
		{"json without error key", http.StatusBadRequest, `{"message":"nope"}`, http.StatusText(http.StatusBadRequest)},
		{"plain text", http.StatusBadGateway, "upstream exploded", http.StatusText(http.StatusBadGateway)},
		{"empty body", http.StatusUnauthorized, "", http.StatusText(http.StatusUnauthorized)},
		{"html body", http.StatusForbidden, "<html>forbidden</html>", http.StatusText(http.StatusForbidden)},
		{"redirect-class status", http.StatusMultipleChoices, "", http.StatusText(http.StatusMultipleChoices)},
	}

	calls := map[string]func(c *Client) error{
		"Action":          func(c *Client) error { return c.Action("w", "pause") },
		"TuneConcurrency": func(c *Client) error { return c.TuneConcurrency("w", 2) },
		"GetWorker":       func(c *Client) error { _, err := c.GetWorker("w"); return err },
		"ListWorkers":     func(c *Client) error { _, err := c.ListWorkers(); return err },
		"GetHealth":       func(c *Client) error { return c.GetHealth() },
	}

	for _, tt := range tests {
		for callName, call := range calls {
			t.Run(tt.name+"/"+callName, func(t *testing.T) {
				srv, _ := recordingServer(t, tt.status, nil, tt.body)
				err := call(NewClient(srv.URL, ""))
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("error %v (%T) is not *APIError", err, err)
				}
				if apiErr.StatusCode != tt.status {
					t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.status)
				}
				if apiErr.Message != tt.wantMsg {
					t.Errorf("Message = %q, want %q", apiErr.Message, tt.wantMsg)
				}
			})
		}
	}
}

func TestAPIErrorFormat(t *testing.T) {
	e := &APIError{StatusCode: 404, Message: "worker not found"}
	want := "API error (status 404): worker not found"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close() // nothing is listening any more

	c := NewClient(endpoint, "tok")
	calls := map[string]func() error{
		"ListWorkers":     func() error { _, err := c.ListWorkers(); return err },
		"GetWorker":       func() error { _, err := c.GetWorker("w"); return err },
		"Action":          func() error { return c.Action("w", "pause") },
		"TuneConcurrency": func() error { return c.TuneConcurrency("w", 1) },
		"GetHealth":       func() error { return c.GetHealth() },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("expected transport error, got nil")
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				t.Errorf("transport failure should not be an *APIError, got %v", err)
			}
		})
	}
}

func TestPoolForStatic(t *testing.T) {
	server := &config.Server{
		Name:      "static",
		Endpoints: []string{"http://c:1/varmq", "http://a:1/varmq", "http://b:1/varmq"},
		Token:     "tok",
	}
	pool, err := PoolFor(server)
	if err != nil {
		t.Fatalf("PoolFor: %v", err)
	}
	defer pool.Close()

	var got []string
	for _, c := range pool.Clients() {
		got = append(got, c.PrimaryEndpoint())
		if c.token != "tok" {
			t.Errorf("client %s token = %q, want %q", c.PrimaryEndpoint(), c.token, "tok")
		}
	}
	want := []string{"http://a:1/varmq", "http://b:1/varmq", "http://c:1/varmq"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints = %v, want %v", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("endpoints not sorted: %v", got)
	}
}

func TestPoolForDNS(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	pool, err := PoolFor(&config.Server{
		Name: "dns",
		DNS:  &config.DNSRef{Host: "localhost", Port: port},
	})
	if err != nil {
		t.Fatalf("PoolFor: %v", err)
	}
	defer pool.Close()

	clients := pool.Clients()
	if len(clients) == 0 {
		t.Fatal("Clients() is empty, want at least one")
	}
	for _, c := range clients {
		ep := c.PrimaryEndpoint()
		if !strings.HasPrefix(ep, "http://") || !strings.HasSuffix(ep, "/varmq") {
			t.Errorf("endpoint %q: want http://.../varmq", ep)
		}
		if !strings.Contains(ep, ":"+strconv.Itoa(port)+"/") {
			t.Errorf("endpoint %q does not use port %d", ep, port)
		}
	}
}

func TestPoolForDNSUnresolvable(t *testing.T) {
	pool, err := PoolFor(&config.Server{
		Name: "dns",
		DNS:  &config.DNSRef{Host: "does-not-exist.invalid", Port: 8080},
	})
	if err == nil {
		pool.Close()
		t.Fatal("expected error for unresolvable host, got nil")
	}
	if pool != nil {
		t.Errorf("pool = %v, want nil on error", pool)
	}
}

func TestPoolForKubernetesBadContext(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\ncontexts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	pool, err := PoolFor(&config.Server{
		Name:       "k8s",
		Kubernetes: &config.KubernetesRef{Context: "nope", Namespace: "default", Service: "x"},
	})
	if err == nil {
		pool.Close()
		t.Fatal("expected error for unknown kube context, got nil")
	}
	if pool != nil {
		t.Errorf("pool = %v, want nil on error", pool)
	}
}

func TestCtxMethodsHonourCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	c := NewClient(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ListWorkersCtx(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("did not return promptly on ctx deadline")
	}
}

func TestGetWorkerCtxKeepsEscapedPath(t *testing.T) {
	srv, got := recordingServer(t, http.StatusOK, nil, `{"name":"a/b c"}`)
	c := NewClient(srv.URL, "")
	if _, err := c.GetWorkerCtx(context.Background(), url.PathEscape("a/b c")); err != nil {
		t.Fatal(err)
	}
	if p := got()[0].EscapedPath; p != "/workers/a%2Fb%20c" {
		t.Fatalf("path = %q", p)
	}
}

func TestMutatingCtxMethodsKeepEscapedPath(t *testing.T) {
	name := url.PathEscape("a/b c")
	tests := []struct {
		name     string
		call     func(c *Client) error
		wantPath string
	}{
		{"action", func(c *Client) error { return c.ActionCtx(context.Background(), name, "pause") }, "/workers/a%2Fb%20c/actions/pause"},
		{"tune concurrency", func(c *Client) error { return c.TuneConcurrencyCtx(context.Background(), name, 4) }, "/workers/a%2Fb%20c/config/concurrency/4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := recordingServer(t, http.StatusOK, nil, "")
			if err := tt.call(NewClient(srv.URL, "")); err != nil {
				t.Fatal(err)
			}
			reqs := got()
			if len(reqs) != 1 || reqs[0].Method != http.MethodPatch || reqs[0].EscapedPath != tt.wantPath {
				t.Fatalf("requests = %+v, want one PATCH %s", reqs, tt.wantPath)
			}
		})
	}
}

// countingServer answers like a varmq mux and counts the TCP connections it
// accepts.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/workers":
			_, _ = w.Write([]byte("[{\"name\":\"w\"}]\n"))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("{\"name\":\"w\"}\n"))
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

func TestFreshMethodsUseNewConnections(t *testing.T) {
	srv, conns := countingServer(t)
	c := NewClient(srv.URL, "")
	ctx := context.Background()

	for range 3 {
		if _, err := c.ListWorkersCtx(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("3 pooled calls opened %d connections, want 1", got)
	}

	for range 3 {
		if _, _, err := c.ListWorkersFresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := conns.Load(); got != 4 {
		t.Fatalf("after 3 ListWorkersFresh: %d connections, want 4", got)
	}

	if _, _, err := c.GetWorkerFresh(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ActionFresh(ctx, "w", "pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TuneConcurrencyFresh(ctx, "w", 3); err != nil {
		t.Fatal(err)
	}
	if got := conns.Load(); got != 7 {
		t.Fatalf("after Get/Action/Tune Fresh: %d connections, want 7", got)
	}
}

func TestFreshMethodsReportInstance(t *testing.T) {
	var mu sync.Mutex
	var patched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(InstanceHeader, "a")
		switch {
		case r.URL.Path == "/workers":
			_, _ = w.Write([]byte(`[{"name":"w"}]`))
		case r.URL.Path == "/workers/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"worker not found"}`))
		case strings.HasSuffix(r.URL.Path, "/actions/pause"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"already paused"}`))
		default:
			mu.Lock()
			patched = append(patched, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "")
	ctx := context.Background()

	workers, inst, err := c.ListWorkersFresh(ctx)
	if err != nil || inst != "a" {
		t.Fatalf("ListWorkersFresh = %v, %q, %v; want instance a, no error", workers, inst, err)
	}
	if len(workers) != 1 || workers[0].Origin != srv.URL || workers[0].Instance != "a" {
		t.Fatalf("workers = %+v, want one tagged with origin %s and instance a", workers, srv.URL)
	}

	var apiErr *APIError
	_, inst, err = c.GetWorkerFresh(ctx, "gone")
	if inst != "a" || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("GetWorkerFresh(gone) = %q, %v; want instance a with a 404 APIError", inst, err)
	}

	apiErr = nil
	inst, err = c.ActionFresh(ctx, "w", "pause")
	if inst != "a" || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("ActionFresh(pause) = %q, %v; want instance a with a 409 APIError", inst, err)
	}

	inst, err = c.TuneConcurrencyFresh(ctx, "w", 3)
	if inst != "a" || err != nil {
		t.Fatalf("TuneConcurrencyFresh = %q, %v; want instance a, no error", inst, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"PATCH /workers/w/config/concurrency/3"}; !reflect.DeepEqual(patched, want) {
		t.Errorf("patched = %v, want %v", patched, want)
	}

	if c.SeenInstances() != 1 || c.LastInstance() != "a" {
		t.Errorf("SeenInstances = %d, LastInstance = %q; want 1 and a", c.SeenInstances(), c.LastInstance())
	}
}

func TestFreshMethodsNoReply(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "")
	_, inst, err := c.ListWorkersFresh(context.Background())
	if err == nil || inst != "" {
		t.Fatalf("ListWorkersFresh = %q, %v; want no instance and an error", inst, err)
	}
}

func TestFreshMethodsEscapeNothing(t *testing.T) {
	srv, got := recordingServer(t, http.StatusOK, nil, `{"name":"a/b"}`)
	c := NewClient(srv.URL, "")
	ctx := context.Background()

	if _, _, err := c.GetWorkerFresh(ctx, url.PathEscape("a/b")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ActionFresh(ctx, url.PathEscape("x y"), "stop"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range got() {
		paths = append(paths, r.EscapedPath)
	}
	if want := []string{"/workers/a%2Fb", "/workers/x%20y/actions/stop"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}
