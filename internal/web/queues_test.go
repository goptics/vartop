package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	for _, id := range []string{"sqliteq:h:fifo:apples", "redisq:shop:prio:{emails}"} {
		shared[0].Queues[id] = &fleettest.Queue{Summary: `{"id":"` + id + `","counts":{"pending":0,"processing":0,"completed":0,"failed":0}}`}
	}
	shared[0].Mu.Unlock()
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, "/queues", false)
	if status != http.StatusOK {
		t.Fatalf("GET /queues = %d:\n%s", status, body)
	}
	if n := strings.Count(body, tenantName+"</a>"); n != 1 {
		t.Errorf("queue listed %d times, want once", n)
	}
	labels := fleettest.StaticLabels(fakes[0].URL, fakes[1].URL, fakes[2].URL)
	la, lab := labels[fakes[0].URL], slices.Sorted(slices.Values([]string{labels[fakes[0].URL], labels[fakes[1].URL]}))
	for _, want := range []string{
		`<a href="/queues/redisq:fifo:%7Btenant%2Forders%7D?server=multi" id="q-` + fleet.DOMID(tenantID) + `"><span class="qn" title="redisq:fifo:{tenant/orders}"><svg class="sico"`,
		`<span class="sr">Redis: </span>`, `<th scope="col">Namespace</th>`, `<td data-label="Namespace"><span class="na">—</span></td>`,
		`<span class="sr">In memory: </span><span class="qnm">emails #7</span> <span class="qkind">FIFO</span>`, // its worker's name
		`<td data-label="Namespace">shop</td>`,
		// the workers consuming it, by queue_ids; none: the instances
		`<th scope="col">Workers</th>`,
		`<a class="chip wchip" href="/workers/orders?server=multi" title="runs on ` + strings.Join(lab, ", ") + `" id="on-` + fleet.DOMID(tenantID+" orders") + `">orders</a>`,
		`<a class="chip wchip" href="/workers/emails?server=multi" title="runs on ` + la + `" id="on-` + fleet.DOMID(mem+" emails") + `">emails</a>`,
		`<td class="on" data-label="Workers"><span class="sub">instances</span> <span class="chip">` + la + `</span></td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /queues lacks %q", want)
		}
	}
	// rows by name, not by ID
	last := -1
	for _, name := range []string{`"qnm">apples<`, `"qnm">emails<`, `"qnm">emails #7<`, `"qnm">tenant/orders<`} {
		i := strings.Index(body, name)
		if i <= last {
			t.Errorf("rows not by name: %s at %d, after %d", name, i, last)
		}
		last = i
	}
	if _, body := fetch(t, srv, "/queues/redisq:shop:prio:%7Bemails%7D?server=multi", false); !strings.Contains(body, `<p class="sub">Namespace <span class="chip">shop</span></p>
  <p class="readfrom">Read from instance-`) {
		t.Errorf("namespaced queue page lacks its namespace:\n%s", body)
	}
	text := visibleText(body)
	// One copy of the shared queue's numbers: 10 submitted, not 20.
	for _, want := range []string{"10", "No queue routes on: instance-", `href="/queues?server=multi" aria-current="page">`} {
		if !strings.Contains(text, want) && !strings.Contains(body, want) {
			t.Errorf("GET /queues lacks %q", want)
		}
	}
	// the cell, not the page text: the page's clock may hold a "20"
	if !strings.Contains(body, `data-label="Submitted (all time)">10</td>`) {
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
	shared[0].Mu.Lock()
	shared[0].Workers["emails"].QueueIDs = []string{tenantID}
	shared[0].Mu.Unlock()
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
		`<th scope="row" class="name"><a href="/workers/orders?server=multi">orders</a></th><td data-label="Status"><span class="status active">running</span></td><td class="num" data-label="Concurrency">4</td>`,
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
	// by name, then in unit order
	var rows []string
	for _, m := range regexp.MustCompile(`data-label="Instance">(instance-\d)</td><th scope="row" class="name"><a href="/workers/(\w+)`).FindAllStringSubmatch(body, -1) {
		rows = append(rows, m[2]+"@"+m[1])
	}
	if len(rows) != 3 || rows[0][:7] != "emails@" || rows[1][:7] != "orders@" || rows[2][:7] != "orders@" || rows[1] > rows[2] {
		t.Errorf("workers on the queue = %q, want emails, then orders by unit", rows)
	}
	// the worker page links back, by name
	_, body = fetch(t, srv, "/workers/orders?server=multi", false)
	if !strings.Contains(body, `<li><a class="srcn" href="`+tenantPath+`?server=multi" aria-label="tenant/orders, FIFO, 2 pending"><span class="qn" title="`+tenantID+`">`) || !strings.Contains(body, "From <code>queue_ids</code>") {
		t.Errorf("worker page lacks its queue:\n%s", body)
	}
	urls := []string{fakes[0].URL, fakes[1].URL, fakes[2].URL}
	if status, body := fetch(t, srv, "/workers/orders?server=multi&instance="+fleettest.StaticLabels(urls...)[fakes[2].URL], false); status != http.StatusOK || strings.Contains(body, `class="sources"`) {
		t.Errorf("an older varmq's worker = %d, lists queues:\n%s", status, body)
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `Pause queue <span class="qn" title="`+tenantID+`">`) || !strings.Contains(body, tenantName) ||
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
	varmqs[0].Mu.Lock()
	varmqs[0].Workers["emails"].QueueIDs = []string{"memory:shop-a:fifo:1"} // the report's queue
	varmqs[0].Mu.Unlock()
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
		// a task's job ID links to its job page, in the table and the panel
		`<td data-label="Job ID"><a class="mono" href="/queues/redisq:shop:fifo:%7Betl%7D/jobs/extract-1?server=multi">extract-1</a></td>`,
		`<dt>Job ID</dt><dd><a class="mono" href="/queues/redisq:shop:fifo:%7Betl%7D/jobs/extract-1?server=multi">extract-1</a></dd>`,
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
	if !strings.Contains(body, `<span class="qnm">emails #1</span>`) || strings.Contains(body, "in-memory #1") {
		t.Errorf("in-memory queue not named after its worker:\n%s", body)
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
		{"redisq:a}b:fifo:{x}", "", "redisq:a}b:fifo:{x}", "", ""},
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
	one := int64(1)
	pod.Workers[evil].Strategy = evil
	for _, id := range ids {
		pod.Workers[evil].Sources = append(pod.Workers[evil].Sources, api.WorkerSource{QueueID: id, Kind: "queue", Priority: &one})
	}
	pod.Workers[evil].Sources = append(pod.Workers[evil].Sources, api.WorkerSource{Kind: evil})
	s := snap(viewT0, pod)
	var groups []fleet.QueueGroup
	for _, id := range ids {
		groups = append(groups, fleet.QueueGroup{QueueSummary: api.QueueSummary{ID: id}, On: []string{evil}})
	}
	d, _ := fleet.BuildDetail([]*fleet.Snapshot{s}, "", evil, 0, instanceNoun)
	jobs := &JobsView{Queue: groups[0], Ctl: QueueCtl{ID: ids[0]}, Workers: queueWorkers(s, ids[0], "dev")}
	evilJSON, _ := json.Marshal(map[string]string{evil: evil})
	job := jobView(groups[0], parseQueueID(ids[0]), evil, "dev", fleet.JobRead{Found: true, From: evil, Job: contract.JobRecord{ID: evil, State: contract.JobPending,
		Payload: evilJSON, Result: evilJSON, EnqueuedAt: 1, Errors: []contract.JobError{{Attempt: 1, At: 2, Error: evil}}}}, viewT0)
	if !job.Ctl.Cancel {
		t.Fatal("hostile job: no cancel button to check")
	}
	for name, body := range map[string]string{
		"list":    run(t, "queues", "layout", Page{Server: "dev", owners: ownersOf(s), qworkers: workerChips(s, "dev"), Queues: &fleet.QueuesView{Groups: groups}}),
		"page":    run(t, "queue", "layout", Page{Server: "dev", owners: ownersOf(s), Jobs: jobs}),
		"worker":  run(t, "detail", "layout", Page{Server: "dev", owners: ownersOf(s), Detail: &d}),
		"confirm": run(t, "confirm", "queue_confirm", QueueConfirmView{Action: "pause", Server: "dev", Queue: parseQueueID(ids[0]), Noun: instanceNoun}),
		"job":     run(t, "job", "layout", Page{Server: "dev", owners: ownersOf(s), Job: job}),
		"job confirm": run(t, "confirm", "job_confirm", JobConfirmView{Server: "dev", ID: evil, Queue: parseQueueID(ids[0]), Noun: instanceNoun, On: []string{evil},
			Ctl: JobCtl{Server: "dev", QueueID: ids[0], ID: evil}}),
	} {
		if strings.Contains(body, "<img") || strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s: a field rendered as markup", name)
		}
		// the worker's sources and its chips on the queue list are drawn, escaped
		if name == "worker" && (strings.Count(body, `class="srcn"`) != len(ids)+1 || !strings.Contains(body, `<small>&lt;script&gt;`)) {
			t.Errorf("worker: sources not drawn:\n%s", body)
		}
		if name == "list" && (strings.Count(body, `class="chip wchip"`) != len(ids) || !strings.Contains(body, `<a class="chip wchip" href="/workers/`+url.PathEscape(evil)+`?server=dev"`)) {
			t.Errorf("list: %d worker chips, want %d, each to the escaped worker page", strings.Count(body, `class="chip wchip"`), len(ids))
		}
	}
}

// jobQueue is tenantID shared by a and b (c is an older varmq), holding
// fleettest.SampleJobs, an ID with '/', '{' and '}', and one ".".
func jobQueue(t *testing.T) ([]*fleettest.Varmq, []*httptest.Server, *fleettest.Queue) {
	t.Helper()
	varmqs, fakes := threeVarmqs(t)
	q := tenantQueue()
	q.Jobs = fleettest.SampleJobs(time.Now().UnixMilli())
	q.Jobs["pending"] = append(q.Jobs["pending"], `{"id":"a/b{c}","state":"pending","payload":1,"enqueued_at":1762228800000,"attempts":0}`,
		`{"id":".","state":"pending","payload":2,"attempts":0}`)
	for _, v := range varmqs[:2] {
		setQueues(v, q)
	}
	return varmqs, fakes, q
}

// jobPage is the page of job id of tenantID on multi.
func jobPage(id string) string { return tenantPath + "/jobs/" + url.PathEscape(id) + "?server=multi" }

// The job page: header, timeline with spans, errors, payload, result; the
// cancel button only while the job waits or runs; links from the queue's
// list (none for "." or no ID), odd IDs one escaped segment end to end.
func TestJobPage(t *testing.T) {
	varmqs, fakes, _ := jobQueue(t)
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, jobPage("fetch-image-1"), false)
	for _, want := range []string{
		`<a class="ql" href="` + tenantPath + `?server=multi"><span class="qn" title="` + tenantID + `">`,
		`<h1 class="qid">fetch-image-1</h1><span class="jstate state-completed"><span class="dot" aria-hidden="true"></span>completed</span>`,
		`data-copy="fetch-image-1"`, `<script src="/static/queue.js" defer>`, `<script src="/static/job.js" defer>`,
		"<title>fetch-image-1 · tenant/orders · multi</title>",
		`<span class="what">Waited</span><span class="dur">0.8s</span>`, `<span class="what">Ran</span><span class="dur">3.4s</span>`,
		`<span class="what">Total</span><span class="dur">4.2s</span>`, "<dt>Took</dt>",
		`<li class="step-finished"><svg`, `&#34;width&#34;: 1600`, `&#34;url&#34;: &#34;https://cdn.example.com/u/7f3a/raw.jpg&#34;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("completed job lacks %q", want)
		}
	}
	if status != http.StatusOK || strings.Contains(body, "Cancel job") || strings.Contains(body, "data-ms") {
		t.Errorf("completed job = %d: a cancel button or a live span:\n%s", status, body)
	}

	_, body = fetch(t, srv, jobPage("ord-1027"), false)
	for _, want := range []string{
		`<li class="step-failed"><svg`, `<span class="what">Failed</span>`, "<dt>Attempts</dt><dd>2</dd>",
		`<code class="err">dial tcp 10.0.3.7:5432: connect: connection refused</code></li><li><span class="what">Attempt 2</span>`,
		"<p class=\"sub\">None.</p>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failed job lacks %q", want)
		}
	}
	if strings.Contains(body, "Cancel job") {
		t.Error("failed job: a cancel button")
	}
	_, body = fetch(t, srv, jobPage("ord-1035"), false)
	if !strings.Contains(body, `<li class="step-started none">`) || !strings.Contains(body, `<span class="what">Cancelled</span>`) ||
		!strings.Contains(body, `<span class="what">Waited</span><span class="dur">—</span>`) || strings.Contains(body, "Cancel job") {
		t.Errorf("cancelled while waiting: no start, no spans to it, no button:\n%s", body)
	}

	_, body = fetch(t, srv, jobPage("ord-1039"), false)
	if !strings.Contains(body, `<span class="what">Ran</span><span class="dur" data-ms="`) || !strings.Contains(body, "<dt>Running for</dt>") ||
		!strings.Contains(body, `hx-post="`+tenantPath+`/jobs/ord-1039/actions/cancel">Cancel job</button>`) {
		t.Errorf("running job: live span and cancel:\n%s", body)
	}

	// '/', '{', '}': one escaped segment to varmq and in the cancel post
	odd := "a/b{c}"
	status, body = fetch(t, srv, jobPage(odd), false)
	if status != http.StatusOK || !strings.Contains(body, `<span class="what">Waited</span><span class="dur" data-ms="`) ||
		!strings.Contains(body, `hx-post="`+tenantPath+`/jobs/a%2Fb%7Bc%7D/actions/cancel"`) {
		t.Errorf("odd ID = %d:\n%s", status, body)
	}
	want := "GET " + tenantPath + "/jobs/a%2Fb%7Bc%7D"
	if !slices.Contains(varmqs[0].Calls(), want) && !slices.Contains(varmqs[1].Calls(), want) {
		t.Errorf("no sharer got %s", want)
	}

	// the list links each ID to its page; not "." (a browser drops it)
	_, body = fetch(t, srv, tenantPath+"?server=multi", false)
	for _, want := range []string{
		`<a class="id" href="` + tenantPath + `/jobs/ord-1042?server=multi" title="ord-1042">ord-1042</a>`,
		`<a class="id" href="` + tenantPath + `/jobs/a%2Fb%7Bc%7D?server=multi" title="a/b{c}">a/b{c}</a>`,
		`<span class="id" title=".">.</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue list lacks %q", want)
		}
	}

	status, body = fetch(t, srv, jobPage("nope"), false)
	if status != http.StatusNotFound || !strings.Contains(body, "No job <span class=\"mono\">nope</span> in this queue") || !strings.Contains(body, "may no longer keep") {
		t.Errorf("unknown job = %d:\n%s", status, body)
	}
	if status, body := fetch(t, srv, "/queues/nope/jobs/x?server=multi", false); status != http.StatusNotFound || !strings.Contains(body, `Queue &#34;nope&#34; not found`) {
		t.Errorf("unknown queue = %d:\n%s", status, body)
	}
	if strings.Contains(body, testToken) {
		t.Error("the page carries the token")
	}
}

// Every unit without the route (older varmq, storage without JobReader):
// said so, not "not found"; an in-memory queue's missing job names
// WithEndedJobs.
func TestJobPageUnsupportedAndMemory(t *testing.T) {
	varmqs, fakes, q := jobQueue(t)
	srv, _ := newTestServer(t, fakes...)
	varmqs[0].Mu.Lock()
	q.JobStatus = http.StatusNotImplemented
	varmqs[0].Mu.Unlock()
	status, body := fetch(t, srv, jobPage("ord-1042"), false)
	if status != http.StatusOK || !strings.Contains(body, "can read one job. That needs varmq API 1.4.0") || strings.Contains(body, "No job") {
		t.Errorf("unsupported = %d:\n%s", status, body)
	}

	const mem = "memory:a:fifo:1"
	varmqs[0].Mu.Lock()
	varmqs[0].Queues = map[string]*fleettest.Queue{mem: {Summary: `{"id":"` + mem + `","counts":{"pending":0,"processing":0,"completed":0,"failed":0},"totals":{"submitted":0,"completed":0,"failed":0}}`}}
	varmqs[0].Mu.Unlock()
	status, body = fetch(t, srv, "/queues/"+mem+"/jobs/r-1?server=multi", false)
	if status != http.StatusNotFound || !strings.Contains(body, "An in-memory queue keeps a job only while it waits or runs") || !strings.Contains(body, "<code>WithEndedJobs</code>") {
		t.Errorf("in-memory missing job = %d:\n%s", status, body)
	}
}

// jobSpans: a missing time, or two out of order, is "—", never negative;
// a waiting or running job's span runs to now, live.
func TestJobSpans(t *testing.T) {
	const now = 100_000
	for _, c := range []struct {
		name               string
		j                  contract.JobRecord
		waited, ran, total JobSpan
	}{
		{"completed", contract.JobRecord{State: contract.JobCompleted, EnqueuedAt: 1000, StartedAt: 1800, FinishedAt: 5200},
			JobSpan{"0.8s", 800, false}, JobSpan{"3.4s", 3400, false}, JobSpan{"4.2s", 4200, false}},
		{"pending", contract.JobRecord{State: contract.JobPending, EnqueuedAt: 40_000},
			JobSpan{"1m 00s", 60_000, true}, JobSpan{Text: "—"}, JobSpan{"1m 00s", 60_000, true}},
		{"running", contract.JobRecord{State: contract.JobProcessing, EnqueuedAt: 40_000, StartedAt: 90_000},
			JobSpan{"50s", 50_000, false}, JobSpan{"10s", 10_000, true}, JobSpan{"1m 00s", 60_000, true}},
		{"in-memory running: no times", contract.JobRecord{State: contract.JobProcessing, Attempts: 1},
			JobSpan{Text: "—"}, JobSpan{Text: "—"}, JobSpan{Text: "—"}},
		{"cancelled waiting", contract.JobRecord{State: contract.JobCancelled, EnqueuedAt: 1000, FinishedAt: 3000},
			JobSpan{Text: "—"}, JobSpan{Text: "—"}, JobSpan{"2.0s", 2000, false}},
		{"no enqueued", contract.JobRecord{State: contract.JobFailed, StartedAt: 1000, FinishedAt: 2000},
			JobSpan{Text: "—"}, JobSpan{"1.0s", 1000, false}, JobSpan{Text: "—"}},
		{"clocks out of order", contract.JobRecord{State: contract.JobCompleted, EnqueuedAt: 5000, StartedAt: 4000, FinishedAt: 3000},
			JobSpan{Text: "—"}, JobSpan{Text: "—"}, JobSpan{Text: "—"}},
		{"enqueued after now", contract.JobRecord{State: contract.JobPending, EnqueuedAt: now + 1},
			JobSpan{Text: "—"}, JobSpan{Text: "—"}, JobSpan{Text: "—"}},
	} {
		w, r, tot := jobSpans(c.j, now)
		if w != c.waited || r != c.ran || tot != c.total {
			t.Errorf("%s: waited %+v ran %+v total %+v, want %+v %+v %+v", c.name, w, r, tot, c.waited, c.ran, c.total)
		}
	}
	for _, id := range []string{"", ".", ".."} {
		if u := jobURL("q", id, "s"); u != "" {
			t.Errorf("jobURL(%q) = %q, want none", id, u)
		}
	}
	if u := jobURL("redisq:fifo:{a/b}", "x.y", "s"); u != "/queues/redisq:fifo:%7Ba%2Fb%7D/jobs/x.y?server=s" {
		t.Errorf("jobURL = %q", u)
	}
}

// A unit that fails, with none holding the job, is the page's banner, not
// "not found" (the job may be on it); the ID "." read as %2E is shown, not
// offered a cancel (a browser cannot post to it); a unit that cannot read
// one job, beside one that says not found, is named as where it may be.
func TestJobPageFailuresAndOddIDs(t *testing.T) {
	varmqs, fakes, _ := jobQueue(t)
	srv, _ := newTestServer(t, fakes...)

	status, body := fetch(t, srv, tenantPath+"/jobs/%2E?server=multi", false)
	if status != http.StatusOK || !strings.Contains(body, `<h1 class="qid">.</h1><span class="jstate state-pending">`) || strings.Contains(body, "Cancel job") {
		t.Errorf("job . = %d:\n%s", status, body)
	}
	want := "GET " + tenantPath + "/jobs/%2E"
	if !slices.Contains(varmqs[0].Calls(), want) && !slices.Contains(varmqs[1].Calls(), want) {
		t.Errorf("no sharer got %s", want)
	}

	// a fails (500), b has no such job: one storage each now
	failing, empty := tenantQueue(), tenantQueue()
	failing.JobStatus = http.StatusInternalServerError
	setQueues(varmqs[0], failing)
	setQueues(varmqs[1], empty)
	status, body = fetch(t, srv, jobPage("ord-1042"), false)
	if status != http.StatusOK || !strings.Contains(body, `<p class="banner">`+tenantID+`: API error (status 500)`) || strings.Contains(body, "No job") {
		t.Errorf("failing unit = %d:\n%s", status, body)
	}

	// b serves no one-job read (router 404), a has no such job
	varmqs[0].Mu.Lock()
	failing.JobStatus = 0
	varmqs[0].Mu.Unlock()
	varmqs[1].Mu.Lock()
	empty.JobStatus = http.StatusNotFound
	varmqs[1].Mu.Unlock()
	b := fleettest.StaticLabels(fakes[0].URL, fakes[1].URL, fakes[2].URL)[fakes[1].URL]
	status, body = fetch(t, srv, jobPage("ord-1042"), false)
	if status != http.StatusNotFound || !strings.Contains(body, "No job") || !strings.Contains(body, "It may be on "+b+": it cannot read one job") {
		t.Errorf("partly unsupported = %d:\n%s", status, body)
	}
}

// A result over maxPayload once indented is cut, and labelled so.
func TestJobResultCut(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"blob": strings.Repeat("x", maxPayload)})
	g := fleet.QueueGroup{QueueSummary: api.QueueSummary{ID: "q"}}
	v := jobView(g, parseQueueID("q"), "j", "dev", fleet.JobRead{Found: true, Job: contract.JobRecord{ID: "j", State: contract.JobCompleted, Payload: []byte("1"), Result: big}}, viewT0)
	if !v.ResultCut || len(v.Result) != maxPayload {
		t.Fatalf("result cut %v, %d bytes, want cut at %d", v.ResultCut, len(v.Result), maxPayload)
	}
	body := run(t, "job", "layout", Page{Server: "dev", Job: v})
	if !strings.Contains(body, `<h2 id="rs-head">Result <span class="sub">(first 64 KiB)</span></h2>`) || len(body) > 2*maxPayload {
		t.Errorf("cut result: no label, or %d bytes rendered", len(body))
	}
}

// The worker page draws its sources in priority order, each with its glyph,
// name and link, and a priority chip under the priority strategy; the
// worker node says how it picks: the strategy with 2+ sources, "Pulls
// directly" with one, nothing (and a note) from queue_ids alone. A topic
// and storage with no Queue ID have no link. No Submitted card.
func TestWorkerSourcesPage(t *testing.T) {
	const (
		redis  = "redisq:shop:fifo:{orders}"
		sqlite = "sqliteq:9f86d081:fifo:images"
		memory = "memory:a:fifo:1"
	)
	_, fake := fleettest.NewVarmq(t, "a", fleettest.SourceWorkers(redis, sqlite, memory))
	// b: an older varmq whose orders consumes one more queue
	const extra = "redisq:fifo:{extra}"
	_, old := fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 1, QueueIDs: []string{extra}}})
	srv, _ := newTestServer(t, fake, old)
	esc := func(id string) string { return "/queues/" + url.PathEscape(id) + "?server=multi" }

	_, body := fetch(t, srv, "/workers/orders?server=multi", false)
	var order []string
	for _, m := range regexp.MustCompile(`<li><a class="srcn" href="([^"]+)" aria-label="[^"]+"><span class="qn" title="[^"]+"><svg class="sico"[^>]*><path d="[^"]{0,12}[^"]*"/></svg><span class="sr">([^<]+)</span>`).FindAllStringSubmatch(body, -1) {
		order = append(order, m[1]+" "+m[2])
	}
	if want := []string{esc(redis) + " Redis: ", esc(sqlite) + " SQLite: ", esc(memory) + " In memory: ", esc(extra) + " Redis: "}; !slices.Equal(order, want) {
		t.Errorf("source nodes = %q, want %q", order, want)
	}
	for _, want := range []string{
		`<h2 class="section" id="src-head">Sources <span class="sub">4, in the worker's order; the strategy picks among them</span></h2>`,
		`<span class="qnm">orders</span> <span class="qkind">FIFO</span></span><span class="prio" title="bind priority: lower picks first">p 1</span></a>`,
		`<span class="prio" title="bind priority: lower picks first">p 5</span>`,
		`<span class="qnm">orders #1</span>`, // in-memory: named by its worker
		`<div class="wnode"><span class="nm"><span class="status active"></span>orders</span><small>Priority: lowest number first</small></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("orders page lacks %q", want)
		}
	}
	if strings.Count(body, `class="prio"`) != 2 || strings.Contains(body, "Submitted") || strings.Contains(body, "srcnote") {
		t.Errorf("orders: a priority chip on the unset one, a Submitted card or the queue_ids note:\n%s", body)
	}

	_, body = fetch(t, srv, "/workers/emails?server=multi", false)
	if !strings.Contains(body, "<small>Pulls directly</small>") || strings.Contains(body, `class="prio"`) || !strings.Contains(body, "what the worker pulls from") {
		t.Errorf("one source:\n%s", body)
	}
	_, body = fetch(t, srv, "/workers/fanout?server=multi", false)
	for _, want := range []string{
		"<small>Round robin: each source in turn</small>",
		`<li><span class="srcn"><svg class="sico" viewBox="0 0 24 24"`, `<span class="qnm">topic</span></span></li>`,
		`<li><span class="srcn"><span class="qnm none">custom storage</span></span></li>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fanout page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="prio"`) {
		t.Error("fanout: a priority chip under round robin")
	}
	_, body = fetch(t, srv, "/workers/legacy?server=multi", false)
	if !strings.Contains(body, "From <code>queue_ids</code>") || !strings.Contains(body, `<span class="status active"></span>legacy</span></div>`) || strings.Count(body, `<li><a class="srcn"`) != 2 {
		t.Errorf("older varmq: want 2 nodes from queue_ids, no label, the note:\n%s", body)
	}
}

// WorkerSources' label: none without a strategy, an unknown one as sent,
// "Pulls directly" for one source whatever the strategy; a priority chip
// only under priority, none for the largest int64 (no priority).
func TestWorkerSourcesLabel(t *testing.T) {
	two, max := int64(2), int64(math.MaxInt64)
	srcs := []api.WorkerSource{{QueueID: "a", Kind: "queue", Priority: &two}, {QueueID: "b", Kind: "queue", Priority: &max}}
	for _, c := range []struct {
		strategy string
		srcs     []api.WorkerSource
		label    string
		prio     []string
	}{
		{"", srcs, "", []string{"", ""}},
		{"lifo_magic", srcs, "lifo_magic", []string{"", ""}},
		{"priority", srcs, "Priority: lowest number first", []string{"p 2", ""}},
		{"max_len", srcs, "Max length: longest queue first", []string{"", ""}},
		{"min_len", srcs[:1], "Pulls directly", []string{""}},
		{"priority", srcs[:1], "Pulls directly", []string{"p 2"}},
	} {
		v := Page{Server: "s", Detail: &fleet.DetailView{Strategy: c.strategy, Sources: c.srcs}}.WorkerSources()
		var prio []string
		for _, n := range v.Nodes {
			prio = append(prio, n.Priority)
		}
		if v.Label != c.label || !slices.Equal(prio, c.prio) {
			t.Errorf("%q: label %q prio %q, want %q %q", c.strategy, v.Label, prio, c.label, c.prio)
		}
	}
	// an older varmq does not say how it picks, even from one queue
	if v := (Page{Detail: &fleet.DetailView{QueueIDs: []string{"a"}}}).WorkerSources(); v == nil || !v.Legacy || v.Label != "" || len(v.Nodes) != 1 {
		t.Errorf("legacy, one queue: %+v, want one node, no label", v)
	}
	if v := (Page{Detail: &fleet.DetailView{}}).WorkerSources(); v != nil {
		t.Errorf("no sources, no queue_ids: %+v, want none", v)
	}
	if v := (Page{Stale: "old", Detail: &fleet.DetailView{QueueIDs: []string{"a"}}}).WorkerSources(); v != nil {
		t.Error("stale page: sources shown")
	}
}

// workerChips leaves out a pod that is down: its workers' queue_ids are
// the last it said, not now.
func TestWorkerChipsSkipsDownPod(t *testing.T) {
	w := smp("Running", 1, 0, 1, 0)
	w.QueueIDs = []string{"q"}
	down := okPod("http://b", map[string]*fleet.Sample{"old": w})
	down.Err = "connection refused"
	up := smp("Running", 1, 0, 1, 0)
	up.QueueIDs = []string{"q"}
	got := workerChips(snap(viewT0, okPod("http://a", map[string]*fleet.Sample{"new": up}), down), "dev")
	if len(got["q"]) != 1 || got["q"][0].Name != "new" {
		t.Errorf("chips = %+v, want only new", got["q"])
	}
}

// TestSourceStateAndCut: a source node says its queue's live state
// (paused, empty, N pending; none when not read), cuts a long name in the
// middle and keeps the whole one in its accessible name.
func TestSourceStateAndCut(t *testing.T) {
	long := "redisq:fifo:{tenant-eu-west-1-orders-priority-retry}"
	ids := []string{"redisq:fifo:{a}", "redisq:fifo:{b}", "redisq:fifo:{c}", long}
	yes := true
	q := func(id string, pending int64, paused *bool) fleet.QueueGroup {
		return fleet.QueueGroup{QueueSummary: api.QueueSummary{ID: id, Counts: &contract.JobCounts{Pending: pending}, Paused: paused}}
	}
	v := fleet.QueuesView{Groups: []fleet.QueueGroup{q(ids[0], 1840, nil), q(ids[1], 0, nil), q(ids[2], 5, &yes)}}
	slices.SortFunc(v.Groups, func(a, b fleet.QueueGroup) int { return strings.Compare(a.ID, b.ID) })
	p := Page{Server: "s", Detail: &fleet.DetailView{Name: "w", QueueIDs: ids}, srcq: &v}
	src := p.WorkerSources()
	for i, want := range []string{"1840 pending", "empty", "paused", ""} {
		if got := src.Nodes[i].State; got != want {
			t.Errorf("node %d state = %q, want %q", i, got, want)
		}
	}
	n := src.Nodes[3]
	if r := []rune(n.Queue.Name); len(r) != srcNameMax || !strings.Contains(n.Queue.Name, "…") || !strings.HasPrefix(n.Queue.Name, "tenant") || !strings.HasSuffix(n.Queue.Name, "retry") {
		t.Errorf("cut name = %q", n.Queue.Name)
	}
	if n.Full != "tenant-eu-west-1-orders-priority-retry" || !strings.HasPrefix(n.Aria(), n.Full+", FIFO") {
		t.Errorf("full = %q, aria = %q", n.Full, n.Aria())
	}
	if got := midCut("short", 28); got != "short" {
		t.Errorf("midCut(short) = %q", got)
	}

	body := run(t, "detail", "layout", p)
	for _, want := range []string{
		`aria-label="a, FIFO, 1840 pending"`, `<span class="srcst">1840 pending</span>`,
		`<span class="srcst">empty</span>`, `<span class="status paused">paused</span>`,
		`<ol class="srcs" aria-label="Sources of w, in the worker's order">`,
		"4, in the worker's order",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sources lack %q", want)
		}
	}
}

// TestQueuePageOrderAndEmpty: an in-memory queue's name carries its
// instance; the job list comes before the workers table; an empty list
// under a non-zero count says so, not "No pending jobs".
func TestQueuePageOrderAndEmpty(t *testing.T) {
	if q := parseQueueID("memory:shop-a:fifo:1"); q.Inst != "shop-a" {
		t.Errorf("memory queue instance = %q, want shop-a", q.Inst)
	}
	if body := run(t, "queues", "qname", parseQueueID("memory:shop-a:fifo:1")); !strings.Contains(body, `<span class="qkind">FIFO</span> <span class="qinst">on shop-a</span>`) {
		t.Errorf("qname lacks the instance:\n%s", body)
	}
	id := "redisq:fifo:{emails}"
	p := Page{Server: "s", Noun: instanceNoun, Jobs: &JobsView{
		Queue: fleet.QueueGroup{QueueSummary: api.QueueSummary{ID: id}}, State: contract.JobPending, Total: 1840,
		Workers: []QueueWorker{{Unit: "i-1", Name: "mailer", URL: "/workers/mailer", Status: "Paused", Class: "paused"}},
	}}
	body := run(t, "queue", "layout", p)
	if !strings.Contains(body, "The count says 1840 pending, but no instance listed any") || strings.Contains(body, "No pending jobs") {
		t.Errorf("empty list under a count:\n%s", body)
	}
	if strings.Index(body, `class="states"`) > strings.Index(body, `id="qworkers-head"`) {
		t.Error("the workers table comes before the job list")
	}
	p.Jobs.Total = 0
	if body := run(t, "queue", "layout", p); !strings.Contains(body, "No pending jobs. New jobs show here until a worker takes them.") {
		t.Errorf("empty pending list lacks its next step:\n%s", body)
	}
}
