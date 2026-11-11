package fleettest

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// HubFrame is one line of a hub stream (VarMQ hub protocol 1). The fake
// spells the wire out itself rather than import internal/hub: an
// independent check of the hub.
type HubFrame struct {
	Type        string `json:"type"`
	Protocol    int    `json:"protocol"`
	HeartbeatMS int64  `json:"heartbeat_ms"`
	ID          string `json:"id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
}

// HubInstanceOptions shape a HubInstance.
type HubInstanceOptions struct {
	// Handler runs each request frame, as a VarMQ instance runs it on its
	// varmq.Handler, and its answer is posted back. Nil: frames are held on
	// Requests for the test to answer with Reply.
	Handler http.Handler
	// NoBeat: never beat (the hub should drop it).
	NoBeat bool
	// Client sends every request (TLS trust, HTTP/2); a plain one when nil.
	Client *http.Client
}

// HubInstance is a fake VarMQ instance connected to a hub: it holds the
// stream open, answers request frames and beats every heartbeat_ms (not
// clamped: tests use short ones).
type HubInstance struct {
	ID          string
	base, token string
	opts        HubInstanceOptions
	client      *http.Client
	cancel      context.CancelFunc
	done        chan struct{} // closed when the stream has ended

	hello    HubFrame
	requests chan HubFrame

	mu    sync.Mutex
	first string // the first line, raw
	pings []time.Time
	beats []int // statuses of the beats sent
}

// DialHub opens id's stream to the hub at base with token. On 200 it
// waits for the first line and returns the instance; otherwise it returns
// the status with a nil instance. The stream ends when the test does.
func DialHub(t *testing.T, base, token, id string, opts HubInstanceOptions) (*HubInstance, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &HubInstance{
		ID: id, base: base, token: token, opts: opts, cancel: cancel,
		client:   cmp.Or(opts.Client, &http.Client{}),
		done:     make(chan struct{}),
		requests: make(chan HubFrame, 64),
	}
	q := url.Values{"instance": {id}, "protocol": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/hub/stream?"+q.Encode(), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("dial hub: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, resp.StatusCode
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("stream Content-Type = %q, want application/x-ndjson", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	if !sc.Scan() {
		cancel()
		t.Fatalf("stream of %q closed before its first line: %v", id, sc.Err())
	}
	h.first = sc.Text()
	if err := json.Unmarshal(sc.Bytes(), &h.hello); err != nil {
		t.Errorf("first line %q: %v", h.first, err)
	}
	t.Cleanup(h.Close)
	go h.read(ctx, sc, resp.Body)
	if !opts.NoBeat && h.hello.Type == "hello" && h.hello.HeartbeatMS > 0 {
		go h.beat(ctx, time.Duration(h.hello.HeartbeatMS)*time.Millisecond)
	}
	return h, http.StatusOK
}

// read reads the stream after hello until it ends.
func (h *HubInstance) read(ctx context.Context, sc *bufio.Scanner, body io.ReadCloser) {
	defer close(h.done)
	defer body.Close()
	for sc.Scan() {
		var f HubFrame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		switch f.Type {
		case "ping":
			h.mu.Lock()
			h.pings = append(h.pings, time.Now())
			h.mu.Unlock()
		case "request":
			if h.opts.Handler == nil {
				select {
				case h.requests <- f:
				case <-ctx.Done():
					return
				}
				continue
			}
			go h.run(f)
		}
	}
}

// run runs f on the handler and posts the answer.
func (h *HubInstance) run(f HubFrame) {
	req := httptest.NewRequest(f.Method, f.Path, nil)
	rec := httptest.NewRecorder()
	h.opts.Handler.ServeHTTP(rec, req)
	_, _ = h.Reply(f.ID, rec.Code, rec.Header(), rec.Body.Bytes())
}

func (h *HubInstance) beat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case <-t.C:
			code, err := h.Beat(false)
			if err != nil {
				return
			}
			h.mu.Lock()
			h.beats = append(h.beats, code)
			h.mu.Unlock()
		}
	}
}

// Hello is the stream's first line, decoded.
func (h *HubInstance) Hello() HubFrame { return h.hello }

// FirstLine is the stream's first line as sent.
func (h *HubInstance) FirstLine() string { return h.first }

// Pings is when each ping arrived.
func (h *HubInstance) Pings() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Time(nil), h.pings...)
}

// Requests yields the request frames held (no Handler).
func (h *HubInstance) Requests() <-chan HubFrame { return h.requests }

// Done is closed once the stream has ended.
func (h *HubInstance) Done() <-chan struct{} { return h.done }

// Reply posts an answer to request id; it returns the hub's status.
func (h *HubInstance) Reply(id string, status int, header http.Header, body []byte) (int, error) {
	return h.ReplyAs(h.ID, id, status, header, bytes.NewReader(body), int64(len(body)))
}

// ReplyAs posts an answer to request id naming instance as the sender,
// with body of length n (-1: unknown, sent chunked).
func (h *HubInstance) ReplyAs(instance, id string, status int, header http.Header, body io.Reader, n int64) (int, error) {
	q := url.Values{"instance": {instance}}
	req, err := http.NewRequest(http.MethodPost, h.base+"/hub/reply/"+url.PathEscape(id)+"?"+q.Encode(), body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = n
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Hub-Status", strconv.Itoa(status))
	for _, k := range []string{"Content-Type", "X-Varmq-Instance"} {
		if v := header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	for k, vs := range header { // extra headers a hub must not pass on
		if req.Header.Get(k) == "" {
			req.Header[k] = vs
		}
	}
	return h.post(req)
}

// Beat beats once (bye: says it leaves); it returns the hub's status.
func (h *HubInstance) Beat(bye bool) (int, error) {
	q := url.Values{"instance": {h.ID}}
	if bye {
		q.Set("bye", "1")
	}
	req, err := http.NewRequest(http.MethodPost, h.base+"/hub/beat?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	return h.post(req)
}

func (h *HubInstance) post(req *http.Request) (int, error) {
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// Close ends the stream without a bye, as a process that dies.
func (h *HubInstance) Close() {
	h.cancel()
	<-h.done
}

// String names the instance.
func (h *HubInstance) String() string { return fmt.Sprintf("hub instance %q", h.ID) }
