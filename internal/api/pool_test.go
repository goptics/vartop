package api

import (
	"sync"
	"testing"
)

// fakeSource is a mutable discovery.Source stub for pool tests.
type fakeSource struct {
	mu     sync.Mutex
	eps    []string
	closed bool
}

func (f *fakeSource) set(eps []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eps = eps
}

func (f *fakeSource) Endpoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.eps...)
}

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// sameClients reports whether a and b hold the same client instances in order.
func sameClients(a, b []*Client) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func byEndpoint(clients []*Client) map[string]*Client {
	m := make(map[string]*Client, len(clients))
	for _, c := range clients {
		m[c.PrimaryEndpoint()] = c
	}
	return m
}

func TestPoolReconcilesClients(t *testing.T) {
	src := &fakeSource{eps: []string{"http://a:8080/varmq", "http://b:8080/varmq"}}
	pool := NewPool(src, "tok")

	first := byEndpoint(pool.Clients())
	if len(first) != 2 {
		t.Fatalf("got %d clients, want 2", len(first))
	}
	if first["http://a:8080/varmq"].token != "tok" {
		t.Error("client did not receive the pool token")
	}

	// New endpoints get clients, removed ones are dropped, shared ones keep identity.
	src.set([]string{"http://b:8080/varmq", "http://c:8080/varmq"})
	second := byEndpoint(pool.Clients())
	if len(second) != 2 {
		t.Fatalf("got %d clients, want 2", len(second))
	}
	if _, ok := second["http://a:8080/varmq"]; ok {
		t.Error("removed endpoint still has a client")
	}
	if second["http://b:8080/varmq"] != first["http://b:8080/varmq"] {
		t.Error("unchanged endpoint got a new client instance")
	}
	if second["http://c:8080/varmq"] == nil {
		t.Error("new endpoint missing a client")
	}

	// Unchanged snapshot returns the same instances.
	if again := pool.Clients(); !sameClients(again, [](*Client){second["http://b:8080/varmq"], second["http://c:8080/varmq"]}) {
		t.Error("repeated call with unchanged snapshot returned different clients")
	}
}

func TestPoolEmptySnapshot(t *testing.T) {
	src := &fakeSource{eps: []string{"http://a:8080/varmq"}}
	pool := NewPool(src, "")

	if got := pool.Clients(); len(got) != 1 {
		t.Fatalf("got %d clients, want 1", len(got))
	}

	src.set(nil)
	if got := pool.Clients(); len(got) != 0 {
		t.Fatalf("got %d clients, want 0", len(got))
	}

	// And back again after endpoints reappear.
	src.set([]string{"http://a:8080/varmq", "http://b:8080/varmq"})
	if got := pool.Clients(); len(got) != 2 {
		t.Fatalf("got %d clients, want 2", len(got))
	}
}

func TestPoolCloseDelegates(t *testing.T) {
	src := &fakeSource{}
	pool := NewPool(src, "")

	if err := pool.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if !src.closed {
		t.Error("source.Close was not called")
	}
}
