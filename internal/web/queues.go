package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
)

// The queue pages read varmq live, on GET: the jobs of a queue are paged
// and too many to collect, and its counts are read with them so that they
// agree. The one write is a queue's pause and resume (queueAction); no job
// is retried, removed or purged.

const (
	defaultJobLimit = 50   // the spec's default page
	maxJobLimit     = 1000 // the spec's largest page
	// maxPayload is the most of a payload a job page shows.
	// ponytail: cut, not paged; a payload viewer if large ones matter.
	maxPayload = 64 << 10
	// maxPreview is the most of a payload or error a job's list line shows.
	maxPreview = 160
)

// liveConfigChangedMsg is the notice of a server whose newest snapshot was
// polled under its previous endpoints: the token read now must not go to
// them. what names what the page reads live: queues, workflows.
func liveConfigChangedMsg(server, what string) string {
	return "The configuration of " + server + " changed; " + what + " show once its first snapshot under it is taken. Reload in a few seconds."
}

// JobsView is the job page of one queue: a page of its jobs in State,
// read from the unit From.
type JobsView struct {
	Queue fleet.QueueGroup
	Ctl   QueueCtl
	State contract.JobState
	// Unlisted: the server refused to list State (varmq main answers 400
	// for cancelled, a state its spec reserves).
	Unlisted bool
	Tabs     []JobTab
	Jobs     []JobRow
	From     string
	Offset   int
	Limit    int
	Total    int64 // the count of State, -1: not known
	// Prev and Next are the URLs of the pages around this one, "" at an end.
	Prev, Next string
	// Pages are the numbered page links, when Total is known; a zero N is a gap.
	Pages []PageLink
}

// QueueCtl is the paused badge and the pause and resume buttons of a queue's
// page (#qstate), and, with OOB, the same as an out-of-band swap for an
// action response. Since, SinceFull and SinceISO say when it was paused
// ("" when not known). CanPause: some unit reporting it serves the pause
// route; NoRoute: the units read the queue and none does (older varmq).
// Pause and Resume: which buttons show — Resume alone when every unit says
// paused, Pause alone when none does, both when they differ or one says
// nothing.
type QueueCtl struct {
	Server, ID                 string
	Paused, CanPause, NoRoute  bool
	Pause, Resume              bool
	Since, SinceFull, SinceISO string
	OOB                        bool
}

// queueCtl is the QueueCtl of queue g on server.
func queueCtl(g fleet.QueueGroup, server string) QueueCtl {
	q := g.QueueSummary
	all, none := g.PauseState()
	c := QueueCtl{Server: server, ID: q.ID, Paused: q.IsPaused(), CanPause: q.CanPause(), NoRoute: q.Err == "" && !q.CanPause(),
		Pause: !all, Resume: !none}
	if c.Paused && q.PausedAt > 0 {
		c.Since = shortTime(time.UnixMilli(q.PausedAt), time.Now())
		c.SinceFull, c.SinceISO = msTime(q.PausedAt)
	}
	return c
}

// PageLink is one numbered link of the pager.
type PageLink struct {
	N       int // from 1; 0: a gap ("…")
	URL     string
	Current bool
}

// pageLinks numbers the pages of total items, limit a page, around the page
// at offset: the first, the last and two either side of it, gaps between.
// Nil for one page or an unknown total. Cost does not grow with total: a
// remote instance reports it, so it can be any int64.
func pageLinks(total int64, offset, limit int, url func(offset int) string) []PageLink {
	if total <= int64(limit) || limit <= 0 {
		return nil
	}
	// capped so (last-1)*limit stays an int offset
	last := int(min((total-1)/int64(limit)+1, int64(math.MaxInt/limit)))
	cur := offset/limit + 1
	var out []PageLink
	add := func(n int) {
		if len(out) > 0 && out[len(out)-1].N < n-1 {
			out = append(out, PageLink{}) // gap
		}
		out = append(out, PageLink{N: n, URL: url((n - 1) * limit), Current: n == cur})
	}
	add(1)
	for n := max(2, cur-2); n <= min(last-1, cur+2); n++ {
		add(n)
	}
	add(last)
	return out
}

// Last is the position of the page's last job, from 1.
func (v *JobsView) Last() int { return v.Offset + len(v.Jobs) }

// JobTab is the link to one Job State of a queue, with its count now (-1:
// not known).
type JobTab struct {
	State   contract.JobState
	URL     string
	Count   int64
	Current bool
}

// JobRow is one job as the page shows it: times as local clock text and
// RFC 3339 (empty when absent), the payload indented. LastError is the text
// of the last of Errors. When is its latest time, short; WhenISO and
// WhenFull the same time in full; Preview its list line: LastError, else the
// payload on one line, cut.
type JobRow struct {
	ID, LastError                      string
	When, WhenISO, WhenFull, Preview   string
	Errors                             []ErrorRow // oldest first
	Attempts, N                        int        // N: its position in the state, from 1
	Enqueued, Started, Finished        string
	EnqueuedISO, StartedISO, FinishISO string
	Payload                            string
	Cut                                bool // Payload holds only its first maxPayload bytes
}

// ErrorRow is one failed attempt of a job as the page shows it.
type ErrorRow struct {
	Attempt   int
	At, AtISO string
	Error     string
}

// queuesURL is the queues page of server.
func queuesURL(server string) string { return pageURL("/queues", server, "", "") }

// jobsURL is the job page of queue id on server, in state, from offset.
// The defaults are left out.
func jobsURL(id, server string, state contract.JobState, offset, limit int) string {
	q := url.Values{}
	if server != "" {
		q.Set("server", server)
	}
	if state != "" && state != api.JobStates[0] {
		q.Set("state", string(state))
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

// readQueues is the queues of p's server, read live (see liveClients).
// Otherwise p says why.
func (h *handler) readQueues(r *http.Request, p *Page, snaps []*fleet.Snapshot) (fleet.QueuesView, bool) {
	clients, label, ok := h.liveClients(p, snaps, "queues")
	if !ok {
		return fleet.QueuesView{}, false
	}
	return fleet.GroupQueues(fleet.FetchQueues(r.Context(), clients, label)), true
}

// liveClients are the clients of the units of the newest of snaps, with
// p's server's token, and how the pages label them, for a page that reads
// varmq live. As for an action (see bind), the snapshot must be current
// under a fresh lease of this environment's collector and polled under the
// server's endpoints now. Otherwise ok is false and p says why; what names
// what the page reads (queues, workflows).
func (h *handler) liveClients(p *Page, snaps []*fleet.Snapshot, what string) (clients []*api.Client, label func(*api.Client, string) string, ok bool) {
	cfg, err := h.config()
	if err != nil {
		p.Banner = "cannot read the config: " + err.Error()
		return nil, nil, false
	}
	srv := cfg.Servers[p.Server]
	if srv == nil {
		p.Banner = fleet.NotConfiguredMsg(p.Server)
		return nil, nil, false
	}
	latest, now := latestOf(snaps), time.Now()
	switch d := h.collector(); {
	case d.Err != nil:
		p.Banner = fleet.LeaseErrMsg(d.Err)
		return nil, nil, false
	case d.Other:
		return nil, nil, false // p.Stale says why
	case !d.Acts(latest, srv.PollInterval(), now):
		if p.Stale == "" {
			p.Stale = h.staleMsg(p.Server, latest, now)
		}
		return nil, nil, false
	case latest.Source != fleet.SourceOf(srv):
		p.Banner = liveConfigChangedMsg(p.Server, what)
		return nil, nil, false
	}
	return fleet.ClientsFor(latest, srv.Token), fleet.UnitLabel(latest), true
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
	v := &JobsView{Queue: g, State: contract.JobState(r.FormValue("state")), Limit: defaultJobLimit}
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
	v.Ctl = queueCtl(g, p.Server)
	if g.Err != "" {
		p.Banner = g.ID + ": " + g.Err
		return v
	}
	jobs, from, err := fleet.FetchJobs(r.Context(), g, v.State, v.Offset, v.Limit)
	var apiErr *api.APIError
	switch {
	case err != nil && v.State == contract.JobCancelled && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest:
		v.Unlisted = true // not a failure: this varmq does not list the state
		return v
	case err != nil:
		p.Banner = g.ID + ": " + err.Error()
		return v
	}
	v.From, v.Total = from, stateCount(g.Counts, v.State)
	for i, j := range jobs {
		v.Jobs = append(v.Jobs, jobRow(j, v.Offset+i+1))
	}
	if v.Offset > 0 {
		v.Prev = jobsURL(g.ID, p.Server, v.State, max(0, v.Offset-v.Limit), v.Limit)
	}
	// The answer has no total: a full page has a next one unless the state's
	// count, read just before, says this page ends it.
	if n := stateCount(g.Counts, v.State); len(jobs) == v.Limit && (n < 0 || int64(v.Offset+len(jobs)) < n) {
		v.Next = jobsURL(g.ID, p.Server, v.State, v.Offset+v.Limit, v.Limit)
	}
	v.Pages = pageLinks(v.Total, v.Offset, v.Limit, func(off int) string { return jobsURL(g.ID, p.Server, v.State, off, v.Limit) })
	return v
}

// stateCount is c's count of state, -1 without counts or for a state they
// do not count (cancelled: spec 1.3.0 counts have no such field).
func stateCount(c *contract.JobCounts, state contract.JobState) int64 {
	if c == nil {
		return -1
	}
	switch state {
	case contract.JobPending:
		return c.Pending
	case contract.JobProcessing:
		return c.Processing
	case contract.JobCompleted:
		return c.Completed
	case contract.JobFailed:
		return c.Failed
	}
	return -1
}

// queueActions are the actions a queue route accepts.
var queueActions = []string{"pause", "resume"}

// QueueConfirmView is the confirmation panel of a queue action: Action on
// queue ID of Server, sent to each of Units (a unit that does not serve the
// route is sent nothing).
type QueueConfirmView struct {
	Action, Server, ID string
	Noun               fleet.Noun
	Units              []fleet.QueueUnit
}

// queueAction pauses or resumes queue id on every unit of the form's
// server that reports it (see fleet.QueueAct). As an action on workers, it
// is refused unless the server's newest snapshot may be acted on (bind).
// Without confirm=1 it only answers the confirmation panel and calls
// nothing. The answer is the results, and the page's #qstate out of band,
// as the units answered the action (see fleet.QueueAct).
func (h *handler) queueAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if !slices.Contains(queueActions, action) {
		badRequest(w, "unknown queue action: want pause or resume")
		return
	}
	t, ok := h.bind(w, r)
	if !ok {
		return
	}
	clients := fleet.ClientsFor(t.latest, t.srv.Token)
	label := fleet.UnitLabel(t.latest)
	g, found := fleet.GroupQueues(fleet.FetchQueues(r.Context(), clients, label)).Group(id)
	switch {
	case !found:
		conflict(w, "Queue "+strconv.Quote(id)+" is no longer reported by any "+t.noun.One+" — reload")
		return
	case g.Err != "" && !g.CanPause():
		conflict(w, "cannot read queue "+strconv.Quote(id)+": "+g.Err)
		return
	case !g.CanPause():
		badRequest(w, "no "+t.noun.One+" reporting this queue supports pause and resume (varmq API 1.2.0)")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		h.fragments(w, fragment{"confirm", "queue_confirm", QueueConfirmView{Action: action, Server: t.key, ID: id, Noun: t.noun, Units: g.Units()}})
		return
	}
	results, after := fleet.QueueAct(mutationCtx(r), g, fleet.UnitsOf(t.latest), t.noun, action)
	ctl := queueCtl(after, t.key)
	ctl.OOB = true
	h.fragments(w, fragment{"results", "results", ResultsView{Results: results, Noun: t.noun}}, fragment{"queue", "qstate", ctl})
}

func jobRow(j contract.JobRecord, n int) JobRow {
	row := JobRow{ID: j.ID, Attempts: j.Attempts, N: n}
	for _, e := range j.Errors {
		er := ErrorRow{Attempt: e.Attempt, Error: e.Error}
		er.At, er.AtISO = msTime(e.At)
		row.Errors, row.LastError = append(row.Errors, er), e.Error
	}
	row.Enqueued, row.EnqueuedISO = msTime(j.EnqueuedAt)
	row.Started, row.StartedISO = msTime(j.StartedAt)
	row.Finished, row.FinishISO = msTime(j.FinishedAt)
	for _, t := range []struct {
		what string
		ms   int64
	}{{"finished", j.FinishedAt}, {"started", j.StartedAt}, {"enqueued", j.EnqueuedAt}} {
		if t.ms != 0 {
			full, iso := msTime(t.ms)
			row.When, row.WhenISO, row.WhenFull = shortTime(time.UnixMilli(t.ms), time.Now()), iso, t.what+" "+full
			break
		}
	}
	row.Preview = row.LastError
	if row.Preview == "" {
		var one bytes.Buffer
		if json.Compact(&one, j.Payload) == nil {
			row.Preview = one.String()
		}
	}
	if r := []rune(row.Preview); len(r) > maxPreview {
		row.Preview = string(r[:maxPreview]) + "…"
	}
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

// shortTime is t as the list shows it: the clock today, the day and clock
// this year, else the date.
func shortTime(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04:05")
	case t.Year() == now.Year():
		return t.Format("Jan 2 15:04")
	}
	return t.Format("2006-01-02")
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

// QueueName is a Queue ID as the pages show it, parsed from the formats
// of the storages vartop knows (see parseQueueID). Links and titles keep
// the ID in full.
type QueueName struct {
	ID      string
	Backend string // redis, sqlite, memory; "": other storage, Name is the ID
	Name    string
	Kind    string // FIFO, Priority; "" when the ID says neither
	NS      string // the redisq namespace, "" for none
	n       string // memory: the queue's number in its process
}

// Store names the backend for a screen reader (the icon is hidden).
func (q QueueName) Store() string {
	return map[string]string{"redis": "Redis", "sqlite": "SQLite", "memory": "In memory"}[q.Backend]
}

var queueKinds = map[string]string{"fifo": "FIFO", "prio": "Priority"}

// parseQueueID reads id by the documented formats:
//
//	redisq:[<ns>:]fifo|prio:{<key>}  ns may hold ':', never '{' or '}'
//	sqliteq:<h>:fifo|prio:<name>     h: hash of the file's path
//	memory:<instance>:fifo|prio:<n>  varmq API 1.4.0
//	memory:<instance>:<n>            before it, no kind
//
// Anything else is other storage: the whole ID as its name.
func parseQueueID(id string) QueueName {
	q := QueueName{ID: id, Name: id}
	switch {
	case strings.HasPrefix(id, "redisq:") && strings.HasSuffix(id, "}"):
		head, key, _ := strings.Cut(id[len("redisq:"):len(id)-1], "{")
		pre, kind, ok := cutKind(head)
		if !ok || pre == ":" || strings.Contains(head, "}") || key == "" || strings.ContainsAny(key, "{}") {
			return q
		}
		q.Backend, q.Name, q.Kind, q.NS = "redis", key, kind, strings.TrimSuffix(pre, ":")
	case strings.HasPrefix(id, "sqliteq:"):
		p := strings.SplitN(id, ":", 4)
		if len(p) != 4 || p[1] == "" || p[3] == "" || queueKinds[p[2]] == "" {
			return q
		}
		q.Backend, q.Name, q.Kind = "sqlite", p[3], queueKinds[p[2]]
	case strings.HasPrefix(id, "memory:"):
		rest := id[len("memory:"):]
		i := strings.LastIndexByte(rest, ':')
		if i < 0 || i == len(rest)-1 || strings.Trim(rest[i+1:], "0123456789") != "" {
			return q
		}
		inst := rest[:i]
		if pre, kind, ok := cutKind(inst + ":"); ok && pre != "" {
			inst, q.Kind = strings.TrimSuffix(pre, ":"), kind
		}
		if inst == "" {
			return q
		}
		q.Backend, q.n = "memory", rest[i+1:]
		q.Name = "in-memory #" + q.n
	}
	return q
}

// cutKind cuts a trailing "fifo:" or "prio:" off s, alone or after a ':':
// what is before it ("" or ending in ':'), and the kind's label.
func cutKind(s string) (pre, kind string, ok bool) {
	for k, label := range queueKinds {
		if t, found := strings.CutSuffix(s, k+":"); found && (t == "" || strings.HasSuffix(t, ":")) {
			return t, label, true
		}
	}
	return "", "", false
}

// queueOwners maps an in-memory Queue ID to the worker consuming it, by
// the workers' queue_ids (varmq API 1.4.0). Nil: names fall back to
// "in-memory #n".
type queueOwners map[string]string

// ownersOf reads s's queue_ids; of two workers on one queue, the first by name.
func ownersOf(s *fleet.Snapshot) queueOwners {
	var o queueOwners
	for _, p := range fleet.PodsOf(s, "") {
		for name, sm := range p.Workers {
			for _, id := range sm.QueueIDs {
				if !strings.HasPrefix(id, "memory:") {
					continue
				}
				if o == nil {
					o = make(queueOwners)
				}
				if cur, ok := o[id]; !ok || name < cur {
					o[id] = name
				}
			}
		}
	}
	return o
}

// name is id parsed, an in-memory queue named after its worker.
func (o queueOwners) name(id string) QueueName {
	q := parseQueueID(id)
	if w := o[id]; w != "" && q.Backend == "memory" {
		q.Name = w + " #" + q.n
	}
	return q
}
