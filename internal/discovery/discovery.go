// Package discovery provides live sources of ready varmq endpoint URLs,
// either a static list or Kubernetes EndpointSlices for a service.
package discovery

// Source is a live set of ready endpoint URLs, safe for concurrent use.
type Source interface {
	Endpoints() []string // sorted snapshot of current ready endpoints
	Close() error        // static: no-op nil; kubernetes: stops informer
}
