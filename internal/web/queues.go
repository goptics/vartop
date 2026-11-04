package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
)

// The queue pages read varmq live, on GET: the jobs of a queue are paged
// and too many to collect, and its counts are read with them so that they
// agree. They are read-only: no job is retried, removed or purged.

const (
	defaultJobLimit = 50   // the spec's default page
	maxJobLimit     = 1000 // the spec's largest page
	// maxPayload is the most of a payload a job page shows.
	// ponytail: cut, not paged; a payload viewer if large ones matter.
	maxPayload = 64 << 10
)

// queueConfigChangedMsg is the notice of a server whose newest snapshot was
// polled under its previous endpoints: the token read now must not go to
// them.
func queueConfigChangedMsg(server string) string {
	return "The configuration of " + server + " changed; queues show once its first snapshot under it is taken. Reload in a few seconds."
}

// JobsView is the job page of one queue: a page of its jobs in State,
// read from the unit From.
type JobsView struct {
	Queue  fleet.QueueGroup
	State  string
	Tabs   []JobTab
	Jobs   []JobRow
	From   string
	Offset int
	Limit  int
	// Prev and Next are the URLs of the pages around this one, "" at an end.
	Prev, Next string
}

// JobTab is the link to one Job State of a queue, with its count now (-1:
// not known).
type JobTab struct {
	State, URL string
	Count      int
	Current    bool
}

// JobRow is one job as the page shows it: times as local clock text and
// RFC 3339 (empty when absent), the payload indented.
type JobRow struct {
	ID, LastError                      string
	Attempts, N                        int // N: its position in the state, from 1
	Enqueued, Started, Finished        string
	EnqueuedISO, StartedISO, FinishISO string
	Payload                            string
	Cut                                bool // Payload holds only its first maxPayload bytes
}

// queuesURL is the queues page of server.
func queuesURL(server string) string { return pageURL("/queues", server, "", "") }

// jobsURL is the job page of queue id on server, in state, from offset.
// The defaults are left out.
func jobsURL(id, server, state string, offset, limit int) string {
	q := url.Values{}
	if server != "" {
		q.Set("server", server)
	}
	if state != "" && state != api.JobStates[0] {
		q.Set("state", state)
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if limit != defaultJobLimit {
		q.Set("limit", strconv.Itoa(limit))
	}
	u := "/queues/" + url.PathEscape(id)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// readQueues is the queues of p's server, read from the units of the newest
// of snaps with the server's token, and whether they could be: as for an
// action (see bind), the snapshot must be current under a fresh lease of
// this environment's collector and polled under the server's endpoints now.
// Otherwise p says why.
func (h *handler) readQueues(r *http.Request, p *Page, snaps []*fleet.Snapshot) (fleet.QueuesView, bool) {
	cfg, err := h.config()
	if err != nil {
		p.Banner = "cannot read the config: " + err.Error()
		return fleet.QueuesView{}, false
	}
	srv := cfg.Servers[p.Server]
	if srv == nil {
		p.Banner = fleet.NotConfiguredMsg(p.Server)
		return fleet.QueuesView{}, false
	}
	latest, now := latestOf(snaps), time.Now()
	switch d := h.collector(); {
	case d.Err != nil:
		p.Banner = fleet.LeaseErrMsg(d.Err)
		return fleet.QueuesView{}, false
	case d.Other:
		return fleet.QueuesView{}, false // p.Stale says why
	case !d.Acts(latest, srv.PollInterval(), now):
		if p.Stale == "" {
			p.Stale = h.staleMsg(p.Server, latest, now)
		}
		return fleet.QueuesView{}, false
	case latest.Source != fleet.SourceOf(srv):
		p.Banner = queueConfigChangedMsg(p.Server)
		return fleet.QueuesView{}, false
	}
	clients := fleet.ClientsFor(latest, srv.Token)
	return fleet.GroupQueues(fleet.FetchQueues(r.Context(), clients, fleet.UnitLabel(latest))), true
}

func (h *handler) queues(w http.ResponseWriter, r *http.Request) {
	p, snaps, ok := h.page(r)
	p.Title = pageTitle("Queues", p.Server)
	p.Pods = nil // queues are shared by the units: no unit select
	if ok {
		if v, read := h.readQueues(r, &p, snaps); read {
			p.Queues = &v
		}
	}
	h.render(w, "queues", p.status(r), p)
}

// queue is the job page of one queue. It is 404 when no unit reports the
// queue, as detail is for a worker. It has no poller: a refresh would
// close the job open in it.
func (h *handler) queue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, snaps, ok := h.page(r)
	p.Title = pageTitle(id, p.Server)
	p.Pods, p.IntervalSec = nil, 0
	status := p.status(r)
	if ok {
		if v, read := h.readQueues(r, &p, snaps); read {
			if g, found := v.Group(id); found {
				p.Jobs = h.jobs(r, &p, g)
			} else {
				p.Banner = "Queue " + strconv.Quote(id) + " not found on any " + p.Noun.One
				status = http.StatusNotFound
			}
		}
	}
	h.render(w, "queue", status, p)
}

// jobs reads the page of g's jobs that r asks for (state, offset, limit;
// bad values give way to the defaults). A read that fails leaves the list
// empty and puts the error in p's banner.
func (h *handler) jobs(r *http.Request, p *Page, g fleet.QueueGroup) *JobsView {
	v := &JobsView{Queue: g, State: r.FormValue("state"), Limit: defaultJobLimit}
	if !slices.Contains(api.JobStates, v.State) {
		v.State = api.JobStates[0]
	}
	if n, err := strconv.Atoi(r.FormValue("offset")); err == nil && n > 0 {
		v.Offset = n
	}
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > 0 {
		v.Limit = min(n, maxJobLimit)
	}
	for _, s := range api.JobStates {
		v.Tabs = append(v.Tabs, JobTab{State: s, URL: jobsURL(g.ID, p.Server, s, 0, v.Limit), Count: stateCount(g.Counts, s), Current: s == v.State})
	}
	if g.Err != "" {
		p.Banner = g.ID + ": " + g.Err
		return v
	}
	jobs, from, err := fleet.FetchJobs(r.Context(), g, v.State, v.Offset, v.Limit)
	if err != nil {
		p.Banner = g.ID + ": " + err.Error()
		return v
	}
	v.From = from
	for i, j := range jobs {
		v.Jobs = append(v.Jobs, jobRow(j, v.Offset+i+1))
	}
	if v.Offset > 0 {
		v.Prev = jobsURL(g.ID, p.Server, v.State, max(0, v.Offset-v.Limit), v.Limit)
	}
	if len(jobs) == v.Limit {
		v.Next = jobsURL(g.ID, p.Server, v.State, v.Offset+v.Limit, v.Limit)
	}
	return v
}

// stateCount is c's count of state, -1 without counts.
func stateCount(c *api.QueueCounts, state string) int {
	if c == nil {
		return -1
	}
	switch state {
	case "pending":
		return c.Pending
	case "processing":
		return c.Processing
	case "completed":
		return c.Completed
	}
	return c.Failed
}

func jobRow(j api.Job, n int) JobRow {
	row := JobRow{ID: j.ID, LastError: j.LastError, Attempts: j.Attempts, N: n}
	row.Enqueued, row.EnqueuedISO = msTime(j.EnqueuedAt)
	row.Started, row.StartedISO = msTime(j.StartedAt)
	row.Finished, row.FinishISO = msTime(j.FinishedAt)
	payload := []byte(j.Payload)
	var buf bytes.Buffer
	if json.Indent(&buf, payload, "", "  ") == nil {
		payload = buf.Bytes()
	}
	if len(payload) > maxPayload {
		payload, row.Cut = payload[:maxPayload], true
	}
	row.Payload = string(payload)
	return row
}

// msTime is Unix ms as local clock text and as RFC 3339; both "" for 0
// (absent).
func msTime(ms int64) (string, string) {
	if ms == 0 {
		return "", ""
	}
	t := time.UnixMilli(ms)
	return t.Local().Format("2006-01-02 15:04:05.000"), t.UTC().Format(time.RFC3339Nano)
}
