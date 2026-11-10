package fleet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
)

// The workflow routes (varmq API 1.3.0) read live, as the queue routes do.
// A durable workflow lives in a store several instances register (varmq
// API 1.5.1: redisq/sqliteq NewWorkflowStore): each lists it, the same
// record. An in-memory one lives in one process. Records are
// grouped by ID and created_at: the same pair from several units is one
// workflow they share; the same ID created at another time is another one
// (an in-memory workflow of another process), shown on its own row.

// UnitWorkflows is what one endpoint answered to GET /workflows.
// Unsupported: it serves no workflow routes (varmq before API 1.3.0).
type UnitWorkflows struct {
	Client      *api.Client
	Label       string
	Workflows   []contract.WorkflowRecord
	Unsupported bool
	Err         error
}

// WorkflowGroup is one workflow across the fleet: the most advanced copy
// of its record (copies read at different moments may differ), and the
// units that report it, in the order fetched.
type WorkflowGroup struct {
	contract.WorkflowRecord
	On      []string      // labels of the units reporting it, each once
	Clients []*api.Client // the clients that reported it, for the cancel route
	labels  []string      // the label of each of Clients
}

// Final reports whether the workflow ended: a final state, or a finish time.
func (g WorkflowGroup) Final() bool { return WorkflowEnded(g.WorkflowRecord) }

// WorkflowEnded reports whether w ended: a final state, or a finish time
// (a state a later varmq adds is shown as it is, and ended once finished).
func WorkflowEnded(w contract.WorkflowRecord) bool {
	return api.WorkflowFinal(w.State) || w.FinishedAt > 0
}

// TaskEnded reports whether a task's state is one it does not leave.
func TaskEnded(s contract.JobState) bool {
	return s == contract.JobCompleted || s == contract.JobFailed || s == contract.JobCancelled
}

// advance ranks how far a copy of a record got: ended beats running, then
// more tasks ended.
func advance(w contract.WorkflowRecord) int {
	n := 0
	for _, t := range w.Tasks {
		if TaskEnded(t.State) {
			n++
		}
	}
	if WorkflowEnded(w) {
		n += 1 << 20
	}
	return n
}

type workflowKey struct {
	id string
	at int64
}

// groupWorkflows groups records by (ID, created_at), newest created_at
// first, then ID (spec WorkflowKey order). add is called once per record.
type workflowGrouper struct {
	groups []WorkflowGroup
	byKey  map[workflowKey]int
}

func (g *workflowGrouper) add(c *api.Client, label string, w contract.WorkflowRecord) {
	if g.byKey == nil {
		g.byKey = make(map[workflowKey]int)
	}
	k := workflowKey{w.ID, w.CreatedAt}
	i, seen := g.byKey[k]
	if !seen {
		i = len(g.groups)
		g.byKey[k] = i
		g.groups = append(g.groups, WorkflowGroup{WorkflowRecord: w})
	} else if advance(w) > advance(g.groups[i].WorkflowRecord) {
		g.groups[i].WorkflowRecord = w
	}
	grp := &g.groups[i]
	if !slices.Contains(grp.Clients, c) {
		grp.Clients, grp.labels = append(grp.Clients, c), append(grp.labels, label)
	}
	if !slices.Contains(grp.On, label) {
		grp.On = append(grp.On, label)
	}
}

func (g *workflowGrouper) sorted() []WorkflowGroup {
	slices.SortStableFunc(g.groups, func(a, b WorkflowGroup) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return g.groups
}

// WorkflowsView is one page of a server's workflows, merged: Groups newest
// first, More when a later page has some. Unsupported and Failed name the
// units that serve no workflow routes and those whose GET /workflows failed
// (label: error).
type WorkflowsView struct {
	Groups      []WorkflowGroup
	More        bool
	Unsupported []string
	Failed      []string
	Units       int // units asked
}

// FetchWorkflows asks every client at once for the first n workflows in
// state ("" for all), newest first: a merged page from offset needs each
// unit's first offset+limit (+1 to know of a next page). n above a page's
// cap is read in pages. label names a client's unit as FetchQueues does.
func FetchWorkflows(ctx context.Context, clients []*api.Client, label func(*api.Client, string) string, state contract.WorkflowState, n int) []UnitWorkflows {
	out := make([]UnitWorkflows, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() {
			u := UnitWorkflows{Client: c}
			for len(u.Workflows) < n {
				ask := min(api.MaxWorkflowLimit, n-len(u.Workflows))
				page, ok, err := c.ListWorkflows(ctx, state, len(u.Workflows), ask)
				if err != nil || !ok {
					u.Unsupported, u.Err = !ok && err == nil, err
					break
				}
				u.Workflows = append(u.Workflows, page...)
				// a next request from past the route's largest offset
				// would be refused: stop there, with up to a page past it
				if len(page) < ask || len(u.Workflows) > api.MaxWorkflowOffset {
					break
				}
			}
			u.Label = label(c, c.LastInstance())
			out[i] = u
		})
	}
	wg.Wait()
	return out
}

// MergeWorkflows groups the units' workflows (see WorkflowGroup) and keeps
// the page from offset, limit long. Copies are never counted twice: a
// workflow several units share is one row.
func MergeWorkflows(units []UnitWorkflows, offset, limit int) WorkflowsView {
	v := WorkflowsView{Units: len(units)}
	var g workflowGrouper
	for _, u := range units {
		switch {
		case u.Err != nil:
			v.Failed = append(v.Failed, fmt.Sprintf("%s: %v", u.Label, u.Err))
			continue
		case u.Unsupported:
			v.Unsupported = append(v.Unsupported, u.Label)
			continue
		}
		for _, w := range u.Workflows {
			g.add(u.Client, u.Label, w)
		}
	}
	all := g.sorted()
	lo, hi := min(offset, len(all)), min(offset+limit, len(all))
	v.Groups, v.More = all[lo:hi], len(all) > offset+limit
	return v
}

// CountWorkflows asks every client at once for GET /workflow-counts and
// merges the answers (see mergeWorkflowCounts). ok false: no counts.
func CountWorkflows(ctx context.Context, clients []*api.Client) (counts map[contract.WorkflowState]int64, ok bool) {
	reads := make([][]api.WorkflowCounts, len(clients))
	known := make([]bool, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() {
			entries, ok, err := c.ListWorkflowCounts(ctx)
			reads[i], known[i] = entries, ok && err == nil
		})
	}
	wg.Wait()
	if len(clients) == 0 || slices.Contains(known, false) {
		return nil, false
	}
	return mergeWorkflowCounts(reads)
}

// maxCountEntries caps one instance's /workflow-counts entries; more: a
// broken or hostile answer, no counts.
const maxCountEntries = 256

// mergeWorkflowCounts is the workflows per state over the units' entries:
// a shared store counted once (its first read, in client order; reads of
// one store may differ, a later one is not truer), the rest summed. States
// varmq does not list are ignored. All or nothing: an entry without
// counts, a negative count, a sum past int64 or too many entries, and no
// number is right, so ok is false.
func mergeWorkflowCounts(reads [][]api.WorkflowCounts) (map[contract.WorkflowState]int64, bool) {
	seen := map[string]bool{}
	sum := map[contract.WorkflowState]int64{}
	var all int64 // every state's sum: the "All" tab
	for _, entries := range reads {
		if len(entries) > maxCountEntries {
			return nil, false
		}
		for _, e := range entries {
			if e.Counts == nil {
				return nil, false
			}
			for _, c := range e.Counts {
				if c < 0 {
					return nil, false
				}
			}
			if e.Shared && e.Store != "" {
				if seen[e.Store] {
					continue
				}
				seen[e.Store] = true
			}
			for s, c := range e.Counts {
				if !slices.Contains(api.WorkflowStates, s) {
					continue
				}
				if sum[s]+c < 0 || all+c < 0 {
					return nil, false
				}
				sum[s] += c
				all += c
			}
		}
	}
	return sum, true
}

// WorkflowRead is what the units answered to GET /workflows/{id}: the
// workflows of that ID (one, but for in-memory ones of several processes),
// newest first; the units that hold it in several storages (409: nothing
// read there); those that serve no workflow routes; those that failed.
type WorkflowRead struct {
	Groups      []WorkflowGroup
	Ambiguous   []string
	Unsupported []string
	Failed      []string
	Units       int
}

// Group is the workflow of r created at at (Unix ms), and whether r has it.
func (r WorkflowRead) Group(at int64) (WorkflowGroup, bool) {
	i := slices.IndexFunc(r.Groups, func(g WorkflowGroup) bool { return g.CreatedAt == at })
	if i < 0 {
		return WorkflowGroup{}, false
	}
	return r.Groups[i], true
}

// FetchWorkflow asks every client at once for workflow id.
func FetchWorkflow(ctx context.Context, clients []*api.Client, label func(*api.Client, string) string, id string) WorkflowRead {
	type answer struct {
		w     contract.WorkflowRecord
		label string
		err   error
	}
	answers := make([]answer, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() {
			w, err := c.GetWorkflow(ctx, id)
			answers[i] = answer{w, label(c, c.LastInstance()), err}
		})
	}
	wg.Wait()
	r := WorkflowRead{Units: len(clients)}
	var g workflowGrouper
	for i, a := range answers {
		switch {
		case a.err == nil:
			g.add(clients[i], a.label, a.w)
		case api.WorkflowNotFound(a.err):
		case api.RouteMissing(a.err):
			r.Unsupported = append(r.Unsupported, a.label)
		case api.WorkflowAmbiguous(a.err):
			r.Ambiguous = append(r.Ambiguous, a.label)
		default:
			r.Failed = append(r.Failed, fmt.Sprintf("%s: %v", a.label, a.err))
		}
	}
	r.Groups = g.sorted()
	return r
}

// CancelWorkflow cancels workflow g through the first of its units that
// takes it: the cancel is stored with the workflow, so one unit does (every
// process working on it sees it), and an in-memory workflow has one unit
// anyway. A unit that fails, holds it no longer, or holds the ID in
// several storages (409) gives way to the next. It returns one result per
// unit asked, in g's order, and the record as the unit that took it
// answered (nil when none did). noUndo: the undo jobs do not run (spec
// undo=false). units label the results' tips (see UnitsOf); n words their
// messages.
func CancelWorkflow(ctx context.Context, g WorkflowGroup, units map[string]PodSnapshot, n Noun, noUndo bool) ([]ActionResult, *contract.WorkflowRecord) {
	var results []ActionResult
	for i, c := range g.Clients {
		key := c.PrimaryEndpoint()
		u := units[key]
		u.Key = key
		res := ActionResult{Pod: g.labels[i], Key: key, Tip: u.Tip(), Outcome: "ok"}
		w, err := c.CancelWorkflow(ctx, g.ID, noUndo)
		var apiErr *api.APIError
		switch {
		case err == nil:
			res.Msg = "now " + string(w.State)
			return append(results, res), &w
		case api.WorkflowNotFound(err):
			res.Outcome, res.Msg = "no change", "workflow not on this "+n.One+" now"
		case api.WorkflowAmbiguous(err):
			res.Outcome, res.Msg = "failed", "several storages on this "+n.One+" hold this workflow ID"
		case api.RouteMissing(err):
			res.Outcome, res.Msg = "failed", "not supported by this "+n.One
		case errors.As(err, &apiErr):
			res.Outcome, res.Msg = "failed", apiErr.Message
		default:
			res.Outcome, res.Msg = "failed", err.Error()
		}
		results = append(results, res)
	}
	return results, nil
}
