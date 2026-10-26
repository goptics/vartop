package fleet

import (
	"context"
	"slices"
	"testing"

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
