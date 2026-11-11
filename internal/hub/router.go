package hub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/discovery"
)

// Options configure a Router.
type Options struct {
	// Heartbeat is the ping and beat period: DefaultHeartbeat when 0,
	// clamped to MinHeartbeat..MaxHeartbeat.
	Heartbeat time.Duration
	// MaxInstances caps connected instances per hub server:
	// DefaultMaxInstances when 0.
	MaxInstances int
	Log          *slog.Logger // discarded when nil
}

type tokenKey [sha256.Size]byte

func keyOf(token string) tokenKey { return sha256.Sum256([]byte(token)) }

// Router serves every hub server on one listener. A request's bearer
// token picks its server; tokens are unique across hub servers.
type Router struct {
	heartbeat    time.Duration
	maxInstances int
	log          *slog.Logger
	mux          *http.ServeMux

	mu     sync.RWMutex
	hubs   map[tokenKey]*Hub
	closed bool
}

// NewRouter returns a Router serving no hub server until Sync.
func NewRouter(o Options) *Router {
	hb := o.Heartbeat
	if hb == 0 {
		hb = DefaultHeartbeat
	}
	return newRouter(min(max(hb, MinHeartbeat), MaxHeartbeat), o.MaxInstances, o.Log)
}

// newRouter is NewRouter with heartbeat unclamped (tests).
func newRouter(heartbeat time.Duration, maxInstances int, log *slog.Logger) *Router {
	if maxInstances <= 0 {
		maxInstances = DefaultMaxInstances
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Router{heartbeat: heartbeat, maxInstances: maxInstances, log: log, hubs: make(map[tokenKey]*Hub)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hub/stream", r.authed((*Hub).stream))
	mux.HandleFunc("POST /hub/reply/{id}", r.authed((*Hub).replyTo))
	mux.HandleFunc("POST /hub/beat", r.authed((*Hub).beatFrom))
	r.mux = mux
	return r
}

// Sync makes the hub servers of cfg the ones served, keyed by token. A
// new token gets a new Hub; a token no longer configured (server removed,
// token changed) closes its Hub and drops its instances. Servers whose
// token Validate refuses are skipped.
func (r *Router) Sync(cfg *config.Config) {
	want := make(map[tokenKey]string)
	if cfg != nil {
		for _, name := range slices.Sorted(maps.Keys(cfg.Servers)) {
			srv := cfg.Servers[name]
			if srv == nil || !srv.Hub || config.ValidHubToken(srv.Token) != nil {
				continue
			}
			k := keyOf(srv.Token)
			if _, dup := want[k]; dup {
				r.log.Error("hub: two hub servers share a token; serving the first", "server", name, "first", want[k])
				continue
			}
			want[k] = name
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for k, h := range r.hubs {
		if name, ok := want[k]; ok {
			h.name.Store(&name)
			continue
		}
		delete(r.hubs, k)
		h.close()
		r.log.Info("hub: server no longer served; its instances dropped", "server", h.label())
	}
	for k, name := range want {
		if r.hubs[k] == nil {
			r.hubs[k] = newHub(name, r.heartbeat, r.maxInstances, r.log)
			r.log.Info("hub: serving server", "server", name)
		}
	}
}

// Servers is the names of the hub servers served, sorted.
func (r *Router) Servers() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, h := range r.hubs {
		names = append(names, h.label())
	}
	slices.Sort(names)
	return names
}

// hub is the Hub of token, nil when none.
func (r *Router) hub(k tokenKey) *Hub {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hubs[k]
}

// auth picks the Hub of the request's bearer token, comparing against
// every hub's token in constant time. The token is read from the
// Authorization header only.
func (r *Router) auth(req *http.Request) *Hub {
	scheme, token, ok := strings.Cut(req.Header.Get("Authorization"), " ")
	ok = ok && strings.EqualFold(scheme, "Bearer") && token != ""
	k := keyOf(token)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var found *Hub
	for hk, h := range r.hubs {
		if subtle.ConstantTimeCompare(k[:], hk[:]) == 1 {
			found = h
		}
	}
	if !ok {
		return nil
	}
	return found
}

// authed checks the token before anything else of the request is read.
func (r *Router) authed(serve func(*Hub, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		h := r.auth(req)
		if h == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vartop hub"`)
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		serve(h, w, req)
	}
}

// ServeHTTP serves the hub routes of protocol 1.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

// Close drops every instance and serves no hub server from then on. Call
// it before http.Server.Shutdown: open streams would hold that up.
func (r *Router) Close() {
	r.mu.Lock()
	r.closed = true
	hubs := r.hubs
	r.hubs = make(map[tokenKey]*Hub)
	r.mu.Unlock()
	for _, h := range hubs {
		h.close()
	}
}

// Transport is the RoundTripper of the hub server with token: it finds
// the server's Hub on every request, so it follows Sync. Without a Hub
// (nil Router: no --hub-listen; or the token not served) it fails.
func (r *Router) Transport(token string) http.RoundTripper {
	if r == nil {
		return off{}
	}
	return transport{r: r, k: keyOf(token)}
}

type transport struct {
	r *Router
	k tokenKey
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if h := t.r.hub(t.k); h != nil {
		return h.RoundTrip(req)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, fmt.Errorf("%w: its hub server is not served", ErrNotConnected)
}

// off is the Transport of a nil Router.
type off struct{}

func (off) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, ErrHubOff
}

// source is the discovery.Source of the hub server with token: its
// connected instances. Close is a no-op: the Router owns the Hub.
type source struct {
	r *Router
	k tokenKey
}

func (s source) Endpoints() []string {
	if h := s.r.hub(s.k); h != nil {
		return h.Endpoints()
	}
	return nil
}

func (s source) Close() error { return nil }

// Source is the discovery.Source of the hub server with token.
func (r *Router) Source(token string) discovery.Source { return source{r: r, k: keyOf(token)} }

// BuildPool makes srv's pool: over its connected instances, through the
// hub, for a hub server; api.PoolFor otherwise. A nil Router fails for a
// hub server with ErrHubOff.
func (r *Router) BuildPool(srv *config.Server) (*api.Pool, error) {
	if !srv.Hub {
		return api.PoolFor(srv)
	}
	if r == nil {
		return nil, fmt.Errorf("hub server %q: %w", srv.Name, ErrHubOff)
	}
	// No token on the clients: frames carry no headers anyway.
	return api.NewPoolVia(r.Source(srv.Token), "", r.Transport(srv.Token)), nil
}

// LoadCert loads the hub listener's TLS certificate.
func LoadCert(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("hub TLS: %w", err)
	}
	return &cert, nil
}

// NewServer is the hub listener's http.Server: bounded headers, a
// ReadHeaderTimeout, and no server-wide read or write timeout, which would
// cut the long-lived stream (handlers set their own deadlines).
func NewServer(r *Router, cert *tls.Certificate, log *slog.Logger) *http.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// Handshake noise from scanners stays out of the log.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	if cert != nil {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}
	}
	return srv
}

// Serve serves r on ln (TLS with cert) until ctx ends, then drops every
// instance and shuts down within timeout.
func Serve(ctx context.Context, ln net.Listener, r *Router, cert *tls.Certificate, timeout time.Duration, log *slog.Logger) error {
	srv := NewServer(r, cert, log)
	errc := make(chan error, 1)
	go func() {
		if cert != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		r.Close()
		return err
	case <-ctx.Done():
	}
	r.Close()
	shut, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(shut)
	if err != nil {
		_ = srv.Close()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}
