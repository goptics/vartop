package fleet

import (
	"context"
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
// action. One that stores the flag but answers an error entry (200) leaves
// its paused as it was; the group's paused comes from an answer that has it.
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
	if now.paused[0] != nil || now.paused[1] == nil || !*now.paused[1] {
		t.Errorf("unit paused = %v, %v, want e's kept (nil), a's from its answer", now.paused[0], now.paused[1])
	}
	if len(res) != 2 || res[0].Outcome != "ok" || res[1].Outcome != "ok" {
		t.Errorf("results = %+v", res)
	}
	want := "PATCH /queues/q/actions/pause"
	if !slices.Contains(fe.Calls(), want) || !slices.Contains(fa.Calls(), want) {
		t.Errorf("calls e %v, a %v, want %s on both", fe.Calls(), fa.Calls(), want)
	}
}
