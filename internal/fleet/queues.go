package fleet

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/goptics/varmq/contract"
	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// UnitQueues is what one endpoint answered to GET /queues. Unsupported: it
// serves no queue routes (older varmq, or no QueueInspector).
type UnitQueues struct {
	Client      *api.Client
	Label       string // the unit's label, as the pages name it
	Queues      []api.QueueSummary
	Unsupported bool
	Err         error
}

// QueueGroup is one Queue ID across the fleet: one copy of its summary, not
// a sum — every instance reads the same storage — and the units that
// report it, in the order fetched.
type QueueGroup struct {
	api.QueueSummary
	On      []string      // labels of the units reporting it, each once
	Clients []*api.Client // the clients that reported it, for the jobs routes
	labels  []string      // the label of each of Clients
}

// QueuesView is the queues of a server: Groups sorted by ID. Unsupported
// and Failed name the units that serve no queue routes, and those whose
// GET /queues failed (label: error).
type QueuesView struct {
	Groups      []QueueGroup
	Unsupported []string
	Failed      []string
	Units       int // units asked
}

// FetchQueues asks every client for GET /queues at once. label names a
// client's unit by the instance that answered ("" if none reported one).
func FetchQueues(ctx context.Context, clients []*api.Client, label func(c *api.Client, instance string) string) []UnitQueues {
	out := make([]UnitQueues, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() {
			qs, ok, err := c.ListQueues(ctx)
			out[i] = UnitQueues{Client: c, Label: label(c, c.LastInstance()), Queues: qs, Unsupported: !ok && err == nil, Err: err}
		})
	}
	wg.Wait()
	return out
}

// GroupQueues groups units' queues by Queue ID. It keeps one copy of each:
// the first unit's that read the storage, an error copy only when every
// unit's is one. Copies are never added up.
func GroupQueues(units []UnitQueues) QueuesView {
	v := QueuesView{Units: len(units)}
	byID := make(map[string]int) // Queue ID → index in v.Groups
	for _, u := range units {
		switch {
		case u.Err != nil:
			v.Failed = append(v.Failed, fmt.Sprintf("%s: %v", u.Label, u.Err))
			continue
		case u.Unsupported:
			v.Unsupported = append(v.Unsupported, u.Label)
			continue
		}
		for _, q := range u.Queues {
			i, seen := byID[q.ID]
			if !seen {
				byID[q.ID] = len(v.Groups)
				v.Groups = append(v.Groups, QueueGroup{QueueSummary: q})
				i = len(v.Groups) - 1
			} else if v.Groups[i].Err != "" && q.Err == "" {
				v.Groups[i].QueueSummary = q // a copy that read the storage beats an error
			}
			g := &v.Groups[i]
			if !slices.Contains(g.On, u.Label) {
				g.On = append(g.On, u.Label)
			}
			g.Clients, g.labels = append(g.Clients, u.Client), append(g.labels, u.Label)
		}
	}
	slices.SortFunc(v.Groups, func(a, b QueueGroup) int { return cmp.Compare(a.ID, b.ID) })
	return v
}

// Group is the group of Queue ID id, and whether v has it.
func (v QueuesView) Group(id string) (QueueGroup, bool) {
	i, ok := slices.BinarySearchFunc(v.Groups, id, func(g QueueGroup, id string) int { return cmp.Compare(g.ID, id) })
	if !ok {
		return QueueGroup{}, false
	}
	return v.Groups[i], true
}

// FetchJobs reads one page of queue g's jobs in state from the first of its
// units that has it: any one does, all read the same storage. A unit that
// fails or no longer has the queue gives way to the next. It returns the
// label of the unit that answered, and the last error when none did.
func FetchJobs(ctx context.Context, g QueueGroup, state contract.JobState, offset, limit int) ([]contract.JobRecord, string, error) {
	var lastErr error
	for i, c := range g.Clients {
		jobs, ok, err := c.ListJobs(ctx, g.ID, state, offset, limit)
		switch {
		case err != nil:
			lastErr = err
		case ok:
			return jobs, g.labels[i], nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no unit has queue %s now", g.ID)
	}
	return nil, "", lastErr
}

// UnitLabel labels the client of an endpoint of latest for the queue pages:
// the unit keyed by that endpoint (instances that share a name are keyed so),
// else the unit whose instance answered, else the endpoint's host.
func UnitLabel(latest *Snapshot) func(*api.Client, string) string {
	return func(c *api.Client, instance string) string {
		ep := c.PrimaryEndpoint()
		var byInstance string
		for _, p := range latest.Pods {
			if p.Key == ep {
				return p.Label
			}
			if instance != "" && p.Instance == instance && byInstance == "" {
				byInstance = p.Label
			}
		}
		return cmp.Or(byInstance, config.ShortOrigin(ep))
	}
}
