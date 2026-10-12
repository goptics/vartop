package api

import (
	"sync"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/discovery"
)

// Pool reconciles api.Clients against a discovery.Source, keeping one Client
// per endpoint URL served by the source.
type Pool struct {
	mu      sync.Mutex
	source  discovery.Source
	token   string
	clients map[string]*Client // endpoint URL -> client
}

// PoolFor builds a Pool over the discovery source matching the server
// configuration: Kubernetes EndpointSlices for kubernetes servers, DNS
// resolution for dns servers, a fixed list otherwise. Callers must Close the pool to stop any informer.
func PoolFor(server *config.Server) (*Pool, error) {
	if server.Kubernetes != nil {
		source, err := discovery.NewKubernetes(*server.Kubernetes)
		if err != nil {
			return nil, err
		}
		return NewPool(source, server.Token), nil
	}
	if server.DNS != nil {
		source, err := discovery.NewDNS(*server.DNS)
		if err != nil {
			return nil, err
		}
		return NewPool(source, server.Token), nil
	}
	return NewPool(discovery.NewStatic(server.GetEndpoints()), server.Token), nil
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
			c = NewClient(ep, p.token)
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
// source knows it (kubernetes), else the instance ID the endpoint reported
// (varmq InstanceHeader), else the endpoint's host.
func (p *Pool) PodName(endpoint string) string {
	if n, ok := p.source.(interface{ PodName(string) string }); ok {
		if name := n.PodName(endpoint); name != "" {
			return name
		}
	}
	p.mu.Lock()
	c := p.clients[endpoint]
	p.mu.Unlock()
	if c != nil {
		if id := c.LastInstance(); id != "" {
			return id
		}
	}
	return config.ShortOrigin(endpoint)
}

// LoadBalanced returns the endpoints that more than one varmq instance has
// answered, i.e. load balancers rather than single pods, with the number of
// instances seen behind each.
func (p *Pool) LoadBalanced() map[string]int {
	var lb map[string]int
	for _, c := range p.Clients() {
		if n := c.SeenInstances(); n > 1 {
			if lb == nil {
				lb = make(map[string]int)
			}
			lb[c.PrimaryEndpoint()] = n
		}
	}
	return lb
}

// Close delegates to the underlying discovery source.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.source.Close()
}
