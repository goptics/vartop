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
	"github.com/goptics/vartop/internal/hub"
)

// newTestCollector starts a collector polling the endpoints every second
// into a Ring; it is closed when the test ends, before the fakes it polls
// shut down.
func newTestCollector(t *testing.T, endpoints ...string) (*Collector, *Ring) {
	t.Helper()
	srv := &config.Server{Name: "test", Interval: 1, Endpoints: endpoints}
	pool, err := api.PoolFor(srv)
	if err != nil {
		t.Fatal(err)
	}
	hist := NewRing(srv.PollInterval())
	c := NewCollector(srv, pool, CollectorInit{}, func(tick Tick) error {
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
			Completed: 90, Successful: 85, Failed: 5},
	})
	_, b := fleettest.NewVarmq(t, "host", map[string]*fleettest.Worker{
		"orders":  {Status: "Paused", Concurrency: 4, Pending: 7, Idle: 4, Completed: 2, Successful: 2},
		"billing": {Status: "Idle", Concurrency: 1, Idle: 1},
	})
	wantEPs := []string{a.URL, b.URL}
	slices.Sort(wantEPs)
	pool := fleettest.StaticPool(t, b.URL, a.URL)

	start := time.Now()
	s := pollOnce(context.Background(), pool, newTracker(time.Second))
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
		// Numbered in endpoint order.
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
	want.Metrics.Completed, want.Metrics.Successful, want.Metrics.Failed = 90, 85, 5
	if !reflect.DeepEqual(sm.WorkerDetails, want) {
		t.Errorf("sample = %+v\nwant     %+v", sm.WorkerDetails, want)
	}
}

func TestPollOnceSkipsWorkerGoneBeforeGet(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{
		"w1": {Status: "Running", Concurrency: 1},
		"w2": nil, // listed, then 404 on GET
	})

	pod := podByEndpoint(t, pollOnce(context.Background(), fleettest.StaticPool(t, srv.URL), newTracker(time.Second)), srv.URL)

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

	pod := podByEndpoint(t, pollOnce(context.Background(), fleettest.StaticPool(t, srv.URL), newTracker(time.Second)), srv.URL)

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
	s := pollOnce(ctx, pool, newTracker(time.Second))
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
}

func TestPollOnceEscapesNames(t *testing.T) {
	f, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"a/b c": {Status: "Running"}})

	pod := podByEndpoint(t, pollOnce(context.Background(), fleettest.StaticPool(t, srv.URL), newTracker(time.Second)), srv.URL)

	if !slices.Contains(f.Calls(), "GET /workers/a%2Fb%20c") {
		t.Errorf("calls = %v, want the escaped name", f.Calls())
	}
	if pod.Err != "" || pod.Workers["a/b c"] == nil {
		t.Errorf("Err = %q, workers %v; want the sample under its raw name", pod.Err, sampleNames(pod))
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
	c := NewCollector(&config.Server{Name: "test", Interval: 1, Endpoints: []string{srv.URL}}, pool, CollectorInit{}, func(Tick) error {
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
		fleettest.StaticPool(t, srv.URL), CollectorInit{}, func(tick Tick) error {
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

// firstTick starts a collector polling srv every second from init and
// returns its first tick; the collector is closed before it returns.
func firstTick(t *testing.T, srv *config.Server, pool *api.Pool, init CollectorInit) Tick {
	t.Helper()
	ticks := make(chan Tick, 1)
	c := NewCollector(srv, pool, init, func(tick Tick) error {
		select {
		case ticks <- tick:
		default:
		}
		return nil
	})
	defer func() { _ = c.Close() }()
	select {
	case tick := <-ticks:
		return tick
	case <-time.After(3 * time.Second):
		t.Fatal("no tick within 3s")
		return Tick{}
	}
}

// TestCollectorLabels: a collector hands its sink the label state with the
// tick that changed it, and numbers units by the state it starts from.
func TestCollectorLabels(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", nil)
	cfg := &config.Server{Name: "test", Interval: 1, Endpoints: []string{srv.URL}}

	first := firstTick(t, cfg, fleettest.StaticPool(t, srv.URL), CollectorInit{})
	if l := first.Snap.Pods[0].Label; l != "instance-1" {
		t.Errorf("first tick labels the unit %q, want instance-1", l)
	}
	if first.Labels == nil || first.Labels.Numbers["#i"].N != 1 {
		t.Errorf("first tick's Labels = %+v, want #i numbered 1", first.Labels)
	}

	saved := LabelState{
		Numbers:  map[string]LabelNumber{"#i": {N: 7, Seen: time.Now().Truncate(time.Minute)}},
		Followed: map[string]string{srv.URL: "#i"},
	}
	resumed := firstTick(t, cfg, fleettest.StaticPool(t, srv.URL), CollectorInit{Labels: &saved})
	if l := resumed.Snap.Pods[0].Label; l != "instance-7" {
		t.Errorf("started from saved labels, the first tick labels the unit %q, want instance-7", l)
	}
}

// TestCollectorSeedsRates: a collector started from the last snapshot of an
// earlier run rates its first tick against it.
func TestCollectorSeedsRates(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running", Completed: 50}})
	at := time.Now().Add(-time.Second).Round(0)
	prev := &Sample{At: at, RateOK: true}
	prev.Name, prev.Instance, prev.Metrics.Completed = "orders", "i", 40
	last := &Snapshot{At: at, Pods: []PodSnapshot{{Key: srv.URL, Via: srv.URL, Instance: "i",
		Workers: map[string]*Sample{"orders": prev}}}}

	tick := firstTick(t, &config.Server{Name: "test", Interval: 1, Endpoints: []string{srv.URL}},
		fleettest.StaticPool(t, srv.URL), CollectorInit{Last: last})
	if sm := podByEndpoint(t, tick.Snap, srv.URL).Workers["orders"]; !sm.RateOK || sm.CompletedRate <= 0 {
		t.Errorf("first sample: RateOK = %v, CompletedRate = %v; want a positive rate against the seed", sm.RateOK, sm.CompletedRate)
	}
	if prev.Metrics.Completed != 40 || !prev.RateOK {
		t.Errorf("seed sample changed: %+v", prev)
	}
}

// panicSource is a discovery source of endpoints that panics while
// panicking is set.
type panicSource struct {
	endpoints []string
	panicking atomic.Bool
}

func (s *panicSource) Endpoints() []string {
	if s.panicking.Load() {
		panic("source broke")
	}
	return s.endpoints
}

func (*panicSource) Close() error { return nil }

// TestCollectorRecoversPanic: a tick that panics hands its sink an error
// snapshot with the panic, and the collector polls on.
func TestCollectorRecoversPanic(t *testing.T) {
	_, srv := fleettest.NewVarmq(t, "i", map[string]*fleettest.Worker{"orders": {Status: "Running"}})
	src := &panicSource{endpoints: []string{srv.URL}}
	src.panicking.Store(true)
	ticks := make(chan Tick, 8)
	c := NewCollector(&config.Server{Name: "test", Interval: 1}, api.NewPool(src, ""), CollectorInit{}, func(tick Tick) error {
		select {
		case ticks <- tick:
		default:
		}
		return nil
	})
	t.Cleanup(func() { _ = c.Close() })

	var tick Tick
	select {
	case tick = <-ticks:
	case <-time.After(3 * time.Second):
		t.Fatal("no tick within 3s")
	}
	if tick.Panic == nil || tick.Panic.Value != "source broke" || !strings.Contains(string(tick.Panic.Stack), "Endpoints") {
		t.Fatalf("Panic = %+v, want the source's panic and its stack", tick.Panic)
	}
	if s := tick.Snap; s == nil || s.Err != "internal error" || len(s.Pods) != 0 || s.At.IsZero() {
		t.Errorf("Snap = %+v, want a dated error snapshot with no units", s)
	}

	src.panicking.Store(false)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case tick = <-ticks:
		case <-deadline:
			t.Fatal("no clean tick within 3s of the source mending")
		}
		if tick.Panic == nil {
			break
		}
	}
	if tick.Snap.Err != "" || len(tick.Snap.Pods) != 1 {
		t.Errorf("after the panic: Snap = %+v, want a clean poll", tick.Snap)
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

// raised runs f and returns what it panicked with, nil if nothing.
func raised(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

// TestPanicGroupReraises: Wait waits for every goroutine, then raises the
// panic of one again, with the stack it was raised on, through any number
// of groups.
func TestPanicGroupReraises(t *testing.T) {
	var f panicGroup
	var finished atomic.Int32
	for i := range 3 {
		f.Go(func() {
			if i == 1 {
				panic("worker broke")
			}
			time.Sleep(50 * time.Millisecond)
			finished.Add(1)
		})
	}
	p, ok := raised(f.Wait).(*TickPanic)
	if !ok || p.Value != "worker broke" || !strings.Contains(string(p.Stack), "TestPanicGroupReraises.func1") {
		t.Errorf("Wait raised %+v, want the goroutine's panic and stack", p)
	}
	if n := finished.Load(); n != 2 {
		t.Errorf("%d goroutines finished before Wait raised, want the other 2", n)
	}

	var outer panicGroup
	outer.Go(func() {
		var inner panicGroup
		inner.Go(func() { panic("deep") })
		inner.Wait()
	})
	p, ok = raised(outer.Wait).(*TickPanic)
	if !ok || p.Value != "deep" || strings.Contains(string(p.Stack), "panicGroup).Wait") {
		t.Errorf("outer Wait raised %+v, want the innermost panic and its own stack", p)
	}

	var calm panicGroup
	calm.Go(func() {})
	if v := raised(calm.Wait); v != nil {
		t.Errorf("Wait with no panic raised %v", v)
	}
}

// TestPollHubServer: a hub server's instances, reached through the hub,
// poll as direct units through the poller, one per instance and never a
// balancer, even when each names itself otherwise; an action reaches each
// once, on the first try; one that disconnects leaves the next poll.
func TestPollHubServer(t *testing.T) {
	const tok = "hub-token-0123456789abcdef"
	srv := &config.Server{Name: "h", Hub: true, Token: tok, Interval: 1}
	r := hub.NewRouter(hub.Options{})
	r.Sync(&config.Config{Servers: map[string]*config.Server{"h": srv}})
	ts := httptest.NewServer(r)
	t.Cleanup(func() {
		r.Close()
		ts.Close()
	})
	// Both fakes answer as "liar": the hub names each by its stream.
	f1, v1 := fleettest.NewVarmq(t, "liar", map[string]*fleettest.Worker{"emails": {Status: "Running", Concurrency: 2}})
	f2, v2 := fleettest.NewVarmq(t, "liar", map[string]*fleettest.Worker{"emails": {Status: "Running", Concurrency: 1}})
	fleettest.DialHub(t, ts.URL, tok, "pod-1", fleettest.HubInstanceOptions{Handler: v1.Config.Handler})
	i2, _ := fleettest.DialHub(t, ts.URL, tok, "pod-2", fleettest.HubInstanceOptions{Handler: v2.Config.Handler})

	pool, err := r.BuildPool(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	tr := newTracker(srv.PollInterval())
	var snap *Snapshot
	for range 3 { // a balancer would show from the second tick on
		snap = pollOnce(context.Background(), pool, tr)
	}
	if len(snap.Pods) != 2 || snap.Behind != nil || snap.Err != "" {
		t.Fatalf("snapshot = %d units, behind %v, err %q; want 2 direct units", len(snap.Pods), snap.Behind, snap.Err)
	}
	for _, p := range snap.Pods {
		if p.Balanced || p.Err != "" || p.Key != hub.Endpoint(p.Instance) || p.Workers["emails"] == nil {
			t.Errorf("unit %+v: want a direct unit of its instance with emails", p)
		}
	}

	sc, err := ScopeOf(ClientsFor(snap, "", r.Transport(tok)), snap, "", instanceNoun)
	if err != nil || sc.ViaBalancer() || len(sc.Direct) != 2 {
		t.Fatalf("scope = %d direct, via balancer %v, %v", len(sc.Direct), sc.ViaBalancer(), err)
	}
	for _, res := range Act(context.Background(), sc, UnitsOf(snap), instanceNoun, []string{"emails"}, ActionSender("pause"))[0] {
		if res.Outcome != "ok" {
			t.Errorf("pause on %s: %s %s", res.Pod, res.Outcome, res.Msg)
		}
	}
	for i, f := range []*fleettest.Varmq{f1, f2} {
		if n := countCalls(f.Calls(), "PATCH /workers/emails/actions/pause"); n != 1 {
			t.Errorf("instance %d got %d pauses, want 1", i+1, n)
		}
	}

	i2.Close()
	fleettest.WaitUntil(t, 5*time.Second, "pod-2 gone from discovery", func() bool { return len(pool.Clients()) == 1 })
	snap = pollOnce(context.Background(), pool, tr)
	if len(snap.Pods) != 1 || snap.Pods[0].Instance != "pod-1" {
		t.Errorf("after pod-2 left: %+v, want pod-1 alone", snap.Pods)
	}
}

// peakServer serves h, counting the requests whose path starts with
// prefix in flight at once, each held for hold; peak is the most seen.
func peakServer(t *testing.T, h http.Handler, prefix string, hold time.Duration) (srv *httptest.Server, peak func() int32) {
	var now, most atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			n := now.Add(1)
			defer now.Add(-1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(hold)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, most.Load
}

// peakRT serves a hub unit's requests on h (its instance path stripped),
// counting worker fetches in flight at once, each held for hold.
type peakRT struct {
	h         http.Handler
	hold      time.Duration
	now, most atomic.Int32
}

func (p *peakRT) RoundTrip(req *http.Request) (*http.Response, error) {
	_, path, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if strings.HasPrefix(path, "workers/") {
		n := p.now.Add(1)
		defer p.now.Add(-1)
		for m := p.most.Load(); n > m && !p.most.CompareAndSwap(m, n); m = p.most.Load() {
		}
		time.Sleep(p.hold)
	}
	r := req.Clone(req.Context())
	r.URL.Path, r.URL.RawPath = "/"+path, ""
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// TestPollDirectBoundsFetches: a hub unit's worker fetches run unitFetches
// at a time (the instance's in-flight cap), however many workers it lists.
func TestPollDirectBoundsFetches(t *testing.T) {
	workers := make(map[string]*fleettest.Worker)
	for i := range 5 * unitFetches {
		workers[fmt.Sprintf("w%d", i)] = &fleettest.Worker{Status: "Running"}
	}
	_, v := fleettest.NewVarmq(t, "a", workers)
	rt := &peakRT{h: v.Config.Handler, hold: 20 * time.Millisecond}
	pod, _ := pollDirect(context.Background(), api.NewClientVia(hub.Endpoint("a"), "", rt))
	if pod.Err != "" || len(pod.Workers) != len(workers) {
		t.Fatalf("pod: err %q, %d workers, want %d", pod.Err, len(pod.Workers), len(workers))
	}
	if p := rt.most.Load(); p > unitFetches || p < 2 {
		t.Errorf("worker fetches in flight at once: %d, want 2 to %d", p, unitFetches)
	}
}

// TestPollDirectPullFetchesInTick: a pull unit fetches more workers at
// once than a hub unit: 100 workers of 70ms each fit a 1s interval's tick
// (0.8s); 8 at a time took 0.9s.
func TestPollDirectPullFetchesInTick(t *testing.T) {
	workers := make(map[string]*fleettest.Worker)
	for i := range 100 {
		workers[fmt.Sprintf("w%d", i)] = &fleettest.Worker{Status: "Running"}
	}
	_, v := fleettest.NewVarmq(t, "a", workers)
	srv, peak := peakServer(t, v.Config.Handler, "/workers/", 70*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	pod, _ := pollDirect(ctx, fleettest.StaticPool(t, srv.URL).Clients()[0])
	if pod.Err != "" || len(pod.Workers) != len(workers) {
		t.Fatalf("pod: err %q, %d workers, want %d", pod.Err, len(pod.Workers), len(workers))
	}
	if p := peak(); p <= unitFetches {
		t.Errorf("worker fetches in flight at once: %d, want over %d", p, unitFetches)
	}
}

// TestPollDirectWorkerCap: a unit that lists over maxUnitWorkers workers
// is errored, and none of them is fetched.
func TestPollDirectWorkerCap(t *testing.T) {
	workers := make(map[string]*fleettest.Worker)
	for i := range maxUnitWorkers + 1 {
		workers[fmt.Sprintf("w%d", i)] = &fleettest.Worker{Status: "Running"}
	}
	v, srv := fleettest.NewVarmq(t, "a", workers)
	pod, answered := pollDirect(context.Background(), fleettest.StaticPool(t, srv.URL).Clients()[0])
	if !strings.Contains(pod.Err, "too many workers") || len(pod.Workers) != 0 || !slices.Equal(answered, []string{"a"}) {
		t.Errorf("pod: err %q, %d workers, answered %v; want too many workers, none, [a]", pod.Err, len(pod.Workers), answered)
	}
	if calls := v.Calls(); len(calls) != 1 {
		t.Errorf("requests %d, want the list only", len(calls))
	}
}
