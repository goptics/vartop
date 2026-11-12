// Package hub is vartop's side of VarMQ hub protocol 1. VarMQ instances
// dial vartop web's hub listener and hold a stream open; vartop sends its
// REST requests down the stream and the instances post the replies back.
// A Hub is one hub server's set of connected instances; a Router serves
// every hub server on one listener and picks the Hub by token.
package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/goptics/vartop/internal/api"
)

// Wire limits of protocol 1, and the hub's own bounds.
const (
	// Protocol is the protocol version spoken.
	Protocol = 1
	// DefaultHeartbeat is the ping and beat period sent in hello.
	DefaultHeartbeat = 5 * time.Second
	// MinHeartbeat and MaxHeartbeat bound heartbeat_ms on the wire.
	MinHeartbeat = time.Second
	MaxHeartbeat = time.Minute
	// MaxInstanceID is the longest instance ID, in bytes.
	MaxInstanceID = 128
	// MaxLine is the longest stream line, newline included.
	MaxLine = 64 << 10
	// MaxReply is the largest reply body.
	MaxReply = 16 << 20
	// MaxInFlight caps the callers waiting on one instance: a request
	// holds a slot from its frame until its reply is read, or its caller
	// left and no reply is being read. It does not bound what runs at
	// the instance: a frame whose caller left may still run there.
	MaxInFlight = 8
	// DefaultMaxInstances caps connected instances per hub server.
	// ponytail: 1000 per hub server, so a token holder cannot grow the
	// stored state past it; raise it with Options.MaxInstances (no flag
	// yet).
	DefaultMaxInstances = 1000
	// missedBeats beats missed in a row drop an instance.
	missedBeats = 3
	// SessionHeader carries the stream's session on beats and replies.
	SessionHeader = "X-Hub-Session"
)

// Hub-wide bounds; tests shorten them through Router.lim.
const (
	// requestTimeout bounds reading a hub request, body included; an
	// authenticated stream clears it.
	requestTimeout = 10 * time.Second
	// replyBudget bounds the reply bytes one hub server buffers at once,
	// so a token holder fills its own server's budget only.
	// ponytail: 256 MiB per hub server; raise it here.
	replyBudget = 256 << 20
	// replyStep is what a reply of unknown length (chunked) reserves of
	// the budget at a time, as its bytes arrive.
	replyStep = 64 << 10
	// joinBurst and joinRate bound new streams per hub server: a token
	// bucket of joinBurst, refilled at joinRate a second. A fleet that
	// redials at once gets in at that pace; the rest get 503 and retry.
	joinBurst = 200
	joinRate  = 20
)

// Scheme and Host make a hub endpoint URL: hub://hub.invalid/<instance>.
// Not http, so the default transport refuses it and a hub endpoint never
// reaches the network (with its token) by mistake.
const (
	Scheme = "hub"
	Host   = "hub.invalid"
)

var (
	// ErrNotConnected: the instance has no open stream. Never queued.
	ErrNotConnected = errors.New("instance not connected to the hub")
	// ErrDisconnected: the stream closed while the request waited.
	ErrDisconnected = errors.New("instance disconnected before it replied")
	// ErrHubOff: vartop web runs without --hub-listen.
	ErrHubOff = errors.New("vartop web runs without --hub-listen, so hub instances cannot connect")
)

// Endpoint is the endpoint URL of instance id: its Client sends through
// the hub's RoundTrip.
func Endpoint(id string) string {
	return Scheme + "://" + Host + "/" + url.PathEscape(id)
}

// split reads a hub endpoint request URL: the instance it names and the
// path plus query to send it, escaped as the caller wrote them.
func split(u *url.URL) (id, path string, ok bool) {
	if u == nil || u.Scheme != Scheme || u.Host != Host || u.User != nil {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(u.EscapedPath(), "/")
	if !ok {
		return "", "", false
	}
	seg, tail, _ := strings.Cut(rest, "/")
	id, err := url.PathUnescape(seg)
	if err != nil || !validInstance(id) {
		return "", "", false
	}
	path = "/" + tail
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return id, path, true
}

// validInstance: 1 to MaxInstanceID bytes of UTF-8 without control
// characters (no byte below 0x20, no 0x7F).
func validInstance(id string) bool {
	if id == "" || len(id) > MaxInstanceID || !utf8.ValidString(id) {
		return false
	}
	for _, c := range []byte(id) {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// frame is one stream line.
type frame struct {
	Type        string `json:"type"`
	Protocol    int    `json:"protocol,omitempty"`
	HeartbeatMS int64  `json:"heartbeat_ms,omitempty"`
	Session     string `json:"session,omitempty"`
	ID          string `json:"id,omitempty"`
	Method      string `json:"method,omitempty"`
	Path        string `json:"path,omitempty"`
}

func line(f frame) ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > MaxLine {
		return nil, fmt.Errorf("hub: frame of %d bytes exceeds the %d byte line limit", len(b), MaxLine)
	}
	return b, nil
}

var pingLine = []byte(`{"type":"ping"}` + "\n")

// reply is an instance's answer to one request, or why there is none.
type reply struct {
	status      int
	contentType string
	body        []byte
	err         error
}

// waiter is the caller of one request. Its in-flight slot is freed once
// the caller and the reply handler that claimed it (if any) are done.
type waiter struct {
	ch   chan reply    // the reply: buffered, sent once
	gone chan struct{} // closed when the caller stops waiting
	refs int           // under conn.mu: the caller, and a claiming reply handler
}

// conn is one connected instance: its open stream.
type conn struct {
	id      string
	session string        // the stream's secret: beats and replies carry it
	frames  chan []byte   // request lines for the stream writer
	slots   chan struct{} // in-flight cap
	done    chan struct{} // closed when dropped
	once    sync.Once
	beat    atomic.Int64 // last beat, as Hub.since
	listed  atomic.Bool  // the session's first beat came: discovery and requests see it

	mu      sync.Mutex
	waiting map[string]*waiter // request ID → its caller
}

func newConn(id string) *conn {
	return &conn{
		id:      id,
		session: rand.Text(),
		frames:  make(chan []byte, MaxInFlight),
		slots:   make(chan struct{}, MaxInFlight),
		done:    make(chan struct{}),
		waiting: make(map[string]*waiter),
	}
}

// drop closes the connection: its stream ends, its callers fail.
func (c *conn) drop() { c.once.Do(func() { close(c.done) }) }

// owns reports whether r carries c's session, compared in constant time.
func (c *conn) owns(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(SessionHeader)), []byte(c.session)) == 1
}

// wait registers w, holding a slot, as the caller of request id.
func (c *conn) wait(id string, w *waiter) {
	c.mu.Lock()
	c.waiting[id] = w
	c.mu.Unlock()
}

// leave: the caller w of request id stops waiting.
func (c *conn) leave(id string, w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waiting[id] == w {
		delete(c.waiting, id)
	}
	close(w.gone)
	c.unref(w)
}

// claim takes the caller of request id for a reply handler, nil when none
// waits: only the first reply for an ID finds it. The handler calls
// release when it returns.
func (c *conn) claim(id string) *waiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.waiting[id]
	if w != nil {
		delete(c.waiting, id)
		w.refs++
	}
	return w
}

// release: the reply handler that claimed w returns.
func (c *conn) release(w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unref(w)
}

// unref drops one holder of w, under c.mu; the last frees w's slot.
func (c *conn) unref(w *waiter) {
	if w.refs--; w.refs == 0 {
		<-c.slots
	}
}

// limits are the bounds a Router gives each of its Hubs.
type limits struct {
	reqTimeout          time.Duration
	joinBurst, joinRate float64
	now                 func() time.Time
	replyMax            int64 // per Hub
}

func defaultLimits() *limits {
	return &limits{reqTimeout: requestTimeout, joinBurst: joinBurst, joinRate: joinRate, now: time.Now, replyMax: replyBudget}
}

// Hub is one hub server's connected instances. Instance IDs are scoped to
// the Hub: two servers may each hold the same ID.
type Hub struct {
	name         atomic.Pointer[string] // config key; logs only
	heartbeat    time.Duration
	maxInstances int
	lim          *limits
	log          *slog.Logger
	epoch        time.Time    // monotonic base of conn.beat
	replyUsed    atomic.Int64 // reply bytes reserved, of lim.replyMax

	mu     sync.Mutex
	conns  map[string]*conn
	closed bool
	tokens float64   // new-stream bucket (see joinBurst)
	filled time.Time // when tokens was last refilled
}

func newHub(name string, heartbeat time.Duration, maxInstances int, lim *limits, log *slog.Logger) *Hub {
	h := &Hub{
		heartbeat: heartbeat, maxInstances: maxInstances, lim: lim, log: log,
		epoch: time.Now(), conns: make(map[string]*conn),
		tokens: lim.joinBurst, filled: lim.now(),
	}
	h.name.Store(&name)
	return h
}

// take reserves n bytes of the hub server's reply budget; false when they
// do not fit.
func (h *Hub) take(n int64) bool {
	if h.replyUsed.Add(n) > h.lim.replyMax {
		h.replyUsed.Add(-n)
		return false
	}
	return true
}

func (h *Hub) give(n int64) { h.replyUsed.Add(-n) }

// label is the hub server's config key, for logs.
func (h *Hub) label() string { return *h.name.Load() }

func (h *Hub) since() int64 { return int64(time.Since(h.epoch)) }

var (
	errDuplicate = errors.New("duplicate instance")
	errFull      = errors.New("instance limit reached")
	errRate      = errors.New("too many new instances; retry later")
	errClosed    = errors.New("hub closed")
)

// add registers c, unless its ID is taken, the hub is full, closed or
// past its new-stream rate.
func (h *Hub) add(c *conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return errClosed
	case h.conns[c.id] != nil:
		return errDuplicate
	case len(h.conns) >= h.maxInstances:
		return errFull
	case !h.join():
		return errRate
	}
	c.beat.Store(h.since()) // the stream's opening starts the liveness clock
	h.conns[c.id] = c
	return nil
}

// join takes a token of the new-stream bucket, under h.mu; false when
// none is left.
func (h *Hub) join() bool {
	now := h.lim.now()
	h.tokens = min(h.lim.joinBurst, h.tokens+now.Sub(h.filled).Seconds()*h.lim.joinRate)
	h.filled = now
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}

// remove unregisters c, if still registered, and drops it. A newer conn
// of the same ID is left alone.
func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	if h.conns[c.id] == c {
		delete(h.conns, c.id)
	}
	h.mu.Unlock()
	c.drop()
}

// get is the registered conn of id, listed or not.
func (h *Hub) get(id string) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[id]
}

// session is the conn of r's instance whose session r carries, nil when
// none: beats, byes and replies belong to one stream.
func (h *Hub) session(r *http.Request) *conn {
	id := r.URL.Query().Get("instance")
	if !validInstance(id) {
		return nil
	}
	if c := h.get(id); c != nil && c.owns(r) {
		return c
	}
	return nil
}

// close drops every instance and refuses new ones.
func (h *Hub) close() {
	h.mu.Lock()
	h.closed = true
	conns := h.conns
	h.conns = make(map[string]*conn)
	h.mu.Unlock()
	for _, c := range conns {
		c.drop()
	}
}

// Endpoints is the endpoint URL of every listed instance, sorted.
func (h *Hub) Endpoints() []string {
	h.mu.Lock()
	out := make([]string, 0, len(h.conns))
	for id, c := range h.conns {
		if c.listed.Load() {
			out = append(out, Endpoint(id))
		}
	}
	h.mu.Unlock()
	slices.Sort(out)
	return out
}

// stream serves GET /hub/stream: register, hello, then request and ping
// lines until the stream closes or the instance is dropped.
func (h *Hub) stream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("instance")
	if !validInstance(id) || r.URL.Query().Get("protocol") != strconv.Itoa(Protocol) {
		http.Error(w, "want instance (1 to 128 bytes of UTF-8, no control characters) and protocol=1", http.StatusBadRequest)
		return
	}
	if r.ContentLength != 0 { // with a body, or chunked: one nobody reads
		http.Error(w, "the stream request has no body", http.StatusBadRequest)
		return
	}
	c := newConn(id)
	switch err := h.add(c); {
	case errors.Is(err, errDuplicate):
		h.log.Warn("hub: instance already connected; refused", "server", h.label(), "instance", id)
		http.Error(w, "instance already connected", http.StatusConflict)
		return
	case err != nil:
		h.log.Warn("hub: instance refused", "server", h.label(), "instance", id, "err", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer h.remove(c)
	_ = r.Body.Close()

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{}) // authenticated: the stream may live on
	hello, _ := line(frame{Type: "hello", Protocol: Protocol, HeartbeatMS: h.heartbeat.Milliseconds(), Session: c.session})
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // proxies: do not buffer
	live(w, r)
	w.WriteHeader(http.StatusOK)
	if !h.write(w, rc, hello) {
		return
	}
	h.log.Info("hub: instance connected", "server", h.label(), "instance", id)
	why := "stream closed"
	defer func() { h.log.Info("hub: instance disconnected", "server", h.label(), "instance", id, "why", why) }()

	ping := time.NewTicker(h.heartbeat)
	defer ping.Stop()
	lost := int64(missedBeats * h.heartbeat)
	for {
		var out []byte
		select {
		case <-r.Context().Done():
			return
		case <-c.done:
			why = "dropped"
			return
		case out = <-c.frames:
		case <-ping.C:
			if h.since()-c.beat.Load() >= lost {
				why = "missed beats"
				return
			}
			out = pingLine
		}
		if !h.write(w, rc, out) {
			why = "write failed"
			return
		}
	}
}

// write writes one line and flushes it, within a deadline: a peer that
// stops reading is dropped, not waited on.
func (h *Hub) write(w http.ResponseWriter, rc *http.ResponseController, b []byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(missedBeats * h.heartbeat))
	if _, err := w.Write(b); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// replyTo serves POST /hub/reply/{id}: hand the body to the caller of
// request id, sent down this instance's stream of this session. The
// caller's slot stays taken until the handler returns, and the body
// counts against the hub server's reply budget while it is read.
func (h *Hub) replyTo(w http.ResponseWriter, r *http.Request) {
	c := h.session(r)
	var wt *waiter
	if c != nil {
		wt = c.claim(r.PathValue("id"))
	}
	if wt == nil {
		http.Error(w, "no request with that id is waiting", http.StatusNotFound)
		return
	}
	defer c.release(wt)
	// Every path from here answers the caller once.
	fail := func(code int, msg string, err error) {
		wt.ch <- reply{err: err}
		http.Error(w, msg, code)
	}
	status, err := strconv.Atoi(r.Header.Get("X-Hub-Status"))
	if err != nil || status < 200 || status > 599 {
		fail(http.StatusBadRequest, "want X-Hub-Status 200 to 599", fmt.Errorf("hub: instance %q replied without a valid X-Hub-Status", c.id))
		return
	}
	tooLarge := fmt.Errorf("hub: reply of instance %q exceeds %d bytes", c.id, MaxReply)
	if r.ContentLength > MaxReply {
		fail(http.StatusRequestEntityTooLarge, "reply too large", tooLarge)
		return
	}
	full := func() {
		fail(http.StatusServiceUnavailable, "hub reply memory is full; retry later", fmt.Errorf("hub: reply of instance %q refused: the hub's reply memory is full", c.id))
	}
	// A known length is reserved up front; chunked, step by step.
	held := max(r.ContentLength, 0)
	if !h.take(held) {
		full()
		return
	}
	var body io.Reader = r.Body
	chunked := &budgeted{h: h, r: r.Body}
	if r.ContentLength < 0 {
		body = chunked
	}
	defer func() { h.give(held + chunked.held) }()
	read, err := readReply(w, r, body, c, wt)
	switch mbe := (*http.MaxBytesError)(nil); {
	case err == nil:
		wt.ch <- reply{status: status, contentType: r.Header.Get("Content-Type"), body: read}
		live(w, r)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errBudget):
		full()
	case errors.As(err, &mbe):
		fail(http.StatusRequestEntityTooLarge, "reply too large", tooLarge)
	case isClosed(c.done):
		fail(http.StatusNotFound, "stream closed", fmt.Errorf("%w: %q", ErrDisconnected, c.id))
	case isClosed(wt.gone):
		fail(http.StatusNotFound, "the caller left", err)
	default:
		fail(http.StatusBadRequest, "cannot read reply", fmt.Errorf("hub: read reply of instance %q: %w", c.id, err))
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

var errBudget = errors.New("hub reply memory is full")

// budgeted reads r, a body of unknown length, at most held bytes, held of
// its Hub's reply budget; when they are read it takes replyStep more, or
// fails with errBudget.
type budgeted struct {
	h          *Hub
	r          io.Reader
	read, held int64
}

func (b *budgeted) Read(p []byte) (int, error) {
	if b.read == b.held {
		if len(p) == 0 {
			return 0, nil
		}
		if !b.h.take(replyStep) {
			return 0, errBudget
		}
		b.held += replyStep
	}
	n, err := b.r.Read(p[:min(int64(len(p)), b.held-b.read)])
	b.read += int64(n)
	return n, err
}

// readReply reads r's body through body, at most MaxReply bytes. It aborts
// the read when the caller stops waiting or the stream drops: nobody would
// read it.
func readReply(w http.ResponseWriter, r *http.Request, body io.Reader, c *conn, wt *waiter) ([]byte, error) {
	rc := http.NewResponseController(w)
	stop, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-stop:
			return
		case <-wt.gone:
		case <-c.done:
		}
		_ = rc.SetReadDeadline(time.Now())
	}()
	defer func() {
		close(stop)
		<-watched // no deadline set after the handler returned
	}()
	var buf bytes.Buffer
	if r.ContentLength > 0 {
		buf.Grow(int(r.ContentLength) + bytes.MinRead)
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, io.NopCloser(body), MaxReply))
	return buf.Bytes(), err
}

// beatFrom serves POST /hub/beat: the instance is alive, or with bye=1
// leaves. The session's first beat lists the instance.
func (h *Hub) beatFrom(w http.ResponseWriter, r *http.Request) {
	c := h.session(r)
	if c == nil {
		http.Error(w, "no open stream with that session for that instance", http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("bye") == "1" {
		h.remove(c)
		h.log.Info("hub: instance said bye", "server", h.label(), "instance", c.id)
	} else {
		c.beat.Store(h.since())
		c.listed.Store(true)
	}
	live(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// RoundTrip sends req to the instance its URL names (see Endpoint) as a
// request frame and returns the instance's reply. It fails at once with
// ErrNotConnected when the instance has no stream or is not listed yet,
// with ErrDisconnected when the stream closes first, and with the
// context's error when req's context ends first. The reply carries only
// its status, Content-Type and body; X-Varmq-Instance is always the
// instance the stream registered as.
func (h *Hub) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close() // the API has no request bodies
	}
	id, path, ok := split(req.URL)
	if !ok {
		return nil, errors.New("hub: not a hub endpoint URL")
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPatch {
		return nil, fmt.Errorf("hub: method %s cannot be sent to an instance", req.Method)
	}
	c := h.get(id)
	if c == nil || !c.listed.Load() {
		return nil, fmt.Errorf("%w: %q", ErrNotConnected, id)
	}
	ctx := req.Context()
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("%w: %q", ErrDisconnected, id)
	}
	rid := rand.Text() // unguessable: an instance cannot answer a request it was not sent
	out, err := line(frame{Type: "request", ID: rid, Method: req.Method, Path: path})
	if err != nil {
		<-c.slots
		return nil, err
	}
	wt := &waiter{ch: make(chan reply, 1), gone: make(chan struct{}), refs: 1}
	c.wait(rid, wt)
	defer c.leave(rid, wt)
	select {
	case c.frames <- out:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("%w: %q", ErrDisconnected, id)
	}

	var rep reply
	select {
	case rep = <-wt.ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		select {
		case rep = <-wt.ch:
		default:
			return nil, fmt.Errorf("%w: %q", ErrDisconnected, id)
		}
	}
	if rep.err != nil {
		return nil, rep.err
	}
	header := make(http.Header, 2)
	if rep.contentType != "" {
		header.Set("Content-Type", rep.contentType)
	}
	header.Set(api.InstanceHeader, id)
	return &http.Response{
		Status:        strconv.Itoa(rep.status) + " " + http.StatusText(rep.status),
		StatusCode:    rep.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(rep.body)),
		ContentLength: int64(len(rep.body)),
		Request:       req,
	}, nil
}
