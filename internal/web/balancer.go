package web

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
)

// attemptCap is the most requests sent through a server's balancers in one
// go to reach k instances: ⌈k·(ln k + 4)⌉ + 2, so a balancer picking at random
// misses one of them with probability under e⁻⁴ (≈2%). k < 1 counts as 1.
func attemptCap(k int) int {
	n := float64(max(k, 1))
	return int(math.Ceil(n*(math.Log(n)+4))) + 2
}

// answer is one instance's first reply to a reach: what send returned (Err
// is an *api.APIError for a non-2xx reply, nil for a 2xx one), the endpoint
// it came through and when it arrived.
type answer[T any] struct {
	V   T
	Err error
	Via string
	At  time.Time
}

// reach sends through eps in turn (attempt i goes to eps[i%len(eps)]), each
// attempt reaching whichever instance the balancer picks, until every
// instance in want has answered, limit attempts were sent, or ctx is done.
// Attempts go in concurrent rounds of min(limit-sent, max(missing, len(eps)))
// with missing the wanted instances yet to answer, so every round tries every
// endpoint; with want empty, exactly one round is sent. got holds each
// instance's first answer, wanted or not. An attempt that names no instance
// (a transport error, a balancer's own error page, a varmq without the
// header) is nobody's answer; the last such error is lastErr. Every attempt
// has returned when reach does.
func reach[T any](ctx context.Context, eps []*api.Client, want []string, limit int,
	send func(context.Context, *api.Client) (T, string, error)) (got map[string]answer[T], sent int, lastErr error) {
	got = make(map[string]answer[T])
	if len(eps) == 0 {
		return got, 0, nil
	}
	missing := func() int {
		n := 0
		for _, id := range want {
			if _, ok := got[id]; !ok {
				n++
			}
		}
		return n
	}
	type reply struct {
		answer[T]
		instance string
	}
	for sent < limit && ctx.Err() == nil {
		round := make([]reply, min(limit-sent, max(missing(), len(eps))))
		var wg sync.WaitGroup
		for i := range round {
			c := eps[(sent+i)%len(eps)]
			wg.Go(func() {
				v, instance, err := send(ctx, c)
				round[i] = reply{answer[T]{V: v, Err: err, Via: c.PrimaryEndpoint(), At: time.Now()}, instance}
			})
		}
		wg.Wait()
		sent += len(round)

		for _, r := range round {
			if r.instance == "" {
				if r.Err != nil {
					lastErr = r.Err
				}
				continue
			}
			if _, ok := got[r.instance]; !ok {
				got[r.instance] = r.answer
			}
		}
		if missing() == 0 {
			break
		}
	}
	return got, sent, lastErr
}

// listFresh is (*api.Client).ListWorkersFresh as a reach send.
func listFresh(ctx context.Context, c *api.Client) ([]api.WorkerSummary, string, error) {
	return c.ListWorkersFresh(ctx)
}

// pollBalanced polls every instance behind the balancer endpoints eps as a
// unit of its own, keyed "#"+instance ID, except the instances in direct,
// which are shown as their direct units only. Lists are fetched first, then
// every listed worker from the instances whose list has it, all on new
// connections and within one attempt cap. heard is called with the endpoint
// and instance of every answer.
//
// ponytail: no per-endpoint request limit; add one if new-connection volume
// bites.
func pollBalanced(ctx context.Context, eps []*api.Client, t *tracker, direct map[string]bool, now time.Time,
	heard func(endpoint, instance string)) []PodSnapshot {
	endpoints := make([]string, len(eps))
	for i, c := range eps {
		endpoints[i] = c.PrimaryEndpoint()
	}
	known := t.known(endpoints, direct, now)
	limit := attemptCap(max(len(known), len(eps)))

	lists, _, lastErr := reach(ctx, eps, slices.Sorted(maps.Keys(known)), limit, listFresh)
	listed := make(map[string][]api.WorkerSummary) // instance → its 2xx list
	having := make(map[string][]string)            // worker → instances whose list has it
	for id, ans := range lists {
		heard(ans.Via, id)
		if direct[id] {
			continue
		}
		if _, ok := known[id]; !ok {
			known[id] = now // newly found
		}
		if ans.Err == nil {
			listed[id] = ans.V
			for _, w := range ans.V {
				having[w.Name] = append(having[w.Name], id)
			}
		}
	}

	names := slices.Sorted(maps.Keys(having))
	fetched := make([]map[string]answer[*api.WorkerDetails], len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		ids := having[name]
		wg.Go(func() {
			fetched[i], _, _ = reach(ctx, eps, ids, limit,
				func(ctx context.Context, c *api.Client) (*api.WorkerDetails, string, error) {
					return c.GetWorkerFresh(ctx, url.PathEscape(name))
				})
		})
	}
	wg.Wait()
	answers := make(map[string]map[string]answer[*api.WorkerDetails], len(names)) // worker → instance → answer
	for i, name := range names {
		answers[name] = fetched[i]
		for id, ans := range fetched[i] {
			heard(ans.Via, id)
		}
	}

	var units []PodSnapshot
	for id, at := range known {
		unit := PodSnapshot{Key: "#" + id, Instance: id, Balanced: true}
		ans, answered := lists[id]
		switch {
		case answered && ans.Err == nil:
			unit.Via = ans.Via
			unit.Workers = make(map[string]*Sample, len(listed[id]))
			for _, w := range listed[id] {
				got, ok := answers[w.Name][id]
				var apiErr *api.APIError
				switch {
				case ok && got.Err == nil:
					unit.Workers[w.Name] = &Sample{At: got.At, WorkerDetails: *got.V}
				case ok && errors.As(got.Err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
					// removed since the list: skip
				default:
					unit.Unreached = append(unit.Unreached, w.Name)
				}
			}
			slices.Sort(unit.Unreached)
		case answered:
			unit.Via, unit.Err = ans.Via, ans.Err.Error()
		case len(lists) == 0 && lastErr != nil:
			unit.Via, unit.Err = t.lastVia(id), lastErr.Error()
		default:
			unit.Via = t.lastVia(id)
			unit.Err = "not reached this refresh · last seen " + now.Sub(at).Round(time.Second).String() + " ago"
		}
		units = append(units, unit)
	}
	if len(known) == 0 && len(lists) == 0 && lastErr != nil {
		for _, ep := range endpoints {
			units = append(units, PodSnapshot{Key: ep, Via: ep, Balanced: true, Err: lastErr.Error()})
		}
	}
	return units
}

// tracker remembers, across a session's ticks, which instances answered
// through which endpoint and when, and the labels units were given. It
// belongs to the poller goroutine.
type tracker struct {
	window     time.Duration                   // how long a sighting counts: max(1 minute, 3 × interval)
	kubernetes bool                            // labels are pod names, not instance-N
	seen       map[string]map[string]time.Time // endpoint → instance → last answer through it
	labels     map[string]string               // unit key → instance-N; never forgotten, so never reused
}

// newTracker returns an empty tracker for a server polled every interval.
func newTracker(interval time.Duration, kubernetes bool) *tracker {
	return &tracker{
		window:     max(time.Minute, 3*interval),
		kubernetes: kubernetes,
		seen:       make(map[string]map[string]time.Time),
		labels:     make(map[string]string),
	}
}

// saw records that instance answered through endpoint at at. An answer
// naming no instance ("") is ignored.
func (t *tracker) saw(endpoint, instance string, at time.Time) {
	if instance == "" {
		return
	}
	ids := t.seen[endpoint]
	if ids == nil {
		ids = make(map[string]time.Time)
		t.seen[endpoint] = ids
	}
	if at.After(ids[instance]) {
		ids[instance] = at
	}
}

// recent reports whether a sighting at at is still within the window at now.
func (t *tracker) recent(at, now time.Time) bool { return now.Sub(at) <= t.window }

// balancer reports whether more than one instance answered through endpoint
// within the window.
func (t *tracker) balancer(endpoint string, now time.Time) bool {
	n := 0
	for _, at := range t.seen[endpoint] {
		if t.recent(at, now) {
			n++
		}
	}
	return n > 1
}

// known returns every instance that answered through any of balancers within
// the window, with when it was last seen through them, except those in
// direct.
func (t *tracker) known(balancers []string, direct map[string]bool, now time.Time) map[string]time.Time {
	known := make(map[string]time.Time)
	for _, ep := range balancers {
		for id, at := range t.seen[ep] {
			if t.recent(at, now) && !direct[id] && at.After(known[id]) {
				known[id] = at
			}
		}
	}
	return known
}

// lastVia returns the endpoint instance last answered through (the smallest
// on a tie), "" if it never did.
func (t *tracker) lastVia(instance string) string {
	var via string
	var last time.Time
	for ep, ids := range t.seen {
		if at, ok := ids[instance]; ok && (at.After(last) || at.Equal(last) && ep < via) {
			via, last = ep, at
		}
	}
	return via
}

// prune forgets sightings older than the window.
func (t *tracker) prune(now time.Time) {
	for ep, ids := range t.seen {
		for id, at := range ids {
			if !t.recent(at, now) {
				delete(ids, id)
			}
		}
		if len(ids) == 0 {
			delete(t.seen, ep)
		}
	}
}

// label names the units of a non-Kubernetes server instance-1, instance-2, …
// in the order the session first saw their keys (new keys of one call in key
// order), never reusing a number. On a Kubernetes server it leaves labels as
// they are.
func (t *tracker) label(pods []PodSnapshot) {
	if t.kubernetes {
		return
	}
	var fresh []string
	for _, p := range pods {
		if _, ok := t.labels[p.Key]; !ok {
			fresh = append(fresh, p.Key)
		}
	}
	slices.Sort(fresh)
	for _, key := range fresh {
		t.labels[key] = fmt.Sprintf("instance-%d", len(t.labels)+1)
	}
	for i := range pods {
		pods[i].Label = t.labels[pods[i].Key]
	}
}
