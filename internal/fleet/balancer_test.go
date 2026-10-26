package fleet

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// lbPoll returns a pool over fleettest.LBServer(urls...) and a tracker for
// it, as NewCollector would make them. The pool is closed when the test ends.
func lbPoll(t *testing.T, urls ...string) (*api.Pool, *tracker) {
	t.Helper()
	srv := fleettest.LBServer(urls...)
	pool, err := api.PoolFor(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool, newTracker(srv.PollInterval(), srv.Kubernetes != nil)
}

// pollUntil polls up to ticks times until cond holds for the snapshot, and
// fails the test if it never does.
func pollUntil(t *testing.T, pool *api.Pool, tr *tracker, ticks int, what string, cond func(*Snapshot) bool) *Snapshot {
	t.Helper()
	for range ticks {
		if s := pollOnce(context.Background(), pool, tr); cond(s) {
			return s
		}
	}
	t.Fatalf("no %s within %d ticks", what, ticks)
	return nil
}

// unitKeys returns the snapshot's unit keys in order.
func unitKeys(s *Snapshot) []string {
	keys := make([]string, len(s.Pods))
	for i, p := range s.Pods {
		keys[i] = p.Key
	}
	return keys
}

// hasKeys is a pollUntil condition: the snapshot's units are exactly keys,
// in any order (units are listed by number, which follows discovery).
func hasKeys(keys ...string) func(*Snapshot) bool {
	keys = slices.Sorted(slices.Values(keys))
	return func(s *Snapshot) bool { return slices.Equal(slices.Sorted(slices.Values(unitKeys(s))), keys) }
}

func TestAttemptCap(t *testing.T) {
	for k, want := range map[int]int{0: 6, 1: 6, 3: 18, 10: 66} {
		if got := attemptCap(k); got != want {
			t.Errorf("attemptCap(%d) = %d, want %d", k, got, want)
		}
	}
}

// reachLists runs one list job wanting want through eps with the given
// budget, and returns it.
func reachLists(ctx context.Context, eps []*api.Client, budget int, want ...string) *job[[]api.WorkerSummary] {
	r := newReacher(eps, budget)
	j := addJob(r, listFresh, nil)
	j.want(want...)
	r.run(ctx)
	return j
}

func TestReachStopsWhenAllAnswered(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)

	j := reachLists(context.Background(), fleettest.StaticPool(t, lb.URL).Clients(), 18, "a", "b", "c")

	// 3 attempts when the round-robin goes a, b, c; a reply slower than the
	// hedge wait (a loaded machine) adds some, within the budget.
	if ids := slices.Sorted(maps.Keys(j.got)); !slices.Equal(ids, []string{"a", "b", "c"}) || j.sent < 3 || j.sent > 18 {
		t.Fatalf("got %v after %d attempts, want a, b, c after 3 to 18", ids, j.sent)
	}
	for id, ans := range j.got {
		if ans.Err != nil || ans.Via != lb.URL || ans.At.IsZero() || len(ans.V) != 1 || ans.V[0].Instance != id {
			t.Errorf("%s: answer %+v, want its 2xx list via %s with an arrival time", id, ans, lb.URL)
		}
	}
}

func TestReachRespectsBudget(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)

	j := reachLists(context.Background(), fleettest.StaticPool(t, lb.URL).Clients(), 18, "a", "b", "c", "zz")

	if ids := slices.Sorted(maps.Keys(j.got)); !slices.Equal(ids, []string{"a", "b", "c"}) || j.sent != 18 {
		t.Errorf("got %v after %d attempts, want a, b, c (never zz) after the 18-attempt budget", ids, j.sent)
	}
}

func TestReachWithoutWantTriesEveryEndpoint(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb1, lb2 := fleettest.NewRoundRobinLB(t, srvs...), fleettest.NewRoundRobinLB(t, srvs...)

	j := reachLists(context.Background(), fleettest.StaticPool(t, lb1.URL, lb2.URL).Clients(), 18)

	if j.sent != 2 || len(j.got) == 0 {
		t.Errorf("got %d answers after %d attempts, want one attempt per endpoint", len(j.got), j.sent)
	}
}

// fakeSend is a job send answering from a script: call n (from 0) gets
// replies[n], or the last one past its end. A reply with block set waits
// for ctx to end and returns its error. inFlight counts calls running.
type fakeSend struct {
	replies  []fakeReply
	calls    atomic.Int32
	inFlight atomic.Int32
}

type fakeReply struct {
	v        int
	instance string
	err      error
	block    bool
}

func (f *fakeSend) send(ctx context.Context, _ *api.Client) (int, string, error) {
	f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	r := f.replies[min(int(f.calls.Add(1))-1, len(f.replies)-1)]
	if r.block {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a request slow to wind down
		return 0, "", ctx.Err()
	}
	return r.v, r.instance, r.err
}

// oneEndpoint is a reacher endpoint list for sends that never dial.
func oneEndpoint() []*api.Client { return []*api.Client{api.NewClient("http://unused", "")} }

// run returns only after every attempt it started has returned, even when
// its context ends first.
func TestReachReturnsWithItsAttempts(t *testing.T) {
	f := &fakeSend{replies: []fakeReply{{block: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := newReacher(oneEndpoint(), 6)
	j := addJob(r, f.send, nil)
	j.want("a")

	r.run(ctx)

	if n := f.inFlight.Load(); n != 0 {
		t.Errorf("%d attempts still running after run returned", n)
	}
	if len(j.got) != 0 || j.sent != 1 || j.lastErr == nil {
		t.Errorf("got %v, sent %d, lastErr %v; want no answer after the one attempt the deadline cut", j.got, j.sent, j.lastErr)
	}
}

// An instance's error reply does not settle it: a later 2xx does, and wins.
func TestReachTakesDefinitiveAnswer(t *testing.T) {
	boom := &api.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	gone := &api.APIError{StatusCode: http.StatusNotFound, Message: "gone"}

	f := &fakeSend{replies: []fakeReply{{instance: "a", err: boom}, {v: 7, instance: "a"}}}
	r := newReacher(oneEndpoint(), 6)
	j := addJob(r, f.send, nil)
	j.want("a")
	r.run(context.Background())
	if got, ok := j.got["a"]; !ok || got.Err != nil || got.V != 7 || j.sent != 2 || len(j.failed) != 0 {
		t.Errorf("got %+v (ok %v) after %d attempts, failed %v; want the 2xx after the 500", got, ok, j.sent, j.failed)
	}

	f = &fakeSend{replies: []fakeReply{{instance: "a", err: boom}}}
	r = newReacher(oneEndpoint(), 6)
	j = addJob(r, f.send, nil)
	j.want("a")
	r.run(context.Background())
	if _, ok := j.got["a"]; ok || j.sent != 6 || j.failed["a"].Err != boom {
		t.Errorf("got %v after %d attempts, failed %v; want the 500 kept after the whole budget", j.got, j.sent, j.failed)
	}

	f = &fakeSend{replies: []fakeReply{{instance: "a", err: gone}}}
	r = newReacher(oneEndpoint(), 6)
	j = addJob(r, f.send, isNotFound)
	j.want("a")
	r.run(context.Background())
	if got, ok := j.got["a"]; !ok || got.Err != gone || j.sent != 1 {
		t.Errorf("got %+v (ok %v) after %d attempts; want the 404 to settle a fetch at once", got, ok, j.sent)
	}
}

// An attempt stuck on a slow instance does not hold its job up: once the
// replies stop coming, the job sends others, and the stuck one is cancelled
// when the job is done.
func TestReachRetriesPastAStuckAttempt(t *testing.T) {
	f := &fakeSend{replies: []fakeReply{{instance: "a"}, {block: true}, {instance: "b"}}}
	r := newReacher(oneEndpoint(), 6)
	j := addJob(r, f.send, nil)
	j.want("a", "b")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	r.run(ctx)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %v, want it to retry well before any deadline", elapsed)
	}
	if ids := slices.Sorted(maps.Keys(j.got)); !slices.Equal(ids, []string{"a", "b"}) || f.inFlight.Load() != 0 {
		t.Errorf("got %v with %d attempts in flight, want a and b and the stuck attempt cancelled", ids, f.inFlight.Load())
	}
}

func TestPollRoundRobinBalancer(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)

	s := pollOnce(context.Background(), pool, tr)

	if keys := unitKeys(s); !slices.Equal(keys, []string{"#a", "#b", "#c"}) {
		t.Fatalf("units = %v, want #a, #b, #c (none keyed by the balancer URL)", keys)
	}
	for i, p := range s.Pods {
		id := strings.TrimPrefix(p.Key, "#")
		if !p.Balanced || p.Err != "" || p.Instance != id || p.Via != lb.URL || len(p.Unreached) != 0 {
			t.Errorf("%s = %+v, want a healthy balanced unit of instance %s via %s", p.Key, p, id, lb.URL)
		}
		if want := fmt.Sprintf("instance-%d", i+1); p.Label != want {
			t.Errorf("%s: Label = %q, want %q", p.Key, p.Label, want)
		}
		sm := p.Workers["w"]
		if sm == nil || sm.Instance != id || sm.Metrics.Completed != uint64(10*(i+1)) || sm.At.Before(s.At) {
			t.Errorf("%s: sample %+v, want instance %s's own w answered after the tick start", p.Key, sm, id)
		}
	}
	if want := map[string]int{lb.URL: 3}; !maps.Equal(s.Behind, want) {
		t.Errorf("Behind = %v, want %v", s.Behind, want)
	}
}

func TestPollPerConnBalancerDetected(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewPerConnLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)

	s := pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))

	for _, p := range s.Pods {
		if !p.Balanced || p.Err != "" || p.Workers["w"] == nil {
			t.Errorf("%s = %+v, want a healthy balanced unit with w", p.Key, p)
		}
	}
}

func TestPollBalancerRates(t *testing.T) {
	const interval = 200 * time.Millisecond
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := fleettest.StaticPool(t, lb.URL), newTracker(interval, false)
	h := NewHistory(interval)

	h.Append(pollOnce(context.Background(), pool, tr))
	first := h.Latest()
	for i, f := range fakes {
		f.Mu.Lock()
		f.Workers["w"].Completed += uint64(100 * (i + 1))
		f.Mu.Unlock()
	}
	time.Sleep(interval)
	h.Append(pollOnce(context.Background(), pool, tr))
	second := h.Latest()

	for i, key := range []string{"#a", "#b", "#c"} {
		prev, cur := podByEndpoint(t, first, key).Workers["w"], podByEndpoint(t, second, key).Workers["w"]
		if prev == nil || cur == nil {
			t.Fatalf("%s: samples %v, %v; want w on both ticks", key, prev, cur)
		}
		want := float64(100*(i+1)) / cur.At.Sub(prev.At).Seconds()
		if !cur.RateOK || math.Abs(cur.CompletedRate-want) > 1e-6 {
			t.Errorf("%s: rate %v (ok %v), want its own instance's %v", key, cur.CompletedRate, cur.RateOK, want)
		}
	}
}

func TestPollBalancerUnreachedWorker(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	fakes[2].Workers["w"].GetStatus = http.StatusInternalServerError
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)

	s := pollUntil(t, pool, tr, 5, "three units", hasKeys("#a", "#b", "#c"))

	c := podByEndpoint(t, s, "#c")
	if c.Err != "" || !slices.Equal(c.Unreached, []string{"w"}) || c.Workers["w"] != nil {
		t.Errorf("#c = %+v, want healthy with w not reached and no sample", c)
	}
	for _, key := range []string{"#a", "#b"} {
		if p := podByEndpoint(t, s, key); p.Err != "" || p.Workers["w"] == nil || len(p.Unreached) != 0 {
			t.Errorf("%s = %+v, want healthy with a w sample", key, p)
		}
	}
}

func TestPollBalancerListError(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	fakes[2].ListStatus = http.StatusInternalServerError
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)

	s := pollUntil(t, pool, tr, 5, "three units", hasKeys("#a", "#b", "#c"))

	if c := podByEndpoint(t, s, "#c"); !strings.Contains(c.Err, "500") || !c.Balanced || len(c.Workers) != 0 {
		t.Errorf("#c = %+v, want a balanced unit errored with its list's 500", c)
	}
	if a := podByEndpoint(t, s, "#a"); a.Err != "" {
		t.Errorf("#a Err = %q, want healthy", a.Err)
	}
}

func TestPollBalancerMissedInstance(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))

	srvs[2].Close() // the balancer now answers c's turns with a bare 502
	// Seen under a second ago, which reads as no age at all.
	if c := podByEndpoint(t, pollOnce(context.Background(), pool, tr), "#c"); c.Err != "not reached this refresh" {
		t.Errorf("#c Err = %q, want not reached this refresh, with no 0s age", c.Err)
	}
	s := pollAt(context.Background(), pool, tr, time.Now().Add(5*time.Second))

	if keys := unitKeys(s); !hasKeys("#a", "#b", "#c")(s) {
		t.Fatalf("units = %v, want #c kept while in the window", keys)
	}
	c := podByEndpoint(t, s, "#c")
	if c.Err != "not reached this refresh · last seen 5s ago" || !c.Balanced || c.Instance != "c" || c.Via != lb.URL {
		t.Errorf("#c = %+v, want balanced instance c via the balancer, not reached this refresh, last seen 5s ago", c)
	}
	for _, key := range []string{"#a", "#b"} {
		if p := podByEndpoint(t, s, key); p.Err != "" || p.Workers["w"] == nil {
			t.Errorf("%s = %+v, want healthy", key, p)
		}
	}
	if s.Behind[lb.URL] != 3 {
		t.Errorf("Behind = %v, want the 3 instances seen within the window", s.Behind)
	}
}

func TestPollBalancerForgetsInstance(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))
	start := time.Now()

	srvs[2].Close()
	ctx := context.Background()
	if s := pollAt(ctx, pool, tr, start.Add(30*time.Second)); !slices.Contains(unitKeys(s), "#c") {
		t.Fatalf("units 30s later = %v, want #c still kept", unitKeys(s))
	}
	// The window is a minute: c was last seen over a minute ago; a and b 40s ago.
	s := pollAt(ctx, pool, tr, start.Add(70*time.Second))

	if keys := unitKeys(s); !hasKeys("#a", "#b")(s) {
		t.Errorf("units = %v, want #c forgotten past the window", keys)
	}
}

func TestPollBalancerBackToDirect(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	start := time.Now()
	tr.saw(lb.URL, "a", start) // b was behind the balancer too, and has left it
	tr.saw(lb.URL, "b", start)
	ctx := context.Background()

	if s := pollAt(ctx, pool, tr, start.Add(30*time.Second)); !slices.Equal(unitKeys(s), []string{"#a", "#b"}) {
		t.Fatalf("units 30s later = %v, want #a and #b while b is in the window", unitKeys(s))
	}
	s := pollAt(ctx, pool, tr, start.Add(70*time.Second))

	if keys := unitKeys(s); !slices.Equal(keys, []string{lb.URL}) {
		t.Fatalf("units = %v, want one unit keyed by the balancer URL", keys)
	}
	if p := s.Pods[0]; p.Balanced || p.Err != "" || p.Instance != "a" || p.Workers["w"] == nil {
		t.Errorf("unit = %+v, want a healthy direct unit of instance a", p)
	}
	if len(s.Behind) != 0 {
		t.Errorf("Behind = %v, want none for a direct endpoint", s.Behind)
	}
}

func TestPollDedupesInstanceAcrossEndpoints(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b")

	t.Run("two balancers", func(t *testing.T) {
		lb1, lb2 := fleettest.NewRoundRobinLB(t, srvs...), fleettest.NewRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb1.URL, lb2.URL)

		s := pollUntil(t, pool, tr, 3, "units #a and #b", hasKeys("#a", "#b"))

		if want := map[string]int{lb1.URL: 2, lb2.URL: 2}; !maps.Equal(s.Behind, want) {
			t.Errorf("Behind = %v, want %v", s.Behind, want)
		}
	})
	t.Run("balancer and own address", func(t *testing.T) {
		lb := fleettest.NewRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL, srvs[0].URL)

		pollUntil(t, pool, tr, 3, "units #b and a's address", hasKeys("#b", srvs[0].URL))

		for range 3 {
			s := pollOnce(context.Background(), pool, tr)
			if keys := unitKeys(s); !hasKeys("#b", srvs[0].URL)(s) {
				t.Fatalf("units = %v, want #b and a's own address, never #a", keys)
			}
			if a := podByEndpoint(t, s, srvs[0].URL); a.Balanced || a.Instance != "a" || a.Err != "" {
				t.Errorf("a = %+v, want the healthy direct unit of instance a", a)
			}
		}
	})
}

func TestPollBalancerIgnoresAnswersWithoutInstance(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b")
	target, err := url.Parse(fleettest.NewRoundRobinLB(t, srvs...).URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var n atomic.Int64
	lb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1)%3 == 0 {
			w.WriteHeader(http.StatusBadGateway) // a balancer's own error page
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(lb.Close)
	pool, tr := lbPoll(t, lb.URL)
	pollUntil(t, pool, tr, 5, "units #a and #b", hasKeys("#a", "#b"))

	for range 3 {
		s := pollOnce(context.Background(), pool, tr)
		if keys := unitKeys(s); !hasKeys("#a", "#b")(s) {
			t.Fatalf("units = %v, want #a and #b", keys)
		}
		for _, p := range s.Pods {
			if p.Err != "" || p.Workers["w"] == nil {
				t.Errorf("%s = %+v, want healthy with its w sample", p.Key, p)
			}
		}
	}
}

func TestPollNoInstanceHeaderStaysDirect(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "", "")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)

	for range 3 {
		s := pollOnce(context.Background(), pool, tr)
		if keys := unitKeys(s); !slices.Equal(keys, []string{lb.URL}) {
			t.Fatalf("units = %v, want one direct unit keyed by the balancer URL", keys)
		}
		if p := s.Pods[0]; p.Balanced || p.Err != "" || p.Workers["w"] == nil {
			t.Errorf("unit = %+v, want a healthy direct unit", p)
		}
		if len(s.Behind) != 0 || BalancerNote(s) != "" {
			t.Errorf("Behind = %v, note %q; want none", s.Behind, BalancerNote(s))
		}
	}
}

func TestPollBalancerDeadline(t *testing.T) {
	fakes, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))

	fakes[2].Mu.Lock()
	fakes[2].Delay = 2 * time.Second
	fakes[2].Mu.Unlock()
	const deadline = 300 * time.Millisecond
	for tick := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		start := time.Now()
		s := pollOnce(ctx, pool, tr)
		cancel()

		if elapsed := time.Since(start); elapsed > deadline+100*time.Millisecond {
			t.Errorf("tick %d took %v, want it bounded by the %v deadline", tick, elapsed, deadline)
		}
		if c := podByEndpoint(t, s, "#c"); c.Err == "" && len(c.Unreached) == 0 {
			t.Errorf("tick %d: #c = %+v, want errored or its workers not reached", tick, c)
		}
		for _, key := range []string{"#a", "#b"} {
			if p := podByEndpoint(t, s, key); p.Err != "" || p.Workers["w"] == nil {
				t.Errorf("tick %d: %s = %+v, want healthy with its w sample", tick, key, p)
			}
		}
	}
}

// A balancer endpoint that holds every request does not hold the tick once
// every instance has answered through the others: the attempt it holds is
// presumed stuck, which counts as back for the one attempt per endpoint.
func TestPollBalancerHungEndpointDoesNotHoldTheTick(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a")
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hung.Close)
	// Three balancer endpoints in front of a, as DNS might list them; the
	// third has started to blackhole.
	endpoints := []string{srvs[0].URL, fleettest.NewRoundRobinLB(t, srvs[0]).URL, hung.URL}
	pool, tr := lbPoll(t, endpoints...)

	const deadline = 800 * time.Millisecond
	for tick := range 3 {
		now := time.Now()
		for _, ep := range endpoints {
			tr.saw(ep, "a", now)
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		units := pollBalanced(ctx, pool.Clients(), tr, nil, now)
		cancel()

		if elapsed := time.Since(now); elapsed > deadline/2 {
			t.Errorf("tick %d took %v, want well under the %v deadline", tick, elapsed, deadline)
		}
		if len(units) != 1 || units[0].Key != "#a" || units[0].Err != "" || units[0].Workers["w"] == nil {
			t.Errorf("tick %d: units = %+v, want #a healthy with its w sample", tick, units)
		}
	}
}

// TestPollBalancerBudgetCountsEveryInstanceBehind: behind a balancer that
// picks at random among a, b and c, where b and c are direct units, polling
// reaches a's list and workers: the budget counts all three instances,
// which makes a miss about 1 in 700 ticks (it was 1 in 6 when only the
// wanted unit counted), so 100 ticks allow 3.
func TestPollBalancerBudgetCountsEveryInstanceBehind(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRandomLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	direct := map[string]bool{"b": true, "c": true}

	misses := 0
	for range 100 {
		now := time.Now()
		for _, id := range []string{"a", "b", "c"} {
			tr.saw(lb.URL, id, now)
		}
		units := pollBalanced(context.Background(), pool.Clients(), tr, direct, now)
		if len(units) != 1 || units[0].Key != "#a" {
			t.Fatalf("units = %+v, want #a alone", units)
		}
		if units[0].Err != "" || units[0].Workers["w"] == nil {
			misses++
		}
	}
	if misses > 3 {
		t.Errorf("a or its worker was not reached on %d of 100 ticks, want at most 3", misses)
	}
}

// A slow direct endpoint does not hold up the balancer next to it.
func TestPollSlowDirectDoesNotStarveBalancer(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b")
	fd, d := fleettest.NewVarmq(t, "d", map[string]*fleettest.Worker{"w": {Status: "Running"}})
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL, d.URL)
	pollUntil(t, pool, tr, 3, "units #a, #b and d", hasKeys("#a", "#b", d.URL))

	fd.Mu.Lock()
	fd.Delay = 2 * time.Second
	fd.Mu.Unlock()
	const deadline = 300 * time.Millisecond
	for tick := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		s := pollOnce(ctx, pool, tr)
		cancel()

		if keys := unitKeys(s); !hasKeys("#a", "#b", d.URL)(s) {
			t.Fatalf("tick %d: units = %v, want #a, #b and d", tick, keys)
		}
		if p := podByEndpoint(t, s, d.URL); p.Err == "" {
			t.Errorf("tick %d: d = %+v, want errored by the deadline", tick, p)
		}
		for _, key := range []string{"#a", "#b"} {
			if p := podByEndpoint(t, s, key); p.Err != "" || p.Workers["w"] == nil {
				t.Errorf("tick %d: %s = %+v, want healthy with its w sample", tick, key, p)
			}
		}
	}
}

// A direct unit whose list got no reply is still its instance's unit, so
// that instance does not also show up through a balancer.
func TestPollDirectUnitWithoutReplyKeepsItsInstance(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	own := fleettest.NewRoundRobinLB(t, srvs[0]) // a's own address, which can go away alone
	pool, tr := lbPoll(t, lb.URL, own.URL)
	pollUntil(t, pool, tr, 3, "units #b and a's address", hasKeys("#b", own.URL))

	own.Close()
	for range 3 {
		s := pollOnce(context.Background(), pool, tr)
		if keys := unitKeys(s); !hasKeys("#b", own.URL)(s) {
			t.Fatalf("units = %v, want #b and a's own address, never #a", keys)
		}
		if a := podByEndpoint(t, s, own.URL); a.Err == "" || a.Instance != "a" {
			t.Errorf("a = %+v, want errored, still instance a's unit", a)
		}
	}
}

func TestPollBalancerEscapesWorkerNames(t *testing.T) {
	workers := func() map[string]*fleettest.Worker {
		return map[string]*fleettest.Worker{"a/b": {Status: "Running"}, "x y": {Status: "Running"}}
	}
	fa, a := fleettest.NewVarmq(t, "a", workers())
	fb, b := fleettest.NewVarmq(t, "b", workers())
	lb := fleettest.NewRoundRobinLB(t, a, b)
	pool, tr := lbPoll(t, lb.URL)

	s := pollUntil(t, pool, tr, 3, "units #a and #b", hasKeys("#a", "#b"))

	for _, p := range s.Pods {
		if got := sampleNames(p); !slices.Equal(got, []string{"a/b", "x y"}) || len(p.Unreached) != 0 {
			t.Errorf("%s workers = %v, unreached %v; want both under their raw names", p.Key, got, p.Unreached)
		}
	}
	calls := append(fa.Calls(), fb.Calls()...)
	if !slices.Contains(calls, "GET /workers/a%2Fb") || !slices.Contains(calls, "GET /workers/x%20y") ||
		slices.Contains(calls, "GET /workers/a/b") {
		t.Errorf("calls = %v, want the escaped names", calls)
	}
}

// Fetches of different workers behind a round-robin balancer must not fall
// into lockstep, each handed the same instance on every try: that misses
// pairs of (instance, worker) tick after tick. A pair missed now and then is
// the attempt cap at work (about one in a few thousand here), which the spec
// allows: one miss over the ten ticks passes, a second fails.
func TestPollBalancerReachesEveryWorker(t *testing.T) {
	for _, tc := range []struct{ instances, workers int }{{2, 2}, {2, 4}, {3, 3}, {3, 6}} {
		t.Run(fmt.Sprintf("%d instances, %d workers", tc.instances, tc.workers), func(t *testing.T) {
			var srvs []*httptest.Server
			var keys []string
			for i := range tc.instances {
				workers := make(map[string]*fleettest.Worker)
				for w := range tc.workers {
					workers[fmt.Sprintf("w%d", w)] = &fleettest.Worker{Status: "Running"}
				}
				id := string(rune('a' + i))
				_, srv := fleettest.NewVarmq(t, id, workers)
				srvs, keys = append(srvs, srv), append(keys, "#"+id)
			}
			lb := fleettest.NewRoundRobinLB(t, srvs...)
			pool, tr := lbPoll(t, lb.URL)
			pollUntil(t, pool, tr, 3, "every unit", hasKeys(keys...))

			var missed map[string]bool // unit key + "/" + worker, on the previous tick
			misses := 0
			for tick := range 10 {
				s := pollOnce(context.Background(), pool, tr)
				now := make(map[string]bool)
				for _, p := range s.Pods {
					if p.Err != "" || len(p.Workers)+len(p.Unreached) != tc.workers {
						t.Fatalf("tick %d: %s = %+v, want healthy with all %d workers listed", tick, p.Key, p, tc.workers)
					}
					misses += len(p.Unreached)
					for _, w := range p.Unreached {
						now[p.Key+"/"+w] = true
						if missed[p.Key+"/"+w] {
							t.Errorf("tick %d: %s/%s not reached two ticks running", tick, p.Key, w)
						}
					}
				}
				missed = now
			}
			if misses > 1 {
				t.Errorf("%d (instance, worker) pairs not reached over 10 ticks, want at most 1", misses)
			}
		})
	}
}

// labelT0 is when the label tests' ticks happen, unless they say otherwise.
var labelT0 = time.Date(2026, 10, 26, 12, 0, 30, 0, time.UTC)

// labels maps each unit key of pods to its label.
func labels(pods []PodSnapshot) map[string]string {
	m := make(map[string]string, len(pods))
	for _, p := range pods {
		m[p.Key] = p.Label
	}
	return m
}

// TestLabelsFollowTheInstance: a unit's number follows its instance while
// no other unit shares that instance, so an endpoint found to be a balancer
// hands its number on to the instance it showed, and units are listed by
// number.
func TestLabelsFollowTheInstance(t *testing.T) {
	tr := newTracker(time.Second, false)
	first := []PodSnapshot{{Key: "http://lb", Instance: "b"}}
	tr.label(first, labelT0)
	second := []PodSnapshot{{Key: "#a", Instance: "a", Balanced: true}, {Key: "#b", Instance: "b", Balanced: true}}
	tr.label(second, labelT0)

	if got, want := labels(first), map[string]string{"http://lb": "instance-1"}; !maps.Equal(got, want) {
		t.Errorf("first tick labels = %v, want %v", got, want)
	}
	if got, want := labels(second), map[string]string{"#a": "instance-2", "#b": "instance-1"}; !maps.Equal(got, want) {
		t.Errorf("second tick labels = %v, want %v (b keeps its number)", got, want)
	}
	if keys := unitKeys(&Snapshot{Pods: second}); !slices.Equal(keys, []string{"#b", "#a"}) {
		t.Errorf("units listed %v, want by number: #b, #a", keys)
	}

	t.Run("a shared instance numbers by key", func(t *testing.T) {
		pods := []PodSnapshot{{Key: "http://z", Instance: "x"}, {Key: "http://y", Instance: "x"}, {Key: "http://w"}}
		newTracker(time.Second, false).label(pods, labelT0)
		want := map[string]string{"http://w": "instance-1", "http://y": "instance-2", "http://z": "instance-3"}
		if got := labels(pods); !maps.Equal(got, want) {
			t.Errorf("labels = %v, want %v", got, want)
		}
	})
	t.Run("an idle fleet behind a balancer numbers from 1", func(t *testing.T) {
		// No workers: the first ticks' lists each reach one instance, so the
		// balancer looks like a direct unit until a second instance answers.
		var srvs []*httptest.Server
		for _, id := range []string{"a", "b", "c"} {
			_, srv := fleettest.NewVarmq(t, id, nil)
			srvs = append(srvs, srv)
		}
		pool, tr := lbPoll(t, fleettest.NewRoundRobinLB(t, srvs...).URL)

		s := pollUntil(t, pool, tr, 5, "three units", hasKeys("#a", "#b", "#c"))

		want := map[string]string{"#a": "instance-1", "#b": "instance-2", "#c": "instance-3"}
		if got := labels(s.Pods); !maps.Equal(got, want) {
			t.Errorf("labels = %v, want %v", got, want)
		}
	})
}

// TestLabelsNotReusedWithinADay: a number no unit shows any more is not
// given to another unit for a day, while stored snapshots may still show
// it (TestLabelNumberFreedAfter24h has the day after).
func TestLabelsNotReusedWithinADay(t *testing.T) {
	tr := newTracker(time.Second, false)

	first := []PodSnapshot{{Key: "#b"}, {Key: "#a"}}
	tr.label(first, labelT0)
	second := []PodSnapshot{{Key: "#c"}, {Key: "#a"}}
	tr.label(second, labelT0.Add(23*time.Hour))

	if got, want := labels(first), map[string]string{"#a": "instance-1", "#b": "instance-2"}; !maps.Equal(got, want) {
		t.Errorf("first tick labels = %v, want %v", got, want)
	}
	if got, want := labels(second), map[string]string{"#a": "instance-1", "#c": "instance-3"}; !maps.Equal(got, want) {
		t.Errorf("second tick labels = %v, want %v (instance-2 is not reused within a day)", got, want)
	}

	t.Run("static list numbers in URL order", func(t *testing.T) {
		_, srvs := fleettest.NewInstances(t, "x", "y", "z")
		urls := []string{srvs[0].URL, srvs[1].URL, srvs[2].URL}
		slices.Sort(urls)
		pool, tr := lbPoll(t, urls[2], urls[0], urls[1])

		s := pollOnce(context.Background(), pool, tr)

		want := map[string]string{urls[0]: "instance-1", urls[1]: "instance-2", urls[2]: "instance-3"}
		if got := labels(s.Pods); !maps.Equal(got, want) {
			t.Errorf("labels = %v, want %v", got, want)
		}
	})
	t.Run("kubernetes leaves labels alone", func(t *testing.T) {
		pods := []PodSnapshot{{Key: "#a", Label: "varmq-0"}, {Key: "http://h:1", Label: "varmq-1"}}
		newTracker(time.Second, true).label(pods, labelT0)
		if got, want := labels(pods), map[string]string{"#a": "varmq-0", "http://h:1": "varmq-1"}; !maps.Equal(got, want) {
			t.Errorf("labels = %v, want %v", got, want)
		}
	})
}

// TestLabelsStayWhileAnInstanceIsDown: a unit keeps its number, and its
// place in the list, while its process is down and once it is back.
func TestLabelsStayWhileAnInstanceIsDown(t *testing.T) {
	// static polls a static server over a, b and c, b down at the collector's
	// start when downAtStart, then up, down, down past the window and up
	// again, and checks that every tick labels and lists the units in URL
	// order.
	static := func(t *testing.T, downAtStart bool) {
		_, srvs := fleettest.NewInstances(t, "a", "b", "c")
		b, down := fleettest.NewDownableAddress(t, srvs[1])
		urls := []string{srvs[0].URL, b.URL, srvs[2].URL}
		pool, tr := lbPoll(t, urls...)
		want, order := fleettest.StaticLabels(urls...), slices.Sorted(slices.Values(urls))
		first := "b"
		if downAtStart {
			first = "" // never seen
		}
		start := time.Now()

		for _, step := range []struct {
			at       time.Duration
			down     bool
			instance string // of b's unit
		}{
			{0, downAtStart, first},
			{time.Second, false, "b"},
			{2 * time.Second, true, "b"}, // seen within the window
			{70 * time.Second, true, ""}, // last seen 69s ago
			{71 * time.Second, false, "b"},
		} {
			down.Store(step.down)
			s := pollAt(context.Background(), pool, tr, start.Add(step.at))

			if p := podByEndpoint(t, s, b.URL); (p.Err != "") != step.down || p.Instance != step.instance {
				t.Fatalf("at %v: b = %+v, want down %v as instance %q", step.at, p, step.down, step.instance)
			}
			if got := labels(s.Pods); !maps.Equal(got, want) {
				t.Errorf("at %v: labels = %v, want %v", step.at, got, want)
			}
			if keys := unitKeys(s); !slices.Equal(keys, order) {
				t.Errorf("at %v: units listed %v, want %v", step.at, keys, order)
			}
		}
	}
	t.Run("static, down at the start", func(t *testing.T) { static(t, true) })
	t.Run("static, up at the start", func(t *testing.T) { static(t, false) })

	t.Run("behind a balancer, across a missed tick", func(t *testing.T) {
		_, srvs := fleettest.NewInstances(t, "a", "b", "c")
		lb := fleettest.NewRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL)
		first := pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))

		srvs[1].Close() // the balancer now answers b's turns with a bare 502
		s := pollOnce(context.Background(), pool, tr)

		if b := podByEndpoint(t, s, "#b"); b.Err != "not reached this refresh" {
			t.Fatalf("#b Err = %q, want not reached this refresh", b.Err)
		}
		if got, want := labels(s.Pods), labels(first.Pods); !maps.Equal(got, want) {
			t.Errorf("labels = %v, want %v as before the miss", got, want)
		}
		if got, want := unitKeys(s), unitKeys(first); !slices.Equal(got, want) {
			t.Errorf("units listed %v, want %v as before the miss", got, want)
		}
	})
}

// TestLabelsOfDownUnits: a unit without an instance keeps the number its
// process had, unless that would give two units one number, and a process
// that comes back with a new instance ID gets a new number.
func TestLabelsOfDownUnits(t *testing.T) {
	type step struct {
		pods []PodSnapshot
		want map[string]string // unit key → label
	}
	run := func(t *testing.T, steps ...step) {
		tr := newTracker(time.Second, false)
		for i, s := range steps {
			tr.label(s.pods, labelT0)
			if got := labels(s.pods); !maps.Equal(got, s.want) {
				t.Errorf("step %d: labels = %v, want %v", i, got, s.want)
			}
		}
	}

	t.Run("a new process gets a new number", func(t *testing.T) {
		run(t,
			step{[]PodSnapshot{{Key: "http://x"}}, map[string]string{"http://x": "instance-1"}},                // down from the start
			step{[]PodSnapshot{{Key: "http://x", Instance: "b"}}, map[string]string{"http://x": "instance-1"}}, // b takes x's number
			step{[]PodSnapshot{{Key: "http://x"}}, map[string]string{"http://x": "instance-1"}},                // b down past the window
			step{[]PodSnapshot{{Key: "http://x", Instance: "b2"}}, map[string]string{"http://x": "instance-2"}},
			step{[]PodSnapshot{{Key: "http://x", Instance: "b2"}, {Key: "http://y", Instance: "c"}},
				map[string]string{"http://x": "instance-2", "http://y": "instance-3"}},
		)
		// x was down from the start while a answered through a balancer,
		// then showed a: x's own number is not handed to a's successor.
		run(t,
			step{[]PodSnapshot{{Key: "#a", Instance: "a", Balanced: true}, {Key: "http://x"}},
				map[string]string{"#a": "instance-1", "http://x": "instance-2"}},
			step{[]PodSnapshot{{Key: "http://x", Instance: "a"}}, map[string]string{"http://x": "instance-1"}},
			step{[]PodSnapshot{{Key: "http://x", Instance: "a2"}}, map[string]string{"http://x": "instance-3"}},
		)
	})
	t.Run("no two units share a number", func(t *testing.T) {
		// a's address is down past the window while a answers through a
		// balancer: the number stays with the process.
		run(t,
			step{[]PodSnapshot{{Key: "http://a", Instance: "a"}, {Key: "#b", Instance: "b", Balanced: true}},
				map[string]string{"#b": "instance-1", "http://a": "instance-2"}},
			step{[]PodSnapshot{{Key: "http://a"}, {Key: "#a", Instance: "a", Balanced: true}, {Key: "#b", Instance: "b", Balanced: true}},
				map[string]string{"#a": "instance-2", "#b": "instance-1", "http://a": "instance-3"}},
			step{[]PodSnapshot{{Key: "http://a"}, {Key: "#b", Instance: "b", Balanced: true}},
				map[string]string{"#b": "instance-1", "http://a": "instance-3"}},
		)
		// b answered at x, then at y while x was out of the pool; both down,
		// the first in key order keeps b's number.
		run(t,
			step{[]PodSnapshot{{Key: "http://x", Instance: "b"}}, map[string]string{"http://x": "instance-1"}},
			step{[]PodSnapshot{{Key: "http://y", Instance: "b"}}, map[string]string{"http://y": "instance-1"}},
			step{[]PodSnapshot{{Key: "http://x"}, {Key: "http://y"}}, map[string]string{"http://x": "instance-1", "http://y": "instance-2"}},
		)
	})
}

// numbersOf is the number of every unit key of pods, from its label.
func numbersOf(t *testing.T, pods []PodSnapshot) map[string]int {
	t.Helper()
	m := make(map[string]int, len(pods))
	for _, p := range pods {
		n, err := strconv.Atoi(strings.TrimPrefix(p.Label, "instance-"))
		if err != nil {
			t.Fatalf("%s: label %q is not instance-N", p.Key, p.Label)
		}
		m[p.Key] = n
	}
	return m
}

// TestLabelStateRoundTrip: a tracker loaded with another's label state
// numbers the same units as that one would, through hand-overs, down units
// and new ones.
func TestLabelStateRoundTrip(t *testing.T) {
	ticks := [][]PodSnapshot{
		{{Key: "http://x"}, {Key: "#a", Instance: "a", Balanced: true}},                                   // x down from the start
		{{Key: "http://x", Instance: "b"}, {Key: "#a", Instance: "a", Balanced: true}},                    // b takes x's number
		{{Key: "http://y", Instance: "b"}, {Key: "http://x"}, {Key: "#c", Instance: "c", Balanced: true}}, // b moved to y, x down
	}
	next := []PodSnapshot{
		{Key: "http://x", Instance: "b"}, {Key: "http://y"}, {Key: "#d", Instance: "d", Balanced: true}, {Key: "#a", Instance: "a", Balanced: true},
	}
	clone := func(pods []PodSnapshot) []PodSnapshot { return slices.Clone(pods) }

	tr := newTracker(time.Second, false)
	for i, pods := range ticks {
		tr.label(clone(pods), labelT0.Add(time.Duration(i)*time.Second))
	}
	state, changed := tr.labels()
	if !changed || len(state.Numbers) == 0 || len(state.Followed) == 0 {
		t.Fatalf("labels() = %+v, %v; want the numbers given, changed", state, changed)
	}

	loaded := newTracker(time.Second, false)
	loaded.loadLabels(state)
	if again, changed := loaded.labels(); changed || !reflect.DeepEqual(again, state) {
		t.Errorf("after loadLabels, labels() = %+v, %v; want the state loaded, unchanged", again, changed)
	}
	at := labelT0.Add(time.Minute)
	want, got := clone(next), clone(next)
	tr.label(want, at)
	loaded.label(got, at)
	if g, w := numbersOf(t, got), numbersOf(t, want); !maps.Equal(g, w) {
		t.Errorf("loaded tracker numbers %v, want %v as the tracker it was saved from", g, w)
	}

	// The state is a copy both ways: changing it changes no tracker.
	state.Numbers["#a"] = LabelNumber{N: 99}
	clear(state.Followed)
	for name, tr := range map[string]*tracker{"saved": tr, "loaded": loaded} {
		if again, _ := tr.labels(); again.Numbers["#a"].N == 99 || len(again.Followed) == 0 {
			t.Errorf("changing the state changed the %s tracker", name)
		}
	}
}

// TestLabelsChanged: labels() reports a change only when a number or what a
// unit follows changed, or a number was seen in a later minute.
func TestLabelsChanged(t *testing.T) {
	tr := newTracker(time.Second, false)
	if _, changed := tr.labels(); changed {
		t.Error("a new tracker's labels changed")
	}
	pods := func() []PodSnapshot { return []PodSnapshot{{Key: "#a", Instance: "a", Balanced: true}} }
	for _, step := range []struct {
		at      time.Time
		pods    []PodSnapshot
		changed bool
	}{
		{labelT0, pods(), true},                                                            // a new number
		{labelT0.Add(10 * time.Second), pods(), false},                                     // same minute, same units
		{labelT0.Add(time.Minute), pods(), true},                                           // seen in a later minute
		{labelT0.Add(time.Minute + time.Second), nil, false},                               // no unit: nothing seen
		{labelT0.Add(2 * time.Minute), append(pods(), PodSnapshot{Key: "http://x"}), true}, // a new unit
	} {
		tr.label(step.pods, step.at)
		state, changed := tr.labels()
		if changed != step.changed {
			t.Errorf("at %v: changed = %v, want %v", step.at, changed, step.changed)
		}
		for key, n := range state.Numbers {
			if n.Seen != n.Seen.Truncate(time.Minute) {
				t.Errorf("at %v: %s seen at %v, want a whole minute", step.at, key, n.Seen)
			}
		}
	}
	if _, changed := tr.labels(); changed {
		t.Error("labels() reported one change twice")
	}
}

// TestLabelNumberFreedAfter24h: a number no unit has shown for over a day
// is free, and a new unit takes the lowest free number.
func TestLabelNumberFreedAfter24h(t *testing.T) {
	now := labelT0
	state := LabelState{
		Numbers: map[string]LabelNumber{
			"#a": {N: 1, Seen: now.Add(-time.Hour).Truncate(time.Minute)},
			"#b": {N: 2, Seen: now.Add(-25 * time.Hour).Truncate(time.Minute)},
			"#c": {N: 3, Seen: now.Add(-time.Hour).Truncate(time.Minute)},
			"#e": {N: 4, Seen: now.Add(-23 * time.Hour).Truncate(time.Minute)},
		},
		Followed: map[string]string{"#a": "#a", "#b": "#b", "#c": "#c", "#e": "#e"},
	}
	tr := newTracker(time.Second, false)
	tr.loadLabels(state)

	pods := []PodSnapshot{
		{Key: "#a", Instance: "a", Balanced: true},
		{Key: "#c", Instance: "c", Balanced: true},
		{Key: "#d", Instance: "d", Balanced: true},
		{Key: "#f", Instance: "f", Balanced: true},
	}
	tr.label(pods, now)

	want := map[string]int{"#a": 1, "#c": 3, "#d": 2, "#f": 5}
	if got := numbersOf(t, pods); !maps.Equal(got, want) {
		t.Errorf("numbers = %v, want %v: b's 2 is free after 25h, e's 4 is not after 23h", got, want)
	}
	got, changed := tr.labels()
	if _, ok := got.Numbers["#b"]; ok || !changed {
		t.Errorf("labels() = %+v, %v; want #b's number gone, changed", got, changed)
	}
	if _, ok := got.Followed["#b"]; ok {
		t.Errorf("Followed still has #b, whose number was freed: %v", got.Followed)
	}
	if n := got.Numbers["#e"]; n.N != 4 || !n.Seen.Equal(state.Numbers["#e"].Seen) {
		t.Errorf("#e = %+v, want number 4 still seen 23h ago", n)
	}
}

// TestLiveNumberNotFreedAfterSleep: after the machine slept for longer than
// a day, the units still there keep their numbers; only the number of one
// that is gone is free.
func TestLiveNumberNotFreedAfterSleep(t *testing.T) {
	tr := newTracker(time.Second, false)
	before := []PodSnapshot{{Key: "http://x", Instance: "x"}, {Key: "http://y", Instance: "y"}, {Key: "http://z", Instance: "z"}}
	tr.label(before, labelT0)

	after := []PodSnapshot{{Key: "http://x", Instance: "x"}, {Key: "http://z", Instance: "z"}, {Key: "http://w", Instance: "w"}}
	tr.label(after, labelT0.Add(30*time.Hour))

	if got, want := numbersOf(t, before), map[string]int{"http://x": 1, "http://y": 2, "http://z": 3}; !maps.Equal(got, want) {
		t.Fatalf("before the sleep: numbers = %v, want %v", got, want)
	}
	if got, want := numbersOf(t, after), map[string]int{"http://x": 1, "http://z": 3, "http://w": 2}; !maps.Equal(got, want) {
		t.Errorf("after 30h asleep: numbers = %v, want %v: x and z keep theirs, w takes y's", got, want)
	}
}

func TestBalancerEndpointDown(t *testing.T) {
	// dialErr is the error a list request to the closed endpoint gets.
	dialErr := func(t *testing.T, endpoint string) string {
		_, _, err := api.NewClient(endpoint, "").ListWorkersFresh(context.Background())
		if err == nil {
			t.Fatalf("%s still answers", endpoint)
		}
		return err.Error()
	}

	t.Run("instances known", func(t *testing.T) {
		_, srvs := fleettest.NewInstances(t, "a", "b")
		lb := fleettest.NewRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL)
		pollUntil(t, pool, tr, 3, "units #a and #b", hasKeys("#a", "#b"))

		lb.Close()
		s := pollOnce(context.Background(), pool, tr)

		want := dialErr(t, lb.URL)
		if keys := unitKeys(s); !hasKeys("#a", "#b")(s) {
			t.Fatalf("units = %v, want the known #a and #b", keys)
		}
		for _, p := range s.Pods {
			if p.Err != want || !p.Balanced {
				t.Errorf("%s Err = %q, want the balancer's dial error %q", p.Key, p.Err, want)
			}
		}
		if want := map[string]int{lb.URL: 2}; !maps.Equal(s.Behind, want) {
			t.Errorf("Behind = %v, want %v: both were seen within the window", s.Behind, want)
		}
	})
	t.Run("none known", func(t *testing.T) {
		_, srvs := fleettest.NewInstances(t, "a", "b")
		lb := fleettest.NewRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL, srvs[0].URL, srvs[1].URL)
		// a and b are direct units, so nothing behind the balancer is a unit of its own.
		pollUntil(t, pool, tr, 3, "the balancer noticed", func(s *Snapshot) bool { return s.Behind[lb.URL] == 2 })
		for range 3 {
			// One list attempt goes through the balancer, finding one instance:
			// the note still counts both.
			if s := pollOnce(context.Background(), pool, tr); s.Behind[lb.URL] != 2 || len(s.Pods) != 2 {
				t.Fatalf("Behind = %v, units %v; want 2 behind and only the direct a and b", s.Behind, unitKeys(s))
			}
		}

		lb.Close()
		s := pollOnce(context.Background(), pool, tr)

		want := PodSnapshot{Key: lb.URL, Via: lb.URL, Balanced: true, Err: dialErr(t, lb.URL)}
		got := podByEndpoint(t, s, lb.URL)
		got.Label = ""
		if !reflect.DeepEqual(got, want) {
			t.Errorf("balancer unit = %+v\nwant %+v", got, want)
		}
		if len(s.Pods) != 3 {
			t.Errorf("units = %v, want the balancer and the direct a and b", unitKeys(s))
		}
	})
}
