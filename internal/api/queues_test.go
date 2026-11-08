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
