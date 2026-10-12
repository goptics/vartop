package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/config"
)

const (
	dnsRefreshInterval = 5 * time.Second
	dnsLookupTimeout   = 3 * time.Second
)

// dnsSource resolves a hostname to every address it publishes and exposes one
// endpoint URL per address. Lookups run in the background so Endpoints never
// blocks (it is called during TUI render).
type dnsSource struct {
	host   string
	port   string
	path   string
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)

	mu   sync.RWMutex
	urls []string // last successful resolution, sorted

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewDNS returns a Source for the varmq instances behind ref.Host. It resolves
// once up front (failing fast on an unknown host or empty answer), then
// re-resolves every few seconds, keeping the last good set when a lookup fails.
func NewDNS(ref config.DNSRef) (Source, error) {
	d := newDNSSource(ref, net.DefaultResolver.LookupIPAddr)
	if err := d.refresh(); err != nil {
		return nil, fmt.Errorf("resolving %q: %w", ref.Host, err)
	}
	go d.run(dnsRefreshInterval)
	return d, nil
}

func newDNSSource(ref config.DNSRef, lookup func(context.Context, string) ([]net.IPAddr, error)) *dnsSource {
	path := ref.Path
	if path == "" {
		path = "/varmq"
	}
	return &dnsSource{
		host:   ref.Host,
		port:   strconv.Itoa(ref.Port),
		path:   path,
		lookup: lookup,
		stopCh: make(chan struct{}),
	}
}

// refresh resolves the host and replaces the endpoint set on success.
func (d *dnsSource) refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()

	addrs, err := d.lookup(ctx, d.host)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return errors.New("no addresses")
	}

	seen := make(map[string]struct{}, len(addrs))
	urls := make([]string, 0, len(addrs))
	for _, a := range addrs {
		u := "http://" + net.JoinHostPort(a.IP.String(), d.port) + d.path
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	sort.Strings(urls)

	d.mu.Lock()
	d.urls = urls
	d.mu.Unlock()
	return nil
}

// run re-resolves on every tick until Close. Failed lookups keep the previous
// set: a DNS blip should not empty the dashboard.
func (d *dnsSource) run(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			_ = d.refresh()
		}
	}
}

func (d *dnsSource) Endpoints() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.urls))
	copy(out, d.urls)
	return out
}

func (d *dnsSource) Close() error {
	d.stopOnce.Do(func() { close(d.stopCh) })
	return nil
}
