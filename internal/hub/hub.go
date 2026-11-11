// Package hub is vartop's side of VarMQ hub protocol 1. VarMQ instances
// dial vartop web's hub listener and hold a stream open; vartop sends its
// REST requests down the stream and the instances post the replies back.
// A Hub is one hub server's set of connected instances; a Router serves
// every hub server on one listener and picks the Hub by token.
package hub

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
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
	// MaxInFlight caps requests waiting on one instance; more callers
	// wait for a slot or their context.
	MaxInFlight = 8
	// DefaultMaxInstances caps connected instances per hub server.
	DefaultMaxInstances = 10000
	// missedBeats beats missed in a row drop an instance.
	missedBeats = 3
	// bodyTimeout bounds reading a reply or beat body.
	bodyTimeout = time.Minute
	// maxContentType clips a reply's Content-Type.
	maxContentType = 256
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

// validInstance: 1 to MaxInstanceID bytes.
func validInstance(id string) bool { return id != "" && len(id) <= MaxInstanceID }

// validRequestID: 1 to 64 bytes of [A-Za-z0-9_-].
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID: 128 random bits, base64url (22 bytes). Unguessable, so an
// instance cannot answer a request it was not sent.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// frame is one stream line.
type frame struct {
	Type        string `json:"type"`
	Protocol    int    `json:"protocol,omitempty"`
	HeartbeatMS int64  `json:"heartbeat_ms,omitempty"`
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

// conn is one connected instance: its open stream.
type conn struct {
	id     string
	frames chan []byte   // request lines for the stream writer
	slots  chan struct{} // in-flight cap
	done   chan struct{} // closed when dropped
	once   sync.Once
	beat   atomic.Int64 // last beat, as Hub.since

	mu      sync.Mutex
	waiting map[string]chan reply // request ID → its caller
}

func newConn(id string) *conn {
	return &conn{
		id:      id,
		frames:  make(chan []byte, MaxInFlight),
		slots:   make(chan struct{}, MaxInFlight),
		done:    make(chan struct{}),
		waiting: make(map[string]chan reply),
	}
}

// drop closes the connection: its stream ends, its callers fail.
func (c *conn) drop() { c.once.Do(func() { close(c.done) }) }

func (c *conn) dropped() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// wait registers a caller for request id; false once dropped.
func (c *conn) wait(id string, ch chan reply) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped() {
		return false
	}
	c.waiting[id] = ch
	return true
}

// claim takes the caller of request id, nil when none waits. Only the
// first reply for an ID finds it.
func (c *conn) claim(id string) chan reply {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.waiting[id]
	delete(c.waiting, id)
	return ch
}

// Hub is one hub server's connected instances. Instance IDs are scoped to
// the Hub: two servers may each hold the same ID.
type Hub struct {
	name         atomic.Pointer[string] // config key; logs only
	heartbeat    time.Duration
	maxInstances int
	log          *slog.Logger
	epoch        time.Time // monotonic base of conn.beat

	mu     sync.Mutex
	conns  map[string]*conn
	closed bool
}

func newHub(name string, heartbeat time.Duration, maxInstances int, log *slog.Logger) *Hub {
	h := &Hub{
		heartbeat: heartbeat, maxInstances: maxInstances, log: log,
		epoch: time.Now(), conns: make(map[string]*conn),
	}
	h.name.Store(&name)
	return h
}

// label is the hub server's config key, for logs.
func (h *Hub) label() string { return *h.name.Load() }

func (h *Hub) since() int64 { return int64(time.Since(h.epoch)) }

var (
	errDuplicate = errors.New("duplicate instance")
	errFull      = errors.New("instance limit reached")
	errClosed    = errors.New("hub closed")
)

// add registers c, unless its ID is taken, the hub is full or closed.
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
	}
	c.beat.Store(h.since()) // the stream's opening counts as a beat
	h.conns[c.id] = c
	return nil
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

func (h *Hub) get(id string) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[id]
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

// Endpoints is the endpoint URL of every connected instance, sorted.
func (h *Hub) Endpoints() []string {
	h.mu.Lock()
	out := make([]string, 0, len(h.conns))
	for id := range h.conns {
		out = append(out, Endpoint(id))
	}
	h.mu.Unlock()
	slices.Sort(out)
	return out
}

// instanceParam is the request's instance query parameter, if valid.
func instanceParam(r *http.Request) (string, bool) {
	id := r.URL.Query().Get("instance")
	return id, validInstance(id)
}

// stream serves GET /hub/stream: register, hello, then request and ping
// lines until the stream closes or the instance is dropped.
func (h *Hub) stream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := instanceParam(r)
	if !ok || r.URL.Query().Get("protocol") != strconv.Itoa(Protocol) {
		http.Error(w, "want instance (1 to 128 bytes) and protocol=1", http.StatusBadRequest)
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

	rc := http.NewResponseController(w)
	hello, _ := line(frame{Type: "hello", Protocol: Protocol, HeartbeatMS: h.heartbeat.Milliseconds()})
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // proxies: do not buffer
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
// request id, sent to this instance.
func (h *Hub) replyTo(w http.ResponseWriter, r *http.Request) {
	rid := r.PathValue("id")
	id, ok := instanceParam(r)
	var c *conn
	if ok && validRequestID(rid) {
		c = h.get(id)
	}
	var ch chan reply
	if c != nil {
		ch = c.claim(rid) // bound to the instance the frame went to
	}
	if ch == nil {
		http.Error(w, "no request with that id is waiting", http.StatusNotFound)
		return
	}
	status, err := strconv.Atoi(r.Header.Get("X-Hub-Status"))
	if err != nil || status < 200 || status > 599 {
		ch <- reply{err: fmt.Errorf("hub: instance %q replied without a valid X-Hub-Status", id)}
		http.Error(w, "want X-Hub-Status 200 to 599", http.StatusBadRequest)
		return
	}
	if r.ContentLength > MaxReply {
		ch <- reply{err: fmt.Errorf("hub: reply of instance %q exceeds %d bytes", id, MaxReply)}
		http.Error(w, "reply too large", http.StatusRequestEntityTooLarge)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxReply))
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			ch <- reply{err: fmt.Errorf("hub: reply of instance %q exceeds %d bytes", id, MaxReply)}
			http.Error(w, "reply too large", http.StatusRequestEntityTooLarge)
			return
		}
		ch <- reply{err: fmt.Errorf("hub: read reply of instance %q: %w", id, err)}
		http.Error(w, "cannot read reply", http.StatusBadRequest)
		return
	}
	ct := r.Header.Get("Content-Type")
	if len(ct) > maxContentType {
		ct = ""
	}
	ch <- reply{status: status, contentType: ct, body: body}
	w.WriteHeader(http.StatusNoContent)
}

// beat serves POST /hub/beat: the instance is alive, or with bye=1 leaves.
func (h *Hub) beatFrom(w http.ResponseWriter, r *http.Request) {
	id, ok := instanceParam(r)
	var c *conn
	if ok {
		c = h.get(id)
	}
	if c == nil {
		http.Error(w, "no open stream for that instance", http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("bye") == "1" {
		h.remove(c)
		h.log.Info("hub: instance said bye", "server", h.label(), "instance", id)
	} else {
		c.beat.Store(h.since())
	}
	w.WriteHeader(http.StatusNoContent)
}

// RoundTrip sends req to the instance its URL names (see Endpoint) as a
// request frame and returns the instance's reply. It fails at once with
// ErrNotConnected when the instance has no stream, with ErrDisconnected
// when the stream closes first, and with the context's error when req's
// context ends first. The reply carries only its status, Content-Type and
// body; X-Varmq-Instance is always the instance the stream registered as.
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
	if c == nil {
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
	defer func() { <-c.slots }()

	rid, err := newRequestID()
	if err != nil {
		return nil, err
	}
	out, err := line(frame{Type: "request", ID: rid, Method: req.Method, Path: path})
	if err != nil {
		return nil, err
	}
	ch := make(chan reply, 1)
	if !c.wait(rid, ch) {
		return nil, fmt.Errorf("%w: %q", ErrDisconnected, id)
	}
	defer c.claim(rid)
	select {
	case c.frames <- out:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("%w: %q", ErrDisconnected, id)
	}

	var rep reply
	select {
	case rep = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		select {
		case rep = <-ch:
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
