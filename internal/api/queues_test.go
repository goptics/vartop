package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goptics/varmq/contract"
)

// Spec-shaped fixtures (varmq spec/openapi.yaml 1.0.0).
const (
	queuesBody = `[{"id":"redisq:ns:fifo:{orders}","counts":{"pending":3,"processing":1,"completed":10,"failed":2},"totals":{"submitted":16,"completed":10,"failed":2}},
		{"id":"redisq:prio:{broken}","error":"redis: connection refused"}]`
	jobsBody = `[{"id":"order-42","state":"failed","payload":{"sku":"A1","qty":12345678901234567890},"enqueued_at":1762228800000,"started_at":1762228801250,"finished_at":1762228801900,"attempts":2,"errors":[{"attempt":1,"at":1762228801000,"error":"timeout"},{"attempt":2,"at":1762228801900,"error":"out of stock"}]},
		{"id":"","state":"pending","payload":"plain","attempts":0}]`
)

func TestListQueues(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, queuesBody)
	qs, ok, err := NewClient(srv.URL, "tok").ListQueues(context.Background())
	if err != nil || !ok {
		t.Fatalf("ListQueues = ok %v, err %v", ok, err)
	}
	if len(qs) != 2 || qs[0].ID != "redisq:ns:fifo:{orders}" || qs[0].Counts.Pending != 3 || qs[0].Totals.Submitted != 16 {
		t.Fatalf("queues = %+v", qs)
	}
	if qs[1].Counts != nil || qs[1].Err != "redis: connection refused" {
		t.Errorf("error entry = %+v", qs[1])
	}
	if r := reqs()[0]; r.Path != "/queues" || r.Auth != "Bearer tok" {
		t.Errorf("request = %+v", r)
	}
}

// An older varmq answers /queues with the router's plain-text 404: no
// queue data, not an error. So does a 405.
func TestListQueuesUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv, _ := recordingServer(t, status, nil, "404 page not found\n")
		qs, ok, err := NewClient(srv.URL, "").ListQueues(context.Background())
		if err != nil || ok || qs != nil {
			t.Errorf("%d: ListQueues = %v, ok %v, err %v; want none, not ok, no error", status, qs, ok, err)
		}
	}
	srv, _ := recordingServer(t, http.StatusInternalServerError, nil, `{"error":"boom"}`)
	if _, _, err := NewClient(srv.URL, "").ListQueues(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("500: err = %v, want boom", err)
	}
}

// The Queue ID is one escaped path segment: '{', '}' and '/' escaped, ':' kept.
func TestListJobsEscapesQueueID(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, jobsBody)
	jobs, ok, err := NewClient(srv.URL, "").ListJobs(context.Background(), "redisq:ns:fifo:{a/b}", contract.JobFailed, 50, 25)
	if err != nil || !ok {
		t.Fatalf("ListJobs = ok %v, err %v", ok, err)
	}
	r := reqs()[0]
	if want := "/queues/redisq:ns:fifo:%7Ba%2Fb%7D/jobs"; r.EscapedPath != want {
		t.Errorf("escaped path = %s, want %s", r.EscapedPath, want)
	}
	j := jobs[0]
	if j.ID != "order-42" || j.Attempts != 2 || j.EnqueuedAt != 1762228800000 || j.FinishedAt != 1762228801900 || len(j.Errors) != 2 || j.Errors[1] != (contract.JobError{Attempt: 2, At: 1762228801900, Error: "out of stock"}) {
		t.Errorf("job = %+v", j)
	}
	// The payload is kept as sent: no float64 rounding of big numbers.
	if got := string(j.Payload); got != `{"sku":"A1","qty":12345678901234567890}` {
		t.Errorf("payload = %s", got)
	}
	if jobs[1].StartedAt != 0 || string(jobs[1].Payload) != `"plain"` {
		t.Errorf("pending job = %+v", jobs[1])
	}
}

func TestListJobsQuery(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	if _, _, err := NewClient(srv.URL, "").ListJobs(context.Background(), "q", contract.JobPending, 0, 50); err != nil {
		t.Fatal(err)
	}
	if got != "limit=50&offset=0&state=pending" {
		t.Errorf("query = %s", got)
	}
}

// A 404 on jobs is "this instance does not have the queue": not ok, no error.
func TestListJobsNotFound(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, nil, `{"error":"queue not found"}`)
	jobs, ok, err := NewClient(srv.URL, "").ListJobs(context.Background(), "q", contract.JobPending, 0, 50)
	if err != nil || ok || jobs != nil {
		t.Errorf("ListJobs = %v, ok %v, err %v", jobs, ok, err)
	}
	srv, _ = recordingServer(t, http.StatusBadRequest, nil, `{"error":"invalid state"}`)
	if _, _, err := NewClient(srv.URL, "").ListJobs(context.Background(), "q", "x", 0, 50); err == nil {
		t.Error("400: no error")
	}
}

// paused is read as sent: absent (varmq before 1.2.0) is nil, not false.
func TestListQueuesPaused(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, nil, `[{"id":"a","counts":{},"totals":{},"paused":true,"paused_at":1762228800000},
		{"id":"b","counts":{},"totals":{},"paused":false},{"id":"c","counts":{},"totals":{}}]`)
	qs, _, err := NewClient(srv.URL, "").ListQueues(context.Background())
	if err != nil || len(qs) != 3 {
		t.Fatalf("ListQueues = %+v, %v", qs, err)
	}
	for i, want := range []struct{ paused, can bool }{{true, true}, {false, true}, {false, false}} {
		if qs[i].IsPaused() != want.paused || qs[i].CanPause() != want.can {
			t.Errorf("%s: paused %v can %v, want %+v", qs[i].ID, qs[i].IsPaused(), qs[i].CanPause(), want)
		}
	}
	if qs[0].PausedAt != 1762228800000 {
		t.Errorf("paused_at = %d", qs[0].PausedAt)
	}
}

// QueueAction PATCHes one escaped path segment and returns the entry sent back.
func TestQueueAction(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, `{"id":"redisq:ns:fifo:{a/b}","counts":{"pending":1,"processing":0,"completed":0,"failed":0},"totals":{"submitted":1,"completed":0,"failed":0},"paused":true,"paused_at":5}`)
	q, err := NewClient(srv.URL, "tok").QueueAction(context.Background(), "redisq:ns:fifo:{a/b}", "pause")
	if err != nil || !q.IsPaused() || q.PausedAt != 5 || q.Counts.Pending != 1 {
		t.Fatalf("QueueAction = %+v, %v", q, err)
	}
	r := reqs()[0]
	if r.Method != http.MethodPatch || r.EscapedPath != "/queues/redisq:ns:fifo:%7Ba%2Fb%7D/actions/pause" || r.Auth != "Bearer tok" {
		t.Errorf("request = %+v", r)
	}
}

// An older varmq (router 404 or 405) and storage that cannot pause (501)
// are unsupported; a queue the server lacks (JSON 404) and a storage error
// (503) are not.
func TestQueueActionUnsupported(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusNotFound, "404 page not found\n", true},
		{http.StatusMethodNotAllowed, "Method Not Allowed\n", true},
		{http.StatusNotImplemented, `{"error":"varmq: storage does not support pause"}`, true},
		{http.StatusNotFound, `{"error":"queue not found"}`, false},
		{http.StatusServiceUnavailable, `{"error":"redis down"}`, false},
	} {
		srv, _ := recordingServer(t, c.status, nil, c.body)
		_, err := NewClient(srv.URL, "").QueueAction(context.Background(), "q", "resume")
		if err == nil || QueueActionUnsupported(err) != c.want {
			t.Errorf("%d %s: err %v, unsupported %v, want %v", c.status, c.body, err, QueueActionUnsupported(err), c.want)
		}
	}
	if QueueActionUnsupported(context.Canceled) {
		t.Error("a transport error is not unsupported")
	}
}

// A spec-shaped WorkflowRecord (varmq spec 1.3.0).
const workflowBody = `{"id":"wf-1","state":"running","created_at":1762228800000,"undo_on_cancel":true,
	"tasks":[{"name":"a","state":"completed","queue_id":"q","job_id":"a-1","result_size":12,"undo":{"name":"a","state":"pending","queue_id":"u","job_id":"a-u"}},
	{"name":"b","state":"waiting","deps":["a"],"always":true,"queue_id":"q","job_id":"b-1"}]}`

// ListWorkflows asks for a page, the state only when one is given, and
// decodes the records, undo sub-records included.
func TestListWorkflows(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, "["+workflowBody+"]")
	c := NewClient(srv.URL, "tok")
	wfs, ok, err := c.ListWorkflows(context.Background(), contract.WorkflowFailed, 25, 50)
	if err != nil || !ok || len(wfs) != 1 {
		t.Fatalf("ListWorkflows = %v, ok %v, err %v", wfs, ok, err)
	}
	w := wfs[0]
	if w.ID != "wf-1" || w.State != contract.WorkflowRunning || w.CreatedAt != 1762228800000 || !w.UndoOnCancel || len(w.Tasks) != 2 ||
		w.Tasks[0].Undo == nil || w.Tasks[0].Undo.JobID != "a-u" || !w.Tasks[1].Always || w.Tasks[1].Deps[0] != "a" || w.Tasks[0].ResultSize != 12 {
		t.Errorf("record = %+v", w)
	}
	if r := reqs()[0]; r.Path != "/workflows" || r.Query != "limit=50&offset=25&state=failed" || r.Auth != "Bearer tok" {
		t.Errorf("request = %+v", r)
	}
	if _, _, err := c.ListWorkflows(context.Background(), "", 0, 10); err != nil || reqs()[1].Query != "limit=10&offset=0" {
		t.Errorf("all states: err %v, query %q", err, reqs()[1].Query)
	}
}

// varmq before 1.3.0 has no /workflows: the router's 404 or a 405 is no
// workflow data, not an error; a 503 (every store failed) is one.
func TestListWorkflowsUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv, _ := recordingServer(t, status, nil, "404 page not found\n")
		wfs, ok, err := NewClient(srv.URL, "").ListWorkflows(context.Background(), "", 0, 10)
		if err != nil || ok || wfs != nil {
			t.Errorf("%d: ListWorkflows = %v, ok %v, err %v", status, wfs, ok, err)
		}
	}
	srv, _ := recordingServer(t, http.StatusServiceUnavailable, nil, `{"error":"redis down"}`)
	if _, ok, err := NewClient(srv.URL, "").ListWorkflows(context.Background(), "", 0, 10); ok || err == nil || !strings.Contains(err.Error(), "redis down") {
		t.Errorf("503: ok %v, err %v", ok, err)
	}
}

// GetWorkflow escapes the ID as one segment; its errors are told apart:
// not found (JSON 404), ambiguous (409), no route (router 404).
func TestGetWorkflow(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, workflowBody)
	w, err := NewClient(srv.URL, "").GetWorkflow(context.Background(), "a/b c")
	if err != nil || w.ID != "wf-1" || reqs()[0].EscapedPath != "/workflows/a%2Fb%20c" {
		t.Errorf("GetWorkflow = %+v, %v; path %q", w, err, reqs()[0].EscapedPath)
	}
	for _, c := range []struct {
		status                    int
		body                      string
		notFound, ambiguous, gone bool
	}{
		{http.StatusNotFound, `{"error":"workflow not found"}`, true, false, false},
		{http.StatusConflict, `{"error":"workflow id in several stores"}`, false, true, false},
		{http.StatusNotFound, "404 page not found\n", false, false, true},
		{http.StatusServiceUnavailable, `{"error":"redis down"}`, false, false, false},
	} {
		srv, _ := recordingServer(t, c.status, nil, c.body)
		_, err := NewClient(srv.URL, "").GetWorkflow(context.Background(), "wf-1")
		if err == nil || WorkflowNotFound(err) != c.notFound || WorkflowAmbiguous(err) != c.ambiguous || RouteMissing(err) != c.gone {
			t.Errorf("%d %s: err %v; not found %v, ambiguous %v, no route %v", c.status, c.body, err, WorkflowNotFound(err), WorkflowAmbiguous(err), RouteMissing(err))
		}
	}
	for _, id := range []string{".", ".."} { // would name another route: not sent
		srv, reqs := recordingServer(t, http.StatusOK, nil, workflowBody)
		c := NewClient(srv.URL, "")
		_, err := c.GetWorkflow(context.Background(), id)
		_, err2 := c.CancelWorkflow(context.Background(), id, false)
		if !WorkflowNotFound(err) || !WorkflowNotFound(err2) || len(reqs()) != 0 {
			t.Errorf("%q: %v, %v, %d requests", id, err, err2, len(reqs()))
		}
	}
	if WorkflowNotFound(context.Canceled) || WorkflowAmbiguous(context.Canceled) || RouteMissing(context.Canceled) {
		t.Error("a transport error is none of them")
	}
}

// CancelWorkflow PATCHes the cancel route, with undo=false only when asked
// (the spec refuses undo=true), and returns the record sent back.
func TestCancelWorkflow(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, strings.Replace(workflowBody, `"running"`, `"compensating"`, 1))
	c := NewClient(srv.URL, "tok")
	w, err := c.CancelWorkflow(context.Background(), "wf/1", false)
	if err != nil || w.State != contract.WorkflowCompensating {
		t.Fatalf("CancelWorkflow = %+v, %v", w, err)
	}
	if _, err := c.CancelWorkflow(context.Background(), "wf/1", true); err != nil {
		t.Fatal(err)
	}
	r := reqs()
	if r[0].Method != http.MethodPatch || r[0].EscapedPath != "/workflows/wf%2F1/actions/cancel" || r[0].Query != "" || r[0].Auth != "Bearer tok" {
		t.Errorf("cancel = %+v", r[0])
	}
	if r[1].Query != "undo=false" {
		t.Errorf("cancel without undo: query %q", r[1].Query)
	}
}

// WorkflowFinal: the five ended states; running, compensating and a state
// a later varmq adds are not.
func TestWorkflowFinal(t *testing.T) {
	for _, s := range WorkflowStates {
		want := s != contract.WorkflowRunning && s != contract.WorkflowCompensating
		if WorkflowFinal(s) != want {
			t.Errorf("WorkflowFinal(%s) = %v", s, !want)
		}
	}
	if WorkflowFinal("paused") {
		t.Error("an unknown state is final")
	}
}

// GetJob sends each ID as one escaped segment, "." and ".." as %2E and
// %2E%2E, and decodes the result; a real ServeMux routes each to the job
// route with the ID as sent.
func TestGetJobEscapesIDs(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /queues/{id}/jobs/{jobId}", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.EscapedPath()+" "+r.PathValue("id")+" "+r.PathValue("jobId"))
		_, _ = w.Write([]byte(`{"id":"x","state":"completed","payload":{"a":1},"attempts":1,"result":{"ok":true}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "")
	for _, id := range []string{"a/b", "{x}", ".", "..", "wf:w:t"} {
		j, ok, err := c.GetJob(context.Background(), "redisq:ns:fifo:{a/b}", id)
		if err != nil || !ok || string(j.Result) != `{"ok":true}` {
			t.Fatalf("%q: GetJob = %+v, ok %v, err %v", id, j, ok, err)
		}
	}
	q := "/queues/redisq:ns:fifo:%7Ba%2Fb%7D/jobs/"
	want := []string{
		q + "a%2Fb redisq:ns:fifo:{a/b} a/b",
		q + "%7Bx%7D redisq:ns:fifo:{a/b} {x}",
		q + "%2E redisq:ns:fifo:{a/b} .",
		q + "%2E%2E redisq:ns:fifo:{a/b} ..",
		q + "wf:w:t redisq:ns:fifo:{a/b} wf:w:t",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A route an older varmq lacks (router 404, 405) and storage without the
// interface (501) are not ok, no error; a JSON 404 is an error, so the
// caller asks the next unit.
func TestGetJobAndCancelUnsupported(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		unsup  bool
	}{
		{http.StatusNotFound, "404 page not found\n", true},
		{http.StatusMethodNotAllowed, "Method Not Allowed\n", true},
		{http.StatusNotImplemented, `{"error":"storage can't read a job by id"}`, true},
		{http.StatusNotFound, `{"error":"job not found"}`, false},
		{http.StatusConflict, `{"error":"varmq: job already ended"}`, false},
	} {
		srv, _ := recordingServer(t, c.status, nil, c.body)
		cl := NewClient(srv.URL, "")
		_, ok, err := cl.GetJob(context.Background(), "q", "j")
		if ok || (err == nil) != c.unsup {
			t.Errorf("%d %s: GetJob ok %v err %v, want unsupported %v", c.status, c.body, ok, err, c.unsup)
		}
		ok, err = cl.CancelJob(context.Background(), "q", "j")
		if ok || (err == nil) != c.unsup {
			t.Errorf("%d %s: CancelJob ok %v err %v, want unsupported %v", c.status, c.body, ok, err, c.unsup)
		}
	}
}

// CancelJob PATCHes the cancel route of one job; 202 is ok.
func TestCancelJob(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusAccepted, nil, "")
	ok, err := NewClient(srv.URL, "tok").CancelJob(context.Background(), "redisq:ns:fifo:{a/b}", "..")
	if err != nil || !ok {
		t.Fatalf("CancelJob = %v, %v", ok, err)
	}
	r := reqs()[0]
	if r.Method != http.MethodPatch || r.EscapedPath != "/queues/redisq:ns:fifo:%7Ba%2Fb%7D/jobs/%2E%2E/actions/cancel" || r.Auth != "Bearer tok" {
		t.Errorf("request = %+v", r)
	}
}

// ListWorkflowCounts decodes the entries as sent: a store without counts
// keeps Counts nil. The router's 404 (older varmq) is no counts, not an
// error; a 503 is one.
func TestListWorkflowCounts(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK, nil, `[
		{"shared":false,"counts":{"running":2,"completed":1,"failed":1,"compensating":0,"compensated":0,"compensation_failed":0,"cancelled":0}},
		{"store":"3f9a1c0e77b2","shared":true,"counts":{"running":4,"completed":9,"failed":0,"compensating":0,"compensated":3,"compensation_failed":1,"cancelled":0}},
		{"store":"a81b22c0d9ef","shared":true}]`)
	es, ok, err := NewClient(srv.URL, "tok").ListWorkflowCounts(context.Background())
	if err != nil || !ok || len(es) != 3 {
		t.Fatalf("ListWorkflowCounts = %+v, ok %v, err %v", es, ok, err)
	}
	if es[0].Shared || es[0].Store != "" || es[0].Counts[contract.WorkflowRunning] != 2 ||
		!es[1].Shared || es[1].Store != "3f9a1c0e77b2" || es[1].Counts[contract.WorkflowCompensationFailed] != 1 ||
		!es[2].Shared || es[2].Counts != nil {
		t.Errorf("entries = %+v", es)
	}
	if r := reqs()[0]; r.Path != "/workflow-counts" || r.Auth != "Bearer tok" {
		t.Errorf("request = %+v", r)
	}
	srv, _ = recordingServer(t, http.StatusNotFound, nil, "404 page not found\n")
	if es, ok, err := NewClient(srv.URL, "").ListWorkflowCounts(context.Background()); err != nil || ok || es != nil {
		t.Errorf("404: %+v, ok %v, err %v", es, ok, err)
	}
	srv, _ = recordingServer(t, http.StatusServiceUnavailable, nil, `{"error":"redis down"}`)
	if _, ok, err := NewClient(srv.URL, "").ListWorkflowCounts(context.Background()); ok || err == nil {
		t.Errorf("503: ok %v, err %v", ok, err)
	}
}
