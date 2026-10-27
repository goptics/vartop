package fleettest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
)

// Worker is one worker as a Varmq serves it.
type Worker struct {
	Status                                   string
	Concurrency, Busy, Pending, Idle         int
	Submitted, Completed, Successful, Failed uint64
	// ActionStatus, when non-zero, makes every PATCH on this worker reply
	// with this status and {"error":"..."} without changing anything.
	ActionStatus int
	// GetStatus, when non-zero, makes GET /workers/{name} reply with this
	// status and {"error":"..."}.
	GetStatus int
}

// Varmq is an in-memory varmq REST API (the routes of varmq's mux.go)
// for tests. Lock Mu to read or change its fields while it is serving.
type Varmq struct {
	Instance   string        // sent as X-Varmq-Instance on every response
	Delay      time.Duration // added before every reply; abandoned if the client goes away
	ListStatus int           // when non-zero, GET /workers replies with it and {"error":"..."}

	Mu sync.Mutex
	// Workers by name. A nil entry is listed by GET /workers but answers
	// 404 everywhere else: a worker removed between list and get.
	Workers  map[string]*Worker
	received []string // "METHOD escaped-path" of every request received

	listInFlight, maxListInFlight int // concurrent GET /workers requests
}

// NewVarmq starts a fake varmq answering as instance with the given
// workers. The server is closed when the test ends.
func NewVarmq(t *testing.T, instance string, workers map[string]*Worker) (*Varmq, *httptest.Server) {
	t.Helper()
	if workers == nil {
		workers = make(map[string]*Worker)
	}
	f := &Varmq{Instance: instance, Workers: workers}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /workers", f.list)
	mux.HandleFunc("GET /workers/{name}", f.get)
	mux.HandleFunc("PATCH /workers/{name}/actions/{action}", f.action)
	mux.HandleFunc("PATCH /workers/{name}/config/concurrency/{n}", f.concurrency)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isList := r.Method == http.MethodGet && r.URL.Path == "/workers"
		f.Mu.Lock()
		f.received = append(f.received, r.Method+" "+r.URL.EscapedPath())
		delay := f.Delay
		if isList {
			f.listInFlight++
			f.maxListInFlight = max(f.maxListInFlight, f.listInFlight)
		}
		f.Mu.Unlock()
		if isList {
			defer func() {
				f.Mu.Lock()
				f.listInFlight--
				f.Mu.Unlock()
			}()
		}

		w.Header().Set(api.InstanceHeader, f.Instance)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// Calls returns a copy of the requests received so far.
func (f *Varmq) Calls() []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return slices.Clone(f.received)
}

// Worker returns a copy of the named worker's current state; ok is false
// if f has no such worker.
func (f *Varmq) Worker(name string) (w Worker, ok bool) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if fw := f.Workers[name]; fw != nil {
		return *fw, true
	}
	return Worker{}, false
}

// MaxListConcurrency is the most GET /workers requests ever in flight at once.
func (f *Varmq) MaxListConcurrency() int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.maxListInFlight
}

func (f *Varmq) list(w http.ResponseWriter, _ *http.Request) {
	f.Mu.Lock()
	if status := f.ListStatus; status != 0 {
		f.Mu.Unlock()
		writeFakeError(w, status, http.StatusText(status))
		return
	}
	names := make([]string, 0, len(f.Workers))
	for name := range f.Workers {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]map[string]any, len(names))
	for i, name := range names {
		out[i] = fakeSummary(name, f.Workers[name])
	}
	f.Mu.Unlock()
	writeFakeJSON(w, http.StatusOK, out)
}

func (f *Varmq) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f.Mu.Lock()
	fw := f.Workers[name]
	failWith := 0
	var body map[string]any
	if fw != nil {
		failWith = fw.GetStatus
		body = fakeSummary(name, fw)
		body["metrics"] = map[string]uint64{
			"submitted":  fw.Submitted,
			"completed":  fw.Completed,
			"successful": fw.Successful,
			"failed":     fw.Failed,
		}
	}
	f.Mu.Unlock()
	switch {
	case body == nil:
		writeFakeError(w, http.StatusNotFound, "worker not found")
	case failWith != 0:
		writeFakeError(w, failWith, http.StatusText(failWith))
	default:
		writeFakeJSON(w, http.StatusOK, body)
	}
}

// fakeActionStatus maps each action to the status the worker ends up in.
var fakeActionStatus = map[string]string{
	"pause":   "Paused",
	"resume":  "Running",
	"stop":    "Stopped",
	"restart": "Running",
}

func (f *Varmq) action(w http.ResponseWriter, r *http.Request) {
	status, ok := fakeActionStatus[r.PathValue("action")]
	if !ok {
		writeFakeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	f.mutate(w, r.PathValue("name"), func(fw *Worker) string {
		fw.Status = status
		return ""
	})
}

// SameConcurrency is varmq's ErrSameConcurrency, which it answers with a 409
// when a worker is set to the concurrency it already has.
const SameConcurrency = "worker already has the same concurrency"

func (f *Varmq) concurrency(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeFakeError(w, http.StatusBadRequest, "invalid concurrency value")
		return
	}
	f.mutate(w, r.PathValue("name"), func(fw *Worker) string {
		if fw.Concurrency == n {
			return SameConcurrency
		}
		fw.Concurrency = n
		return ""
	})
}

// mutate applies change to the named worker unless it is missing (404) or
// set to fail with its ActionStatus. A change that returns a message changed
// nothing: the reply is a 409 with that message.
func (f *Varmq) mutate(w http.ResponseWriter, name string, change func(*Worker) (conflict string)) {
	f.Mu.Lock()
	fw := f.Workers[name]
	failWith, msg := 0, ""
	switch {
	case fw == nil:
		failWith = http.StatusNotFound
	case fw.ActionStatus != 0:
		failWith = fw.ActionStatus
	default:
		if msg = change(fw); msg != "" {
			failWith = http.StatusConflict
		}
	}
	f.Mu.Unlock()

	switch {
	case failWith == 0:
		w.WriteHeader(http.StatusOK)
	case msg != "":
		writeFakeError(w, failWith, msg)
	case failWith == http.StatusNotFound:
		writeFakeError(w, failWith, "worker not found")
	case failWith == http.StatusConflict:
		writeFakeError(w, failWith, "worker is not running")
	default:
		writeFakeError(w, failWith, http.StatusText(failWith))
	}
}

// fakeSummary is the GET /workers entry for a worker; a nil worker has only a name.
func fakeSummary(name string, fw *Worker) map[string]any {
	if fw == nil {
		fw = &Worker{}
	}
	return map[string]any{
		"name":             name,
		"status":           fw.Status,
		"concurrency":      fw.Concurrency,
		"num_processing":   fw.Busy,
		"num_pending":      fw.Pending,
		"num_idle_workers": fw.Idle,
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeFakeError(w http.ResponseWriter, status int, msg string) {
	writeFakeJSON(w, status, map[string]string{"error": msg})
}

// NewInstances starts one fake varmq per instance ID, each with a running
// worker "w" whose Completed counter is 10 × its position (10, 20, 30, …).
func NewInstances(t *testing.T, ids ...string) ([]*Varmq, []*httptest.Server) {
	t.Helper()
	fakes := make([]*Varmq, len(ids))
	srvs := make([]*httptest.Server, len(ids))
	for i, id := range ids {
		fakes[i], srvs[i] = NewVarmq(t, id, map[string]*Worker{
			"w": {Status: "Running", Concurrency: 1, Completed: uint64(10 * (i + 1))},
		})
	}
	return fakes, srvs
}
