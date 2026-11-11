package web

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
)

// The workflow pages read varmq live (API 1.3.0), as the queue pages do:
// the list, one page merged across the units, and one workflow with its
// task graph, polled while it runs. The one write is the cancel.

const defaultWorkflowLimit = 25

// countsTimeout bounds GET /workflow-counts on the list page (a var: tests
// shorten it).
var countsTimeout = 2 * time.Second

// WorkflowsView is the list page: one page of the server's workflows in
// State ("" for all), merged (see fleet.MergeWorkflows).
type WorkflowsView struct {
	State       contract.WorkflowState
	Tabs        []WorkflowTab // all, then the states that are no rollback
	Rollback    []WorkflowTab // the rollback states, a group of its own
	Rows        []WorkflowRow
	Offset      int
	Prev, Next  string
	Unsupported []string // units that serve no workflow routes
	Failed      []string // units whose list failed: label: error
	NoRoutes    bool     // no unit asked serves them
	Read        bool     // the units were read: Rows is the page (else the banner says why not)
}

// First and Last are the positions of the page's first and last rows,
// from 1.
func (v *WorkflowsView) First() int { return v.Offset + 1 }
func (v *WorkflowsView) Last() int  { return v.Offset + len(v.Rows) }

// Empty is what a page without rows says, by state: "No failed
// workflows", "No workflows rolling back".
func (v *WorkflowsView) Empty() string {
	switch v.State {
	case "":
		return "No workflows"
	case contract.WorkflowCompensating:
		return "No workflows rolling back"
	case contract.WorkflowCompensated:
		return "No workflows rolled back"
	case contract.WorkflowCompensationFailed:
		return "No workflows whose rollback failed"
	}
	return "No " + stateView(string(v.State)).Label + " workflows"
}

// WorkflowTab is the link to one state of the list ("" for all). Count is
// its workflows, "" when the fleet's counts are not known (then no tab has
// one); Name its accessible name, the count with it.
type WorkflowTab struct {
	Label   string
	Class   string
	URL     string
	Current bool
	Count   string
	Zero    bool
	Name    string
	state   contract.WorkflowState
}

// workflowTabs are the list's tabs in order; the rollback states are a
// group (label "Rollback:"), so their labels do not repeat the word.
var workflowTabs = []struct {
	state       contract.WorkflowState
	label, name string
	rollback    bool
}{
	{"", "All", "All", false},
	{contract.WorkflowRunning, "Running", "Running", false},
	{contract.WorkflowCompleted, "Completed", "Completed", false},
	{contract.WorkflowFailed, "Failed", "Failed", false},
	{contract.WorkflowCancelled, "Cancelled", "Cancelled", false},
	{contract.WorkflowCompensating, "In progress", "Rollback in progress", true},
	{contract.WorkflowCompensated, "Done", "Rollback done", true},
	{contract.WorkflowCompensationFailed, "Failed", "Rollback failed", true},
}

// setCounts writes the per-state counts on the tabs: "All" their sum.
func (v *WorkflowsView) setCounts(counts map[contract.WorkflowState]int64) {
	var all int64
	for _, n := range counts {
		all += n
	}
	for _, tabs := range [][]WorkflowTab{v.Tabs, v.Rollback} {
		for i := range tabs {
			t := &tabs[i]
			n := counts[t.state]
			if t.state == "" {
				n = all
			}
			t.Count, t.Zero = thousands(n), n == 0
			t.Name += ", " + t.Count + " workflow"
			if n != 1 {
				t.Name += "s"
			}
		}
	}
}

// thousands is n with a comma between each three digits: 1,284.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// WorkflowRow is one workflow of the list.
type WorkflowRow struct {
	ID, URL                      string
	State                        StateView
	Progress                     Progress
	Created, CreatedISO, CreFull string
	Duration                     string
	Running                      bool // Duration is so far
	On                           []string
	// Note is how a rollback stands, under the state: "1 of 2 undone",
	// "1 of 2 undo jobs failed"; "" for a workflow that does not roll back.
	Note string
}

// StateView is a workflow or task state as the pages show it: Label the
// words (the compensation states read as a rollback: see stateWords),
// Class its CSS class: a known state's own, "unknown" for one a later
// varmq adds, shown as it is.
type StateView struct{ Label, Class string }

var knownStates = []string{
	"running", "completed", "failed", "compensating", "compensated", "compensation_failed", "cancelled",
	"waiting", "pending", "processing",
}

// stateWords: the one word set of the compensation states on every
// workflow page.
var stateWords = map[string]string{
	"compensating": "rolling back", "compensated": "rolled back", "compensation_failed": "rollback failed",
}

func stateView(s string) StateView {
	v := StateView{Label: cmp.Or(stateWords[s], strings.ReplaceAll(s, "_", " ")), Class: "unknown"}
	if slices.Contains(knownStates, s) {
		v.Class = strings.ReplaceAll(s, "_", "-")
	}
	if s == "" {
		v.Label = "unknown"
	}
	return v
}

// Progress counts a workflow's tasks by how they stand, and their share in
// percent of all, for the progress bar.
type Progress struct {
	Total, Completed, Failed, Cancelled, Live      int
	PctCompleted, PctFailed, PctCancelled, PctLive string
}

// Ended is how many tasks ended, however.
func (p Progress) Ended() int { return p.Completed + p.Failed + p.Cancelled }

func progressOf(w contract.WorkflowRecord) Progress {
	p := Progress{Total: len(w.Tasks)}
	for _, t := range w.Tasks {
		switch {
		case t.State == contract.JobCompleted:
			p.Completed++
		case t.State == contract.JobFailed:
			p.Failed++
		case t.State == contract.JobCancelled:
			p.Cancelled++
		case taskLive(t.State):
			p.Live++
		}
	}
	pct := func(n int) string {
		if p.Total == 0 {
			return "0"
		}
		return num(float64(n) * 100 / float64(p.Total))
	}
	p.PctCompleted, p.PctFailed, p.PctCancelled, p.PctLive = pct(p.Completed), pct(p.Failed), pct(p.Cancelled), pct(p.Live)
	return p
}

// taskLive: pending (queued or running: varmq writes no state on a pop) or
// processing.
func taskLive(s contract.JobState) bool {
	return s == contract.JobPending || s == contract.JobProcessing
}

// span is a duration as the pages show it: 0.4s, 38s, 1m 35s, 2h 05m, 3d 4h.
func span(d time.Duration) string {
	switch {
	case d < 0:
		return "—"
	case d < 10*time.Second:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// timeOK: a record time (Unix ms) the pages trust: set, and at most a day
// past now (clocks apart a little; more is broken or hostile).
func timeOK(ms int64, now time.Time) bool {
	return ms > 0 && ms <= now.UnixMilli()+24*time.Hour.Milliseconds()
}

// msSpan is d ms as a Duration, clamped: the product cannot overflow.
func msSpan(d int64) time.Duration {
	const lim = math.MaxInt64 / int64(time.Millisecond)
	return time.Duration(min(max(d, -lim), lim)) * time.Millisecond
}

// runSpan is how long w ran: to its end, or so far at now (running true).
// "—" for a time not to trust (timeOK).
func runSpan(w contract.WorkflowRecord, now time.Time) (string, bool) {
	if !timeOK(w.CreatedAt, now) {
		return "—", false
	}
	if w.FinishedAt != 0 {
		if !timeOK(w.FinishedAt, now) {
			return "—", false
		}
		return span(msSpan(w.FinishedAt - w.CreatedAt)), false
	}
	return span(now.Sub(time.UnixMilli(w.CreatedAt))), !fleet.WorkflowEnded(w)
}

// rollbackOf counts w's undo jobs to run, and those that completed. Only a
// completed task is undone: a task that never completed shows an undo
// record (waiting) all the same, never run, not counted.
func rollbackOf(w contract.WorkflowRecord) (total, done int) {
	for _, t := range w.Tasks {
		if t.State == contract.JobCompleted && t.Undo != nil {
			total++
			if t.Undo.State == contract.JobCompleted {
				done++
			}
		}
	}
	return total, done
}

// rollbackNote is how w's rollback stands in a few words, "" when it does
// not roll back or its rollback ended well.
func rollbackNote(w contract.WorkflowRecord) string {
	total, done := rollbackOf(w)
	switch {
	case total == 0:
	case w.State == contract.WorkflowCompensating:
		return fmt.Sprintf("%d of %d undone", done, total)
	case w.State == contract.WorkflowCompensationFailed:
		return fmt.Sprintf("%d of %d not rolled back", total-done, total)
	}
	return ""
}

// taskTimes is how long t waited in its queue and ran, as the pages show
// them; "" for a time the record does not carry or not to trust
// (timeOK). A task in flight of a running workflow ran so far, at now.
func taskTimes(t contract.TaskRecord, running bool, now time.Time) (wait, run string, soFar bool) {
	at := func(ms int64) int64 {
		if timeOK(ms, now) {
			return ms
		}
		return 0
	}
	enq, start, fin := at(t.EnqueuedAt), at(t.StartedAt), at(t.FinishedAt)
	ms := func(d int64) string {
		if d < 50 { // "0.0s" reads as no time
			return "<0.1s"
		}
		return span(msSpan(d))
	}
	if enq > 0 && start >= enq {
		wait = ms(start - enq)
	}
	switch {
	case start == 0:
	case fin >= start:
		run = ms(fin - start)
	case fin == 0 && running && taskLive(t.State) && now.UnixMilli() >= start:
		run, soFar = ms(now.UnixMilli()-start), true
	}
	return wait, run, soFar
}

// slowestOnPath is the task that ran longest on the path that set the
// workflow's end: from the last task to finish, back through each one's
// last dependency to finish. -1: no times, or a path of one task.
func slowestOnPath(tasks []contract.TaskRecord, deps [][]int, now time.Time) int {
	timed := func(i int) bool {
		t := tasks[i]
		return timeOK(t.StartedAt, now) && timeOK(t.FinishedAt, now) && t.FinishedAt >= t.StartedAt
	}
	// a tie (same ms) goes to the later task: varmq adds a task after its
	// deps, so a dependent beats its dependency
	last := func(of []int) int {
		at := -1
		for _, i := range of {
			if timed(i) && (at < 0 || tasks[i].FinishedAt >= tasks[at].FinishedAt) {
				at = i
			}
		}
		return at
	}
	all := make([]int, len(tasks))
	for i := range all {
		all[i] = i
	}
	slow, steps := -1, 0
	for i := last(all); i >= 0 && steps <= len(tasks); i, steps = last(deps[i]), steps+1 {
		if slow < 0 || tasks[i].FinishedAt-tasks[i].StartedAt > tasks[slow].FinishedAt-tasks[slow].StartedAt {
			slow = i
		}
	}
	if steps < 2 {
		return -1
	}
	return slow
}

// workflowsURL is the list page of server in state, from offset.
func workflowsURL(server string, state contract.WorkflowState, offset int) string {
	q := url.Values{}
	if server != "" {
		q.Set("server", server)
	}
	if state != "" {
		q.Set("state", string(state))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if len(q) == 0 {
		return "/workflows"
	}
	return "/workflows?" + q.Encode()
}

// workflowURL is the page of workflow id on server; at (its created_at,
// Unix ms) picks one of several workflows of that ID, 0 for any.
func workflowURL(id, server string, at int64) string {
	q := url.Values{}
	if server != "" {
		q.Set("server", server)
	}
	if at != 0 {
		q.Set("at", strconv.FormatInt(at, 10))
	}
	u := "/workflows/" + url.PathEscape(id)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// noWorkflowRoutesMsg is the note of a server none of whose units serves
// the workflow routes.
func noWorkflowRoutesMsg(n fleet.Noun) string {
	return "No workflow data: no " + n.One + " serves the workflow routes. They need varmq API 1.3.0."
}

func (h *handler) workflows(w http.ResponseWriter, r *http.Request) {
	p, snaps, ok := h.page(r)
	p.Title = pageTitle("Workflows", p.Server)
	p.Pods = nil // workflows are read from every unit: no unit select
	v := &WorkflowsView{State: contract.WorkflowState(r.FormValue("state"))}
	if !slices.Contains(api.WorkflowStates, v.State) {
		v.State = ""
	}
	if n, err := strconv.Atoi(r.FormValue("offset")); err == nil && n > 0 {
		v.Offset = min(n, api.MaxWorkflowOffset)
	}
	// The poller asks for this very page, its state with it. Only the
	// first page is live: a later one is browsed, and each refresh of it
	// would read every unit's offset+limit records again.
	p.SelfURL = workflowsURL(p.Server, v.State, v.Offset)
	if v.Offset > 0 {
		p.IntervalSec = 0
	}
	for _, t := range workflowTabs {
		tab := WorkflowTab{Label: t.label, Name: t.name, Class: "all", URL: workflowsURL(p.Server, t.state, 0), Current: t.state == v.State, state: t.state}
		if t.state != "" {
			tab.Class = stateView(string(t.state)).Class
		}
		if t.rollback {
			v.Rollback = append(v.Rollback, tab)
		} else {
			v.Tabs = append(v.Tabs, tab)
		}
	}
	p.WorkflowList = v
	if ok {
		if clients, label, live := h.liveClients(&p, snaps, "workflows"); live {
			v.Read = true
			var wg sync.WaitGroup
			wg.Go(func() {
				// slow counts do not hold the list long: past the
				// timeout, tabs without counts
				ctx, cancel := context.WithTimeout(r.Context(), countsTimeout)
				defer cancel()
				if counts, ok := fleet.CountWorkflows(ctx, clients); ok {
					v.setCounts(counts)
				}
			})
			units := fleet.FetchWorkflows(r.Context(), clients, label, v.State, v.Offset+defaultWorkflowLimit+1)
			wg.Wait()
			m := fleet.MergeWorkflows(units, v.Offset, defaultWorkflowLimit)
			v.Unsupported, v.Failed = m.Unsupported, m.Failed
			v.NoRoutes = m.Units > 0 && len(m.Unsupported) == m.Units
			now := time.Now()
			for _, g := range m.Groups {
				// another row with this ID on the page: link each to its own
				var at int64
				if slices.ContainsFunc(m.Groups, func(o fleet.WorkflowGroup) bool { return o.ID == g.ID && o.CreatedAt != g.CreatedAt }) {
					at = g.CreatedAt
				}
				row := WorkflowRow{ID: g.ID, URL: workflowURL(g.ID, p.Server, at), State: stateView(string(g.State)), Progress: progressOf(g.WorkflowRecord), On: g.On}
				if g.CreatedAt > 0 {
					row.Created = shortTime(time.UnixMilli(g.CreatedAt), now)
					row.CreFull, row.CreatedISO = msTime(g.CreatedAt)
				}
				row.Duration, row.Running = runSpan(g.WorkflowRecord, now)
				row.Note = rollbackNote(g.WorkflowRecord)
				v.Rows = append(v.Rows, row)
			}
			if v.Offset > 0 {
				v.Prev = workflowsURL(p.Server, v.State, max(0, v.Offset-defaultWorkflowLimit))
			}
			if m.More && v.Offset+defaultWorkflowLimit <= api.MaxWorkflowOffset {
				v.Next = workflowsURL(p.Server, v.State, v.Offset+defaultWorkflowLimit)
			}
		}
	}
	h.render(w, "workflows", p.status(r), p)
}

// WorkflowView is one workflow's page.
type WorkflowView struct {
	ID         string
	State      StateView
	Progress   Progress
	On         []string
	Created    string
	CreatedISO string
	Finished   string
	FinishISO  string
	Duration   string
	Running    bool
	Tasks      []TaskView
	Graph      GraphView
	Ctl        WorkflowCtl
	Rollback   *RollbackView // nil unless it rolls back or its rollback failed
	Timed      bool          // a task carries a time: the table has the Waited and Ran columns
	// Pick: no workflow is shown, only Others to pick from: several
	// workflows have the ID and the URL names none of them, or (Gone) the
	// one it names (by at) is no longer reported.
	Pick, Gone bool
	// Others are the other workflows of this ID (in-memory ones of other
	// processes); Ambiguous the units holding the ID in several storages
	// (409); Failed those whose read failed.
	Others    []WorkflowChoice
	Ambiguous []string
	Failed    []string
}

// RollbackView is how a workflow's undo jobs stand, for the summary of one
// that rolls back (compensating) or whose rollback failed. Left and Undone
// are in the order undo runs: the last task first.
type RollbackView struct {
	Failed      bool       // compensation_failed: Left is not rolled back for good
	Total, Done int        // undo jobs, and those that completed
	Left        []TaskView // tasks whose undo job did not complete
	Undone      []TaskView // tasks rolled back
	Cause       []TaskView // the failed tasks: why it rolls back
}

// WorkflowChoice links to one of several workflows of an ID.
type WorkflowChoice struct {
	URL, Created, State string
	On                  []string
}

// TaskView is one task as the graph, its panel and the table show it.
type TaskView struct {
	I             int // its index in the record: the node, panel and row id
	Name, Short   string
	State         StateView
	Live          bool
	Queue         QueueName
	QueueURL      string
	JobURL        string // its job's page, "" for none (see jobURL)
	QueueShort    string // Queue's name as a node shows it, cut
	JobID, Reason string
	Brief         string // Reason cut (see brief): the summary and table; the panel has it whole
	Result        string // stored result size, "" for none
	Deps          []string
	Always        bool
	Undo          *TaskView
	Label         string // the node's accessible name
	// Wait and Run: how long it waited in its queue and ran, "" when the
	// record carries no times; RunSoFar: it runs still. Slowest: it ran
	// longest on the path that set the workflow's end.
	Wait, Run string
	RunSoFar  bool
	Slowest   bool
	Chip      string // the state chip that counts it: its state, processing as pending; "" for none
}

// WorkflowCtl is the cancel buttons of a workflow's page (#wfctl), and,
// with OOB, the same as an out-of-band swap for an action response.
type WorkflowCtl struct {
	Server, ID string
	At         int64
	Final      bool
	Undo       bool // undo_on_cancel: "Cancel without undo" differs
	OOB        bool
}

func workflowCtl(server string, w contract.WorkflowRecord) WorkflowCtl {
	return WorkflowCtl{Server: server, ID: w.ID, At: w.CreatedAt, Final: fleet.WorkflowEnded(w), Undo: w.UndoOnCancel}
}

// workflow is one workflow's page. It is 404 when no unit has it (but to
// htmx: a poll gets a 200, so the notice reaches the banner).
func (h *handler) workflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, snaps, ok := h.page(r)
	p.Title = pageTitle(id, p.Server)
	at, _ := strconv.ParseInt(r.FormValue("at"), 10, 64)
	// #live polls itself (Page.OwnPoll), and stops once the workflow
	// ended, or is not found, or is one of several: nothing changes then.
	// A read that failed for now (stale data, an error) keeps polling.
	p.Pods, p.OwnPoll, p.SelfURL = nil, true, workflowURL(id, p.Server, at)
	stop := func() { p.IntervalSec = 0 }
	status := p.status(r)
	if ok {
		if clients, label, live := h.liveClients(&p, snaps, "workflows"); live {
			read := fleet.FetchWorkflow(r.Context(), clients, label, id)
			g, found := read.Group(at)
			if at == 0 && len(read.Groups) == 1 {
				g, found = read.Groups[0], true
			}
			switch {
			case found:
				v := workflowView(g, p.Server, p.owners, time.Now())
				v.Ambiguous, v.Failed = read.Ambiguous, read.Failed
				for _, o := range read.Groups {
					if o.CreatedAt != g.CreatedAt {
						v.Others = append(v.Others, choiceOf(o, p.Server))
					}
				}
				if v.Ctl.Final {
					stop()
				}
				p.Workflow = v
			case at == 0 && len(read.Groups) > 1 || at != 0 && len(read.Groups) > 0 && len(read.Failed) == 0 && len(read.Ambiguous) == 0:
				// several of the ID and the URL names none, or the one
				// it names is gone and others are left: pick one. The
				// latter not while a unit failed or answered 409: it may
				// hold that one (the banner says so, the page polls on)
				stop()
				p.Workflow = &WorkflowView{ID: id, Pick: true, Gone: at != 0, Ambiguous: read.Ambiguous, Failed: read.Failed}
				for _, o := range read.Groups {
					p.Workflow.Others = append(p.Workflow.Others, choiceOf(o, p.Server))
				}
			default:
				switch {
				case len(read.Ambiguous) > 0:
					p.Banner = fmt.Sprintf("Workflow %q is held by more than one storage on %s (409): varmq reads and cancels it there by ID only, so vartop cannot", id, strings.Join(read.Ambiguous, ", "))
				case read.Units > 0 && len(read.Unsupported) == read.Units:
					p.Banner = noWorkflowRoutesMsg(p.Noun)
					stop()
				case len(read.Failed) > 0:
					p.Banner = "Workflow " + strconv.Quote(id) + ": " + strings.Join(read.Failed, "; ")
				default:
					p.Banner = "Workflow " + strconv.Quote(id) + " not found on any " + p.Noun.One
					stop()
					if !isHTMX(r) {
						status = http.StatusNotFound
					}
				}
			}
		}
	}
	h.render(w, "workflow", status, p)
}

func choiceOf(g fleet.WorkflowGroup, server string) WorkflowChoice {
	full, _ := msTime(g.CreatedAt)
	return WorkflowChoice{URL: workflowURL(g.ID, server, g.CreatedAt), Created: full, State: stateView(string(g.State)).Label, On: g.On}
}

// workflowView is g's page model on server at now; owners names its
// in-memory queues.
func workflowView(g fleet.WorkflowGroup, server string, owners queueOwners, now time.Time) *WorkflowView {
	w := g.WorkflowRecord
	v := &WorkflowView{ID: w.ID, State: stateView(string(w.State)), Progress: progressOf(w), On: g.On, Ctl: workflowCtl(server, w)}
	v.Created, v.CreatedISO = msTime(w.CreatedAt)
	v.Finished, v.FinishISO = msTime(w.FinishedAt)
	v.Duration, v.Running = runSpan(w, now)
	index := make(map[string]int, len(w.Tasks))
	for i, t := range w.Tasks {
		if _, dup := index[t.Name]; !dup {
			index[t.Name] = i
		}
	}
	deps := make([][]int, len(w.Tasks))
	for i, t := range w.Tasks {
		v.Tasks = append(v.Tasks, taskView(i, t, server, owners, !v.Ctl.Final, now))
		for _, d := range t.Deps {
			if j, ok := index[d]; ok {
				deps[i] = append(deps[i], j)
			}
		}
	}
	// the path that set the end: known once the workflow ended; not after
	// a rollback (its undo jobs set the end)
	rolled := w.State == contract.WorkflowCompensating || w.State == contract.WorkflowCompensated || w.State == contract.WorkflowCompensationFailed
	if i := slowestOnPath(w.Tasks, deps, now); i >= 0 && v.Ctl.Final && !rolled {
		v.Tasks[i].Slowest = true
		v.Tasks[i].Label += ", slowest on the path to the end"
	}
	v.Timed = slices.ContainsFunc(v.Tasks, func(t TaskView) bool {
		return t.Wait != "" || t.Run != "" || t.Undo != nil && (t.Undo.Wait != "" || t.Undo.Run != "")
	})
	if w.State == contract.WorkflowCompensating || w.State == contract.WorkflowCompensationFailed {
		rb := &RollbackView{Failed: w.State == contract.WorkflowCompensationFailed}
		rb.Total, rb.Done = rollbackOf(w)
		for _, t := range slices.Backward(v.Tasks) {
			switch {
			case t.State.Class != "completed" || t.Undo == nil: // never undone: see rollbackOf
			case t.Undo.State.Class == "completed":
				rb.Undone = append(rb.Undone, t)
			default:
				rb.Left = append(rb.Left, t)
			}
		}
		for _, t := range v.Tasks {
			if t.State.Class == "failed" {
				rb.Cause = append(rb.Cause, t)
			}
		}
		v.Rollback = rb
	}
	if l, ok := layoutDAG(len(w.Tasks), deps); ok {
		v.Graph = graphView(l, v.Tasks)
	}
	return v
}

// maxNodeName is the most of a task name a node shows; the rest is cut
// with "…" (the full name is the node's title and panel).
const maxNodeName = 20

// maxNodeQueue is the most of a queue name a node's meta line shows.
const maxNodeQueue = 12

func taskView(i int, t contract.TaskRecord, server string, owners queueOwners, running bool, now time.Time) TaskView {
	v := TaskView{I: i, Name: t.Name, Short: cut(t.Name, maxNodeName), State: stateView(string(t.State)),
		Live: running && taskLive(t.State), Queue: owners.name(t.QueueID),
		JobID: t.JobID, Reason: t.Reason, Brief: brief(t.Reason), Deps: t.Deps, Always: t.Always}
	v.Wait, v.Run, v.RunSoFar = taskTimes(t, running, now)
	if c := v.State.Class; slices.Contains(chipStates, c) {
		v.Chip = c
	} else if c == "processing" {
		v.Chip = "pending"
	}
	if t.QueueID != "" {
		v.QueueShort = cut(v.Queue.Name, maxNodeQueue)
		v.QueueURL = jobsURL(t.QueueID, server, "", 0, defaultJobLimit)
		v.JobURL = jobURL(t.QueueID, t.JobID, server)
	}
	if t.ResultSize > 0 {
		v.Result = byteSize(t.ResultSize)
	}
	if t.Undo != nil {
		u := taskView(i, *t.Undo, server, owners, running, now)
		u.Name = t.Name + " (undo)"
		v.Undo = &u
	}
	v.Label = t.Name + ", " + v.State.Label
	if len(t.Deps) > 0 {
		v.Label += ", after " + strings.Join(t.Deps, ", ")
	}
	if t.Always {
		v.Label += ", runs always"
	}
	if v.Run != "" {
		v.Label += ", ran " + v.Run
		if v.RunSoFar {
			v.Label += " so far"
		}
	}
	if v.Undo != nil {
		v.Label += ", undo " + v.Undo.State.Label
	}
	return v
}

// maxReason is the most of a failure reason the summary and table show.
const maxReason = 500

// brief is s, past maxReason runes cut there with "…".
func brief(s string) string {
	if r := []rune(s); len(r) > maxReason {
		return string(r[:maxReason]) + "…"
	}
	return s
}

// cut is s cut to n runes, "…" ending a cut one.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// byteSize is n bytes, short: 812 B, 9.1 KiB, 1.1 MiB.
func byteSize(n int) string {
	switch {
	case n < 1024:
		return strconv.Itoa(n) + " B"
	case n < 1<<20:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KiB"
	}
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
}

// chipStates are the task states the graph's chips count and filter, in
// order; a processing task counts as pending: the chip "pending or
// processing".
var chipStates = []string{"waiting", "pending", "completed", "failed", "cancelled"}

// StateChip is one state of the graph's legend: its tasks, and the key the
// nodes and rows of that state carry (TaskView.Chip).
type StateChip struct {
	Key, Label string
	Count      int
}

// GraphView is what the "graph" template draws.
type GraphView struct {
	Width, Height string
	WellH         string // the well's height that shows it whole: CSS reserves it, script or not
	NodeW, NodeH  string
	HaloW         string      // the pulse ring's width
	UndoX, UndoFX string      // the undo badge's left in a box; the wider "undo failed" one's
	SlowX         string      // the slowest mark's left
	Nodes         []GraphNode // in topological order: the tab order
	Edges         []GraphEdge
	Chips         []StateChip
	Slowest       bool // a node carries the slowest mark: the legend explains it
}

// GraphNode is one task's box at X, Y (its top left).
type GraphNode struct {
	X, Y string
	Task TaskView
}

// GraphEdge is one dependency, From's task to To's. Always: into a task
// that runs always (dashed). Live: into a task in flight (animated). Dead:
// out of a task that failed or was cancelled, into one that does not run
// always: nothing passes it (dotted).
type GraphEdge struct {
	From, To           int
	Path               string
	Always, Live, Dead bool
}

func graphView(l dagLayout, tasks []TaskView) GraphView {
	g := GraphView{Width: num(l.width), Height: num(l.height), WellH: num(l.height + 24), NodeW: num(dagNodeW), NodeH: num(dagNodeH),
		HaloW: num(dagNodeW + 2), UndoX: num(dagNodeW - 84), UndoFX: num(dagNodeW - 108), SlowX: num(dagNodeW - 22)}
	count := map[string]int{}
	order := make([]int, len(tasks))
	for i, t := range tasks {
		order[i] = i
		count[t.Chip]++
		g.Slowest = g.Slowest || t.Slowest
	}
	// by layer, then down the layer: the order a reader follows, so the tab order
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Or(cmp.Compare(l.layer[a], l.layer[b]), cmp.Compare(l.y[a], l.y[b]))
	})
	for _, i := range order {
		g.Nodes = append(g.Nodes, GraphNode{X: num(dagX(l.layer[i])), Y: num(l.y[i] - dagNodeH/2), Task: tasks[i]})
	}
	for _, c := range l.chains {
		from, to := c[0], c[len(c)-1]
		stopped := tasks[from].State.Class == "failed" || tasks[from].State.Class == "cancelled"
		g.Edges = append(g.Edges, GraphEdge{From: from, To: to, Path: l.path(c), Always: tasks[to].Always, Live: tasks[to].Live, Dead: stopped && !tasks[to].Always})
	}
	for _, k := range chipStates {
		label := k
		if k == "pending" {
			label = "pending or processing"
		}
		g.Chips = append(g.Chips, StateChip{Key: k, Label: label, Count: count[k]})
	}
	return g
}

// WorkflowConfirmView is the confirmation panel of a workflow cancel.
type WorkflowConfirmView struct {
	Server, ID string
	At         int64
	NoUndo     bool
	Undo       bool // undo_on_cancel
	Undoable   int  // completed tasks with an undo job
	Noun       fleet.Noun
	On         []string
}

// workflowAction cancels workflow id (the one created at the form's at)
// through the first unit reporting it that takes it (see
// fleet.CancelWorkflow); undo=false in the form: without its undo jobs. As
// an action on workers, it is refused unless the server's newest snapshot
// may be acted on (bind), and when the workflow ended. Without confirm=1
// it only answers the confirmation panel and calls nothing. The answer is
// the results, and #wfctl out of band from the record after.
func (h *handler) workflowAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.PathValue("action") != "cancel" {
		badRequest(w, "unknown workflow action: want cancel")
		return
	}
	var noUndo bool
	switch r.PostFormValue("undo") {
	case "":
	case "false":
		noUndo = true
	default:
		badRequest(w, "undo: want false or nothing")
		return
	}
	at, err := strconv.ParseInt(r.PostFormValue("at"), 10, 64)
	if err != nil {
		badRequest(w, "which workflow: the form names no created_at")
		return
	}
	t, ok := h.bind(w, r)
	if !ok {
		return
	}
	read := fleet.FetchWorkflow(r.Context(), h.clientsFor(t.latest, t.srv), fleet.UnitLabel(t.latest), id)
	g, found := read.Group(at)
	switch {
	case !found && len(read.Ambiguous) > 0:
		conflict(w, "Workflow "+strconv.Quote(id)+" is held by more than one storage on "+strings.Join(read.Ambiguous, ", ")+": vartop cannot cancel it")
		return
	case !found:
		conflict(w, "Workflow "+strconv.Quote(id)+" is no longer reported by any "+t.noun.One+" — reload")
		return
	case g.Final():
		conflict(w, "Workflow "+strconv.Quote(id)+" already ended ("+stateView(string(g.State)).Label+") — reload")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		cv := WorkflowConfirmView{Server: t.key, ID: id, At: at, NoUndo: noUndo, Undo: g.UndoOnCancel, Noun: t.noun, On: g.On}
		for _, task := range g.Tasks {
			if task.State == contract.JobCompleted && task.Undo != nil {
				cv.Undoable++
			}
		}
		h.fragments(w, fragment{"confirm", "workflow_confirm", cv})
		return
	}
	results, after := fleet.CancelWorkflow(mutationCtx(r), g, fleet.UnitsOf(t.latest), t.noun, noUndo)
	rec := g.WorkflowRecord
	if after != nil {
		rec = *after
	}
	ctl := workflowCtl(t.key, rec)
	ctl.OOB = true
	h.fragments(w, fragment{"results", "results", ResultsView{Results: results, Noun: t.noun}}, fragment{"workflow", "wfctl", ctl})
}
