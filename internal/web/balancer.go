package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
)

// attemptCap is the most requests a reach job sends through a server's
// balancers to reach k instances: ⌈k·(ln k + 4)⌉ + 2, so a balancer picking
// at random misses one of them with probability under e⁻⁴ (≈2%). k < 1
// counts as 1.
func attemptCap(k int) int {
	n := float64(max(k, 1))
	return int(math.Ceil(n*(math.Log(n)+4))) + 2
}

// answer is one reply to a reach attempt: what send returned (Err is an
// *api.APIError for a non-2xx reply, nil for a 2xx one), the endpoint it
// came through and when it arrived.
type answer[T any] struct {
	V   T
	Err error
	Via string
	At  time.Time
}

// reacher sends its jobs' requests through balancer endpoints, each attempt
// reaching whichever instance the balancer picks, until every job has a
// definitive reply from every instance it wants or has spent its budget.
// There are no rounds: each finished attempt is recorded at once and the
// freed slot refilled. A slot goes to a job drawn at random, not to the job
// whose attempt came back: that job would take the balancer's next pick
// again, and since which attempt comes back first follows which instance it
// reached, jobs side by side behind a round-robin balancer would each get
// the same instance over and over. When no reply has come for twice the
// slowest round trip so far, the attempts in flight are presumed stuck on a
// slow instance: they stop counting as in flight, so others are sent.
//
// ponytail: budgets are per job, so an instance that never answers costs
// jobs × budget connections; add a shared budget if that bites.
type reacher struct {
	eps     []*api.Client
	budget  int // attempts per job
	jobs    []reachJob
	replies chan func()   // a finished attempt's bookkeeping, run by run's loop
	flying  int           // attempts started and not yet recorded
	slowest time.Duration // longest round trip of a reply naming an instance
	gen     int           // bumped each time the attempts in flight are presumed stuck
}

// minHedge is the shortest wait for a reply before attempts in flight are
// presumed stuck.
const minHedge = 10 * time.Millisecond

// reachJob is a job[T] as the reacher schedules it.
type reachJob interface {
	load(eps, budget int) (owed, target, active, left int)
	settled(eps, budget int) bool
	start(ctx context.Context, r *reacher, wg *sync.WaitGroup)
	stall() // presume every attempt in flight stuck
}

// newReacher returns a reacher over the balancer endpoints eps that lets
// each job send at most budget attempts.
func newReacher(eps []*api.Client, budget int) *reacher {
	return &reacher{eps: eps, budget: budget, replies: make(chan func())}
}

// job is one request (a worker list, one worker's details, an action) that a
// reacher gets a definitive reply to from each instance it wants: a 2xx, or
// an error definitive accepts (a 404 for a worker that is gone). While some
// wanted instance is missing it has about max(missing, endpoints) attempts
// in flight, and it sends at least one attempt per endpoint. Its fields are
// written by the reacher's loop only; read them after run.
type job[T any] struct {
	send       func(context.Context, *api.Client) (T, string, error)
	definitive func(error) bool                   // error replies that settle an instance; nil for none
	onReply    func(instance string, a answer[T]) // run in the loop for each reply naming an instance, once recorded

	wants   map[string]bool      // instances it needs a definitive reply from
	got     map[string]answer[T] // first definitive reply per instance, wanted or not
	failed  map[string]answer[T] // latest other reply per instance without a definitive one
	lastErr error                // latest error of an attempt that named no instance
	sent    int
	flying  int // attempts in flight
	stuck   int // of those, the ones presumed stuck
}

// addJob adds a job sending send to r. Its wants start empty; see want.
func addJob[T any](r *reacher, send func(context.Context, *api.Client) (T, string, error), definitive func(error) bool) *job[T] {
	j := &job[T]{
		send:       send,
		definitive: definitive,
		wants:      make(map[string]bool),
		got:        make(map[string]answer[T]),
		failed:     make(map[string]answer[T]),
	}
	r.jobs = append(r.jobs, j)
	return j
}

// want adds instances to those j needs a definitive reply from. It may be
// called from an onReply while the reacher runs.
func (j *job[T]) want(ids ...string) {
	for _, id := range ids {
		j.wants[id] = true
	}
}

// missing is how many wanted instances have no definitive reply yet.
func (j *job[T]) missing() int {
	n := 0
	for id := range j.wants {
		if _, ok := j.got[id]; !ok {
			n++
		}
	}
	return n
}

// load is what j asks of the next launch: the attempts it is owed outright
// (one per endpoint, so new instances get noticed, once it misses nobody);
// while it misses someone, the attempts it wants in flight; its attempts in
// flight not presumed stuck; and its budget left.
func (j *job[T]) load(eps, budget int) (owed, target, active, left int) {
	left = budget - j.sent
	if m := j.missing(); m > 0 {
		return 0, max(m, eps), j.flying - j.stuck, left
	}
	return max(0, min(eps-j.sent, left)), 0, 0, left
}

// settled reports whether j is done: every wanted instance replied
// definitively and its one attempt per endpoint came back or is presumed
// stuck (a hung endpoint must not hold a job that has what it wants), or
// its whole budget came back.
func (j *job[T]) settled(eps, budget int) bool {
	if j.missing() == 0 {
		return j.sent-(j.flying-j.stuck) >= min(eps, budget)
	}
	return j.sent >= budget && j.flying == 0
}

// start sends one attempt of j through the next endpoint in j's turn.
func (j *job[T]) start(ctx context.Context, r *reacher, wg *sync.WaitGroup) {
	c := r.eps[j.sent%len(r.eps)]
	j.sent++
	j.flying++
	r.flying++
	gen, began := r.gen, time.Now()
	wg.Go(func() {
		v, instance, err := j.send(ctx, c)
		a := answer[T]{V: v, Err: err, Via: c.PrimaryEndpoint(), At: time.Now()}
		r.replies <- func() {
			j.flying--
			if gen < r.gen {
				j.stuck--
			}
			if instance != "" {
				r.slowest = max(r.slowest, a.At.Sub(began))
			}
			j.record(instance, a)
		}
	})
}

func (j *job[T]) stall() { j.stuck = j.flying }

// record files one reply. One that names no instance (a transport error, a
// balancer's own error page, a varmq without the header) is nobody's: only
// its error is kept.
func (j *job[T]) record(instance string, a answer[T]) {
	if instance == "" {
		if a.Err != nil {
			j.lastErr = a.Err
		}
		return
	}
	if _, done := j.got[instance]; !done {
		if a.Err == nil || j.definitive != nil && j.definitive(a.Err) {
			j.got[instance] = a
			delete(j.failed, instance)
		} else {
			j.failed[instance] = a
		}
	}
	if j.onReply != nil {
		j.onReply(instance, a)
	}
}

// run sends the jobs' attempts until every job has settled or ctx is done,
// then cancels the attempts still in flight and returns once every one has
// returned. Replies recorded by then stay in the jobs.
func (r *reacher) run(ctx context.Context) {
	if len(r.eps) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for {
		if ctx.Err() == nil {
			r.launch(ctx, &wg)
		}
		if r.flying == 0 || r.settled() {
			break
		}
		var hedge <-chan time.Time
		if r.slowest > 0 {
			hedge = time.After(max(minHedge, 2*r.slowest))
		}
		select {
		case finish := <-r.replies:
			r.flying--
			finish()
		case <-hedge:
			r.gen++
			for _, j := range r.jobs {
				j.stall()
			}
		}
	}
	cancel()
	for ; r.flying > 0; r.flying-- {
		<-r.replies // every job has settled: late replies are dropped
	}
	wg.Wait()
}

// launch starts the attempts owed outright, then fills the free slots (the
// targets of the jobs missing someone, less their attempts in flight) with
// jobs drawn at random, weighted by target, within their budgets. The batch
// goes out in random order.
func (r *reacher) launch(ctx context.Context, wg *sync.WaitGroup) {
	type candidate struct {
		job          reachJob
		weight, left int
	}
	var batch []reachJob
	var draw []candidate
	free, total := 0, 0
	for _, j := range r.jobs {
		owed, target, active, left := j.load(len(r.eps), r.budget)
		for range owed {
			batch = append(batch, j)
		}
		if target > 0 && left > 0 {
			free += target - active
			draw = append(draw, candidate{j, target, left})
			total += target
		}
	}
	for ; free > 0 && total > 0; free-- {
		n, i := rand.IntN(total), 0
		for n >= draw[i].weight {
			n -= draw[i].weight
			i++
		}
		batch = append(batch, draw[i].job)
		if draw[i].left--; draw[i].left == 0 {
			total -= draw[i].weight
			draw[i].weight = 0
		}
	}
	rand.Shuffle(len(batch), func(i, k int) { batch[i], batch[k] = batch[k], batch[i] })
	for _, j := range batch {
		j.start(ctx, r, wg)
	}
}

// settled reports whether every job has settled.
func (r *reacher) settled() bool {
	for _, j := range r.jobs {
		if !j.settled(len(r.eps), r.budget) {
			return false
		}
	}
	return true
}

// listFresh is (*api.Client).ListWorkersFresh as a job's send.
func listFresh(ctx context.Context, c *api.Client) ([]api.WorkerSummary, string, error) {
	return c.ListWorkersFresh(ctx)
}

// isNotFound reports whether err is a 404 reply.
func isNotFound(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// pollBalanced polls every instance behind the balancer endpoints eps as a
// unit of its own, keyed "#"+instance ID, except the instances in skip. One
// reacher runs the list job and a fetch job per listed worker: an instance's
// list adds it to the fetch jobs of its workers as soon as it arrives. Each
// job's budget counts every instance the balancers pick among, those in
// skip too. Every reply is recorded in t, dated now; pollBalanced writes t,
// so nothing else may use t while it runs.
func pollBalanced(ctx context.Context, eps []*api.Client, t *tracker, skip map[string]bool, now time.Time) []PodSnapshot {
	endpoints := make([]string, len(eps))
	behind := 0
	for i, c := range eps {
		endpoints[i] = c.PrimaryEndpoint()
		behind += t.behind(endpoints[i], now)
	}
	known := t.known(endpoints, skip, now)
	r := newReacher(eps, attemptCap(max(len(known), behind, len(eps))))

	lists := addJob(r, listFresh, nil)
	lists.want(slices.Collect(maps.Keys(known))...)
	fetches := make(map[string]*job[*api.WorkerDetails]) // by worker name
	replied := false                                     // some list reply named an instance
	lists.onReply = func(id string, a answer[[]api.WorkerSummary]) {
		t.saw(a.Via, id, now)
		replied = true
		if skip[id] {
			return
		}
		lists.want(id) // known from now on, new or not
		list, ok := lists.got[id]
		if !ok {
			return
		}
		for _, w := range list.V {
			f := fetches[w.Name]
			if f == nil {
				name := url.PathEscape(w.Name)
				f = addJob(r, func(ctx context.Context, c *api.Client) (*api.WorkerDetails, string, error) {
					return c.GetWorkerFresh(ctx, name)
				}, isNotFound)
				f.onReply = func(id string, a answer[*api.WorkerDetails]) { t.saw(a.Via, id, now) }
				fetches[w.Name] = f
			}
			f.want(id)
		}
	}
	r.run(ctx)

	var units []PodSnapshot
	for id := range lists.wants {
		unit := PodSnapshot{Key: "#" + id, Instance: id, Balanced: true}
		if list, ok := lists.got[id]; ok {
			unit.Via = list.Via
			unit.Workers = make(map[string]*Sample, len(list.V))
			for _, w := range list.V {
				got, ok := fetches[w.Name].got[id]
				switch {
				case !ok:
					unit.Unreached = append(unit.Unreached, w.Name)
				case got.Err == nil:
					unit.Workers[w.Name] = &Sample{At: got.At, WorkerDetails: *got.V}
				default:
					// 404: removed since the list
				}
			}
			slices.Sort(unit.Unreached)
		} else if failed, ok := lists.failed[id]; ok {
			unit.Via, unit.Err = failed.Via, failed.Err.Error()
		} else {
			unit.Via = t.lastVia(id)
			switch age := now.Sub(known[id]).Round(time.Second); {
			case !replied && lists.lastErr != nil:
				unit.Err = lists.lastErr.Error()
			case age == 0: // seen this tick, by a direct pass
				unit.Err = "not reached this refresh"
			default:
				unit.Err = "not reached this refresh · last seen " + age.String() + " ago"
			}
		}
		units = append(units, unit)
	}
	if len(known) == 0 && !replied && lists.lastErr != nil {
		for _, ep := range endpoints {
			units = append(units, PodSnapshot{Key: ep, Via: ep, Balanced: true, Err: lists.lastErr.Error()})
		}
	}
	return units
}

// tracker remembers, across a session's ticks, which instances answered
// through which endpoint and when, and the numbers units were given. It
// belongs to the poller goroutine, which lends it to the balancer pass while
// it waits for the direct endpoints.
type tracker struct {
	window     time.Duration                   // how long a sighting counts: max(1 minute, 3 × interval)
	kubernetes bool                            // labels are pod names, not instance-N
	seen       map[string]map[string]time.Time // endpoint → instance → last answer through it
	numbers    map[string]int                  // what a unit's number follows (see label) → its instance-N; never forgotten, so never reused
	direct     map[string]bool                 // instances shown as a direct unit by the latest tick
}

// newTracker returns an empty tracker for a server polled every interval.
func newTracker(interval time.Duration, kubernetes bool) *tracker {
	return &tracker{
		window:     max(time.Minute, 3*interval),
		kubernetes: kubernetes,
		seen:       make(map[string]map[string]time.Time),
		numbers:    make(map[string]int),
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

// behind is how many instances answered through endpoint within the window.
func (t *tracker) behind(endpoint string, now time.Time) int {
	n := 0
	for _, at := range t.seen[endpoint] {
		if t.recent(at, now) {
			n++
		}
	}
	return n
}

// balancer reports whether more than one instance answered through endpoint
// within the window.
func (t *tracker) balancer(endpoint string, now time.Time) bool { return t.behind(endpoint, now) > 1 }

// lastSeen returns the instance that last answered through endpoint (the
// smallest on a tie), "" if none did within the window.
func (t *tracker) lastSeen(endpoint string, now time.Time) string {
	var instance string
	var last time.Time
	for id, at := range t.seen[endpoint] {
		if t.recent(at, now) && (at.After(last) || at.Equal(last) && id < instance) {
			instance, last = id, at
		}
	}
	return instance
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
// and lists them in that order. A unit's number follows its instance when
// no other unit of pods has the same one, so it stays with the process
// while its endpoint turns from direct to balancer or back; otherwise it
// follows the unit's key. Numbers are given in the order the session first
// saw what they follow (new ones of one call in unit key order, so a
// static list numbers in endpoint order) and never reused. On a Kubernetes
// server it leaves labels and order as they are.
func (t *tracker) label(pods []PodSnapshot) {
	if t.kubernetes {
		return
	}
	units := make(map[string]int, len(pods)) // instance → units of pods it answered for
	for _, p := range pods {
		units[p.Instance]++
	}
	follows := func(p PodSnapshot) string {
		if p.Instance != "" && units[p.Instance] == 1 {
			return "#" + p.Instance // a balanced unit's key; no endpoint URL starts with "#"
		}
		return p.Key
	}
	slices.SortFunc(pods, func(a, b PodSnapshot) int { return strings.Compare(a.Key, b.Key) })
	for _, p := range pods {
		if f := follows(p); t.numbers[f] == 0 {
			t.numbers[f] = len(t.numbers) + 1
		}
	}
	slices.SortFunc(pods, func(a, b PodSnapshot) int { return cmp.Compare(t.numbers[follows(a)], t.numbers[follows(b)]) })
	for i := range pods {
		pods[i].Label = fmt.Sprintf("instance-%d", t.numbers[follows(pods[i])])
	}
}
