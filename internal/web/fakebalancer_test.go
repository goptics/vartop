package web

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goptics/vartop/internal/config"
)

// newRoundRobinLB starts a load balancer that sends each request to the next
// backend in turn, like an HTTP (L7) balancer. A backend that is down gets
// its turn anyway, answered with a bare 502 (no instance header). It is
// closed when the test ends.
func newRoundRobinLB(t *testing.T, backends ...*httptest.Server) *httptest.Server {
	t.Helper()
	var next atomic.Uint64
	return newFakeLB(t, backends, func(*http.Request) uint64 { return next.Add(1) - 1 })
}

// newPerConnLB starts a load balancer that picks the next backend once per
// client TCP connection and keeps it for every request on that connection,
// like kube-proxy. It is closed when the test ends.
func newPerConnLB(t *testing.T, backends ...*httptest.Server) *httptest.Server {
	t.Helper()
	var (
		mu    sync.Mutex
		next  uint64
		conns = make(map[string]uint64) // client RemoteAddr → backend
	)
	return newFakeLB(t, backends, func(r *http.Request) uint64 {
		mu.Lock()
		defer mu.Unlock()
		n, ok := conns[r.RemoteAddr]
		if !ok {
			n = next
			next++
			conns[r.RemoteAddr] = n
		}
		return n
	})
}

// newFakeLB proxies every request to backends[pick(r) % len(backends)],
// keeping its escaped path. It is closed when the test ends.
func newFakeLB(t *testing.T, backends []*httptest.Server, pick func(*http.Request) uint64) *httptest.Server {
	t.Helper()
	targets := make([]*url.URL, len(backends))
	for i, b := range backends {
		u, err := url.Parse(b.URL)
		if err != nil {
			t.Fatal(err)
		}
		targets[i] = u
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targets[pick(pr.In)%uint64(len(targets))])
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	lb := httptest.NewServer(proxy)
	t.Cleanup(lb.Close)
	return lb
}

// lbServer is a static server over the given endpoints, polled every second.
func lbServer(urls ...string) *config.Server {
	return &config.Server{Name: "lb", Interval: 1, Endpoints: urls}
}
