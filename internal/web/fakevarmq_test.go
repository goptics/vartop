package web

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

// fakeWorker is one worker as a fakeVarmq serves it.
type fakeWorker struct {
	Status                                   string
	Concurrency, Busy, Pending, Idle         int
	Submitted, Completed, Successful, Failed uint64
	// ActionStatus, when non-zero, makes every PATCH on this worker reply
	// with this status and {"error":"..."} without changing anything.
	ActionStatus int
}

// fakeVarmq is an in-memory varmq REST API (the routes of varmq's mux.go)
// for tests. Lock mu to read or change its fields while it is serving.
type fakeVarmq struct {
	Instance string        // sent as X-Varmq-Instance on every response
	Delay    time.Duration // added before every reply; abandoned if the client goes away

	mu sync.Mutex
	// Workers by name. A nil entry is listed by GET /workers but answers
	// 404 everywhere else: a worker removed between list and get.
	Workers map[string]*fakeWorker
	Calls   []string // "METHOD escaped-path" of every request received

	listInFlight, maxListInFlight int // concurrent GET /workers requests
}

// newFakeVarmq starts a fake varmq answering as instance with the given
// workers. The server is closed when the test ends.
func newFakeVarmq(t *testing.T, instance string, workers map[string]*fakeWorker) (*fakeVarmq, *httptest.Server) {
	t.Helper()
	if workers == nil {
		workers = make(map[string]*fakeWorker)
	}
	f := &fakeVarmq{Instance: instance, Workers: workers}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /workers", f.list)
	mux.HandleFunc("GET /workers/{name}", f.get)
	mux.HandleFunc("PATCH /workers/{name}/actions/{action}", f.action)
	mux.HandleFunc("PATCH /workers/{name}/config/concurrency/{n}", f.concurrency)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isList := r.Method == http.MethodGet && r.URL.Path == "/workers"
		f.mu.Lock()
		f.Calls = append(f.Calls, r.Method+" "+r.URL.EscapedPath())
		delay := f.Delay
		if isList {
			f.listInFlight++
			f.maxListInFlight = max(f.maxListInFlight, f.listInFlight)
		}
		f.mu.Unlock()
		if isList {
			defer func() {
				f.mu.Lock()
				f.listInFlight--
				f.mu.Unlock()
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

// calls returns a copy of the requests received so far.
func (f *fakeVarmq) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Calls)
}

// maxListConcurrency is the most GET /workers requests ever in flight at once.
func (f *fakeVarmq) maxListConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxListInFlight
}

func (f *fakeVarmq) list(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	names := make([]string, 0, len(f.Workers))
	for name := range f.Workers {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]map[string]any, len(names))
	for i, name := range names {
		out[i] = fakeSummary(name, f.Workers[name])
	}
	f.mu.Unlock()
	writeFakeJSON(w, http.StatusOK, out)
}

func (f *fakeVarmq) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	f.mu.Lock()
	fw := f.Workers[name]
	var body map[string]any
	if fw != nil {
		body = fakeSummary(name, fw)
		body["metrics"] = map[string]uint64{
			"submitted":  fw.Submitted,
			"completed":  fw.Completed,
			"successful": fw.Successful,
			"failed":     fw.Failed,
		}
	}
	f.mu.Unlock()
	if body == nil {
		writeFakeError(w, http.StatusNotFound, "worker not found")
		return
	}
	writeFakeJSON(w, http.StatusOK, body)
}

// fakeActionStatus maps each action to the status the worker ends up in.
var fakeActionStatus = map[string]string{
	"pause":   "Paused",
	"resume":  "Running",
	"stop":    "Stopped",
	"restart": "Running",
}

func (f *fakeVarmq) action(w http.ResponseWriter, r *http.Request) {
	status, ok := fakeActionStatus[r.PathValue("action")]
	if !ok {
		writeFakeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	f.mutate(w, r.PathValue("name"), func(fw *fakeWorker) { fw.Status = status })
}

func (f *fakeVarmq) concurrency(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeFakeError(w, http.StatusBadRequest, "invalid concurrency value")
		return
	}
	f.mutate(w, r.PathValue("name"), func(fw *fakeWorker) { fw.Concurrency = n })
}

// mutate applies change to the named worker unless it is missing (404) or
// set to fail with its ActionStatus.
func (f *fakeVarmq) mutate(w http.ResponseWriter, name string, change func(*fakeWorker)) {
	f.mu.Lock()
	fw := f.Workers[name]
	failWith := 0
	switch {
	case fw == nil:
		failWith = http.StatusNotFound
	case fw.ActionStatus != 0:
		failWith = fw.ActionStatus
	default:
		change(fw)
	}
	f.mu.Unlock()

	switch failWith {
	case 0:
		w.WriteHeader(http.StatusOK)
	case http.StatusNotFound:
		writeFakeError(w, failWith, "worker not found")
	case http.StatusConflict:
		writeFakeError(w, failWith, "worker is not running")
	default:
		writeFakeError(w, failWith, http.StatusText(failWith))
	}
}

// fakeSummary is the GET /workers entry for a worker; a nil worker has only a name.
func fakeSummary(name string, fw *fakeWorker) map[string]any {
	if fw == nil {
		fw = &fakeWorker{}
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
