package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// lbServer imitates a load balancer in front of n varmq instances: each
// request is answered by the next instance in turn.
func lbServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	var next atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(InstanceHeader, fmt.Sprintf("pod-%d", int(next.Add(1)-1)%n))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/workers" {
			_, _ = w.Write([]byte(`[{"name":"orders"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientDetectsLoadBalancer(t *testing.T) {
	srv := lbServer(t, 2)
	c := NewClient(srv.URL, "")

	workers, err := c.ListWorkers()
	if err != nil {
		t.Fatal(err)
	}
	if workers[0].Instance != "pod-0" {
		t.Fatalf("row instance = %q, want pod-0", workers[0].Instance)
	}
	if c.SeenInstances() != 1 {
		t.Fatalf("seen = %d after one request, want 1", c.SeenInstances())
	}

	if err := c.GetHealth(); err != nil {
		t.Fatal(err)
	}
	if c.SeenInstances() != 2 || c.LastInstance() != "pod-1" {
		t.Fatalf("seen = %d last = %q, want 2 / pod-1", c.SeenInstances(), c.LastInstance())
	}

	p := NewPool(&fakeSource{eps: []string{srv.URL}}, "")
	p.clients[srv.URL] = c
	if lb := p.LoadBalanced(); lb[srv.URL] != 2 {
		t.Fatalf("LoadBalanced = %v", lb)
	}
	if got := p.PodName(srv.URL); got != "pod-1" {
		t.Fatalf("PodName = %q, want last instance pod-1", got)
	}
}

func TestClientInstanceCapAndLegacyServer(t *testing.T) {
	srv := lbServer(t, 100)
	c := NewClient(srv.URL, "")
	for i := 0; i < 50; i++ {
		_ = c.GetHealth()
	}
	if c.SeenInstances() != maxSeenInstances {
		t.Fatalf("seen = %d, want cap %d", c.SeenInstances(), maxSeenInstances)
	}

	// A varmq without the header: no instance, no false load-balancer signal.
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer legacy.Close()
	lc := NewClient(legacy.URL, "")
	_ = lc.GetHealth()
	if lc.SeenInstances() != 0 || lc.LastInstance() != "" {
		t.Fatalf("legacy server reported instance %q", lc.LastInstance())
	}
	p := NewPool(&fakeSource{eps: []string{legacy.URL}}, "")
	if p.LoadBalanced() != nil {
		t.Fatal("legacy endpoint flagged as load-balanced")
	}
}
