package fleet

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet/fleettest"
)

// sharedOrders is one Redis queue as every instance bound to it reports it:
// the same numbers, read from the same storage.
var sharedOrders = &fleettest.Queue{
	Summary: `{"id":"redisq:ns:fifo:{orders}","counts":{"pending":3,"processing":1,"completed":10,"failed":2},"totals":{"submitted":16,"completed":10,"failed":2}}`,
	Jobs: map[string][]string{
		"failed": {`{"id":"order-42","state":"failed","payload":{"sku":"A1"},"finished_at":1762228801900,"attempts":2,"errors":[{"attempt":2,"at":1762228801900,"error":"out of stock"}]}`},
	},
}

func queueFake(t *testing.T, instance string, queues map[string]*fleettest.Queue) (*fleettest.Varmq, *httptest.Server) {
	t.Helper()
	f, srv := fleettest.NewVarmq(t, instance, nil)
	f.Mu.Lock()
	f.Queues = queues
	f.Mu.Unlock()
	return f, srv
}

func byInstance(c *api.Client, instance string) string { return instance }

// Two instances sharing a queue give one group, its numbers not summed,
// reported by both; an instance without the queue routes is named, not failed.
func TestGroupQueuesSharedQueue(t *testing.T) {
	_, a := queueFake(t, "a", map[string]*fleettest.Queue{"redisq:ns:fifo:{orders}": sharedOrders})
	_, b := queueFake(t, "b", map[string]*fleettest.Queue{
		"redisq:ns:fifo:{orders}": sharedOrders,
		"redisq:fifo:{emails}":    {Summary: `{"id":"redisq:fifo:{emails}","counts":{"pending":0,"processing":0,"completed":0,"failed":0},"totals":{"submitted":0,"completed":0,"failed":0}}`},
	})
	_, old := fleettest.NewVarmq(t, "old", nil) // no Queues: an older varmq
	clients := []*api.Client{api.NewClient(a.URL, ""), api.NewClient(b.URL, ""), api.NewClient(old.URL, "")}

	v := GroupQueues(FetchQueues(context.Background(), clients, byInstance))
	if len(v.Groups) != 2 || v.Groups[0].ID != "redisq:fifo:{emails}" || v.Groups[1].ID != "redisq:ns:fifo:{orders}" {
		t.Fatalf("groups = %+v", v.Groups)
	}
	orders := v.Groups[1]
	if orders.Counts.Pending != 3 || orders.Totals.Submitted != 16 {
		t.Errorf("orders counts %+v totals %+v: want one copy, not a sum", orders.Counts, orders.Totals)
	}
	if !slices.Equal(orders.On, []string{"a", "b"}) || !slices.Equal(v.Groups[0].On, []string{"b"}) {
		t.Errorf("on = %v, %v", orders.On, v.Groups[0].On)
	}
	if !slices.Equal(v.Unsupported, []string{"old"}) || v.Failed != nil || v.Units != 3 {
		t.Errorf("unsupported %v, failed %v, units %d", v.Unsupported, v.Failed, v.Units)
	}
}

// A copy that read the storage wins over an error copy, whichever came first.
func TestGroupQueuesPrefersReadCopy(t *testing.T) {
	bad := api.QueueSummary{ID: "q", Err: "connection lost"}
	good := api.QueueSummary{ID: "q", Counts: &contract.JobCounts{Pending: 1}}
	v := GroupQueues([]UnitQueues{
		{Label: "a", Queues: []api.QueueSummary{bad}},
		{Label: "b", Queues: []api.QueueSummary{good}},
		{Label: "c", Err: context.DeadlineExceeded},
	})
	g, ok := v.Group("q")
	if !ok || g.Err != "" || g.Counts.Pending != 1 || !slices.Equal(g.On, []string{"a", "b"}) {
		t.Errorf("group = %+v, %v", g, ok)
	}
	if len(v.Failed) != 1 || !strings.HasPrefix(v.Failed[0], "c: ") {
		t.Errorf("failed = %v", v.Failed)
	}
	if _, ok := v.Group("nope"); ok {
		t.Error("Group(nope) found")
	}
}

// FetchJobs reads from the first unit that has the queue: one that lost it
// (404) gives way to the next.
func TestFetchJobsFallsBack(t *testing.T) {
	gone, a := queueFake(t, "a", map[string]*fleettest.Queue{"redisq:ns:fifo:{orders}": sharedOrders})
	fb, b := queueFake(t, "b", map[string]*fleettest.Queue{"redisq:ns:fifo:{orders}": sharedOrders})
	clients := []*api.Client{api.NewClient(a.URL, ""), api.NewClient(b.URL, "")}
	v := GroupQueues(FetchQueues(context.Background(), clients, byInstance))
	g, _ := v.Group("redisq:ns:fifo:{orders}")

	gone.Mu.Lock()
	gone.Queues = map[string]*fleettest.Queue{} // a no longer binds it
	gone.Mu.Unlock()

	jobs, from, err := FetchJobs(context.Background(), g, contract.JobFailed, 0, 50)
	if err != nil || from != "b" || len(jobs) != 1 || len(jobs[0].Errors) != 1 || jobs[0].Errors[0].Error != "out of stock" {
		t.Fatalf("FetchJobs = %+v from %q, err %v", jobs, from, err)
	}
	calls := fb.Calls()
	if want := "GET /queues/redisq:ns:fifo:%7Borders%7D/jobs"; calls[len(calls)-1] != want {
		t.Errorf("b got %s, want %s", calls[len(calls)-1], want)
	}

	fb.Mu.Lock()
	fb.Queues = map[string]*fleettest.Queue{}
	fb.Mu.Unlock()
	if _, _, err := FetchJobs(context.Background(), g, contract.JobFailed, 0, 50); err == nil {
		t.Error("no unit has it: no error")
	}
}

// UnitLabel: two direct units whose instances share a name (one host) keep
// their own labels; a balanced unit is found by the instance that answered.
func TestUnitLabel(t *testing.T) {
	latest := &Snapshot{Pods: []PodSnapshot{
		{Key: "http://h:1/api", Instance: "host", Label: "instance-1"},
		{Key: "http://h:2/api", Instance: "host", Label: "instance-2"},
		{Key: "#pod-a", Instance: "pod-a", Label: "instance-3"},
	}}
	label := UnitLabel(latest)
	for _, c := range []struct{ ep, instance, want string }{
		{"http://h:2/api", "host", "instance-2"},
		{"http://h:1/api", "host", "instance-1"},
		{"http://svc/api", "pod-a", "instance-3"},
		{"http://other:9/api", "", "other"},
	} {
		if got := label(api.NewClient(c.ep, ""), c.instance); got != c.want {
			t.Errorf("UnitLabel(%s, %q) = %q, want %q", c.ep, c.instance, got, c.want)
		}
	}
}

// pausableQueue is a fresh shared queue that reports paused (API 1.2.0+).
func pausableQueue(id string) *fleettest.Queue {
	return &fleettest.Queue{Summary: `{"id":"` + id + `","counts":{"pending":1,"processing":0,"completed":0,"failed":0},"totals":{"submitted":1,"completed":0,"failed":0},"paused":false}`}
}

// QueueAct sends the action to every unit that reports the queue and serves
// the route; an older unit (no paused in its entry) is sent nothing. The
// group's paused comes from a copy that reports it, whichever unit was first.
func TestQueueAct(t *testing.T) {
	const id = "redisq:ns:fifo:{orders}"
	shared := pausableQueue(id)
	old, o := queueFake(t, "old", map[string]*fleettest.Queue{id: {Summary: `{"id":"` + id + `","counts":{"pending":1,"processing":0,"completed":0,"failed":0},"totals":{"submitted":1,"completed":0,"failed":0}}`}})
	fa, a := queueFake(t, "a", map[string]*fleettest.Queue{id: shared})
	_, b := queueFake(t, "b", map[string]*fleettest.Queue{id: shared})
	clients := []*api.Client{api.NewClient(o.URL, ""), api.NewClient(a.URL, ""), api.NewClient(b.URL, "")}
	g, _ := GroupQueues(FetchQueues(context.Background(), clients, byInstance)).Group(id)
	if !g.CanPause() || g.IsPaused() {
		t.Fatalf("group paused %v, can %v: want a copy that reports it", g.Paused, g.CanPause())
	}
	if u := g.Units(); len(u) != 3 || u[0].Pausable || !u[1].Pausable || u[1].Label != "a" || u[1].Endpoint != a.URL {
		t.Errorf("units = %+v", u)
	}

	if all, none := g.PauseState(); all || none {
		t.Errorf("older unit says nothing: PauseState = %v, %v, want neither", all, none)
	}
	res, now := QueueAct(context.Background(), g, nil, instanceNoun, "pause")
	if !now.IsPaused() || now.PausedAt == 0 || now.ID != id || g.IsPaused() {
		t.Errorf("answered group paused %v at %d (before: %v), want paused, g untouched", now.IsPaused(), now.PausedAt, g.IsPaused())
	}
	if *g.paused[1] || !*now.paused[1] { // PauseState hides it: old's nil
		t.Errorf("unit a paused: g %v, now %v, want g untouched", *g.paused[1], *now.paused[1])
	}
	want := []ActionResult{
		{Pod: "old", Key: o.URL, Tip: o.URL, Outcome: "no change", Msg: "not supported by this instance"},
		{Pod: "a", Key: a.URL, Tip: a.URL, Outcome: "ok"},
		{Pod: "b", Key: b.URL, Tip: b.URL, Outcome: "ok"},
	}
	if !slices.Equal(res, want) {
		t.Errorf("results =\n%+v\nwant\n%+v", res, want)
	}
	if !shared.Paused() {
		t.Error("queue not paused")
	}
	for _, c := range old.Calls() {
		if strings.HasPrefix(c, "PATCH") {
			t.Errorf("older unit sent %s", c)
		}
	}
	if want := "PATCH /queues/redisq:ns:fifo:%7Borders%7D/actions/pause"; !slices.Contains(fa.Calls(), want) {
		t.Errorf("a calls %v, want %s", fa.Calls(), want)
	}
	g, _ = GroupQueues(FetchQueues(context.Background(), clients, byInstance)).Group(id)
	if !g.IsPaused() || g.PausedAt == 0 {
		t.Errorf("after pause: paused %v at %d", g.IsPaused(), g.PausedAt)
	}
	QueueAct(context.Background(), g, nil, instanceNoun, "resume")
	if shared.Paused() {
		t.Error("queue not resumed")
	}
}

// An instance that cannot pause is a failure saying so, never a crash; one
// that lost the queue is no change; a storage error is failed with it.
func TestQueueActUnsupported(t *testing.T) {
	const id = "q"
	for _, c := range []struct {
		status       int
		drop         bool
		outcome, msg string
	}{
		{status: http.StatusNotImplemented, outcome: "failed", msg: "not supported by this instance"},
		{status: http.StatusNotFound, outcome: "failed", msg: "not supported by this instance"}, // router 404: older route set
		{status: http.StatusServiceUnavailable, outcome: "failed", msg: "Service Unavailable"},
		{drop: true, outcome: "no change", msg: "queue not on this instance now"},
	} {
		q := pausableQueue(id)
		f, s := queueFake(t, "a", map[string]*fleettest.Queue{id: q})
		g, _ := GroupQueues(FetchQueues(context.Background(), []*api.Client{api.NewClient(s.URL, "")}, byInstance)).Group(id)
		f.Mu.Lock()
		q.ActionStatus = c.status
		if c.drop {
			f.Queues = map[string]*fleettest.Queue{}
		}
		f.Mu.Unlock()
		res, now := QueueAct(context.Background(), g, nil, instanceNoun, "pause")
		if all, none := now.PauseState(); now.IsPaused() || all || !none {
			t.Errorf("%d: a failure changed the group: paused %v, state %v %v", c.status, now.IsPaused(), all, none)
		}
		if len(res) != 1 || res[0].Outcome != c.outcome || res[0].Msg != c.msg {
			t.Errorf("%d drop %v: %+v, want %s (%s)", c.status, c.drop, res, c.outcome, c.msg)
		}
	}
}

// A unit whose entry is an error may serve the route: it is sent the
// action. One that answers an error entry (200: flag stored) is taken at
// the action's word; the group's paused comes from an answer that has it,
// else the same word.
func TestQueueActErrorEntries(t *testing.T) {
	const id = "q"
	bad := &fleettest.Queue{Summary: `{"id":"q","error":"redis down"}`, ActionErr: "redis down"}
	fe, e := queueFake(t, "e", map[string]*fleettest.Queue{id: bad})
	fa, a := queueFake(t, "a", map[string]*fleettest.Queue{id: pausableQueue(id)})
	clients := []*api.Client{api.NewClient(e.URL, ""), api.NewClient(a.URL, "")}
	g, _ := GroupQueues(FetchQueues(context.Background(), clients, byInstance)).Group(id)
	if u := g.Units(); len(u) != 2 || !u[0].Pausable || !u[1].Pausable {
		t.Errorf("units = %+v, want both sent the action", u)
	}
	if all, none := g.PauseState(); all || none {
		t.Errorf("error entry says nothing: PauseState = %v, %v, want neither", all, none)
	}

	res, now := QueueAct(context.Background(), g, nil, instanceNoun, "pause")
	if !now.CanPause() || !now.IsPaused() || now.PausedAt == 0 {
		t.Errorf("answered group: can %v paused %v at %d, want a's answer", now.CanPause(), now.IsPaused(), now.PausedAt)
	}
	if now.paused[0] == nil || !*now.paused[0] || now.paused[1] == nil || !*now.paused[1] {
		t.Errorf("unit paused = %v, %v, want both paused", now.paused[0], now.paused[1])
	}
	if all, _ := now.PauseState(); !all {
		t.Error("PauseState: want all paused (Resume alone)")
	}
	if len(res) != 2 || res[0].Outcome != "ok" || res[1].Outcome != "ok" {
		t.Errorf("results = %+v", res)
	}
	want := "PATCH /queues/q/actions/pause"
	if !slices.Contains(fe.Calls(), want) || !slices.Contains(fa.Calls(), want) {
		t.Errorf("calls e %v, a %v, want %s on both", fe.Calls(), fa.Calls(), want)
	}

	// Every answer an error entry: the group takes the action's word too.
	// paused_at: unknown on a fresh pause, kept on a repeat one, gone on resume.
	q1, q2 := pausableQueue(id), pausableQueue(id)
	q1.ActionErr, q2.ActionErr = "redis down", "redis down"
	_, s1 := queueFake(t, "s1", map[string]*fleettest.Queue{id: q1})
	_, s2 := queueFake(t, "s2", map[string]*fleettest.Queue{id: q2})
	clients = []*api.Client{api.NewClient(s1.URL, ""), api.NewClient(s2.URL, "")}
	read := func() QueueGroup {
		g, _ := GroupQueues(FetchQueues(context.Background(), clients, byInstance)).Group(id)
		return g
	}
	for _, c := range []struct {
		action string
		paused bool
		keepAt bool // PausedAt is g's
	}{{"pause", true, false}, {"pause", true, true}, {"resume", false, false}} {
		g := read()
		_, now := QueueAct(context.Background(), g, nil, instanceNoun, c.action)
		wantAt := int64(0)
		if c.keepAt {
			wantAt = g.PausedAt
			if wantAt == 0 {
				t.Fatalf("%s: repeat pause read no paused_at", c.action)
			}
		}
		all, none := now.PauseState()
		if !now.CanPause() || now.IsPaused() != c.paused || now.PausedAt != wantAt || all != c.paused || none == c.paused {
			t.Errorf("%s (keep at %v): can %v paused %v at %d, state %v %v; want paused %v at %d",
				c.action, c.keepAt, now.CanPause(), now.IsPaused(), now.PausedAt, all, none, c.paused, wantAt)
		}
	}
}

// A unit that cannot be reached is failed with the transport error.
func TestQueueActTransportError(t *testing.T) {
	const id = "q"
	_, s := queueFake(t, "a", map[string]*fleettest.Queue{id: pausableQueue(id)})
	g, _ := GroupQueues(FetchQueues(context.Background(), []*api.Client{api.NewClient(s.URL, "")}, byInstance)).Group(id)
	s.Close()
	res, now := QueueAct(context.Background(), g, nil, instanceNoun, "pause")
	if len(res) != 1 || res[0].Outcome != "failed" || res[0].Msg == "" {
		t.Errorf("results = %+v, want failed with the error", res)
	}
	if now.IsPaused() || !now.CanPause() || now.paused[0] == nil || *now.paused[0] {
		t.Errorf("a failure changed the group: paused %v, can %v", now.IsPaused(), now.CanPause())
	}
}

// workflowFake is a fake varmq serving wfs on its workflow routes.
func workflowFake(t *testing.T, instance string, wfs ...*fleettest.Workflow) (*fleettest.Varmq, *httptest.Server) {
	t.Helper()
	f, srv := fleettest.NewVarmq(t, instance, nil)
	f.Mu.Lock()
	f.Workflows = append([]*fleettest.Workflow{}, wfs...)
	f.Mu.Unlock()
	return f, srv
}

// A durable workflow two units share is one row reported by both; an
// in-memory one of the same ID created at another time is its own row;
// rows go newest first; an older varmq is named, not failed.
func TestMergeWorkflows(t *testing.T) {
	const now = int64(1762228800000)
	shared := fleettest.SampleWorkflows(now)
	clash := fleettest.NewWorkflow(contract.WorkflowRecord{ID: fleettest.DiamondID, State: contract.WorkflowRunning, CreatedAt: now - 1000})
	_, a := workflowFake(t, "a", shared...)
	_, b := workflowFake(t, "b", append([]*fleettest.Workflow{clash}, shared[:3]...)...)
	_, old := fleettest.NewVarmq(t, "old", nil)
	clients := []*api.Client{api.NewClient(a.URL, ""), api.NewClient(b.URL, ""), api.NewClient(old.URL, "")}

	v := MergeWorkflows(FetchWorkflows(context.Background(), clients, byInstance, "", 50), 0, 50)
	var got []string
	for _, g := range v.Groups {
		got = append(got, g.ID+"@"+strings.Join(g.On, "+"))
	}
	want := []string{fleettest.DiamondID + "@b", fleettest.RunningID + "@a+b", fleettest.DiamondID + "@a+b", fleettest.SagaID + "@a+b", fleettest.CancelID + "@a"}
	if !slices.Equal(got, want) || v.More || v.Units != 3 || !slices.Equal(v.Unsupported, []string{"old"}) || v.Failed != nil {
		t.Errorf("merged = %v (more %v, unsupported %v, failed %v), want %v", got, v.More, v.Unsupported, v.Failed, want)
	}
	if g := v.Groups[1]; len(g.Clients) != 2 || g.Final() || len(g.Tasks) != 7 {
		t.Errorf("running group = %+v", g)
	}

	// a page from offset 1, two long: one more after it
	v = MergeWorkflows(FetchWorkflows(context.Background(), clients, byInstance, "", 1+2+1), 1, 2)
	if len(v.Groups) != 2 || v.Groups[0].ID != fleettest.RunningID || !v.More {
		t.Errorf("page 2 = %+v, more %v", v.Groups, v.More)
	}
	// a state filter goes to every unit
	v = MergeWorkflows(FetchWorkflows(context.Background(), clients, byInstance, contract.WorkflowCompensated, 50), 0, 50)
	if len(v.Groups) != 1 || v.Groups[0].ID != fleettest.SagaID {
		t.Errorf("compensated = %+v", v.Groups)
	}
}

// Copies of one workflow read at different moments: the one further on
// wins, ended over running.
func TestMergeWorkflowsKeepsMostAdvanced(t *testing.T) {
	rec := contract.WorkflowRecord{ID: "w", State: contract.WorkflowRunning, CreatedAt: 5, Tasks: []contract.TaskRecord{{Name: "t", State: contract.JobPending}}}
	done := rec
	done.State, done.FinishedAt, done.Tasks = contract.WorkflowCompleted, 9, []contract.TaskRecord{{Name: "t", State: contract.JobCompleted}}
	_, a := workflowFake(t, "a", fleettest.NewWorkflow(rec))
	_, b := workflowFake(t, "b", fleettest.NewWorkflow(done))
	for _, order := range [][]*api.Client{
		{api.NewClient(a.URL, ""), api.NewClient(b.URL, "")},
		{api.NewClient(b.URL, ""), api.NewClient(a.URL, "")},
	} {
		v := MergeWorkflows(FetchWorkflows(context.Background(), order, byInstance, "", 10), 0, 10)
		if len(v.Groups) != 1 || v.Groups[0].State != contract.WorkflowCompleted || len(v.Groups[0].On) != 2 {
			t.Errorf("merged = %+v", v.Groups)
		}
	}

	// the same tasks ended either way (undo jobs are nested, not counted):
	// the copy that ended wins on that alone
	tasks := []contract.TaskRecord{{Name: "t", State: contract.JobFailed, Undo: &contract.TaskRecord{Name: "t", State: contract.JobCompleted}}}
	comp := contract.WorkflowRecord{ID: "s", State: contract.WorkflowCompensating, CreatedAt: 5, Tasks: tasks}
	done = comp
	done.State, done.FinishedAt = contract.WorkflowCompensated, 9
	_, c := workflowFake(t, "c", fleettest.NewWorkflow(comp))
	_, d := workflowFake(t, "d", fleettest.NewWorkflow(done))
	for _, order := range [][]*api.Client{
		{api.NewClient(c.URL, ""), api.NewClient(d.URL, "")},
		{api.NewClient(d.URL, ""), api.NewClient(c.URL, "")},
	} {
		v := MergeWorkflows(FetchWorkflows(context.Background(), order, byInstance, "", 10), 0, 10)
		if len(v.Groups) != 1 || v.Groups[0].State != contract.WorkflowCompensated || !v.Groups[0].Final() {
			t.Errorf("compensating vs compensated = %+v", v.Groups)
		}
	}
}

// A unit's first offset+limit+1 workflows past the route's page cap are
// read in pages.
func TestFetchWorkflowsPages(t *testing.T) {
	var wfs []*fleettest.Workflow
	for i := range api.MaxWorkflowLimit + 300 {
		wfs = append(wfs, fleettest.NewWorkflow(contract.WorkflowRecord{ID: fmt.Sprintf("w%04d", i), State: contract.WorkflowCompleted, CreatedAt: int64(i + 1)}))
	}
	f, s := workflowFake(t, "a", wfs...)
	units := FetchWorkflows(context.Background(), []*api.Client{api.NewClient(s.URL, "")}, byInstance, "", api.MaxWorkflowLimit+101)
	if u := units[0]; u.Err != nil || len(u.Workflows) != api.MaxWorkflowLimit+101 || u.Workflows[0].ID != fmt.Sprintf("w%04d", api.MaxWorkflowLimit+299) {
		t.Fatalf("unit = %d workflows, err %v", len(u.Workflows), u.Err)
	}
	var lists int
	for _, c := range f.Calls() {
		if c == "GET /workflows" {
			lists++
		}
	}
	if lists != 2 {
		t.Errorf("%d list requests, want 2 pages", lists)
	}

	// up to the route's largest offset, and a page past it: the merged page
	// at offset 10000 needs each unit's records past 10000
	wfs = wfs[:0]
	for i := range api.MaxWorkflowOffset + 40 {
		wfs = append(wfs, fleettest.NewWorkflow(contract.WorkflowRecord{ID: fmt.Sprintf("w%05d", i), CreatedAt: int64(i + 1)}))
	}
	_, s = workflowFake(t, "b", wfs...)
	units = FetchWorkflows(context.Background(), []*api.Client{api.NewClient(s.URL, "")}, byInstance, "", api.MaxWorkflowOffset+26)
	if u := units[0]; u.Err != nil || len(u.Workflows) != api.MaxWorkflowOffset+26 {
		t.Errorf("at the offset cap: %d workflows, err %v; want %d", len(u.Workflows), u.Err, api.MaxWorkflowOffset+26)
	}
}

// FetchWorkflow groups the copies by created_at and names the units that
// hold the ID twice (409), lack the route, or fail.
func TestFetchWorkflow(t *testing.T) {
	const now = int64(1762228800000)
	wfs := fleettest.SampleWorkflows(now)
	_, a := workflowFake(t, "a", wfs...)
	_, b := workflowFake(t, "b", wfs[0], fleettest.NewWorkflow(contract.WorkflowRecord{ID: fleettest.DiamondID, CreatedAt: 1})) // two stores hold it
	_, c := workflowFake(t, "c", wfs[0])
	_, old := fleettest.NewVarmq(t, "old", nil)
	clients := []*api.Client{api.NewClient(a.URL, ""), api.NewClient(b.URL, ""), api.NewClient(c.URL, ""), api.NewClient(old.URL, "")}

	r := FetchWorkflow(context.Background(), clients, byInstance, fleettest.DiamondID)
	if len(r.Groups) != 1 || !slices.Equal(r.Groups[0].On, []string{"a", "c"}) || !slices.Equal(r.Ambiguous, []string{"b"}) || !slices.Equal(r.Unsupported, []string{"old"}) || r.Failed != nil {
		t.Errorf("read = %+v", r)
	}
	if g, ok := r.Group(now - 12*60_000); !ok || g.ID != fleettest.DiamondID || !g.Final() {
		t.Errorf("Group(created_at) = %+v, %v", g, ok)
	}
	if _, ok := r.Group(1); ok {
		t.Error("Group of another created_at found")
	}
	if r := FetchWorkflow(context.Background(), clients, byInstance, "nope"); r.Groups != nil || r.Ambiguous != nil || len(r.Unsupported) != 1 {
		t.Errorf("unknown ID read = %+v", r)
	}
}

// CancelWorkflow goes to the first unit that takes it: one that fails, or
// lost the workflow, gives way to the next; undo=false turns the undo jobs
// off.
func TestCancelWorkflow(t *testing.T) {
	wfs := fleettest.SampleWorkflows(1762228800000)
	running := wfs[1]
	_, empty := workflowFake(t, "gone")
	down, dn := workflowFake(t, "down", running)
	down.Mu.Lock()
	down.WorkflowStatus = http.StatusServiceUnavailable
	down.Mu.Unlock()
	fb, b := workflowFake(t, "b", running)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	g := WorkflowGroup{WorkflowRecord: running.Record(),
		Clients: []*api.Client{api.NewClient(dead.URL, ""), api.NewClient(dn.URL, ""), api.NewClient(empty.URL, ""), api.NewClient(b.URL, "")},
		labels:  []string{"dead", "down", "gone", "b"}}

	res, after := CancelWorkflow(context.Background(), g, nil, instanceNoun, true)
	if len(res) != 4 {
		t.Fatalf("results = %+v, want one per unit asked", res)
	}
	if res[1].Outcome != "failed" || res[1].Msg != "Service Unavailable" {
		t.Errorf("503 unit = %+v, want failed with the API message", res[1])
	}
	res = append(res[:1], res[2:]...) // the rest as before
	if after == nil || after.State != contract.WorkflowCancelled || after.UndoOnCancel || running.Record().State != contract.WorkflowCancelled {
		t.Fatalf("after = %+v", after)
	}
	if len(res) != 3 || res[0].Outcome != "failed" || res[1].Outcome != "no change" || res[1].Msg != "workflow not on this instance now" ||
		res[2] != (ActionResult{Pod: "b", Key: b.URL, Tip: b.URL, Outcome: "ok", Msg: "now cancelled"}) {
		t.Errorf("results = %+v", res)
	}
	if want := "PATCH /workflows/" + fleettest.RunningID + "/actions/cancel"; !slices.Contains(fb.Calls(), want) {
		t.Errorf("b calls %v, want %s", fb.Calls(), want)
	}

	// the saga's undo jobs run on a plain cancel of a running copy
	saga := fleettest.NewWorkflow(contract.WorkflowRecord{ID: "s", State: contract.WorkflowRunning, CreatedAt: 1, UndoOnCancel: true,
		Tasks: []contract.TaskRecord{{Name: "a", State: contract.JobCompleted, Undo: &contract.TaskRecord{Name: "a", State: contract.JobWaiting}}, {Name: "b", State: contract.JobWaiting, Deps: []string{"a"}}}})
	_, s := workflowFake(t, "s", saga)
	g = WorkflowGroup{WorkflowRecord: saga.Record(), Clients: []*api.Client{api.NewClient(s.URL, "")}, labels: []string{"s"}}
	if res, after := CancelWorkflow(context.Background(), g, nil, instanceNoun, false); after == nil || after.State != contract.WorkflowCompensating ||
		after.Tasks[0].Undo.State != contract.JobPending || after.Tasks[1].State != contract.JobCancelled || res[0].Msg != "now compensating" {
		t.Errorf("plain cancel = %+v, %+v", after, res)
	}
}
