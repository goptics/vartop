package fleettest

import (
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goptics/vartop/internal/config"
)

// NewRoundRobinLB starts a load balancer that sends each request to the next
// backend in turn, like an HTTP (L7) balancer. A backend that is down gets
// its turn anyway, answered with a bare 502 (no instance header). It is
// closed when the test ends.
func NewRoundRobinLB(t *testing.T, backends ...*httptest.Server) *httptest.Server {
	t.Helper()
	var next atomic.Uint64
	return NewLB(t, backends, func(*http.Request) uint64 { return next.Add(1) - 1 })
}

// NewPerConnLB starts a load balancer that picks the next backend once per
// client TCP connection and keeps it for every request on that connection,
// like kube-proxy. It is closed when the test ends.
func NewPerConnLB(t *testing.T, backends ...*httptest.Server) *httptest.Server {
	t.Helper()
	var (
		mu    sync.Mutex
		next  uint64
		conns = make(map[string]uint64) // client RemoteAddr → backend
	)
	return NewLB(t, backends, func(r *http.Request) uint64 {
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

// NewRandomLB starts a load balancer that sends each request to a backend
// picked at random, like kube-proxy's iptables mode or a flow hash on new
// connections. It is closed when the test ends.
func NewRandomLB(t *testing.T, backends ...*httptest.Server) *httptest.Server {
	t.Helper()
	return NewLB(t, backends, func(*http.Request) uint64 { return rand.Uint64() })
}

// NewHangingLB starts a balancer in front of lb that passes every request on
// until hang is set, then holds each one unanswered until the client gives
// up, like a balancer whose backends blackhole. It is closed when the test
// ends.
func NewHangingLB(t *testing.T, lb *httptest.Server) (srv *httptest.Server, hang *atomic.Bool) {
	t.Helper()
	target, err := url.Parse(lb.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	hang = new(atomic.Bool)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			<-r.Context().Done()
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, hang
}

// NewDownableAddress starts an address in front of backend that passes every
// request on until down is set, then answers each one with a bare 502 (no
// instance header), like the address of a process that is down. It is
// closed when the test ends.
func NewDownableAddress(t *testing.T, backend *httptest.Server) (srv *httptest.Server, down *atomic.Bool) {
	t.Helper()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	down = new(atomic.Bool)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, down
}

// NewLB proxies every request to backends[pick(r) % len(backends)],
// keeping its escaped path. It is closed when the test ends.
func NewLB(t *testing.T, backends []*httptest.Server, pick func(*http.Request) uint64) *httptest.Server {
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

// LBServer is a static server over the given endpoints, polled every second.
func LBServer(urls ...string) *config.Server {
	return &config.Server{Name: "lb", Interval: 1, Endpoints: urls}
}
