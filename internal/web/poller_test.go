package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// staticPool is a pool over fixed endpoints, closed when the test ends.
func staticPool(t *testing.T, endpoints ...string) *api.Pool {
	t.Helper()
	pool, err := api.PoolFor(&config.Server{Interval: 1, Endpoints: endpoints})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// newTestSession starts a session polling the endpoints every second; it is
// closed when the test ends, before the fakes it polls shut down.
func newTestSession(t *testing.T, endpoints ...string) *Session {
	t.Helper()
	srv := &config.Server{Name: "test", Interval: 1, Endpoints: endpoints}
	pool, err := api.PoolFor(srv)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(srv, pool)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// waitUntil polls cond every 10ms and fails the test if it is still false
// after timeout.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func podByEndpoint(t *testing.T, s *Snapshot, endpoint string) PodSnapshot {
	t.Helper()
	for _, p := range s.Pods {
		if p.Key == endpoint {
			return p
		}
	}
	t.Fatalf("no pod %s in snapshot", endpoint)
	return PodSnapshot{}
}

func sampleNames(p PodSnapshot) []string {
	names := make([]string, 0, len(p.Workers))
	for name := range p.Workers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func countCalls(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestPollOnceBuildsSnapshot(t *testing.T) {
	_, a := newFakeVarmq(t, "host", map[string]*fakeWorker{
		"orders": {Status: "Running", Concurrency: 8, Busy: 6, Pending: 80, Idle: 2,
			Submitted: 100, Completed: 90, Successful: 85, Failed: 5},
	})
	_, b := newFakeVarmq(t, "host", map[string]*fakeWorker{
		"orders":  {Status: "Paused", Concurrency: 4, Pending: 7, Idle: 4, Submitted: 3, Completed: 2, Successful: 2},
		"billing": {Status: "Idle", Concurrency: 1, Idle: 1},
	})
	wantEPs := []string{a.URL, b.URL}
	slices.Sort(wantEPs)
	pool := staticPool(t, b.URL, a.URL)

	start := time.Now()
	s := pollOnce(context.Background(), pool)
	end := time.Now()

	if s.At.Before(start) || s.At.After(end) {
		t.Errorf("Snapshot.At = %v, want the tick start within [%v, %v]", s.At, start, end)
	}
	var gotEPs []string
	for _, p := range s.Pods {
		gotEPs = append(gotEPs, p.Key)
	}
	if !slices.Equal(gotEPs, wantEPs) {
		t.Fatalf("pods = %v, want sorted %v", gotEPs, wantEPs)
	}
	for _, p := range s.Pods {
		// Both pods report instance "host": the shared label falls back to host:port.
		if want := strings.TrimPrefix(p.Key, "http://"); p.Label != want {
			t.Errorf("%s: Label = %q, want %q", p.Key, p.Label, want)
		}
		if p.Err != "" || p.Balanced {
			t.Errorf("%s: Err = %q, Balanced = %v; want healthy and direct", p.Key, p.Err, p.Balanced)
		}
		if p.Via != p.Key || p.Instance != "host" {
			t.Errorf("%s: Via = %q, Instance = %q; want the endpoint and the instance that answered", p.Key, p.Via, p.Instance)
		}
	}

	if got := sampleNames(podByEndpoint(t, s, b.URL)); !slices.Equal(got, []string{"billing", "orders"}) {
		t.Errorf("pod b workers = %v", got)
	}
	sm := podByEndpoint(t, s, a.URL).Workers["orders"]
	if sm == nil {
		t.Fatal("pod a has no orders sample")
	}
	if sm.At.Before(s.At) || sm.At.After(end) {
		t.Errorf("Sample.At = %v, want arrival time within [%v, %v]", sm.At, s.At, end)
	}
	want := api.WorkerDetails{WorkerSummary: api.WorkerSummary{
		Name: "orders", Status: "Running", Concurrency: 8, NumProcessing: 6, NumPending: 80, NumIdleWorkers: 2,
		Origin: a.URL, Instance: "host",
	}}
	want.Metrics.Submitted, want.Metrics.Completed, want.Metrics.Successful, want.Metrics.Failed = 100, 90, 85, 5
	if !reflect.DeepEqual(sm.WorkerDetails, want) {
		t.Errorf("sample = %+v\nwant     %+v", sm.WorkerDetails, want)
	}
}

func TestPollOnceSkipsWorkerGoneBeforeGet(t *testing.T) {
	f, srv := newFakeVarmq(t, "i", map[string]*fakeWorker{
		"w1": {Status: "Running", Concurrency: 1},
		"w2": nil, // listed, then 404 on GET
	})

	pod := podByEndpoint(t, pollOnce(context.Background(), staticPool(t, srv.URL)), srv.URL)

	if countCalls(f.calls(), "GET /workers/w2") != 1 {
		t.Fatalf("w2 was not fetched: calls %v", f.calls())
	}
	if pod.Err != "" {
		t.Errorf("Err = %q, want healthy pod", pod.Err)
	}
	if got := sampleNames(pod); !slices.Equal(got, []string{"w1"}) {
		t.Errorf("workers = %v, want [w1]", got)
	}
}

func TestPollOnceWorkerFetchErrorMarksPod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workers":
			_, _ = w.Write([]byte(`[{"name":"w1"},{"name":"w2"}]`))
		case "/workers/w1":
			_, _ = w.Write([]byte(`{"name":"w1","status":"Running"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}
	}))
	t.Cleanup(srv.Close)

	pod := podByEndpoint(t, pollOnce(context.Background(), staticPool(t, srv.URL)), srv.URL)

	// Not a 404: the worker may still exist, so the pod is unknown rather
	// than healthy-without-w2.
	if !strings.HasPrefix(pod.Err, "worker w2: ") || !strings.Contains(pod.Err, "boom") {
		t.Errorf("Err = %q, want the w2 fetch error, naming w2", pod.Err)
	}
}

func TestPollOnceSlowPodTimesOut(t *testing.T) {
	_, a := newFakeVarmq(t, "a", map[string]*fakeWorker{"orders": {Status: "Running"}})
	fb, b := newFakeVarmq(t, "b", map[string]*fakeWorker{"orders": {Status: "Running"}})
	fb.Delay = 500 * time.Millisecond
	pool := staticPool(t, a.URL, b.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s := pollOnce(ctx, pool)
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Errorf("pollOnce took %v, want it bounded by the 100ms deadline", elapsed)
	}

	if pb := podByEndpoint(t, s, b.URL); pb.Err == "" || len(pb.Workers) != 0 {
		t.Errorf("slow pod: Err = %q, workers %v; want errored", pb.Err, sampleNames(pb))
	}
	pa := podByEndpoint(t, s, a.URL)
	if pa.Err != "" || !slices.Equal(sampleNames(pa), []string{"orders"}) {
		t.Errorf("fast pod: Err = %q, workers %v; want healthy with orders", pa.Err, sampleNames(pa))
	}
	// Captured after its requests, so the label is the instance that answered.
	if pa.Label != "a" {
		t.Errorf("fast pod Label = %q, want its instance %q", pa.Label, "a")
	}
}

func TestPollOnceEscapesNames(t *testing.T) {
	f, srv := newFakeVarmq(t, "i", map[string]*fakeWorker{"a/b c": {Status: "Running"}})

	pod := podByEndpoint(t, pollOnce(context.Background(), staticPool(t, srv.URL)), srv.URL)

	if !slices.Contains(f.calls(), "GET /workers/a%2Fb%20c") {
		t.Errorf("calls = %v, want the escaped name", f.calls())
	}
	if pod.Err != "" || pod.Workers["a/b c"] == nil {
		t.Errorf("Err = %q, workers %v; want the sample under its raw name", pod.Err, sampleNames(pod))
	}
}

func TestLabelCollisionsFallBack(t *testing.T) {
	pods := []PodSnapshot{
		{Key: "http://h:1/a", Label: "x"},
		{Key: "http://h:1/b", Label: "x"},
		{Key: "http://h:2", Label: "x"},
		{Key: "http://h:3", Label: "y"},
	}

	dedupeLabels(pods)

	got := make([]string, len(pods))
	for i, p := range pods {
		got[i] = p.Label
	}
	want := []string{"http://h:1/a", "http://h:1/b", "h:2", "y"}
	if !slices.Equal(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
}

func TestLabelCollisionsFallBackOnVia(t *testing.T) {
	pods := []PodSnapshot{
		{Key: "#a", Via: "http://h:1/lb", Label: "x"},
		{Key: "http://h:2", Via: "http://h:2", Label: "x"},
	}

	dedupeLabels(pods)

	if got, want := []string{pods[0].Label, pods[1].Label}, []string{"h:1", "h:2"}; !slices.Equal(got, want) {
		t.Errorf("labels = %q, want %q (host:port of the address each was reached through)", got, want)
	}
}

// An empty worker list still says which instance answered.
func TestPollPodEmptyListNamesInstance(t *testing.T) {
	_, srv := newFakeVarmq(t, "idle-host", map[string]*fakeWorker{})
	pod := pollPod(context.Background(), staticPool(t, srv.URL).Clients()[0])

	if pod.Key != srv.URL || pod.Via != srv.URL || pod.Instance != "idle-host" || pod.Err != "" || len(pod.Workers) != 0 {
		t.Errorf("pod = %+v, want a healthy direct unit of instance idle-host with no workers", pod)
	}
}

func TestSessionPollsAndPollNow(t *testing.T) {
	f, srv := newFakeVarmq(t, "i", map[string]*fakeWorker{"orders": {Status: "Running"}})
	s := newTestSession(t, srv.URL)

	if s.Interval != time.Second || s.History.Interval() != time.Second {
		t.Fatalf("Interval = %v, History interval = %v; want 1s", s.Interval, s.History.Interval())
	}
	if cs := s.Clients(); len(cs) != 1 || cs[0].PrimaryEndpoint() != srv.URL {
		t.Fatalf("Clients() = %v, want one client for %s", cs, srv.URL)
	}
	waitUntil(t, 3*time.Second, "first snapshot", func() bool { return s.History.Latest() != nil })
	first := s.History.Latest()

	f.mu.Lock()
	f.Workers["orders"].Status = "Paused"
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !s.PollNow(ctx) {
		t.Fatal("PollNow = false, want true")
	}

	latest := s.History.Latest()
	if !latest.At.After(first.At) {
		t.Errorf("Latest().At = %v, want after %v", latest.At, first.At)
	}
	if sm := podByEndpoint(t, latest, srv.URL).Workers["orders"]; sm == nil || sm.Status != "Paused" {
		t.Errorf("after PollNow orders = %+v, want the Paused state set before it", sm)
	}
}

func TestSessionCloseStopsPolling(t *testing.T) {
	f, srv := newFakeVarmq(t, "i", map[string]*fakeWorker{"orders": {Status: "Running"}})
	s := newTestSession(t, srv.URL)
	waitUntil(t, 3*time.Second, "first snapshot", func() bool { return s.History.Latest() != nil })

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let a tick cancelled mid-flight wind down
	snaps, calls := len(s.History.Snapshots()), len(f.calls())
	time.Sleep(2 * s.Interval)

	if got := len(s.History.Snapshots()); got != snaps {
		t.Errorf("history grew from %d to %d snapshots after Close", snaps, got)
	}
	if got := len(f.calls()); got != calls {
		t.Errorf("fake got %d new calls after Close", got-calls)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if s.PollNow(ctx) {
		t.Error("PollNow on a closed session = true, want false")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("PollNow on a closed session took %v, want an immediate false", elapsed)
	}
}

func TestTicksDoNotOverlap(t *testing.T) {
	f, srv := newFakeVarmq(t, "i", map[string]*fakeWorker{"orders": {Status: "Running"}})
	f.Delay = 1500 * time.Millisecond // 1.5 × the 1s interval
	s := newTestSession(t, srv.URL)

	waitUntil(t, 8*time.Second, "three snapshots", func() bool { return len(s.History.Snapshots()) >= 3 })

	// Inline ticks with the timer reset after each one start 0.8s (the
	// deadline-cut tick) + 1s (the interval) apart; overlapping or
	// ticker-driven ticks would start 1s apart.
	snaps := s.History.Snapshots()
	for i := 1; i < len(snaps); i++ {
		if gap := snaps[i].At.Sub(snaps[i-1].At); gap < 1500*time.Millisecond {
			t.Errorf("tick %d started %v after the previous one, want ≥ 1.5s (tick time + interval)", i, gap)
		}
	}
	if got := f.maxListConcurrency(); got != 1 {
		t.Errorf("max concurrent list calls = %d, want 1", got)
	}
	// The 0.8 × interval tick deadline cuts each 1.5s list short.
	if err := podByEndpoint(t, snaps[len(snaps)-1], srv.URL).Err; err == "" {
		t.Error("latest pod is healthy; want it errored by the tick deadline")
	}
}
