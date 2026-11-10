package fleettest

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
)

// Workflow is one workflow as a Varmq serves it (spec WorkflowRecord).
// Several Varmqs may share one: a durable store they all register.
type Workflow struct {
	mu           sync.Mutex
	rec          contract.WorkflowRecord
	actionStatus int
}

// SetActionStatus makes PATCH .../actions/cancel reply with status and
// {"error":"..."}, changing nothing; 0 undoes it.
func (w *Workflow) SetActionStatus(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.actionStatus = status
}

// NewWorkflow is a Workflow holding rec.
func NewWorkflow(rec contract.WorkflowRecord) *Workflow { return &Workflow{rec: rec} }

// Record is a copy of w's record now.
func (w *Workflow) Record() contract.WorkflowRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.rec
	r.Tasks = slices.Clone(r.Tasks)
	return r
}

// cancel cancels w as varmq does, unless it ended: waiting tasks are
// cancelled. So are pending ones here, as if a worker had reached each at
// once (varmq's pre-run check). The undo jobs of completed tasks run when
// undo_on_cancel holds; noUndo turns it off first.
func (w *Workflow) cancel(noUndo bool, now int64) contract.WorkflowRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := &w.rec
	if api.WorkflowFinal(r.State) || r.FinishedAt > 0 {
		return *r
	}
	if noUndo {
		r.UndoOnCancel = false
	}
	r.Tasks = slices.Clone(r.Tasks)
	undoing := false
	for i := range r.Tasks {
		t := &r.Tasks[i]
		switch {
		case t.State == contract.JobWaiting || t.State == contract.JobPending:
			t.State, t.Reason = contract.JobCancelled, "workflow cancelled"
		case t.State == contract.JobCompleted && t.Undo != nil && r.UndoOnCancel:
			u := *t.Undo
			u.State, undoing = contract.JobPending, true
			t.Undo = &u
		}
	}
	if undoing {
		r.State = contract.WorkflowCompensating
	} else {
		r.State, r.FinishedAt = contract.WorkflowCancelled, now
	}
	return *r
}

// byID is the workflows of f with id.
func (f *Varmq) byID(id string) []*Workflow {
	var out []*Workflow
	for _, w := range f.Workflows {
		if w.Record().ID == id {
			out = append(out, w)
		}
	}
	return out
}

// workflows lists as varmq's GET /workflows: newest created_at first, then
// ID; state, offset and limit checked as the spec says.
func (f *Varmq) workflows(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	all, status := slices.Clone(f.Workflows), f.WorkflowStatus
	f.Mu.Unlock()
	switch {
	case all == nil:
		http.NotFound(w, nil)
		return
	case status != 0:
		writeFakeError(w, status, http.StatusText(status))
		return
	}
	q := r.URL.Query()
	state := contract.WorkflowState(q.Get("state"))
	offset, err1 := strconv.Atoi(cmp.Or(q.Get("offset"), "0"))
	limit, err2 := strconv.Atoi(cmp.Or(q.Get("limit"), "50"))
	if state != "" && !slices.Contains(api.WorkflowStates, state) || err1 != nil || err2 != nil ||
		offset < 0 || offset > api.MaxWorkflowOffset || limit < 1 || limit > api.MaxWorkflowLimit {
		writeFakeError(w, http.StatusBadRequest, "invalid state, offset or limit")
		return
	}
	recs := []contract.WorkflowRecord{}
	for _, wf := range all {
		if rec := wf.Record(); state == "" || rec.State == state {
			recs = append(recs, rec)
		}
	}
	slices.SortStableFunc(recs, func(a, b contract.WorkflowRecord) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	writeFakeJSON(w, http.StatusOK, recs[min(offset, len(recs)):min(offset+limit, len(recs))])
}

// workflowCounts answers as varmq's GET /workflow-counts: one entry, every
// state present.
func (f *Varmq) workflowCounts(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	all, status := slices.Clone(f.Workflows), f.WorkflowStatus
	e := api.WorkflowCounts{Store: f.WorkflowStore, Shared: f.WorkflowStore != ""}
	missing, uncounted, delay := f.NoWorkflowCounts, f.UncountedWorkflows, f.CountsDelay
	f.Mu.Unlock()
	select {
	case <-time.After(delay):
	case <-r.Context().Done():
		return
	}
	switch {
	case all == nil || missing:
		http.NotFound(w, nil)
		return
	case status != 0:
		writeFakeError(w, status, http.StatusText(status))
		return
	}
	if !uncounted {
		e.Counts = make(map[contract.WorkflowState]int64, len(api.WorkflowStates))
		for _, s := range api.WorkflowStates {
			e.Counts[s] = 0
		}
		for _, wf := range all {
			e.Counts[wf.Record().State]++
		}
	}
	writeFakeJSON(w, http.StatusOK, []api.WorkflowCounts{e})
}

// one is the workflow of r's ID, or false after answering 404 (the router's
// with no Workflows; JSON when none has the ID) or 409 (several have it).
func (f *Varmq) one(w http.ResponseWriter, r *http.Request) (*Workflow, bool) {
	f.Mu.Lock()
	served, status := f.Workflows != nil, f.WorkflowStatus
	found := f.byID(r.PathValue("id"))
	f.Mu.Unlock()
	switch {
	case !served:
		http.NotFound(w, nil)
	case status != 0:
		writeFakeError(w, status, http.StatusText(status))
	case len(found) == 0:
		writeFakeError(w, http.StatusNotFound, "workflow not found")
	case len(found) > 1:
		writeFakeError(w, http.StatusConflict, "workflow id in several stores")
	default:
		return found[0], true
	}
	return nil, false
}

func (f *Varmq) workflow(w http.ResponseWriter, r *http.Request) {
	if wf, ok := f.one(w, r); ok {
		writeFakeJSON(w, http.StatusOK, wf.Record())
	}
}

// workflowAction cancels as varmq's PATCH /workflows/{id}/actions/cancel:
// undo absent or false (true is refused), an ended workflow unchanged, the
// record after in reply.
func (f *Varmq) workflowAction(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("action") != "cancel" {
		writeFakeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	undo, has := r.URL.Query()["undo"]
	if has && (len(undo) != 1 || undo[0] != "false") {
		writeFakeError(w, http.StatusBadRequest, "invalid undo")
		return
	}
	wf, ok := f.one(w, r)
	if !ok {
		return
	}
	wf.mu.Lock()
	status := wf.actionStatus
	wf.mu.Unlock()
	if status != 0 {
		writeFakeError(w, status, http.StatusText(status))
		return
	}
	writeFakeJSON(w, http.StatusOK, wf.cancel(has, time.Now().UnixMilli()))
}

// Sample workflow IDs: the ones SampleWorkflows returns, in its order.
const (
	DiamondID = "thumb-7f3a"          // completed; a failed task absorbed by an always join
	RunningID = "etl-2026-11-08"      // running; a long edge, two tasks in flight
	SagaID    = "checkout-5521"       // compensated: a task failed, undo jobs ran
	CancelID  = "nightly-report-1107" // cancelled, no undo
)

// SampleWorkflows are four workflows created before now (Unix ms), for
// tests and screenshots: a diamond whose join is always, a running ETL
// with a long edge, a saga compensated after a failure, and a cancelled
// report. Their queues are on every backend the pages name: redisq,
// sqliteq, in-memory and other storage. Tasks that were queued carry
// their times (contract v0.9.0), undo jobs too; one in flight has no end,
// one never queued none. Each call returns new ones.
func SampleWorkflows(now int64) []*Workflow {
	const (
		sec    = int64(1000)
		minute = 60 * sec
	)
	t := func(name, queue string, state contract.JobState, deps ...string) contract.TaskRecord {
		return contract.TaskRecord{Name: name, QueueID: queue, JobID: name + "-1", State: state, Deps: deps}
	}
	// timed sets t's times, ms after its workflow's creation; start or
	// end 0: not yet
	timed := func(t *contract.TaskRecord, created, enqueued, started, finished int64) {
		t.EnqueuedAt = created + enqueued
		if started > 0 {
			t.StartedAt = created + started
		}
		if finished > 0 {
			t.FinishedAt = created + finished
		}
	}
	diamondAt, etlAt, sagaAt, reportAt := now-12*minute, now-95*sec, now-31*minute, now-2*60*minute
	diamond := []contract.TaskRecord{
		t("fetch-image", "sqliteq:9f86d081:fifo:images", contract.JobCompleted),
		t("resize", "sqliteq:9f86d081:fifo:images", contract.JobCompleted, "fetch-image"),
		t("scan-nsfw", "redisq:shop:prio:{moderation}", contract.JobFailed, "fetch-image"),
		t("publish", "s3sync/cdn", contract.JobCompleted, "resize", "scan-nsfw"),
	}
	diamond[0].ResultSize, diamond[1].ResultSize = 48213, 9120
	diamond[2].Reason = "classifier: model not loaded"
	diamond[3].Always = true
	timed(&diamond[0], diamondAt, 0, 200, 6400)
	timed(&diamond[1], diamondAt, 6400, 6500, 31*sec)
	timed(&diamond[2], diamondAt, 6400, 7900, 9100)
	timed(&diamond[3], diamondAt, 31*sec, 31200, 38*sec)

	etl := []contract.TaskRecord{
		t("extract", "redisq:shop:fifo:{etl}", contract.JobCompleted),
		t("validate", "redisq:shop:fifo:{etl}", contract.JobCompleted, "extract"),
		t("audit-log", "redisq:shop:fifo:{audit}", contract.JobPending, "extract"),
		t("transform-orders", "redisq:shop:fifo:{etl}", contract.JobCompleted, "validate"),
		t("transform-refunds", "redisq:shop:fifo:{etl}", contract.JobPending, "validate"),
		t("load-warehouse", "redisq:shop:fifo:{warehouse}", contract.JobWaiting, "transform-orders", "transform-refunds"),
		t("report", "redisq:shop:fifo:{reports}", contract.JobWaiting, "load-warehouse", "audit-log"),
	}
	etl[0].ResultSize, etl[1].ResultSize, etl[3].ResultSize = 1_204_311, 112, 802_144
	etl[6].Always = true
	timed(&etl[0], etlAt, 0, 300, 21*sec)
	timed(&etl[1], etlAt, 21*sec, 21100, 24500)
	timed(&etl[2], etlAt, 21*sec, 0, 0) // queued still
	timed(&etl[3], etlAt, 24500, 25*sec, 61*sec)
	timed(&etl[4], etlAt, 24500, 26*sec, 0) // runs

	saga := []contract.TaskRecord{
		t("reserve-stock", "redisq:shop:fifo:{inventory}", contract.JobCompleted),
		t("charge-card", "redisq:shop:fifo:{payments}", contract.JobCompleted, "reserve-stock"),
		t("book-shipping", "redisq:shop:fifo:{shipping}", contract.JobFailed, "charge-card"),
		t("send-receipt", "redisq:shop:fifo:{emails}", contract.JobCancelled, "book-shipping"),
	}
	saga[0].Undo = &contract.TaskRecord{Name: "reserve-stock", QueueID: "redisq:shop:fifo:{inventory-undo}", JobID: "release-stock-1", State: contract.JobCompleted}
	saga[1].Undo = &contract.TaskRecord{Name: "charge-card", QueueID: "redisq:shop:fifo:{payments-undo}", JobID: "refund-1", State: contract.JobCompleted, ResultSize: 64}
	saga[1].ResultSize = 256
	saga[2].Reason = "carrier API: 503 Service Unavailable after 3 attempts"
	saga[3].Reason = "dependency failed"
	timed(&saga[0], sagaAt, 0, 40, 420)
	timed(&saga[1], sagaAt, 420, 450, 1900)
	timed(&saga[2], sagaAt, 1900, 1950, 3100)
	timed(saga[1].Undo, sagaAt, 3100, 3150, 3900) // undo runs last task first
	timed(saga[0].Undo, sagaAt, 3900, 3920, 4200)

	report := []contract.TaskRecord{
		t("gather", "memory:shop-a:fifo:1", contract.JobCompleted),
		t("aggregate", "memory:shop-a:fifo:1", contract.JobCancelled, "gather"),
		t("render-pdf", "memory:shop-a:fifo:1", contract.JobCancelled, "aggregate"),
		t("email", "memory:shop-a:fifo:2", contract.JobCancelled, "render-pdf"),
	}
	for i := 1; i < 4; i++ {
		report[i].Reason = "workflow cancelled"
	}
	timed(&report[0], reportAt, 0, sec, 40*sec)

	return []*Workflow{
		NewWorkflow(contract.WorkflowRecord{ID: DiamondID, State: contract.WorkflowCompleted, CreatedAt: diamondAt, FinishedAt: diamondAt + 38*sec, UndoOnCancel: true, Tasks: diamond}),
		NewWorkflow(contract.WorkflowRecord{ID: RunningID, State: contract.WorkflowRunning, CreatedAt: etlAt, UndoOnCancel: true, Tasks: etl}),
		NewWorkflow(contract.WorkflowRecord{ID: SagaID, State: contract.WorkflowCompensated, CreatedAt: sagaAt, FinishedAt: sagaAt + 4200, UndoOnCancel: true, Tasks: saga}),
		NewWorkflow(contract.WorkflowRecord{ID: CancelID, State: contract.WorkflowCancelled, CreatedAt: reportAt, FinishedAt: reportAt + 41*sec, Tasks: report}),
	}
}
