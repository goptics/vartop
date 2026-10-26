package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// TestActionThroughRandomBalancerReachesItsInstance: behind a balancer that
// picks at random among a, b and c, where b and c are also listed by their
// own addresses, a merged action reaches a. Its budget counts all three
// instances, which makes a miss about 1 in 1500 (it was 1 in 11 when only
// the wanted unit counted), so 100 actions allow 2.
func TestActionThroughRandomBalancerReachesItsInstance(t *testing.T) {
	_, srvs := fleettest.NewInstances(t, "a", "b", "c")
	lb := fleettest.NewRandomLB(t, srvs...)
	pool, tr := lbPoll(t, lb.URL, srvs[1].URL, srvs[2].URL)
	latest := pollUntil(t, pool, tr, 30, "#a and 3 instances behind the balancer", func(s *Snapshot) bool {
		return slices.Contains(unitKeys(s), "#a") && s.Behind[lb.URL] == 3
	})
	sc, err := ScopeOf(pool.Clients(), latest, "", instanceNoun)
	if err != nil {
		t.Fatal(err)
	}

	misses := 0
	for range 100 {
		for _, res := range Act(context.Background(), sc, UnitsOf(latest), instanceNoun, []string{"w"}, ActionSender("pause"))[0] {
			if res.Key == "#a" && res.Outcome != "ok" {
				misses++
			}
		}
	}
	if misses > 2 {
		t.Errorf("a was not reached by %d of 100 actions, want at most 2", misses)
	}
}

// TestActCarriesPanic: a send that panics on a goroutine Act fans out to is
// raised again on Act's caller, once every send has returned, with the
// stack it was raised on: an HTTP handler's recovery then gets it, instead
// of the process ending.
func TestActCarriesPanic(t *testing.T) {
	_, a := fleettest.NewVarmq(t, "a", nil)
	_, b := fleettest.NewVarmq(t, "b", nil)
	sc := ActScope{Direct: fleettest.StaticPool(t, a.URL, b.URL).Clients()}
	var finished atomic.Int32
	send := Sender{Direct: func(_ context.Context, c *api.Client, _ string) error {
		if c.PrimaryEndpoint() == a.URL {
			panic("reply broke")
		}
		time.Sleep(50 * time.Millisecond)
		finished.Add(1)
		return nil
	}}

	v := raised(func() { Act(context.Background(), sc, nil, instanceNoun, []string{"w", "v"}, send) })
	p, ok := v.(*TickPanic)
	if !ok || p.Value != "reply broke" || !strings.Contains(string(p.Stack), "TestActCarriesPanic") {
		t.Fatalf("Act raised %v, want the send's panic with the stack it was raised on", v)
	}
	if n := finished.Load(); n != 2 {
		t.Errorf("%d sends finished before Act raised, want the other 2", n)
	}
}

// TestTickPanicError: a TickPanic printed shows what panicked and where.
func TestTickPanicError(t *testing.T) {
	p := &TickPanic{Value: "reply broke", Stack: []byte("goroutine 7 [running]:\nfleet.fanOut()")}
	var err error = p
	for _, want := range []string{"reply broke", "goroutine 7 [running]:\nfleet.fanOut()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, lacks %q", err.Error(), want)
		}
	}
}

// TestClientsFor: an action's clients come from the latest snapshot: one
// per direct unit, at its key, and one per balancer endpoint, whether
// units were reached through it or it only counts instances behind it;
// sorted, each once, carrying the token.
func TestClientsFor(t *testing.T) {
	var auth atomic.Value
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(direct.Close)
	latest := &Snapshot{
		Pods: []PodSnapshot{
			{Key: direct.URL, Via: direct.URL, Label: "instance-1"},
			{Key: "#a", Via: "http://lb1", Instance: "a", Label: "instance-2", Balanced: true},
			{Key: "#b", Via: "http://lb1", Instance: "b", Label: "instance-3", Balanced: true},
			{Key: "http://lb2", Via: "http://lb2", Label: "instance-4", Balanced: true, Err: "no instance answered"},
			{Key: "http://c", Via: "http://c", Label: "instance-5", Err: "connection refused"},
		},
		Behind: map[string]int{"http://lb1": 2, "http://lb2": 0, "http://lb3": 1},
	}

	clients := ClientsFor(latest, "tok")
	var got []string
	for _, c := range clients {
		got = append(got, c.PrimaryEndpoint())
	}
	want := slices.Sorted(slices.Values([]string{direct.URL, "http://c", "http://lb1", "http://lb2", "http://lb3"}))
	if !slices.Equal(got, want) {
		t.Fatalf("ClientsFor = %q, want %q", got, want)
	}
	if _, err := clients[slices.Index(got, direct.URL)].ListWorkersCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := auth.Load(); a != "Bearer tok" {
		t.Errorf("Authorization = %v, want the token", a)
	}
	if c := ClientsFor(nil, "tok"); len(c) != 0 {
		t.Errorf("ClientsFor(nil) = %d clients, want none", len(c))
	}
}
