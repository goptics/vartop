// Package fleettest has fakes for the tests of fleet and of the packages
// built on it: an in-memory varmq REST API, load balancers in front of
// several of them, and helpers to poll them.
package fleettest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// StaticPool is a pool over fixed endpoints, closed when the test ends.
func StaticPool(t *testing.T, endpoints ...string) *api.Pool {
	t.Helper()
	pool, err := api.PoolFor(&config.Server{Interval: 1, Endpoints: endpoints})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// WaitUntil polls cond every 10ms and fails the test if it is still false
// after timeout.
func WaitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// StaticLabels is how a collector first labels the units of a non-Kubernetes
// server over the endpoints urls: instance-1, instance-2, … in URL order.
func StaticLabels(urls ...string) map[string]string {
	labels := make(map[string]string, len(urls))
	for i, u := range slices.Sorted(slices.Values(urls)) {
		labels[u] = fmt.Sprintf("instance-%d", i+1)
	}
	return labels
}
