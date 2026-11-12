package hub

import (
	"container/list"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	// MaxConnsPerIP caps the hub listener's tokenless connections from
	// one source IP: 0 is no limit. vartop web's --hub-max-conns-per-ip
	// sets it.
	MaxConnsPerIP int
	Log           *slog.Logger // discarded when nil
}

type tokenKey [sha256.Size]byte

func keyOf(token string) tokenKey { return sha256.Sum256([]byte(token)) }

// Router serves every hub server on one listener. A request's bearer
// token picks its server; tokens are unique across hub servers.
type Router struct {
	heartbeat    time.Duration
	maxInstances int
	perIP        int          // Options.MaxConnsPerIP
	connCap      atomic.Int64 // the listener's connection cap; Sync sets it
	lim          *limits
	log          *slog.Logger
	full         rateLog // share refusals (see authed)
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
	r := newRouter(min(max(hb, MinHeartbeat), MaxHeartbeat), o.MaxInstances, o.Log)
	r.perIP = max(o.MaxConnsPerIP, 0)
	return r
}

// newRouter is NewRouter with heartbeat unclamped (tests).
func newRouter(heartbeat time.Duration, maxInstances int, log *slog.Logger) *Router {
	if maxInstances <= 0 {
		maxInstances = DefaultMaxInstances
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Router{heartbeat: heartbeat, maxInstances: maxInstances, lim: defaultLimits(), log: log, hubs: make(map[tokenKey]*Hub)}
	r.setCap()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hub/stream", r.authed((*Hub).stream))
	mux.HandleFunc("POST /hub/reply/{id}", r.authed((*Hub).replyTo))
	mux.HandleFunc("POST /hub/beat", r.authed((*Hub).beatFrom))
	r.mux = mux
	return r
}

// Sync makes the hub servers of cfg the ones served, keyed by token. A
// new token gets a new Hub; a token no longer configured (server removed,
// token changed) closes its Hub and drops its instances. cfg is one
// config.Load accepted: tokens valid and distinct.
func (r *Router) Sync(cfg *config.Config) {
	want := make(map[tokenKey]string)
	if cfg != nil {
		for name, srv := range cfg.Servers {
			if srv != nil && srv.Hub {
				want[keyOf(srv.Token)] = name
			}
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
			r.hubs[k] = newHub(name, r.heartbeat, r.maxInstances, r.lim, r.log)
			r.log.Info("hub: serving server", "server", name)
		}
	}
	r.setCap()
}

// setCap sizes the listener's connection cap to the hub servers served,
// under r.mu or before r is shared: maxInstances each (see
// connsPerInstance), at least one server's worth.
func (r *Router) setCap() {
	r.connCap.Store(int64(connsPerInstance*r.maxInstances*max(len(r.hubs), 1) + connHeadroom))
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
// A valid token does not authenticate the connection: a request accepted
// for a live session does (see live). Until then the request holds a
// place in its Hub's share of authenticated connections; past the share
// the connection is cut, as the listener cuts one past its cap.
func (r *Router) authed(serve func(*Hub, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		h := r.auth(req)
		if h == nil {
			// Close: the server then drops the connection at once rather
			// than read an unread body first.
			w.Header().Set("Connection", "close")
			w.Header().Set("WWW-Authenticate", `Bearer realm="vartop hub"`)
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		st, _ := req.Context().Value(connKey{}).(*connState)
		if st == nil || st.authed.Load() {
			serve(h, w, req) // a proxy's connection may carry several servers'
			return
		}
		if st.c != nil && !st.c.l.reserve(h) {
			r.full.warn(r.log, "hub: server's share of connections full; connection refused", "server", h.label())
			panic(http.ErrAbortHandler)
		}
		g := &grant{st: st, h: h}
		defer g.settle(false)
		// Not reused unless authenticated: it would die at the timeout
		// under a client's next request.
		w.Header().Set("Connection", "close")
		serve(h, w, req.WithContext(context.WithValue(req.Context(), grantKey{}, g)))
	}
}

// grantKey keys a request's context to its *grant.
type grantKey struct{}

// grant is a request's place in h's share, taken while its connection is
// not authenticated.
type grant struct {
	st   *connState
	h    *Hub
	once sync.Once
}

// settle, on its first call: keep (the request was accepted for a live
// session) authenticates the connection for g.h, holding the place for
// it; otherwise, or when the connection already was, the place goes back.
func (g *grant) settle(keep bool) {
	g.once.Do(func() {
		if keep {
			g.st.authed.Store(true)
		}
		if g.st.c != nil {
			g.st.c.l.settle(g.st.c, g.h, keep)
		}
	})
}

// live: r was accepted for a live session (a stream registered, a beat
// or reply of its session taken); its connection is authenticated. Call
// it before the success status is written.
func live(w http.ResponseWriter, r *http.Request) {
	if g, ok := r.Context().Value(grantKey{}).(*grant); ok {
		w.Header().Del("Connection")
		g.settle(true)
	}
}

// ServeHTTP serves the hub routes of protocol 1. Every request must be
// read, body included, within the request timeout; only an authenticated
// stream clears it. So no client, with or without a token, holds a
// connection by sending a body slowly.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(r.lim.reqTimeout))
	r.mux.ServeHTTP(w, req)
}

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

// connKey keys a connection's context to its *connState.
type connKey struct{}

// connState is one hub connection's: authenticated or not, and its slot
// of the capListener, nil when Serve did not cap the listener.
type connState struct {
	authed atomic.Bool // a request on it was accepted for a live session
	c      *capConn
}

// NewServer is the hub listener's http.Server: bounded headers, a
// ReadHeaderTimeout, and no server-wide read or write timeout, which would
// cut the long-lived stream (handlers set their own deadlines). A
// connection that carried no request accepted for a live session within
// the request timeout is closed: a client without the token, or with the
// token alone, holds no connection, idle HTTP/2 included, for longer.
func NewServer(r *Router, cert *tls.Certificate, log *slog.Logger) *http.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          stdlog.New(serverLog{log}, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tls.Conn); ok {
				c = tc.NetConn() // close the socket, no TLS alert to wait on
			}
			st := new(connState)
			st.c, _ = c.(*capConn)
			time.AfterFunc(r.lim.reqTimeout, func() {
				if !st.authed.Load() {
					_ = c.Close()
				}
			})
			return context.WithValue(ctx, connKey{}, st)
		},
	}
	if cert != nil {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}
	}
	return srv
}

// serverLog passes the http.Server's errors to log as warnings, but for
// TLS handshake failures: scanner noise on a public port.
type serverLog struct{ log *slog.Logger }

func (l serverLog) Write(p []byte) (int, error) {
	if msg := strings.TrimSpace(string(p)); !strings.Contains(msg, "TLS handshake error") {
		l.log.Warn("hub: " + msg)
	}
	return len(p), nil
}

// connsPerInstance and connHeadroom size the hub listener's connection
// cap, shared by every hub server: over HTTP/1.1 an instance holds its
// stream, a connection per reply in flight (MaxInFlight) and one for
// beats; the headroom serves new streams and scanners.
// ponytail: 10 per instance and 256 spare; raise connsPerInstance if
// instances keep more connections open (VarMQ keeps up to 10 idle).
const (
	connsPerInstance = 10
	connHeadroom     = 256
	// DefaultMaxConnsPerIP is --hub-max-conns-per-ip's default.
	// ponytail: 64 tokenless connections per source IP; authenticated
	// ones do not count.
	DefaultMaxConnsPerIP = 64
)

// capListener accepts at most *max connections at once. Of one source IP
// it holds at most perIP (0: no limit) tokenless connections: one more
// closes the IP's oldest tokenless one, so a tokenless client cannot lock
// out the instances of its address. A connection past *max is closed at
// once, so a flood of idle connections cannot pile up.
type capListener struct {
	net.Listener
	max     *atomic.Int64
	perIP   int
	share   int64        // authenticated connections per Hub; 0: no limit
	log     *slog.Logger // nil: discarded
	evicted rateLog
	refused rateLog

	mu    sync.Mutex
	n     int64
	byIP  map[string]*list.List // tokenless capConns of each IP, oldest first
	byHub map[*Hub]int64        // authenticated capConns and grants of each Hub
}

func (l *capListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := c.RemoteAddr().String()
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
		cc := &capConn{Conn: c, l: l, ip: ip}
		ok, old := l.admit(cc)
		if old != nil {
			_ = old.Close()
			l.evicted.warn(l.log, "hub: tokenless connections of one IP at the limit; its oldest closed", "ip", ip)
		}
		if ok {
			return cc, nil
		}
		_ = c.Close()
		l.refused.warn(l.log, "hub: connection limit reached; connection refused", "ip", ip)
	}
}

// admit takes a slot for c; false when none is free. At the limit of c's
// IP it takes the slot of the IP's oldest tokenless connection, old, for
// the caller to close.
func (l *capListener) admit(c *capConn) (ok bool, old *capConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.byIP[c.ip]
	switch {
	case l.perIP > 0 && q != nil && q.Len() >= l.perIP:
		old = q.Remove(q.Front()).(*capConn)
		old.elem = nil
	case l.n >= l.max.Load():
		return false, nil
	}
	if q == nil {
		if l.byIP == nil {
			l.byIP = make(map[string]*list.List)
		}
		q = list.New()
		l.byIP[c.ip] = q
	}
	c.elem = q.PushBack(c)
	l.n++
	return true, old
}

// unqueue takes c off its IP's tokenless connections, under l.mu.
func (l *capListener) unqueue(c *capConn) {
	if c.elem == nil {
		return
	}
	q := l.byIP[c.ip]
	q.Remove(c.elem)
	c.elem = nil
	if q.Len() == 0 {
		delete(l.byIP, c.ip)
	}
}

// reserve takes a place of h's share; false when it is full.
func (l *capListener) reserve(h *Hub) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.share > 0 && l.byHub[h] >= l.share {
		return false
	}
	if l.byHub == nil {
		l.byHub = make(map[*Hub]int64)
	}
	l.byHub[h]++
	return true
}

// settle settles a place of h's share reserved for a request on c: keep
// authenticates c for h, which then holds it and no longer counts for its
// IP. A c already authenticated, evicted or closed gives it back.
func (l *capListener) settle(c *capConn, h *Hub, keep bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if keep && c.elem != nil {
		l.unqueue(c)
		c.hub = h
		return
	}
	l.give(h)
}

// give gives back a place of h's share, under l.mu.
func (l *capListener) give(h *Hub) {
	if l.byHub[h]--; l.byHub[h] <= 0 {
		delete(l.byHub, h)
	}
}

// free gives back c's slot, and its place of its Hub's share.
func (l *capListener) free(c *capConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	l.unqueue(c)
	if c.hub != nil {
		l.give(c.hub)
		c.hub = nil
	}
}

// open is the number of connections held.
func (l *capListener) open() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// capConn frees its slot of the capListener on its first Close.
type capConn struct {
	net.Conn
	l    *capListener
	ip   string
	elem *list.Element // under l.mu: in l.byIP while tokenless and open
	hub  *Hub          // under l.mu: the Hub it is authenticated for, while open
	once sync.Once
}

func (c *capConn) Close() error {
	c.once.Do(func() { c.l.free(c) })
	return c.Conn.Close()
}

// rateLog logs a warning at most once per logEvery, with the number of
// those skipped since.
type rateLog struct {
	mu      sync.Mutex
	next    time.Time
	skipped int
}

const logEvery = 10 * time.Second

func (r *rateLog) warn(log *slog.Logger, msg string, args ...any) {
	if log == nil {
		return
	}
	r.mu.Lock()
	now := time.Now()
	if now.Before(r.next) {
		r.skipped++
		r.mu.Unlock()
		return
	}
	r.next, args = now.Add(logEvery), append(args, "skipped", r.skipped)
	r.skipped = 0
	r.mu.Unlock()
	log.Warn(msg, args...)
}

// CloseWrite passes on the TCP half-close http.Server sends before it
// closes a connection.
func (c *capConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Serve serves r on ln (TLS with cert) until ctx ends, then drops every
// instance and shuts down within timeout. It caps ln's open connections
// (see connsPerInstance), the authenticated ones of each hub server, and
// the tokenless ones of one source IP (Options.MaxConnsPerIP).
func Serve(ctx context.Context, ln net.Listener, r *Router, cert *tls.Certificate, timeout time.Duration, log *slog.Logger) error {
	ln = &capListener{Listener: ln, max: &r.connCap, perIP: r.perIP, share: int64(connsPerInstance * r.maxInstances), log: log}
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
