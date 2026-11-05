package web

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// tenantQueue is a Redis queue whose key holds a '/', as the spec allows.
const tenantID = "redisq:fifo:{tenant/orders}"

func tenantQueue() *fleettest.Queue {
	return &fleettest.Queue{
		Summary: `{"id":"redisq:fifo:{tenant/orders}","counts":{"pending":2,"processing":0,"completed":7,"failed":1},"totals":{"submitted":10,"completed":7,"failed":1}}`,
		Jobs: map[string][]string{
			"pending": {`{"id":"","state":"pending","payload":{"n":1},"enqueued_at":1762228800000,"attempts":0}`, `{"id":"","state":"pending","payload":{"n":2},"attempts":0}`},
			"failed":  {`{"id":"order-42","state":"failed","payload":{"sku":"<A1>"},"enqueued_at":1762228800000,"started_at":1762228801250,"finished_at":1762228801900,"attempts":2,"errors":[{"attempt":1,"at":1762228801000,"error":"timeout"},{"attempt":2,"at":1762228801900,"error":"out of stock"}]}`},
		},
	}
}

// queueFakes returns the two varmqs that share the queue, and all fakes.
func queueFakes(t *testing.T) ([]*fleettest.Varmq, []*httptest.Server) {
	t.Helper()
	varmqs, fakes := threeVarmqs(t)
	for _, v := range varmqs[:2] { // a and b share the queue; c is an older varmq
		v.Mu.Lock()
		v.Queues = map[string]*fleettest.Queue{tenantID: tenantQueue()}
		v.Mu.Unlock()
	}
	return varmqs[:2], fakes
}

func TestQueuesPage(t *testing.T) {
	_, fakes := queueFakes(t)
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, "/queues", false)
	if status != http.StatusOK {
		t.Fatalf("GET /queues = %d:\n%s", status, body)
	}
	if n := strings.Count(body, ">"+tenantID+"</a>"); n != 1 {
		t.Errorf("queue listed %d times, want once", n)
	}
	text := visibleText(body)
	// One copy of the shared queue's numbers: 10 submitted, not 20.
	for _, want := range []string{"10", "No queue routes on: instance-", `aria-current="page">Queues`} {
		if !strings.Contains(text, want) && !strings.Contains(body, want) {
			t.Errorf("GET /queues lacks %q", want)
		}
	}
	if strings.Contains(text, "20") {
		t.Errorf("GET /queues summed the shared queue:\n%s", text)
	}
	if !strings.Contains(body, `href="/queues/redisq:fifo:%7Btenant%2Forders%7D?server=multi"`) {
		t.Errorf("GET /queues lacks the escaped job page link:\n%s", body)
	}
	if strings.Contains(body, testToken) {
		t.Error("the page carries the token")
	}
}

func TestQueueJobsPage(t *testing.T) {
	shared, fakes := queueFakes(t)
	srv, _ := newTestServer(t, fakes...)
	page := "/queues/redisq:fifo:%7Btenant%2Forders%7D"

	status, body := fetch(t, srv, page+"?server=multi&state=failed", false)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", page, status, body)
	}
	for _, want := range []string{
		`<h1 class="qid">` + tenantID + "</h1>",
		"order-42", "2 attempts",
		`<span class="err">out of stock</span>`,                                     // summary: the final error
		`<span class="what">Attempt 1</span><time datetime="2025-11-04T04:00:01Z">`, // errors oldest first
		`<code class="err">timeout</code></li><li><span class="what">Attempt 2</span><time datetime="2025-11-04T04:00:01.9Z">`,
		"&#34;sku&#34;: &#34;", // indented payload
		`<time datetime="2025-11-04T04:00:01.9Z">`,
		`class="state-failed" aria-current="page"><span class="label">failed</span><span class="count">1</span>`,
		`<a href="/queues/redisq:fifo:%7Btenant%2Forders%7D?server=multi" class="state-pending"><span class="label">pending</span><span class="count">2</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failed page lacks %q", want)
		}
	}
	// either sharer may serve the jobs: unit order is not fixed
	want := "GET /queues/redisq:fifo:%7Btenant%2Forders%7D/jobs"
	if !slices.Contains(shared[0].Calls(), want) && !slices.Contains(shared[1].Calls(), want) {
		t.Errorf("no sharer got %s", want)
	}

	// pending (2), one per page: a next link, no previous; the second, last page the other way.
	_, body = fetch(t, srv, page+"?server=multi&limit=1", false)
	if !strings.Contains(body, "(no ID yet)") || !strings.Contains(body, `rel="next"`) || strings.Contains(body, `rel="prev"`) {
		t.Errorf("first pending page:\n%s", body)
	}
	_, body = fetch(t, srv, page+"?server=multi&limit=1&offset=1", false)
	if !strings.Contains(body, `start="2"`) || !strings.Contains(body, `rel="prev"`) || strings.Contains(body, `rel="next"`) {
		t.Errorf("second pending page:\n%s", body)
	}

	if status, _ := fetch(t, srv, "/queues/nope?server=multi", false); status != http.StatusNotFound {
		t.Errorf("unknown queue = %d, want 404", status)
	}
}

// A fleet of older varmqs has no queue data; its worker pages are untouched.
func TestQueuesUnsupported(t *testing.T) {
	srv, _ := newTestServer(t, threeFakes(t)...)
	status, body := fetch(t, srv, "/queues", false)
	if status != http.StatusOK || !strings.Contains(body, "No queue data") {
		t.Errorf("GET /queues = %d:\n%s", status, body)
	}
	if status, body := fetch(t, srv, "/", false); status != http.StatusOK || !strings.Contains(body, "orders") {
		t.Errorf("GET / = %d", status)
	}
}
