// Package discovery provides live sources of ready varmq endpoint URLs,
// either a static list or the addresses a DNS name resolves to.
package discovery

// Source is a live set of ready endpoint URLs, safe for concurrent use.
type Source interface {
	Endpoints() []string // sorted snapshot of current ready endpoints
	Close() error        // no-op nil for static and dns
}
