package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// newTestCollector starts a collector polling the endpoints every second
// into a History; it is closed when the test ends, before the fakes it polls
// shut down.
func newTestCollector(t *testing.T, endpoints ...string) (*Collector, *History) {
	t.Helper()
	srv := &config.Server{Name: "test", Interval: 1, Endpoints: endpoints}
	pool, err := api.PoolFor(srv)
	if err != nil {
		t.Fatal(err)
	}
	hist := NewHistory(srv.PollInterval())
	c := NewCollector(srv, pool, func(tick Tick) error {
		hist.Append(tick.Snap)
		return nil
	})
	t.Cleanup(func() { _ = c.Close() })
	return c, hist
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
	_, a := fleettest.NewVarmq(t, "host", map[string]*fleettest.Worker{
		"orders": {Status: "Running", Concurrency: 8, Busy: 6, Pending: 80, Idle: 2,
			Submitted: 100, Completed: 90, Successful: 85, Failed: 5},
	})
	_, b := fleettest.NewVarmq(t, "host", map[string]*fleettest.Worker{
		"orders":  {Status: "Paused", Concurrency: 4, Pending: 7, Idle: 4, Submitted: 3, Completed: 2, Successful: 2},
		"billing": {Status: "Idle", Concurrency: 1, Idle: 1},
	})
	wantEPs := []string{a.URL, b.URL}
	slices.Sort(wantEPs)
	pool := fleettest.StaticPool(t, b.URL, a.URL)

	start := time.Now()
	s := pollOnce(context.Background(), pool, newTracker(time.Second, false))
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
	for i, p := range s.Pods {
		// Not a Kubernetes server: numbered in endpoint order.
		if want := fmt.Sprintf("instance-%d", i+1); p.Label != want {
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
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{
		"w1": {Status: "Running", Concurrency: 1},
		"w2": nil, // listed, then 404 on GET
	})

	pod := podByEndpoint(t, pollOnce(context.Background(), fleettest.StaticPool(t, srv.URL), newTracker(time.Second, false)), srv.URL)

	if countCalls(f.Calls(), "GET /workers/w2") != 1 {
		t.Fatalf("w2 was not fetched: calls %v", f.Calls())
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

	pod := podByEndpoint(t, pollOnce(context.Background(), fleettest.StaticPool(t, srv.URL), newTracker(time.Second, false)), srv.URL)

	// Not a 404: the worker may still exist, so the pod is unknown rather
	// than healthy-without-w2.
	if !strings.HasPrefix(pod.Err, "worker w2: ") || !strings.Contains(pod.Err, "boom") {
		t.Errorf("Err = %q, want the w2 fetch error, naming w2", pod.Err)
	}
}

func TestPollOnceSlowPodTimesOut(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	fb, b := fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	fb.Delay = 500 * time.Millisecond
	pool := fleettest.StaticPool(t, a.URL, b.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s := pollOnce(ctx, pool, newTracker(time.Second, true))
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
	// Labelled as on a Kubernetes server: captured after its requests, the
	// pod name falls back to the instance that answered.
	if pa.Label != "a" {
		t.Errorf("fast pod Label = %q, want its instance %q", pa.Label, "a")
	}
}

func TestPollOnceEscapesNames(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"a/b c": {Status: "Running"}})

	pod := podByEndpoint(t, pollOnce(context.Background(), fleettest.StaticPool(t, srv.URL), newTracker(time.Second, false)), srv.URL)

	if !slices.Contains(f.Calls(), "GET /workers/a%2Fb%20c") {
		t.Errorf("calls = %v, want the escaped name", f.Calls())
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
func TestPollDirectEmptyListNamesInstance(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "idle-host", map[string]*fleettest.Worker{})
	pod, answered := pollDirect(context.Background(), fleettest.StaticPool(t, srv.URL).Clients()[0])

	if pod.Key != srv.URL || pod.Via != srv.URL || pod.Instance != "idle-host" || pod.Err != "" || len(pod.Workers) != 0 {
		t.Errorf("pod = %+v, want a healthy direct unit of instance idle-host with no workers", pod)
	}
	if !slices.Equal(answered, []string{"idle-host"}) {
		t.Errorf("answered = %v, want [idle-host]", answered)
	}
}

func TestCollectorPollsAndPollNow(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	c, hist := newTestCollector(t, srv.URL)

	fleettest.WaitUntil(t, 3*time.Second, "first snapshot", func() bool { return hist.Latest() != nil })
	first := hist.Latest()

	f.Mu.Lock()
	f.Workers["orders"].Status = "Paused"
	f.Mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !c.PollNow(ctx) {
		t.Fatal("PollNow = false, want true")
	}

	latest := hist.Latest()
	if !latest.At.After(first.At) {
		t.Errorf("Latest().At = %v, want after %v", latest.At, first.At)
	}
	if sm := podByEndpoint(t, latest, srv.URL).Workers["orders"]; sm == nil || sm.Status != "Paused" {
		t.Errorf("after PollNow orders = %+v, want the Paused state set before it", sm)
	}
}

func TestCollectorCloseStopsPolling(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	c, hist := newTestCollector(t, srv.URL)
	fleettest.WaitUntil(t, 3*time.Second, "first snapshot", func() bool { return hist.Latest() != nil })

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let a tick cancelled mid-flight wind down
	snaps, calls := len(hist.Snapshots()), len(f.Calls())
	time.Sleep(2 * hist.Interval())

	if got := len(hist.Snapshots()); got != snaps {
		t.Errorf("history grew from %d to %d snapshots after Close", snaps, got)
	}
	if got := len(f.Calls()); got != calls {
		t.Errorf("fake got %d new calls after Close", got-calls)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if c.PollNow(ctx) {
		t.Error("PollNow on a closed collector = true, want false")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("PollNow on a closed collector took %v, want an immediate false", elapsed)
	}
}

// TestCollectorStopsOnSinkError: a sink that fails ends the polling. The
// sink is not called again, nothing more is polled, and PollNow answers
// false at once.
func TestCollectorStopsOnSinkError(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	pool := fleettest.StaticPool(t, srv.URL)
	var sunk atomic.Int32
	c := NewCollector(&config.Server{Name: "test", Interval: 1, Endpoints: []string{srv.URL}}, pool, func(Tick) error {
		sunk.Add(1)
		return errors.New("disk full")
	})
	t.Cleanup(func() { _ = c.Close() })
	fleettest.WaitUntil(t, 3*time.Second, "the first snapshot sunk", func() bool { return sunk.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if c.PollNow(ctx) {
		t.Error("PollNow after a sink error = true, want false")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("PollNow after a sink error took %v, want an immediate false", elapsed)
	}
	calls := len(f.Calls())
	time.Sleep(1500 * time.Millisecond) // past the next tick's start
	if got := sunk.Load(); got != 1 {
		t.Errorf("sink called %d times, want once: a sink error ends the polling", got)
	}
	if got := len(f.Calls()); got != calls {
		t.Errorf("fake got %d new calls after the sink error", got-calls)
	}
}

// TestCollectorRatesSnapshots: the snapshots a collector hands its sink are
// rated, the second tick's against the first.
func TestCollectorRatesSnapshots(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running", Completed: 10}})
	ticks := make(chan Tick, 4)
	c := NewCollector(&config.Server{Name: "test", Interval: 1, Endpoints: []string{srv.URL}},
		fleettest.StaticPool(t, srv.URL), func(tick Tick) error {
			select {
			case ticks <- tick:
			default:
			}
			return nil
		})
	t.Cleanup(func() { _ = c.Close() })

	next := func() *Sample {
		t.Helper()
		select {
		case tick := <-ticks:
			return podByEndpoint(t, tick.Snap, srv.URL).Workers["orders"]
		case <-time.After(3 * time.Second):
			t.Fatal("no tick within 3s")
			return nil
		}
	}
	if first := next(); first.RateOK {
		t.Errorf("first sample rated: %+v", first)
	}
	f.Mu.Lock()
	f.Workers["orders"].Completed = 1000
	f.Mu.Unlock()
	if second := next(); !second.RateOK || second.CompletedRate <= 0 {
		t.Errorf("second sample: RateOK = %v, CompletedRate = %v; want a positive rate", second.RateOK, second.CompletedRate)
	}
}

// TestCollectorLabels: a collector hands its sink the label state with the
// tick that changed it, and numbers units by the state LoadLabels gives it.
func TestCollectorLabels(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	ticks := make(chan Tick, 8)
	c := NewCollector(&config.Server{Name: "test", Interval: 1, Endpoints: []string{srv.URL}},
		fleettest.StaticPool(t, srv.URL), func(tick Tick) error {
			select {
			case ticks <- tick:
			default:
			}
			return nil
		})
	t.Cleanup(func() { _ = c.Close() })

	select {
	case first := <-ticks:
		if l := first.Snap.Pods[0].Label; l != "instance-1" {
			t.Errorf("first tick labels the unit %q, want instance-1", l)
		}
		if first.Labels == nil || first.Labels.Numbers["#i"].N != 1 {
			t.Errorf("first tick's Labels = %+v, want #i numbered 1", first.Labels)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no tick within 3s")
	}

	c.LoadLabels(LabelState{
		Numbers:  map[string]LabelNumber{"#i": {N: 7, Seen: time.Now().Truncate(time.Minute)}},
		Followed: map[string]string{srv.URL: "#i"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !c.PollNow(ctx) {
		t.Fatal("PollNow = false, want true")
	}
	var last Tick
	for len(ticks) > 0 {
		last = <-ticks
	}
	if last.Snap == nil {
		t.Fatal("no tick after PollNow")
	}
	if l := last.Snap.Pods[0].Label; l != "instance-7" {
		t.Errorf("after LoadLabels the unit is labelled %q, want instance-7", l)
	}
}

func TestTicksDoNotOverlap(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	f.Delay = 1500 * time.Millisecond // 1.5 × the 1s interval
	_, hist := newTestCollector(t, srv.URL)

	fleettest.WaitUntil(t, 8*time.Second, "three snapshots", func() bool { return len(hist.Snapshots()) >= 3 })

	// Inline ticks with the timer reset after each one start 0.8s (the
	// deadline-cut tick) + 1s (the interval) apart; overlapping or
	// ticker-driven ticks would start 1s apart.
	snaps := hist.Snapshots()
	for i := 1; i < len(snaps); i++ {
		if gap := snaps[i].At.Sub(snaps[i-1].At); gap < 1500*time.Millisecond {
			t.Errorf("tick %d started %v after the previous one, want ≥ 1.5s (tick time + interval)", i, gap)
		}
	}
	if got := f.MaxListConcurrency(); got != 1 {
		t.Errorf("max concurrent list calls = %d, want 1", got)
	}
	// The 0.8 × interval tick deadline cuts each 1.5s list short.
	if err := podByEndpoint(t, snaps[len(snaps)-1], srv.URL).Err; err == "" {
		t.Error("latest pod is healthy; want it errored by the tick deadline")
	}
}
