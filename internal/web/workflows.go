package web

import (
	"fmt"
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

// The workflow pages read varmq live (API 1.3.0), as the queue pages do:
// the list, one page merged across the units, and one workflow with its
// task graph, polled while it runs. The one write is the cancel.

const defaultWorkflowLimit = 25

// WorkflowsView is the list page: one page of the server's workflows in
// State ("" for all), merged (see fleet.MergeWorkflows).
type WorkflowsView struct {
	State       contract.WorkflowState
	Tabs        []WorkflowTab
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

// StateLabel is State in words.
func (v *WorkflowsView) StateLabel() string { return stateView(string(v.State)).Label }

// WorkflowTab is the link to one state of the list ("" for all).
type WorkflowTab struct {
	Label   string
	Class   string
	URL     string
	Current bool
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
}

// StateView is a workflow or task state as the pages show it: Label the
// words (compensation_failed reads "compensation failed"), Class its CSS
// class: a known state's own, "unknown" for one a later varmq adds, shown
// as it is.
type StateView struct{ Label, Class string }

var knownStates = []string{
	"running", "completed", "failed", "compensating", "compensated", "compensation_failed", "cancelled",
	"waiting", "pending", "processing",
}

func stateView(s string) StateView {
	v := StateView{Label: strings.ReplaceAll(s, "_", " "), Class: "unknown"}
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

// runSpan is how long w ran: to its end, or so far at now (running true).
func runSpan(w contract.WorkflowRecord, now time.Time) (string, bool) {
	if w.CreatedAt == 0 {
		return "—", false
	}
	if w.FinishedAt > 0 {
		return span(time.Duration(w.FinishedAt-w.CreatedAt) * time.Millisecond), false
	}
	return span(now.Sub(time.UnixMilli(w.CreatedAt))), !fleet.WorkflowEnded(w)
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
	for _, s := range append([]contract.WorkflowState{""}, api.WorkflowStates...) {
		tab := WorkflowTab{Label: "all", Class: "all", URL: workflowsURL(p.Server, s, 0), Current: s == v.State}
		if s != "" {
			sv := stateView(string(s))
			tab.Label, tab.Class = sv.Label, sv.Class
		}
		v.Tabs = append(v.Tabs, tab)
	}
	p.WorkflowList = v
	if ok {
		if clients, label, live := h.liveClients(&p, snaps, "workflows"); live {
			v.Read = true
			units := fleet.FetchWorkflows(r.Context(), clients, label, v.State, v.Offset+defaultWorkflowLimit+1)
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
	Final      bool
	Progress   Progress
	On         []string
	Created    string
	CreatedISO string
	Finished   string
	FinishISO  string
	Duration   string
	Running    bool
	Undo       bool // undo_on_cancel
	Tasks      []TaskView
	Graph      GraphView
	Ctl        WorkflowCtl
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

// WorkflowChoice links to one of several workflows of an ID.
type WorkflowChoice struct {
	URL, Created, State string
	On                  []string
}

// TaskView is one task as the graph, its panel and the table show it.
type TaskView struct {
	I                 int // its index in the record: the node, panel and row id
	Name, Short       string
	State             StateView
	Live              bool
	QueueID, QueueURL string
	QueueShort        string
	JobID, Reason     string
	Result            string // stored result size, "" for none
	Deps              []string
	Always            bool
	Undo              *TaskView
	Label             string // the node's accessible name
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
				v := workflowView(g, p.Server, time.Now())
				v.Ambiguous, v.Failed = read.Ambiguous, read.Failed
				for _, o := range read.Groups {
					if o.CreatedAt != g.CreatedAt {
						v.Others = append(v.Others, choiceOf(o, p.Server))
					}
				}
				if v.Final {
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

// workflowView is g's page model on server at now.
func workflowView(g fleet.WorkflowGroup, server string, now time.Time) *WorkflowView {
	w := g.WorkflowRecord
	v := &WorkflowView{ID: w.ID, State: stateView(string(w.State)), Final: g.Final(),
		Progress: progressOf(w), On: g.On, Undo: w.UndoOnCancel, Ctl: workflowCtl(server, w)}
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
		v.Tasks = append(v.Tasks, taskView(i, t, server, !v.Final))
		for _, d := range t.Deps {
			if j, ok := index[d]; ok {
				deps[i] = append(deps[i], j)
			}
		}
	}
	if l, ok := layoutDAG(len(w.Tasks), deps); ok {
		v.Graph = graphView(l, v.Tasks)
	}
	return v
}

// maxNodeName is the most of a task name a node shows; the rest is cut
// with "…" (the full name is the node's title and panel).
const maxNodeName = 20

func taskView(i int, t contract.TaskRecord, server string, running bool) TaskView {
	v := TaskView{I: i, Name: t.Name, Short: cut(t.Name, maxNodeName), State: stateView(string(t.State)),
		Live: running && taskLive(t.State), QueueID: t.QueueID, QueueShort: queueShort(t.QueueID),
		JobID: t.JobID, Reason: t.Reason, Deps: t.Deps, Always: t.Always}
	if t.QueueID != "" {
		v.QueueURL = jobsURL(t.QueueID, server, "", 0, defaultJobLimit)
	}
	if t.ResultSize > 0 {
		v.Result = byteSize(t.ResultSize)
	}
	if t.Undo != nil {
		u := taskView(i, *t.Undo, server, running)
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
	if v.Undo != nil {
		v.Label += ", undo " + v.Undo.State.Label
	}
	return v
}

// cut is s cut to n runes, "…" ending a cut one.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// queueShort is the part of a Queue ID a node shows: the name inside
// braces (Redis hash tag), else the last ':' part.
func queueShort(id string) string {
	if i, j := strings.LastIndex(id, "{"), strings.LastIndex(id, "}"); i >= 0 && j > i {
		return id[i+1 : j]
	}
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
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

// GraphView is what the "graph" template draws.
type GraphView struct {
	Width, Height string
	NodeW, NodeH  string
	HaloW, UndoX  string // the pulse ring's width; the undo badge's left, in a box
	Nodes         []GraphNode
	Edges         []GraphEdge
}

// GraphNode is one task's box at X, Y (its top left).
type GraphNode struct {
	X, Y string
	Task TaskView
}

// GraphEdge is one dependency, From's task to To's. Always: into a task
// that runs always (dashed). Live: into a task in flight (animated).
type GraphEdge struct {
	From, To     int
	Path         string
	Always, Live bool
}

func graphView(l dagLayout, tasks []TaskView) GraphView {
	g := GraphView{Width: num(l.width), Height: num(l.height), NodeW: num(dagNodeW), NodeH: num(dagNodeH), HaloW: num(dagNodeW + 2), UndoX: num(dagNodeW - 84)}
	for i, t := range tasks {
		g.Nodes = append(g.Nodes, GraphNode{X: num(dagX(l.layer[i])), Y: num(l.y[i] - dagNodeH/2), Task: t})
	}
	for _, c := range l.chains {
		from, to := c[0], c[len(c)-1]
		g.Edges = append(g.Edges, GraphEdge{From: from, To: to, Path: l.path(c), Always: tasks[to].Always, Live: tasks[to].Live})
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
	read := fleet.FetchWorkflow(r.Context(), fleet.ClientsFor(t.latest, t.srv.Token), fleet.UnitLabel(t.latest), id)
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
