package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/fleet"
)

// The one-job page reads one job live (fleet.FetchJob), as the queue page
// reads its list; its one write is the job's cancel (jobAction).

// jobURL is the page of job jobID of queue id on server; "" for an ID a
// browser cannot address: none, "." or ".." (its URL parser drops the
// segment, %2E or not).
func jobURL(id, jobID, server string) string {
	if jobID == "" || jobID == "." || jobID == ".." || id == "" {
		return ""
	}
	return pageURL("/queues/"+url.PathEscape(id)+"/jobs/"+url.PathEscape(jobID), server, "", "")
}

// JobView is the page of one job: Job as From read it, when Found.
// Otherwise NoRoute says no unit reporting the queue can read one job,
// else none has it (Memory: an in-memory queue, which keeps no ended job
// by default).
type JobView struct {
	Queue    QueueName
	QueueURL string
	ID       string
	Found    bool
	NoRoute  bool
	Memory   bool
	From     string
	// Unsupported labels the units reporting the queue that cannot read
	// one job (the page names them only when some can).
	Unsupported []string

	State string
	Row   JobRow // times, errors, payload
	// Waited, Ran and Total are the spans between its times (see jobSpans).
	Waited, Ran, Total JobSpan
	// Result is the stored result, indented; "" for none. ResultCut: its
	// first maxPayload bytes only.
	Result    string
	ResultCut bool
	Ctl       JobCtl
}

// JobSpan is a span of a job's timeline as the page shows it: Text, "—"
// when a time it needs is missing. Live: it runs on at now, from Ms (the
// span at render time); job.js ticks it.
type JobSpan struct {
	Text string
	Ms   int64
	Live bool
}

// JobCtl is a job's cancel button (#jobctl): Cancel while it waits or runs.
type JobCtl struct {
	Server, QueueID, ID string
	Cancel              bool
}

// Post is the cancel route of c's job.
func (c JobCtl) Post() string {
	return "/queues/" + url.PathEscape(c.QueueID) + "/jobs/" + url.PathEscape(c.ID) + "/actions/cancel"
}

// jobSpans are the spans of j's timeline at now (Unix ms): waited
// (enqueued to started), ran (started to finished) and total (enqueued to
// finished). A pending job's waited and total, and a running job's ran
// and total, run on to now. A span is "—" when a time it needs is missing
// or the two are out of order (clocks of two hosts): never negative.
func jobSpans(j contract.JobRecord, now int64) (waited, ran, total JobSpan) {
	live := j.State == contract.JobPending || j.State == contract.JobProcessing
	mk := func(from, to int64, running bool) JobSpan {
		if running && to == 0 && live {
			to = now
		} else {
			running = false
		}
		if from == 0 || to == 0 || to < from {
			return JobSpan{Text: "—"}
		}
		ms := to - from
		return JobSpan{Text: span(time.Duration(ms) * time.Millisecond), Ms: ms, Live: running}
	}
	waited = mk(j.EnqueuedAt, j.StartedAt, j.State == contract.JobPending)
	ran = mk(j.StartedAt, j.FinishedAt, j.State == contract.JobProcessing)
	total = mk(j.EnqueuedAt, j.FinishedAt, true)
	return waited, ran, total
}

// jobView is the view of job r read of queue g on server, at now.
func jobView(g fleet.QueueGroup, q QueueName, jobID, server string, r fleet.JobRead, now time.Time) *JobView {
	v := &JobView{Queue: q, QueueURL: jobsURL(g.ID, server, "", 0, defaultJobLimit), ID: jobID,
		Found: r.Found, NoRoute: r.NoRoute(g), Memory: q.Backend == "memory", From: r.From, Unsupported: r.Unsupported}
	if !r.Found {
		return v
	}
	j := r.Job
	v.State, v.Row = string(j.State), jobRow(j, 0)
	v.Waited, v.Ran, v.Total = jobSpans(j, now.UnixMilli())
	if len(j.Result) > 0 {
		res := []byte(j.Result)
		var buf bytes.Buffer
		if json.Indent(&buf, res, "", "  ") == nil {
			res = buf.Bytes()
		}
		if len(res) > maxPayload {
			res, v.ResultCut = res[:maxPayload], true
		}
		v.Result = string(res)
	}
	v.Ctl = JobCtl{Server: server, QueueID: g.ID, ID: jobID,
		Cancel: (j.State == contract.JobPending || j.State == contract.JobProcessing) && jobURL(g.ID, jobID, server) != ""}
	return v
}

// job is the page of one job of a queue. It is 404 when no unit reports
// the queue, or none that can read one job has the job. It has no poller:
// job.js ticks the running spans.
func (h *handler) job(w http.ResponseWriter, r *http.Request) {
	id, jobID := r.PathValue("id"), r.PathValue("jobId")
	p, snaps, ok := h.page(r)
	q := p.QName(id)
	p.Title = pageTitle(jobID+" · "+q.Name, p.Server)
	p.Pods, p.IntervalSec = nil, 0
	status := p.status(r)
	if ok {
		if v, read := h.readQueues(r, &p, snaps); read {
			if g, found := v.Group(id); found {
				res := fleet.FetchJob(r.Context(), g, jobID)
				p.Job = jobView(g, q, jobID, p.Server, res, time.Now())
				switch {
				case res.Err != nil:
					p.Banner = id + ": " + res.Err.Error()
				case !res.Found && !p.Job.NoRoute:
					status = http.StatusNotFound
				}
			} else {
				p.Banner = "Queue " + strconv.Quote(id) + " not found on any " + p.Noun.One
				status = http.StatusNotFound
			}
		}
	}
	h.render(w, "job", status, p)
}

// JobConfirmView is the confirmation panel of a job's cancel: job ID of
// Queue on Server, sent to the first of On that takes it.
type JobConfirmView struct {
	Server, ID string
	Queue      QueueName
	Noun       fleet.Noun
	On         []string
	Ctl        JobCtl
}

// jobAction cancels job jobId of queue id (fleet.CancelJob), behind the
// guards of every action (bind). Without confirm=1 it only answers the
// confirmation panel and calls nothing. The answer is the results: a
// cancel is cooperative, so the page's state is left as read.
func (h *handler) jobAction(w http.ResponseWriter, r *http.Request) {
	id, jobID := r.PathValue("id"), r.PathValue("jobId")
	if r.PathValue("action") != "cancel" {
		badRequest(w, "unknown job action: want cancel")
		return
	}
	t, ok := h.bind(w, r)
	if !ok {
		return
	}
	g, found := fleet.GroupQueues(fleet.FetchQueues(r.Context(), h.clientsFor(t.latest, t.srv), fleet.UnitLabel(t.latest))).Group(id)
	if !found {
		conflict(w, "Queue "+strconv.Quote(id)+" is no longer reported by any "+t.noun.One+" — reload")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		h.fragments(w, fragment{"confirm", "job_confirm", JobConfirmView{Server: t.key, ID: jobID, Queue: ownersOf(t.latest).name(id), Noun: t.noun, On: g.On,
			Ctl: JobCtl{Server: t.key, QueueID: id, ID: jobID}}})
		return
	}
	results := fleet.CancelJob(mutationCtx(r), g, fleet.UnitsOf(t.latest), t.noun, jobID)
	h.fragments(w, fragment{"results", "results", ResultsView{Results: results, Noun: t.noun}})
}
