package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
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

// tenantPath is the page of tenantID, its ID escaped.
const tenantPath = "/queues/redisq:fifo:%7Btenant%2Forders%7D"

// setQueues makes f serve q as tenantID.
func setQueues(f *fleettest.Varmq, q *fleettest.Queue) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Queues = map[string]*fleettest.Queue{tenantID: q}
}

// queueFakes returns the two varmqs that share the queue, and all fakes.
func queueFakes(t *testing.T) ([]*fleettest.Varmq, []*httptest.Server) {
	t.Helper()
	varmqs, fakes := threeVarmqs(t)
	for _, v := range varmqs[:2] { // a and b share the queue; c is an older varmq
		v.Mu.Lock()
		v.Queues = map[string]*fleettest.Queue{tenantID: tenantQueue()}
		v.Workers["orders"].QueueIDs = []string{tenantID}
		v.Mu.Unlock()
	}
	return varmqs[:2], fakes
}

// tenantName is tenantID as the pages name it.
const tenantName = `<span class="qnm">tenant/orders</span> <span class="qkind">FIFO</span></span>`

func TestQueuesPage(t *testing.T) {
	shared, fakes := queueFakes(t)
	const mem = "memory:a:fifo:7" // emails' in-memory queue on a
	shared[0].Mu.Lock()
	shared[0].Queues[mem] = &fleettest.Queue{Summary: `{"id":"memory:a:fifo:7","counts":{"pending":0,"processing":0,"completed":0,"failed":0},"totals":{"submitted":0,"completed":0,"failed":0}}`}
	shared[0].Workers["emails"].QueueIDs = []string{mem}
	shared[0].Mu.Unlock()
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, "/queues", false)
	if status != http.StatusOK {
		t.Fatalf("GET /queues = %d:\n%s", status, body)
	}
	if n := strings.Count(body, tenantName+"</a>"); n != 1 {
		t.Errorf("queue listed %d times, want once", n)
	}
	for _, want := range []string{
		`<a href="/queues/redisq:fifo:%7Btenant%2Forders%7D?server=multi"><span class="qn" title="redisq:fifo:{tenant/orders}"><svg class="sico"`,
		`<span class="sr">Redis: </span>`, `<th scope="col">Namespace</th>`, `<td data-label="Namespace"><span class="na">—</span></td>`,
		`<span class="sr">In memory: </span><span class="qnm">emails #7</span> <span class="qkind">FIFO</span>`, // its worker's name
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /queues lacks %q", want)
		}
	}
	text := visibleText(body)
	// One copy of the shared queue's numbers: 10 submitted, not 20.
	for _, want := range []string{"10", "No queue routes on: instance-", `href="/queues?server=multi" aria-current="page">`} {
		if !strings.Contains(text, want) && !strings.Contains(body, want) {
			t.Errorf("GET /queues lacks %q", want)
		}
	}
	// the cell, not the page text: the page's clock may hold a "20"
	if !strings.Contains(body, `data-label="Submitted (total)">10</td>`) {
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
		`<h1 class="qtitle"><span class="qn" title="` + tenantID + `">`, tenantName + "</h1>",
		`<code class="mono">` + tenantID + `</code> <button type="button" class="copy" data-copy="` + tenantID + `" hidden>Copy ID</button>`,
		`<span aria-current="page"><span class="qn" title="` + tenantID + `">`, `<script src="/static/queue.js" defer>`,
		"<title>tenant/orders · multi</title>",
		// a and b's orders consume it; c, an older varmq, says nothing
		`<h2 class="section" id="qworkers-head">Workers on this queue</h2>`,
		`<th scope="row" class="name"><a href="/workers/orders?server=multi">orders</a></th><td data-label="Status"><span class="status active">Running</span></td><td class="num" data-label="Concurrency">4</td>`,
		"order-42", "2 attempts",
		`<span class="preview err">out of stock</span>`, // list line: the final error
		"1–1 of 1",
		`<li class="step-failed"><svg class="ico"`,                                  // a failed job's last step // list range and state count
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
	if !strings.Contains(body, "(no ID yet)") || !strings.Contains(body, `<span class="preview">{&#34;n&#34;:1}</span>`) || !strings.Contains(body, `rel="next"`) || strings.Contains(body, `rel="prev"`) ||
		!strings.Contains(body, `aria-current="true">1</a>`) || !strings.Contains(body, `aria-label="Page 2">2</a>`) {
		t.Errorf("first pending page:\n%s", body)
	}
	_, body = fetch(t, srv, page+"?server=multi&limit=1&offset=1", false)
	if !strings.Contains(body, `start="2"`) || !strings.Contains(body, `rel="prev"`) || strings.Contains(body, `rel="next"`) {
		t.Errorf("second pending page:\n%s", body)
	}

	if n := strings.Count(body, `<a href="/workers/orders?server=multi">`); n != 2 {
		t.Errorf("workers on the queue: %d rows, want a's and b's", n)
	}
	// the worker page links back, by name
	_, body = fetch(t, srv, "/workers/orders?server=multi", false)
	if !strings.Contains(body, `<p class="wqueues"><span class="sub">Queues</span> <a class="ql" href="`+tenantPath+`?server=multi"><span class="qn" title="`+tenantID+`">`) {
		t.Errorf("worker page lacks its queue:\n%s", body)
	}
	_, body = fetch(t, srv, "/workers/emails?server=multi", false)
	if strings.Contains(body, "wqueues") {
		t.Error("a worker without queue_ids lists queues")
	}

	if status, _ := fetch(t, srv, "/queues/nope?server=multi", false); status != http.StatusNotFound {
		t.Errorf("unknown queue = %d, want 404", status)
	}
}

// The cancelled tab: no count (spec counts have none, so never failed's),
// and a varmq that refuses the state (400) is a note, not an error.
func TestQueueCancelledTab(t *testing.T) {
	shared, fakes := queueFakes(t)
	srv, _ := newTestServer(t, fakes...)
	page := "/queues/redisq:fifo:%7Btenant%2Forders%7D?server=multi"
	_, body := fetch(t, srv, page, false)
	if want := `class="state-cancelled"><span class="label">cancelled</span><span class="count none" title="not counted by this varmq">—</span>`; !strings.Contains(body, want) {
		t.Errorf("pending page lacks the cancelled tab %q:\n%s", want, body)
	}
	status, body := fetch(t, srv, page+"&state=cancelled", false)
	if status != http.StatusOK || !strings.Contains(body, "This varmq does not list cancelled jobs") ||
		strings.Contains(body, `<p class="banner">`) || !strings.Contains(body, `class="state-cancelled" aria-current="page"`) {
		t.Errorf("cancelled page = %d:\n%s", status, body)
	}

	// Only a 400 on the cancelled tab is the note; any other failure is the banner.
	edit := func(change func(q *fleettest.Queue)) {
		for _, v := range shared {
			v.Mu.Lock()
			change(v.Queues[tenantID])
			v.Mu.Unlock()
		}
	}
	for _, c := range []struct {
		state  string
		status int
	}{{"cancelled", http.StatusServiceUnavailable}, {"pending", http.StatusBadRequest}} {
		edit(func(q *fleettest.Queue) { q.JobsStatus = c.status })
		_, body := fetch(t, srv, page+"&state="+c.state, false)
		if !strings.Contains(body, `<p class="banner">`) || strings.Contains(body, "does not list") {
			t.Errorf("%s answering %d: want the banner, not the note:\n%s", c.state, c.status, body)
		}
	}

	// A varmq that lists cancelled jobs: the timeline ends Cancelled.
	edit(func(q *fleettest.Queue) {
		q.JobsStatus = 0
		q.Jobs["cancelled"] = []string{`{"id":"order-7","state":"cancelled","payload":{},"enqueued_at":1762228800000,"finished_at":1762228801000,"attempts":0}`}
	})
	_, body = fetch(t, srv, page+"&state=cancelled", false)
	if !strings.Contains(body, `<li class="step-cancelled">`) || !strings.Contains(body, `<span class="what">Cancelled</span>`) ||
		strings.Contains(body, "does not list") || !strings.Contains(body, "order-7") {
		t.Errorf("listed cancelled job:\n%s", body)
	}
}

// pausableTenant is tenantQueue on varmq API 1.2.0+: it reports paused.
func pausableTenant() *fleettest.Queue {
	q := tenantQueue()
	q.Summary = strings.Replace(q.Summary, `"totals"`, `"paused":false,"totals"`, 1)
	return q
}

// Pause and resume: a confirmation panel first, nothing sent; then the
// action on both sharers (one storage), results, and #qstate out of band.
func TestQueuePauseResume(t *testing.T) {
	varmqs, fakes := threeVarmqs(t)
	shared := pausableTenant()
	for _, v := range varmqs[:2] {
		v.Mu.Lock()
		v.Queues = map[string]*fleettest.Queue{tenantID: shared}
		v.Mu.Unlock()
	}
	srv, _ := newTestServer(t, fakes...)
	esc := "/queues/redisq:fifo:%7Btenant%2Forders%7D"

	pauseBtn, resumeBtn := `hx-post="`+esc+`/actions/pause"`, `hx-post="`+esc+`/actions/resume"`
	_, body := fetch(t, srv, esc+"?server=multi", false)
	if !strings.Contains(body, pauseBtn) || strings.Contains(body, resumeBtn) ||
		strings.Contains(body, "status paused") || !strings.Contains(body, `<div id="results" aria-live="polite">`) {
		t.Errorf("running queue: want Pause alone:\n%s", body)
	}

	resp, body := post(t, srv, esc+"/actions/pause", multi(), true)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Confirm pause of queue") ||
		!strings.Contains(body, "also ones that serve no API") || !strings.Contains(body, `<input type="hidden" name="confirm" value="1">`) {
		t.Errorf("confirm panel = %d:\n%s", resp.StatusCode, body)
	}
	noPatches(t, varmqs...)

	resp, body = post(t, srv, esc+"/actions/pause", multi("confirm", "1"), true)
	if resp.StatusCode != http.StatusOK || strings.Count(body, `<li class="ok">`) != 2 {
		t.Errorf("pause = %d:\n%s", resp.StatusCode, body)
	}
	want := "PATCH " + esc + "/actions/pause"
	wantPatches(t, varmqs[0], want)
	wantPatches(t, varmqs[1], want)
	noPatches(t, varmqs[2])
	if !shared.Paused() {
		t.Error("queue not paused")
	}
	if _, oob, ok := strings.Cut(body, `<div id="qstate" class="qctl" hx-swap-oob="outerHTML">`); !ok || !strings.Contains(oob, `<span class="status paused" title="paused at `) || !strings.Contains(oob, `since <time datetime="20`) ||
		!strings.Contains(oob, resumeBtn) || strings.Contains(oob, pauseBtn) {
		t.Errorf("no paused #qstate (Resume alone) out of band:\n%s", body)
	}
	if _, body := fetch(t, srv, esc+"?server=multi", false); !strings.Contains(body, resumeBtn) || strings.Contains(body, pauseBtn) {
		t.Errorf("paused queue: want Resume alone:\n%s", body)
	}
	if strings.Contains(body, testToken) {
		t.Error("the response carries the token")
	}
	_, body = fetch(t, srv, "/queues?server=multi", false)
	if !strings.Contains(body, `</a> <span class="status paused" title="paused at `) {
		t.Errorf("queue list lacks the paused badge:\n%s", body)
	}

	_, body = post(t, srv, esc+"/actions/resume", multi("confirm", "1"), true)
	if shared.Paused() || strings.Contains(body, "status paused") || !strings.Contains(body, pauseBtn) || strings.Contains(body, resumeBtn) {
		t.Errorf("resume left it paused, or not Pause alone:\n%s", body)
	}

	// Units that differ (one storage each, say): both buttons.
	other := pausableTenant()
	other.Summary = strings.Replace(other.Summary, `"paused":false`, `"paused":true`, 1)
	varmqs[1].Mu.Lock()
	varmqs[1].Queues = map[string]*fleettest.Queue{tenantID: other}
	varmqs[1].Mu.Unlock()
	if _, body := fetch(t, srv, esc+"?server=multi", false); !strings.Contains(body, pauseBtn) || !strings.Contains(body, resumeBtn) {
		t.Errorf("mixed: want both buttons:\n%s", body)
	}

	// An older instance (no paused) beside 1.2.0 ones: shown not supported, sent nothing.
	setQueues(varmqs[1], shared)
	setQueues(varmqs[2], tenantQueue())
	_, body = post(t, srv, esc+"/actions/pause", multi(), true)
	if strings.Count(body, "<td>pause</td>") != 2 || !strings.Contains(body, `<td><span class="na">not supported by this instance</span></td>`) {
		t.Errorf("mixed confirm panel:\n%s", body)
	}
	_, body = post(t, srv, esc+"/actions/pause", multi("confirm", "1"), true)
	if strings.Count(body, `<li class="ok">`) != 2 || strings.Count(body, `<li class="nochange">`) != 1 ||
		!strings.Contains(body, `<span class="outcome">no change (not supported by this instance)</span>`) {
		t.Errorf("mixed pause results:\n%s", body)
	}
	noPatches(t, varmqs[2])

	// Stored, then the read back failed (200 error entry): paused, Resume alone.
	varmqs[2].Mu.Lock()
	varmqs[2].Queues = nil
	varmqs[2].Mu.Unlock()
	stored := pausableTenant()
	stored.ActionErr = "redis down"
	setQueues(varmqs[0], stored)
	setQueues(varmqs[1], stored)
	_, body = post(t, srv, esc+"/actions/pause", multi("confirm", "1"), true)
	if _, oob, ok := strings.Cut(body, `<div id="qstate" class="qctl" hx-swap-oob="outerHTML">`); !ok || !strings.Contains(oob, `<span class="status paused">paused</span>`) ||
		!strings.Contains(oob, resumeBtn) || strings.Contains(oob, pauseBtn) {
		t.Errorf("error-entry answers: want paused, Resume alone:\n%s", body)
	}

	for _, c := range []struct {
		path   string
		status int
	}{
		{esc + "/actions/purge", http.StatusBadRequest},
		{"/queues/nope/actions/pause", http.StatusConflict},
	} {
		if resp, body := post(t, srv, c.path, multi("confirm", "1"), true); resp.StatusCode != c.status {
			t.Errorf("POST %s = %d, want %d:\n%s", c.path, resp.StatusCode, c.status, body)
		}
	}
}

// An older varmq (no paused in GET /queues) gets no buttons and is sent
// nothing; storage that cannot pause (501) is a failed result saying so.
func TestQueuePauseUnsupported(t *testing.T) {
	varmqs, fakes := queueFakes(t) // tenantQueue: no paused, an older varmq
	srv, _ := newTestServer(t, fakes...)
	esc := "/queues/redisq:fifo:%7Btenant%2Forders%7D"
	_, body := fetch(t, srv, esc+"?server=multi", false)
	if strings.Contains(body, "/actions/pause") || !strings.Contains(body, "Pause and resume need varmq API 1.2.0") {
		t.Errorf("older varmq: controls shown:\n%s", body)
	}
	resp, body := post(t, srv, esc+"/actions/pause", multi("confirm", "1"), true)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "supports pause and resume") {
		t.Errorf("POST on older varmq = %d:\n%s", resp.StatusCode, body)
	}
	noPatches(t, varmqs...)

	q := pausableTenant()
	q.ActionStatus = http.StatusNotImplemented
	for _, v := range varmqs {
		v.Mu.Lock()
		v.Queues = map[string]*fleettest.Queue{tenantID: q}
		v.Mu.Unlock()
	}
	resp, body = post(t, srv, esc+"/actions/pause", url.Values{"server": {"multi"}, "confirm": {"1"}}, true)
	if resp.StatusCode != http.StatusOK || strings.Count(body, `<span class="outcome">failed (not supported by this instance)</span>`) != 2 {
		t.Errorf("501 = %d:\n%s", resp.StatusCode, body)
	}

	// Storage unreadable everywhere: no paused to go by; said so, not "older varmq".
	for _, v := range varmqs {
		v.Mu.Lock()
		v.Queues = map[string]*fleettest.Queue{tenantID: {Summary: `{"id":"` + tenantID + `","error":"redis down"}`}}
		v.Mu.Unlock()
	}
	_, body = fetch(t, srv, esc+"?server=multi", false)
	if strings.Contains(body, "need varmq API 1.2.0") || !strings.Contains(body, "redis down") {
		t.Errorf("unreadable queue page:\n%s", body)
	}
	sent := len(patches(varmqs[0])) + len(patches(varmqs[1]))
	resp, body = post(t, srv, esc+"/actions/pause", multi("confirm", "1"), true)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "cannot read queue") {
		t.Errorf("POST on unreadable queue = %d:\n%s", resp.StatusCode, body)
	}
	if n := len(patches(varmqs[0])) + len(patches(varmqs[1])); n != sent {
		t.Errorf("unreadable queue: %d PATCHes sent", n-sent)
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

func TestShortTime(t *testing.T) {
	now := time.Date(2026, 11, 5, 15, 0, 0, 0, time.Local)
	for in, want := range map[time.Time]string{
		now.Add(-2 * time.Hour):                         "13:00:00",
		time.Date(2026, 1, 2, 9, 30, 0, 0, time.Local):  "Jan 2 09:30",
		time.Date(2025, 11, 5, 15, 0, 0, 0, time.Local): "2025-11-05", // same day, last year
	} {
		if got := shortTime(in, now); got != want {
			t.Errorf("shortTime(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPageLinks(t *testing.T) {
	url := func(off int) string { return strconv.Itoa(off) }
	show := func(ps []PageLink) string {
		var b strings.Builder
		for _, p := range ps {
			switch {
			case p.N == 0:
				b.WriteString("… ")
			case p.Current:
				b.WriteString("[" + strconv.Itoa(p.N) + "] ")
			default:
				b.WriteString(strconv.Itoa(p.N) + " ")
			}
		}
		return strings.TrimSpace(b.String())
	}
	for _, c := range []struct {
		total         int64
		offset, limit int
		want          string
	}{
		{50, 0, 50, ""}, // one page
		{-1, 0, 50, ""}, // total unknown
		{120, 0, 50, "[1] 2 3"},
		{10000, 0, 50, "[1] 2 3 … 200"},
		{10000, 5000, 50, "1 … 99 100 [101] 102 103 … 200"},
		{10000, 9950, 50, "1 … 198 199 [200]"},
		{100, 900, 50, "1 2"},                            // offset past the end: no current page
		{1 << 62, 0, 1, "[1] 2 3 … 4611686018427387904"}, // a huge remote count stays O(1)
	} {
		if got := show(pageLinks(c.total, c.offset, c.limit, url)); got != c.want {
			t.Errorf("pageLinks(%d, %d, %d) = %q, want %q", c.total, c.offset, c.limit, got, c.want)
		}
	}
	if ps := pageLinks(120, 50, 50, url); ps[0].URL != "0" || ps[2].URL != "100" {
		t.Errorf("page URLs = %q, %q, want offsets 0, 100", ps[0].URL, ps[2].URL)
	}
}

// workflowFakes returns three varmqs: a and b share the sample workflows
// but the cancelled one (in-memory on a); c is an older varmq. wfs are the
// samples, in SampleWorkflows' order.
func workflowFakes(t *testing.T) ([]*fleettest.Varmq, []*httptest.Server, []*fleettest.Workflow) {
	t.Helper()
	varmqs, fakes := threeVarmqs(t)
	wfs := fleettest.SampleWorkflows(time.Now().UnixMilli())
	varmqs[0].Mu.Lock()
	varmqs[0].Workflows = slices.Clone(wfs)
	varmqs[0].Mu.Unlock()
	varmqs[1].Mu.Lock()
	varmqs[1].Workflows = slices.Clone(wfs[:3]) // own array: tests append
	varmqs[1].Mu.Unlock()
	return varmqs, fakes, wfs
}

// The list: one row per workflow however many units report it, newest
// first, a state filter the poller keeps, and the older varmq named.
func TestWorkflowsPage(t *testing.T) {
	varmqs, fakes, _ := workflowFakes(t)
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, "/workflows?server=multi", false)
	if status != http.StatusOK {
		t.Fatalf("GET /workflows = %d:\n%s", status, body)
	}
	ids := []string{fleettest.RunningID, fleettest.DiamondID, fleettest.SagaID, fleettest.CancelID}
	last := -1
	for _, id := range ids {
		link := `<a href="/workflows/` + id + `?server=multi">` + id + `</a>`
		if n := strings.Count(body, link); n != 1 {
			t.Errorf("%s listed %d times, want once", id, n)
		}
		if i := strings.Index(body, link); i < last {
			t.Errorf("%s out of order: newest first", id)
		} else {
			last = i
		}
	}
	for _, want := range []string{
		`<span class="n">3/7</span>`, `<span class="sub">so far</span>`, `<span class="wst s-compensated">compensated</span>`,
		"No workflow routes on: instance-", `href="/workflows?server=multi" aria-current="page"><svg`,
		`<a href="/workflows?server=multi" class="s-all" aria-current="page">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	if strings.Contains(body, testToken) {
		t.Error("the page carries the token")
	}

	_, body = fetch(t, srv, "/workflows?server=multi&state=compensated", false)
	if strings.Contains(body, ">"+fleettest.RunningID+"</a>") || !strings.Contains(body, ">"+fleettest.SagaID+"</a>") ||
		!strings.Contains(body, `hx-get="/workflows?server=multi&amp;state=compensated"`) {
		t.Errorf("compensated list, or its poller:\n%s", body)
	}
	_, body = fetch(t, srv, "/workflows?server=multi&state=compensation_failed", false)
	if !strings.Contains(body, "No compensation failed workflows.") {
		t.Errorf("empty state list:\n%s", visibleText(body))
	}
	if _, body = fetch(t, srv, "/workflows?server=multi&offset=2", false); strings.Contains(body, `id="poller"`) || !strings.Contains(body, `rel="prev"`) || !strings.Contains(body, "<span class=\"gap\">3–4</span>") {
		t.Errorf("a later page: browsed, not polled:\n%s", body)
	}
	if _, body = fetch(t, srv, "/workflows?server=multi&state=bogus", false); !strings.Contains(body, `class="s-all" aria-current="page"`) {
		t.Error("an unknown state is not all")
	}

	// one unit's list fails: named, the other's rows still listed
	varmqs[0].Mu.Lock()
	varmqs[0].WorkflowStatus = http.StatusServiceUnavailable
	varmqs[0].Mu.Unlock()
	if _, body = fetch(t, srv, "/workflows?server=multi", false); !strings.Contains(body, `<p class="err">instance-`) || !strings.Contains(body, "Service Unavailable") ||
		!strings.Contains(body, ">"+fleettest.RunningID+"</a>") {
		t.Errorf("one unit failing:\n%s", visibleText(body))
	}
	varmqs[0].Mu.Lock()
	varmqs[0].WorkflowStatus = 0
	varmqs[0].Mu.Unlock()

	// 34 in all: page 1 links to page 2, page 2 to none
	var more []*fleettest.Workflow
	for i := range 30 {
		more = append(more, fleettest.NewWorkflow(contract.WorkflowRecord{ID: fmt.Sprintf("w%02d", i), State: contract.WorkflowCompleted, CreatedAt: int64(i + 1)}))
	}
	varmqs[2].Mu.Lock()
	varmqs[2].Workflows = more
	varmqs[2].Mu.Unlock()
	if _, body = fetch(t, srv, "/workflows?server=multi", false); !strings.Contains(body, `<a href="/workflows?offset=25&amp;server=multi" rel="next">`) {
		t.Errorf("page 1 has no next link:\n%s", visibleText(body))
	}
	if _, body = fetch(t, srv, "/workflows?server=multi&offset=25", false); strings.Contains(body, `rel="next"`) || !strings.Contains(body, `<span class="gap">26–34</span>`) {
		t.Errorf("page 2, the last, links on:\n%s", visibleText(body))
	}
}

// A fleet of older varmqs has no workflow data, said so on both pages.
func TestWorkflowsUnsupported(t *testing.T) {
	srv, _ := newTestServer(t, threeFakes(t)...)
	if status, body := fetch(t, srv, "/workflows", false); status != http.StatusOK || !strings.Contains(body, "No workflow data: no instance serves the workflow routes") {
		t.Errorf("GET /workflows = %d:\n%s", status, body)
	}
	if status, body := fetch(t, srv, "/workflows/x", false); status != http.StatusOK || !strings.Contains(body, "No workflow data") || strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("GET /workflows/x = %d:\n%s", status, body)
	}
}

// One workflow: the graph and table; polled while it runs, not once it
// ended; 404 when no unit has it (200 to a poll, the notice in the banner).
func TestWorkflowPage(t *testing.T) {
	varmqs, fakes, _ := workflowFakes(t)
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, "/workflows/"+fleettest.RunningID+"?server=multi", false)
	if status != http.StatusOK {
		t.Fatalf("running = %d:\n%s", status, body)
	}
	for _, want := range []string{
		`hx-get="/workflows/` + fleettest.RunningID + `?server=multi" hx-trigger="every 1s"`,
		`<h1 class="qid">` + fleettest.RunningID + `</h1>`, "Running for", "refresh every 1s",
		`<a class="node t-pending live" href="#task-4"`, "Cancel without undo",
		`<a class="ql" href="/queues/redisq:shop:fifo:%7Betl%7D?server=multi"><span class="qn" title="redisq:shop:fifo:{etl}">`,
		`<span class="qnm">etl</span> <span class="qkind">FIFO</span></span></a>`, `<text class="meta" x="30" y="42">pending · audit</text>`, `<title>audit-log · redisq:shop:fifo:{audit}</title>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("running page lacks %q", want)
		}
	}
	if strings.Count(body, `<span class="chip">instance-`) != 2 { // a and b, the sharers
		t.Errorf("running workflow reported by %d units", strings.Count(body, `<span class="chip">instance-`))
	}
	if strings.Count(body, `<a class="node `) != 7 || strings.Count(body, `<path class="edge`) != 8 {
		t.Errorf("graph: %d nodes, %d edges", strings.Count(body, `<a class="node `), strings.Count(body, `<path class="edge`))
	}
	if strings.Contains(body, testToken) || strings.Contains(body, `id="poller"`) {
		t.Error("token on the page, or the layout poller")
	}

	_, body = fetch(t, srv, "/workflows/"+fleettest.SagaID+"?server=multi", false)
	if strings.Contains(body, `hx-trigger="every`) || strings.Contains(body, "Cancel workflow") || !strings.Contains(body, "Took") || !strings.Contains(body, `<g class="undo t-completed"`) {
		t.Errorf("ended workflow polls or offers cancel:\n%s", body)
	}

	// one unit's storage fails: the copy the other reports shows, the
	// failure named, and the page keeps polling; all fail: the banner, still
	// polling (a failure may pass)
	varmqs[0].Mu.Lock()
	varmqs[0].WorkflowStatus = http.StatusServiceUnavailable
	varmqs[0].Mu.Unlock()
	running := "/workflows/" + fleettest.RunningID + "?server=multi"
	if _, body := fetch(t, srv, running, true); !strings.Contains(body, `<p class="err">instance-`) || !strings.Contains(body, "Service Unavailable") ||
		!strings.Contains(body, `<a class="node `) || !strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("one unit failing:\n%s", visibleText(body))
	}
	varmqs[1].Mu.Lock()
	varmqs[1].WorkflowStatus = http.StatusServiceUnavailable
	varmqs[1].Mu.Unlock()
	if status, body := fetch(t, srv, running, true); status != http.StatusOK || !strings.Contains(body, `<p class="banner">Workflow &#34;`+fleettest.RunningID+`&#34;: instance-`) ||
		strings.Contains(body, `<a class="node `) || !strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("every unit failing = %d:\n%s", status, visibleText(body))
	}
	// a full load with no graph yet still brings the script, for the graph
	// a later poll swaps in
	if _, body := fetch(t, srv, running, false); !strings.Contains(body, `<script src="/static/workflow.js" defer></script>`) || !strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("full load while every unit fails: no workflow.js or no poll:\n%s", body)
	}
	for _, v := range varmqs[:2] {
		v.Mu.Lock()
		v.WorkflowStatus = 0
		v.Mu.Unlock()
	}

	// the in-memory one: on its unit only
	if _, body = fetch(t, srv, "/workflows/"+fleettest.CancelID+"?server=multi", false); strings.Count(body, `<span class="chip">instance-`) != 1 {
		t.Errorf("in-memory workflow reported by:\n%s", body)
	}

	if status, body := fetch(t, srv, "/workflows/nope?server=multi", false); status != http.StatusNotFound || !strings.Contains(body, `Workflow &#34;nope&#34; not found on any instance`) {
		t.Errorf("unknown = %d:\n%s", status, body)
	}
	if status, body := fetch(t, srv, "/workflows/nope?server=multi", true); status != http.StatusOK || !strings.Contains(body, "not found on any instance") || strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("unknown to a poll = %d", status)
	}
}

// Several storages on a unit hold the ID (409 there): the other units'
// copy shows, with a note; when every unit says 409, the banner says why.
// Two in-memory workflows of one ID: pick one.
func TestWorkflowPageAmbiguous(t *testing.T) {
	varmqs, fakes, wfs := workflowFakes(t)
	srv, _ := newTestServer(t, fakes...)
	page := "/workflows/" + fleettest.DiamondID + "?server=multi"
	varmqs[1].Mu.Lock()
	varmqs[1].Workflows = append(varmqs[1].Workflows, fleettest.NewWorkflow(contract.WorkflowRecord{ID: fleettest.DiamondID, CreatedAt: 1}))
	varmqs[1].Mu.Unlock()
	if _, body := fetch(t, srv, page, false); !strings.Contains(body, "holds a workflow with this ID (409)") || !strings.Contains(body, `<a class="node `) {
		t.Errorf("one unit 409:\n%s", visibleText(body))
	}
	varmqs[0].Mu.Lock()
	varmqs[0].Workflows = append(varmqs[0].Workflows, fleettest.NewWorkflow(contract.WorkflowRecord{ID: fleettest.DiamondID, CreatedAt: 2}))
	varmqs[0].Mu.Unlock()
	if _, body := fetch(t, srv, page, false); !strings.Contains(body, "is held by more than one storage on instance-") || strings.Contains(body, `<a class="node `) {
		t.Errorf("every unit 409:\n%s", visibleText(body))
	}

	// a and b each hold their own in-memory "twin"
	twin := func(at int64) *fleettest.Workflow {
		return fleettest.NewWorkflow(contract.WorkflowRecord{ID: "twin", State: contract.WorkflowCompleted, CreatedAt: at, FinishedAt: at + 1, Tasks: wfs[0].Record().Tasks})
	}
	for i, v := range varmqs[:2] {
		v.Mu.Lock()
		v.Workflows = []*fleettest.Workflow{twin(int64(1000 + i))}
		v.Mu.Unlock()
	}
	status, body := fetch(t, srv, "/workflows/twin?server=multi", false)
	if status != http.StatusOK || !strings.Contains(body, "Pick one") || !strings.Contains(body, `href="/workflows/twin?at=1000&amp;server=multi"`) || strings.Contains(body, `<a class="node `) {
		t.Errorf("twins without at:\n%s", body)
	}
	if _, body := fetch(t, srv, "/workflows/twin?server=multi&at=1001", false); !strings.Contains(body, `<a class="node `) || !strings.Contains(body, "Other workflows share this ID") {
		t.Errorf("twin at 1001:\n%s", visibleText(body))
	}
	if _, body := fetch(t, srv, "/workflows?server=multi", false); !strings.Contains(body, `href="/workflows/twin?at=1000&amp;server=multi"`) ||
		!strings.Contains(body, `href="/workflows/twin?at=1001&amp;server=multi"`) {
		t.Errorf("list links twins to their own pages:\n%s", body)
	}

	// the unit holding the twin a link names fails for now: said so, the
	// page polls on, not "no longer reported"
	varmqs[0].Mu.Lock()
	varmqs[0].WorkflowStatus = http.StatusServiceUnavailable
	varmqs[0].Mu.Unlock()
	status, body = fetch(t, srv, "/workflows/twin?server=multi&at=1000", false)
	if status != http.StatusOK || strings.Contains(body, "no longer reported") || !strings.Contains(body, "Service Unavailable") ||
		!strings.Contains(body, `<p class="banner">Workflow &#34;twin&#34;: instance-`) || !strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("stale at, its unit failing = %d:\n%s", status, visibleText(body))
	}
	// three twins, the linked one's unit failing: two left still poll on
	varmqs[2].Mu.Lock()
	varmqs[2].Workflows = []*fleettest.Workflow{twin(1002)}
	varmqs[2].Mu.Unlock()
	status, body = fetch(t, srv, "/workflows/twin?server=multi&at=1000", false)
	if status != http.StatusOK || strings.Contains(body, "no longer reported") || !strings.Contains(body, "Service Unavailable") ||
		!strings.Contains(body, `<p class="banner">Workflow &#34;twin&#34;: instance-`) || !strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("stale at of three, its unit failing = %d:\n%s", status, visibleText(body))
	}
	// no at, a unit failing: still pick one of those read, the failure noted
	status, body = fetch(t, srv, "/workflows/twin?server=multi", false)
	if status != http.StatusOK || !strings.Contains(body, "Pick one") || !strings.Contains(body, `href="/workflows/twin?at=1001&amp;server=multi"`) ||
		!strings.Contains(body, `href="/workflows/twin?at=1002&amp;server=multi"`) || !strings.Contains(body, "Service Unavailable") || strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("no at of three, a unit failing = %d:\n%s", status, visibleText(body))
	}
	// the same, its unit answering 409
	varmqs[0].Mu.Lock()
	varmqs[0].WorkflowStatus = 0
	varmqs[0].Workflows = []*fleettest.Workflow{twin(1000), twin(1000)}
	varmqs[0].Mu.Unlock()
	status, body = fetch(t, srv, "/workflows/twin?server=multi&at=1000", false)
	if status != http.StatusOK || strings.Contains(body, "no longer reported") ||
		!strings.Contains(body, "is held by more than one storage on instance-") || !strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("stale at of three, its unit 409 = %d:\n%s", status, visibleText(body))
	}
	// the linked twin gone, two left, every unit read: offered, not "not found"
	varmqs[0].Mu.Lock()
	varmqs[0].Workflows = []*fleettest.Workflow{}
	varmqs[0].Mu.Unlock()
	status, body = fetch(t, srv, "/workflows/twin?server=multi&at=1000", false)
	if status != http.StatusOK || strings.Contains(body, "not found") || !strings.Contains(body, "no longer reported") ||
		!strings.Contains(body, `href="/workflows/twin?at=1001&amp;server=multi"`) || !strings.Contains(body, `href="/workflows/twin?at=1002&amp;server=multi"`) ||
		strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("stale at, two twins left = %d:\n%s", status, visibleText(body))
	}
	varmqs[2].Mu.Lock()
	varmqs[2].Workflows = nil
	varmqs[2].Mu.Unlock()

	// the twin a link named is gone, one is left: offered, not "not found"
	varmqs[0].Mu.Lock()
	varmqs[0].Workflows = []*fleettest.Workflow{}
	varmqs[0].Mu.Unlock()
	status, body = fetch(t, srv, "/workflows/twin?server=multi&at=1000", false)
	if status != http.StatusOK || strings.Contains(body, "not found") || !strings.Contains(body, "no longer reported") ||
		!strings.Contains(body, `href="/workflows/twin?at=1001&amp;server=multi"`) || strings.Contains(body, `hx-trigger="every`) {
		t.Errorf("stale at, one twin left = %d:\n%s", status, visibleText(body))
	}
}

func TestParseQueueID(t *testing.T) {
	for _, c := range []struct {
		id                      string
		backend, name, kind, ns string
	}{
		{"redisq:fifo:{orders}", "redis", "orders", "FIFO", ""},
		{"redisq:shop:prio:{emails}", "redis", "emails", "Priority", "shop"},
		{"redisq:eu:west:fifo:{tenant/orders}", "redis", "tenant/orders", "FIFO", "eu:west"}, // ns with ':'
		{"redisq:fifo:fifo:{x}", "redis", "x", "FIFO", "fifo"},
		{"redisq:fifo:{}", "", "redisq:fifo:{}", "", ""},
		{"redisq:fifo:{a{b}", "", "redisq:fifo:{a{b}", "", ""},
		{"redisq:list:{orders}", "", "redisq:list:{orders}", "", ""},
		{"redisq::fifo:{orders}", "", "redisq::fifo:{orders}", "", ""},
		{"redisq:shopfifo:{orders}", "", "redisq:shopfifo:{orders}", "", ""},
		{"sqliteq:9f86d081:fifo:images", "sqlite", "images", "FIFO", ""},
		{"sqliteq:9f86d081:prio:a:b", "sqlite", "a:b", "Priority", ""},
		{"sqliteq:9f86d081:list:images", "", "sqliteq:9f86d081:list:images", "", ""},
		{"sqliteq::fifo:images", "", "sqliteq::fifo:images", "", ""},
		{"memory:shop-a:fifo:3", "memory", "in-memory #3", "FIFO", ""},
		{"memory:host:9000:prio:12", "memory", "in-memory #12", "Priority", ""},
		{"memory:shop-a:3", "memory", "in-memory #3", "", ""}, // before API 1.4.0
		{"memory:fifo:3", "memory", "in-memory #3", "", ""},   // instance "fifo"
		{"memory:a:reports", "", "memory:a:reports", "", ""},
		{"memory::3", "", "memory::3", "", ""},
		{"memory:a:fifo:", "", "memory:a:fifo:", "", ""},
		{"pgq.ledger/settlements", "", "pgq.ledger/settlements", "", ""},
		{"", "", "", "", ""},
	} {
		q := parseQueueID(c.id)
		if q.ID != c.id || q.Backend != c.backend || q.Name != c.name || q.Kind != c.kind || q.NS != c.ns {
			t.Errorf("parseQueueID(%q) = %+v, want %s %q %s ns %q", c.id, q, c.backend, c.name, c.kind, c.ns)
		}
	}

	pod := okPod("http://a", map[string]*fleet.Sample{"reports": smp("Running", 1, 0, 1, 0), "aaa": smp("Running", 1, 0, 1, 0), "orders": smp("Running", 1, 0, 1, 0)})
	pod.Workers["reports"].QueueIDs = []string{"memory:a:fifo:1", "memory:a:prio:2"}
	pod.Workers["aaa"].QueueIDs = []string{"memory:a:prio:2"} // two on one queue: the first by name
	pod.Workers["orders"].QueueIDs = []string{"redisq:fifo:{orders}"}
	o := ownersOf(snap(viewT0, pod))
	for id, want := range map[string]string{"memory:a:fifo:1": "reports #1", "memory:a:prio:2": "aaa #2", "memory:a:fifo:9": "in-memory #9", "redisq:fifo:{orders}": "orders"} {
		if got := o.name(id).Name; got != want {
			t.Errorf("name(%q) = %q, want %q", id, got, want)
		}
	}
	if o := ownersOf(nil); o.name("memory:a:fifo:1").Name != "in-memory #1" {
		t.Error("no snapshot: not the fallback")
	}
}

// Queue IDs, namespaces and worker names come from varmq: text, never
// markup, in names, titles, data-copy and links.
func TestQueueNamesHostile(t *testing.T) {
	const evil = `<script>alert(1)</script>"'><img src=x onerror=y>`
	ids := []string{"redisq:" + evil + ":fifo:{" + evil + "}", "sqliteq:h:prio:" + evil, "memory:a:fifo:1", evil}
	pod := okPod("http://a", map[string]*fleet.Sample{evil: smp("Running", 1, 0, 1, 0)})
	pod.Workers[evil].QueueIDs = ids
	s := snap(viewT0, pod)
	var groups []fleet.QueueGroup
	for _, id := range ids {
		groups = append(groups, fleet.QueueGroup{QueueSummary: api.QueueSummary{ID: id}, On: []string{evil}})
	}
	d, _ := fleet.BuildDetail([]*fleet.Snapshot{s}, "", evil, 0, instanceNoun)
	jobs := &JobsView{Queue: groups[0], Ctl: QueueCtl{ID: ids[0]}, Workers: queueWorkers(s, ids[0], "dev")}
	for name, body := range map[string]string{
		"list":    run(t, "queues", "layout", Page{Server: "dev", owners: ownersOf(s), Queues: &fleet.QueuesView{Groups: groups}}),
		"page":    run(t, "queue", "layout", Page{Server: "dev", owners: ownersOf(s), Jobs: jobs}),
		"worker":  run(t, "detail", "layout", Page{Server: "dev", owners: ownersOf(s), Detail: &d}),
		"confirm": run(t, "confirm", "queue_confirm", QueueConfirmView{Action: "pause", Server: "dev", ID: ids[0], Queue: parseQueueID(ids[0]), Noun: instanceNoun}),
	} {
		if strings.Contains(body, "<img") || strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: a field rendered as markup", name)
		}
	}
}
