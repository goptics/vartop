// Package discovery provides live sources of ready varmq endpoint URLs,
// either a static list or Kubernetes EndpointSlices for a service.
package discovery

import (
	"sort"
	"sync"
)

// Source is a live set of ready endpoint URLs, safe for concurrent use.
type Source interface {
	Endpoints() []string // sorted snapshot of current ready endpoints
	Close() error        // static: no-op nil; kubernetes: stops informer
}

// registry tracks ready endpoint URLs with a mutex-guarded map.
type registry struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

func newRegistry() *registry {
	return &registry{values: make(map[string]struct{})}
}

func (r *registry) add(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[url] = struct{}{}
}

func (r *registry) remove(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, url)
}

// Endpoints returns a sorted snapshot of the current endpoint URLs.
func (r *registry) Endpoints() []string {
	r.mu.RLock()
	urls := make([]string, 0, len(r.values))
	for u := range r.values {
		urls = append(urls, u)
	}
	r.mu.RUnlock()

	sort.Strings(urls)
	return urls
}
