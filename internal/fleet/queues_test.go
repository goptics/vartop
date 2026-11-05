package fleet

import (
	"context"
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
