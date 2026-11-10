package fleettest

import (
	"cmp"
	"encoding/json"
	"maps"
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
	Status                           string
	Concurrency, Busy, Pending, Idle int
	Completed, Successful, Failed    uint64
	// Strategy and Sources, served by GET /workers/{name} when Sources is
	// non-nil (varmq API 1.5.0); nil: an older varmq.
	Strategy string
	Sources  []api.WorkerSource
	// QueueIDs, served as queue_ids when non-nil; nil: an older varmq.
	QueueIDs []string
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
	Workers map[string]*Worker
	// Queues by Queue ID, served by GET /queues and /queues/{id}/jobs. Nil:
	// those routes answer the router's plain-text 404, as an older varmq.
	Queues map[string]*Queue
	// Workflows served by the workflow routes, in any order. Several
	// Varmqs may share one Workflow (a durable store); one ID twice on a
	// Varmq is held by two of its storages (409 by ID). Nil: those routes
	// answer the router's plain-text 404, as varmq before API 1.3.0.
	Workflows []*Workflow
	// WorkflowStatus, when non-zero, makes every workflow route reply with
	// it and {"error":"..."}: storage that cannot be read.
	WorkflowStatus int
	// WorkflowStore names the durable store that holds Workflows: GET
	// /workflow-counts answers one shared entry. "": one in-memory entry.
	WorkflowStore string
	// NoWorkflowCounts: GET /workflow-counts answers the router's 404, as
	// varmq before the route. UncountedWorkflows: its entry has no counts.
	NoWorkflowCounts, UncountedWorkflows bool
	received                             []string // "METHOD escaped-path" of every request received

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
	mux.HandleFunc("GET /queues", f.queues)
	mux.HandleFunc("GET /queues/{id}/jobs", f.jobs)
	mux.HandleFunc("GET /queues/{id}/jobs/{jobId}", f.job)
	mux.HandleFunc("PATCH /queues/{id}/jobs/{jobId}/actions/cancel", f.jobCancel)
	mux.HandleFunc("PATCH /queues/{id}/actions/{action}", f.queueAction)
	mux.HandleFunc("GET /workflows", f.workflows)
	mux.HandleFunc("GET /workflow-counts", f.workflowCounts)
	mux.HandleFunc("GET /workflows/{id}", f.workflow)
	mux.HandleFunc("PATCH /workflows/{id}/actions/{action}", f.workflowAction)

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
		if fw.Sources != nil {
			body["strategy"], body["sources"] = fw.Strategy, fw.Sources
		}
		body["metrics"] = map[string]uint64{
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

// Queue is one queue as a Varmq serves it, as spec-shaped JSON. Several
// Varmqs may share one Queue: its storage, the pause flag included.
type Queue struct {
	Summary string              // its GET /queues entry, a QueueSummary object
	Jobs    map[string][]string // Job objects by Job State, in the order served; a cancel moves one to "cancelled"
	// ActionStatus, when non-zero, makes PATCH /queues/{id}/actions/*
	// reply with it, changing nothing: 404 as the router's plain text (an
	// older varmq), others with {"error":"..."}.
	ActionStatus int
	// ActionErr, when set, makes a queue action store the flag, then answer
	// 200 {"id":...,"error":ActionErr}: varmq when re-reading the queue fails.
	ActionErr string
	// ActionDrop closes the connection on a queue action: a transport error.
	ActionDrop bool
	// JobsStatus, when non-zero, makes GET /queues/{id}/jobs reply with it
	// and {"error":"..."}.
	JobsStatus int
	// JobStatus and CancelStatus, when non-zero, make GET
	// /queues/{id}/jobs/{jobId} and its cancel reply with it: 404 as the
	// router's plain text (varmq before API 1.4.0), others with
	// {"error":"..."} (501: storage without JobReader or JobCanceller).
	JobStatus, CancelStatus int

	mu sync.Mutex // guards Summary, and Jobs while served, across the Varmqs sharing it
}

// summary is q's GET /queues entry.
func (q *Queue) summary() json.RawMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return json.RawMessage(q.Summary)
}

// Paused reports whether q's entry says paused.
func (q *Queue) Paused() bool {
	var e struct{ Paused bool }
	_ = json.Unmarshal(q.summary(), &e)
	return e.Paused
}

func (f *Varmq) queues(w http.ResponseWriter, _ *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Queues == nil {
		http.NotFound(w, nil)
		return
	}
	ids := slices.Sorted(maps.Keys(f.Queues))
	out := make([]json.RawMessage, len(ids))
	for i, id := range ids {
		out[i] = f.Queues[id].summary()
	}
	writeFakeJSON(w, http.StatusOK, out)
}

// queueAction pauses or resumes a queue as varmq's handleQueueAction does:
// idempotent, paused_at kept on a repeat pause, the entry in reply.
func (f *Varmq) queueAction(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	var q *Queue
	if f.Queues != nil {
		q = f.Queues[r.PathValue("id")]
	}
	f.Mu.Unlock()
	switch {
	case f.Queues == nil:
		http.NotFound(w, nil)
		return
	case q == nil:
		writeFakeError(w, http.StatusNotFound, "queue not found")
		return
	case q.ActionStatus == http.StatusNotFound:
		http.NotFound(w, nil)
		return
	case q.ActionStatus != 0:
		writeFakeError(w, q.ActionStatus, http.StatusText(q.ActionStatus))
		return
	case q.ActionDrop:
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = c.Close()
		}
		return
	}
	action := r.PathValue("action")
	if action != "pause" && action != "resume" {
		writeFakeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	q.mu.Lock()
	var e map[string]any
	_ = json.Unmarshal([]byte(q.Summary), &e)
	if action == "resume" {
		e["paused"] = false
		delete(e, "paused_at")
	} else if e["paused"] != true {
		e["paused"], e["paused_at"] = true, time.Now().UnixMilli()
	}
	b, _ := json.Marshal(e)
	q.Summary = string(b)
	if q.ActionErr != "" {
		b, _ = json.Marshal(map[string]any{"id": e["id"], "error": q.ActionErr})
	}
	q.mu.Unlock()
	writeFakeJSON(w, http.StatusOK, json.RawMessage(b))
}

func (f *Varmq) jobs(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Queues == nil {
		http.NotFound(w, nil)
		return
	}
	q := f.Queues[r.PathValue("id")]
	if q == nil {
		writeFakeError(w, http.StatusNotFound, "queue not found")
		return
	}
	if q.JobsStatus != 0 {
		writeFakeError(w, q.JobsStatus, http.StatusText(q.JobsStatus))
		return
	}
	q.mu.Lock() // Jobs: a cancel changes them, on any Varmq sharing q
	defer q.mu.Unlock()
	state := r.URL.Query().Get("state")
	offset, err1 := strconv.Atoi(cmp.Or(r.URL.Query().Get("offset"), "0"))
	limit, err2 := strconv.Atoi(cmp.Or(r.URL.Query().Get("limit"), "50"))
	_, listsCancelled := q.Jobs["cancelled"] // a varmq that lists it (spec 1.3.0)
	known := slices.Contains([]string{"pending", "processing", "completed", "failed"}, state) || state == "cancelled" && listsCancelled
	if !known || err1 != nil || err2 != nil || offset < 0 || limit < 1 || limit > 1000 {
		writeFakeError(w, http.StatusBadRequest, "invalid state, offset or limit")
		return
	}
	all := q.Jobs[state]
	page := all[min(offset, len(all)):min(offset+limit, len(all))]
	out := make([]json.RawMessage, len(page))
	for i, j := range page {
		out[i] = listed(j)
	}
	writeFakeJSON(w, http.StatusOK, out)
}

// listed is Job object j as a list page serves it: result left out,
// result_size its length.
func listed(j string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(j), &m) != nil || m["result"] == nil {
		return json.RawMessage(j)
	}
	m["result_size"], _ = json.Marshal(len(m["result"]))
	delete(m, "result")
	b, _ := json.Marshal(m)
	return b
}

// findJob is the state and index in q.Jobs of the job with ID id; ok false
// when none. Hold q.mu.
func (q *Queue) findJob(id string) (state string, i int, ok bool) {
	for st, jobs := range q.Jobs {
		for i, j := range jobs {
			var e struct{ ID string }
			if json.Unmarshal([]byte(j), &e) == nil && e.ID == id {
				return st, i, true
			}
		}
	}
	return "", 0, false
}

// jobQueue is the queue of r's {id} for a one-job route, or answers as
// varmq does and returns nil: no queue routes, no such queue, status.
// Hold f.Mu.
func (f *Varmq) jobQueue(w http.ResponseWriter, r *http.Request, status func(*Queue) int) *Queue {
	if f.Queues == nil {
		http.NotFound(w, nil)
		return nil
	}
	q := f.Queues[r.PathValue("id")]
	switch {
	case q == nil:
		writeFakeError(w, http.StatusNotFound, "queue not found")
	case status(q) == http.StatusNotFound:
		http.NotFound(w, nil)
	case status(q) != 0:
		writeFakeError(w, status(q), http.StatusText(status(q)))
	default:
		return q
	}
	return nil
}

// job serves GET /queues/{id}/jobs/{jobId} (API 1.4.0): the job, result
// and all.
func (f *Varmq) job(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	q := f.jobQueue(w, r, func(q *Queue) int { return q.JobStatus })
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	st, i, ok := q.findJob(r.PathValue("jobId"))
	if !ok {
		writeFakeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeFakeJSON(w, http.StatusOK, json.RawMessage(q.Jobs[st][i]))
}

// jobCancel serves PATCH /queues/{id}/jobs/{jobId}/actions/cancel as an
// in-memory queue does: a waiting or running job is cancelled at once
// (202), an ended one is 409, an unknown one 404.
func (f *Varmq) jobCancel(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	q := f.jobQueue(w, r, func(q *Queue) int { return q.CancelStatus })
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	st, i, ok := q.findJob(r.PathValue("jobId"))
	switch {
	case !ok:
		writeFakeError(w, http.StatusNotFound, "job not found")
		return
	case st != "pending" && st != "processing":
		writeFakeError(w, http.StatusConflict, "varmq: job already ended")
		return
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(q.Jobs[st][i]), &m)
	m["state"], m["finished_at"] = "cancelled", time.Now().UnixMilli()
	b, _ := json.Marshal(m)
	q.Jobs[st] = slices.Delete(slices.Clone(q.Jobs[st]), i, i+1)
	q.Jobs["cancelled"] = append([]string{string(b)}, q.Jobs["cancelled"]...)
	w.WriteHeader(http.StatusAccepted)
}

// SampleJobs are jobs in every state, keyed by state as Queue.Jobs, with
// times before now (Unix ms), for tests and screenshots: two pending, one
// running, two completed (one with a result: fetch-image-1, the first task
// of SampleWorkflows' diamond), one failed after two attempts, one
// cancelled while it waited (no start time).
func SampleJobs(now int64) map[string][]string {
	const sec = int64(1000)
	j := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return string(b)
	}
	return map[string][]string{
		"pending": {
			j(map[string]any{"id": "ord-1042", "state": "pending", "payload": map[string]any{"order": 1042, "sku": "TEE-BLK-M", "qty": 2}, "enqueued_at": now - 14*sec, "attempts": 0}),
			j(map[string]any{"id": "ord-1043", "state": "pending", "payload": map[string]any{"order": 1043, "sku": "MUG-WHT", "qty": 1}, "enqueued_at": now - 3*sec, "attempts": 0}),
		},
		"processing": {
			j(map[string]any{"id": "ord-1039", "state": "processing", "payload": map[string]any{"order": 1039, "sku": "HOOD-GRY-L", "qty": 1, "gift": true}, "enqueued_at": now - 95*sec, "started_at": now - 41*sec, "attempts": 1}),
		},
		"completed": {
			j(map[string]any{"id": "fetch-image-1", "state": "completed", "payload": map[string]any{"url": "https://cdn.example.com/u/7f3a/raw.jpg"}, "enqueued_at": now - 12*60*sec, "started_at": now - 12*60*sec + 840, "finished_at": now - 12*60*sec + 4210, "attempts": 1,
				"result": map[string]any{"bytes": 48213, "width": 1600, "height": 1200, "etag": "\"9b2cf535f27731c974343645a3985328\""}}),
			j(map[string]any{"id": "ord-1031", "state": "completed", "payload": map[string]any{"order": 1031, "sku": "CAP-RED", "qty": 3}, "enqueued_at": now - 20*60*sec, "started_at": now - 20*60*sec + 120, "finished_at": now - 20*60*sec + 1830, "attempts": 1}),
		},
		"failed": {
			j(map[string]any{"id": "ord-1027", "state": "failed", "payload": map[string]any{"order": 1027, "sku": "TEE-WHT-S", "qty": 1}, "enqueued_at": now - 55*60*sec, "started_at": now - 54*60*sec, "finished_at": now - 54*60*sec + 30*sec, "attempts": 2,
				"errors": []map[string]any{
					{"attempt": 1, "at": now - 55*60*sec + 31*sec, "error": "dial tcp 10.0.3.7:5432: connect: connection refused"},
					{"attempt": 2, "at": now - 54*60*sec + 30*sec, "error": "payment gateway: context deadline exceeded after 30s"},
				}}),
		},
		"cancelled": {
			j(map[string]any{"id": "ord-1035", "state": "cancelled", "payload": map[string]any{"order": 1035, "sku": "SOCK-BLU", "qty": 4}, "enqueued_at": now - 30*60*sec, "finished_at": now - 29*60*sec, "attempts": 0}),
		},
	}
}

// fakeSummary is the GET /workers entry for a worker; a nil worker has only a name.
func fakeSummary(name string, fw *Worker) map[string]any {
	if fw == nil {
		fw = &Worker{}
	}
	m := map[string]any{
		"name":             name,
		"status":           fw.Status,
		"concurrency":      fw.Concurrency,
		"num_processing":   fw.Busy,
		"num_pending":      fw.Pending,
		"num_idle_workers": fw.Idle,
	}
	if fw.QueueIDs != nil {
		m["queue_ids"] = fw.QueueIDs
	}
	return m
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

// SourceWorkers are workers on queues redis, sqlite and memory (Queue IDs)
// as varmq API 1.5.0 serves them, for tests and screenshots: "orders"
// picks by priority from all three (1, 5, none set), "emails" pulls from
// redis alone, "fanout" takes turns between sqlite, a topic and storage
// with no Queue ID, and "legacy" is an older varmq's: queue_ids only.
func SourceWorkers(redis, sqlite, memory string) map[string]*Worker {
	p := func(n int64) *int64 { return &n }
	q := func(id string, prio *int64) api.WorkerSource {
		return api.WorkerSource{QueueID: id, Kind: "queue", Priority: prio}
	}
	return map[string]*Worker{
		"orders": {Status: "Running", Concurrency: 8, Busy: 2, Idle: 6, QueueIDs: []string{memory, redis, sqlite},
			Strategy: "priority", Sources: []api.WorkerSource{q(redis, p(1)), q(sqlite, p(5)), q(memory, nil)}},
		"emails": {Status: "Running", Concurrency: 2, Idle: 2, QueueIDs: []string{redis},
			Strategy: "priority", Sources: []api.WorkerSource{q(redis, nil)}},
		"fanout": {Status: "Idle", Concurrency: 1, Idle: 1, QueueIDs: []string{sqlite},
			Strategy: "round_robin", Sources: []api.WorkerSource{q(sqlite, p(2)), {Kind: "topic"}, {Kind: "queue"}}},
		"legacy": {Status: "Running", Concurrency: 1, Idle: 1, QueueIDs: []string{redis, sqlite}},
	}
}
