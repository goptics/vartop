package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
		`<h1 class="qid">` + tenantID + "</h1>",
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

	if status, _ := fetch(t, srv, "/queues/nope?server=multi", false); status != http.StatusNotFound {
		t.Errorf("unknown queue = %d, want 404", status)
	}
}

// The cancelled tab: no count (spec counts have none, so never failed's),
// and a varmq that refuses the state (400) is a note, not an error.
func TestQueueCancelledTab(t *testing.T) {
	_, fakes := queueFakes(t)
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
