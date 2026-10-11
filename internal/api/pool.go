package api

import (
	"sync"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
	"github.com/goptics/varmq/cmd/vartop/internal/discovery"
)

// Pool reconciles api.Clients against a discovery.Source, keeping one Client
// per endpoint URL served by the source.
type Pool struct {
	mu      sync.Mutex
	source  discovery.Source
	token   string
	clients map[string]*Client // endpoint URL -> client
}

// NewPool returns a Pool backed by the given endpoint source.
func NewPool(source discovery.Source, token string) *Pool {
	return &Pool{
		source:  source,
		token:   token,
		clients: make(map[string]*Client),
	}
}

// Clients reconciles against the current source snapshot: creates clients for
// new endpoints, drops removed ones, and reuses instances for unchanged URLs.
// Repeated calls with an unchanged snapshot return the same client instances.
func (p *Pool) Clients() []*Client {
	p.mu.Lock()
	defer p.mu.Unlock()

	endpoints := p.source.Endpoints()
	clients := make([]*Client, 0, len(endpoints))
	next := make(map[string]struct{}, len(endpoints))

	for _, ep := range endpoints {
		if _, dup := next[ep]; dup {
			continue // stale static lists may repeat a URL; one client per URL
		}
		next[ep] = struct{}{}
		c := p.clients[ep]
		if c == nil {
			c = NewClient(&config.Server{Endpoints: []string{ep}, Token: p.token})
			p.clients[ep] = c
		}
		clients = append(clients, c)
	}

	if len(next) < len(p.clients) {
		for ep := range p.clients {
			if _, ok := next[ep]; !ok {
				delete(p.clients, ep)
			}
		}
	}

	return clients
}

// PodName returns a display name for a pod endpoint: the pod name when the
// source knows it (kubernetes), otherwise the endpoint's host:port.
func (p *Pool) PodName(endpoint string) string {
	if n, ok := p.source.(interface{ PodName(string) string }); ok {
		if name := n.PodName(endpoint); name != "" {
			return name
		}
	}
	return config.ShortOrigin(endpoint)
}

// Close delegates to the underlying discovery source.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.source.Close()
}
