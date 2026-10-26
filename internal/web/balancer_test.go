package web

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
)

// newFakes starts one fake varmq per instance ID, each with a running worker
// "w" whose Completed counter is 10 × its position (10, 20, 30, …).
func newFakes(t *testing.T, ids ...string) ([]*fakeVarmq, []*httptest.Server) {
	t.Helper()
	fakes := make([]*fakeVarmq, len(ids))
	srvs := make([]*httptest.Server, len(ids))
	for i, id := range ids {
		fakes[i], srvs[i] = newFakeVarmq(t, id, map[string]*fakeWorker{
			"w": {Status: "Running", Concurrency: 1, Completed: uint64(10 * (i + 1))},
		})
	}
	return fakes, srvs
}

// lbPoll returns a pool over lbServer(urls...) and a tracker for it, as
// NewSession would make them. The pool is closed when the test ends.
func lbPoll(t *testing.T, urls ...string) (*api.Pool, *tracker) {
	t.Helper()
	srv := lbServer(urls...)
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

// hasKeys is a pollUntil condition: the snapshot's units are exactly keys.
func hasKeys(keys ...string) func(*Snapshot) bool {
	return func(s *Snapshot) bool { return slices.Equal(unitKeys(s), keys) }
}

func TestAttemptCap(t *testing.T) {
	for k, want := range map[int]int{0: 6, 1: 6, 3: 18, 10: 66} {
		if got := attemptCap(k); got != want {
			t.Errorf("attemptCap(%d) = %d, want %d", k, got, want)
		}
	}
}

func TestReachStopsWhenAllAnswered(t *testing.T) {
	_, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)

	got, sent, _ := reach(context.Background(), staticPool(t, lb.URL).Clients(), []string{"a", "b", "c"}, 18, listFresh)

	if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, []string{"a", "b", "c"}) || sent != 3 {
		t.Fatalf("got %v after %d attempts, want a, b, c after 3", ids, sent)
	}
	for id, ans := range got {
		if ans.Err != nil || ans.Via != lb.URL || ans.At.IsZero() || len(ans.V) != 1 || ans.V[0].Instance != id {
			t.Errorf("%s: answer %+v, want its 2xx list via %s with an arrival time", id, ans, lb.URL)
		}
	}
}

func TestReachRespectsLimit(t *testing.T) {
	_, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)

	got, sent, _ := reach(context.Background(), staticPool(t, lb.URL).Clients(), []string{"a", "b", "c", "zz"}, 18, listFresh)

	if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, []string{"a", "b", "c"}) || sent != 18 {
		t.Errorf("got %v after %d attempts, want a, b, c (never zz) after the 18-attempt limit", ids, sent)
	}
}

func TestReachWithoutWantSendsOneRound(t *testing.T) {
	_, srvs := newFakes(t, "a", "b", "c")
	lb1, lb2 := newRoundRobinLB(t, srvs...), newRoundRobinLB(t, srvs...)

	got, sent, _ := reach(context.Background(), staticPool(t, lb1.URL, lb2.URL).Clients(), nil, 18, listFresh)

	if sent != 2 || len(got) == 0 {
		t.Errorf("got %d answers after %d attempts, want one attempt per endpoint", len(got), sent)
	}
}

// reach returns only after every attempt it started has returned, even when
// its context ends first.
func TestReachReturnsWithItsAttempts(t *testing.T) {
	var inFlight atomic.Int32
	slow := func(ctx context.Context, c *api.Client) (int, string, error) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // a request slow to wind down
		return 0, "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	got, sent, lastErr := reach(ctx, []*api.Client{api.NewClient("http://unused", "")}, []string{"a"}, 6, slow)

	if n := inFlight.Load(); n != 0 {
		t.Errorf("%d attempts still running after reach returned", n)
	}
	if len(got) != 0 || sent != 1 || lastErr == nil {
		t.Errorf("got %v, sent %d, lastErr %v; want no answer after the one attempt the deadline cut", got, sent, lastErr)
	}
}

func TestPollRoundRobinBalancer(t *testing.T) {
	_, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)
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
	_, srvs := newFakes(t, "a", "b", "c")
	lb := newPerConnLB(t, srvs...)
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
	fakes, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)
	pool, tr := staticPool(t, lb.URL), newTracker(interval, false)
	h := NewHistory(interval)

	h.Append(pollOnce(context.Background(), pool, tr))
	first := h.Latest()
	for i, f := range fakes {
		f.mu.Lock()
		f.Workers["w"].Completed += uint64(100 * (i + 1))
		f.mu.Unlock()
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
	fakes, srvs := newFakes(t, "a", "b", "c")
	fakes[2].Workers["w"].GetStatus = http.StatusInternalServerError
	lb := newRoundRobinLB(t, srvs...)
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
	fakes, srvs := newFakes(t, "a", "b", "c")
	fakes[2].ListStatus = http.StatusInternalServerError
	lb := newRoundRobinLB(t, srvs...)
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
	_, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))

	srvs[2].Close() // the balancer now answers c's turns with a bare 502
	s := pollOnce(context.Background(), pool, tr)

	if keys := unitKeys(s); !slices.Equal(keys, []string{"#a", "#b", "#c"}) {
		t.Fatalf("units = %v, want #c kept while in the window", keys)
	}
	c := podByEndpoint(t, s, "#c")
	if !strings.HasPrefix(c.Err, "not reached this refresh · last seen ") || !strings.HasSuffix(c.Err, " ago") ||
		!c.Balanced || c.Instance != "c" || c.Via != lb.URL {
		t.Errorf("#c = %+v, want balanced instance c via the balancer, not reached this refresh", c)
	}
	for _, key := range []string{"#a", "#b"} {
		if p := podByEndpoint(t, s, key); p.Err != "" || p.Workers["w"] == nil {
			t.Errorf("%s = %+v, want healthy", key, p)
		}
	}
	if s.Behind[lb.URL] != 2 {
		t.Errorf("Behind = %v, want the 2 instances that answered", s.Behind)
	}
}

func TestPollBalancerForgetsInstance(t *testing.T) {
	_, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)
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

	if keys := unitKeys(s); !slices.Equal(keys, []string{"#a", "#b"}) {
		t.Errorf("units = %v, want #c forgotten past the window", keys)
	}
}

func TestPollBalancerBackToDirect(t *testing.T) {
	_, srvs := newFakes(t, "a")
	lb := newRoundRobinLB(t, srvs...)
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
	_, srvs := newFakes(t, "a", "b")

	t.Run("two balancers", func(t *testing.T) {
		lb1, lb2 := newRoundRobinLB(t, srvs...), newRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb1.URL, lb2.URL)

		s := pollUntil(t, pool, tr, 3, "units #a and #b", hasKeys("#a", "#b"))

		if want := map[string]int{lb1.URL: 2, lb2.URL: 2}; !maps.Equal(s.Behind, want) {
			t.Errorf("Behind = %v, want %v", s.Behind, want)
		}
	})
	t.Run("balancer and own address", func(t *testing.T) {
		lb := newRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL, srvs[0].URL)

		s := pollUntil(t, pool, tr, 3, "units #b and a's address", hasKeys("#b", srvs[0].URL))

		if a := podByEndpoint(t, s, srvs[0].URL); a.Balanced || a.Instance != "a" || a.Err != "" {
			t.Errorf("a = %+v, want the healthy direct unit of instance a", a)
		}
	})
}

func TestPollBalancerIgnoresAnswersWithoutInstance(t *testing.T) {
	_, srvs := newFakes(t, "a", "b")
	target, err := url.Parse(newRoundRobinLB(t, srvs...).URL)
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
		if keys := unitKeys(s); !slices.Equal(keys, []string{"#a", "#b"}) {
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
	_, srvs := newFakes(t, "", "")
	lb := newRoundRobinLB(t, srvs...)
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
	fakes, srvs := newFakes(t, "a", "b", "c")
	lb := newRoundRobinLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL)
	pollUntil(t, pool, tr, 3, "three units", hasKeys("#a", "#b", "#c"))

	fakes[2].mu.Lock()
	fakes[2].Delay = 2 * time.Second
	fakes[2].mu.Unlock()
	const deadline = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	s := pollOnce(ctx, pool, tr)

	if elapsed := time.Since(start); elapsed > deadline+100*time.Millisecond {
		t.Errorf("pollOnce took %v, want it bounded by the %v deadline", elapsed, deadline)
	}
	if c := podByEndpoint(t, s, "#c"); c.Err == "" && len(c.Unreached) == 0 {
		t.Errorf("#c = %+v, want errored or its workers not reached", c)
	}
	for _, key := range []string{"#a", "#b"} {
		if p := podByEndpoint(t, s, key); p.Err != "" {
			t.Errorf("%s Err = %q, want healthy", key, p.Err)
		}
	}
}

func TestPollBalancerEscapesWorkerNames(t *testing.T) {
	workers := func() map[string]*fakeWorker {
		return map[string]*fakeWorker{"a/b": {Status: "Running"}, "x y": {Status: "Running"}}
	}
	fa, a := newFakeVarmq(t, "a", workers())
	fb, b := newFakeVarmq(t, "b", workers())
	lb := newRoundRobinLB(t, a, b)
	pool, tr := lbPoll(t, lb.URL)

	s := pollUntil(t, pool, tr, 3, "units #a and #b", hasKeys("#a", "#b"))

	// Not every unit is asserted to have both: concurrent per-name reaches
	// can fall into lockstep behind a round-robin balancer, each name handed
	// the same instance on every retry. Each name reaches some instance.
	sampled := map[string]bool{}
	for _, p := range s.Pods {
		for _, name := range sampleNames(p) {
			sampled[name] = true
		}
	}
	if got := slices.Sorted(maps.Keys(sampled)); !slices.Equal(got, []string{"a/b", "x y"}) {
		t.Errorf("sampled workers = %v, want both under their raw names", got)
	}
	calls := append(fa.calls(), fb.calls()...)
	if !slices.Contains(calls, "GET /workers/a%2Fb") || !slices.Contains(calls, "GET /workers/x%20y") ||
		slices.Contains(calls, "GET /workers/a/b") {
		t.Errorf("calls = %v, want the escaped names", calls)
	}
}

func TestLabelsNeverReused(t *testing.T) {
	labels := func(pods []PodSnapshot) map[string]string {
		m := make(map[string]string, len(pods))
		for _, p := range pods {
			m[p.Key] = p.Label
		}
		return m
	}
	tr := newTracker(time.Second, false)

	first := []PodSnapshot{{Key: "#b"}, {Key: "#a"}}
	tr.label(first)
	second := []PodSnapshot{{Key: "#c"}, {Key: "#a"}}
	tr.label(second)

	if got, want := labels(first), map[string]string{"#a": "instance-1", "#b": "instance-2"}; !maps.Equal(got, want) {
		t.Errorf("first tick labels = %v, want %v", got, want)
	}
	if got, want := labels(second), map[string]string{"#a": "instance-1", "#c": "instance-3"}; !maps.Equal(got, want) {
		t.Errorf("second tick labels = %v, want %v (instance-2 is never reused)", got, want)
	}

	t.Run("static list numbers in URL order", func(t *testing.T) {
		_, srvs := newFakes(t, "x", "y", "z")
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
		newTracker(time.Second, true).label(pods)
		if got, want := labels(pods), map[string]string{"#a": "varmq-0", "http://h:1": "varmq-1"}; !maps.Equal(got, want) {
			t.Errorf("labels = %v, want %v", got, want)
		}
	})
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
		_, srvs := newFakes(t, "a", "b")
		lb := newRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL)
		pollUntil(t, pool, tr, 3, "units #a and #b", hasKeys("#a", "#b"))

		lb.Close()
		s := pollOnce(context.Background(), pool, tr)

		want := dialErr(t, lb.URL)
		if keys := unitKeys(s); !slices.Equal(keys, []string{"#a", "#b"}) {
			t.Fatalf("units = %v, want the known #a and #b", keys)
		}
		for _, p := range s.Pods {
			if p.Err != want || !p.Balanced {
				t.Errorf("%s Err = %q, want the balancer's dial error %q", p.Key, p.Err, want)
			}
		}
		if len(s.Behind) != 0 {
			t.Errorf("Behind = %v, want none: no instance answered", s.Behind)
		}
	})
	t.Run("none known", func(t *testing.T) {
		_, srvs := newFakes(t, "a", "b")
		lb := newRoundRobinLB(t, srvs...)
		pool, tr := lbPoll(t, lb.URL, srvs[0].URL, srvs[1].URL)
		// a and b are direct units, so nothing behind the balancer is a unit of its own.
		pollUntil(t, pool, tr, 3, "the balancer noticed", func(s *Snapshot) bool { return s.Behind[lb.URL] == 2 })

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
