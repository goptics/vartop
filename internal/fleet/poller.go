package fleet

import (
	"cmp"
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// Tick is what a collector hands its sink after each poll.
type Tick struct {
	Snap *Snapshot // rated; immutable once handed over
}

// Collector polls one server: a single background goroutine fans out to
// every unit in the pool each interval, rates the Snapshot and hands it to
// the sink. That goroutine is the sink's only caller, so a collector's late
// tick can never reach another collector's sink.
type Collector struct {
	interval time.Duration
	pool     *api.Pool
	sink     func(Tick) error
	tracker  *tracker        // the poller goroutine's
	rater    *Rater          // the poller goroutine's
	ctx      context.Context // cancelled by Close
	cancel   context.CancelFunc
	pollNow  chan chan struct{} // the poller closes each received channel once its snapshot is sunk
	stopped  chan struct{}      // closed when the poller goroutine returns
}

// NewCollector starts polling srv through pool, which the collector now
// owns, and hands every snapshot to sink. The first poll starts
// immediately; each next one starts srv's poll interval after the previous
// one ended. A sink error ends the polling: the sink is not called again,
// and the caller, which saw the error, still has to Close the collector.
func NewCollector(srv *config.Server, pool *api.Pool, sink func(Tick) error) *Collector {
	ctx, cancel := context.WithCancel(context.Background())
	interval := srv.PollInterval()
	c := &Collector{
		interval: interval,
		pool:     pool,
		sink:     sink,
		tracker:  newTracker(interval, srv.Kubernetes != nil),
		rater:    NewRater(interval),
		ctx:      ctx,
		cancel:   cancel,
		pollNow:  make(chan chan struct{}),
		stopped:  make(chan struct{}),
	}
	go c.run()
	return c
}

// PollNow asks the poller for an immediate poll and waits for it. It returns
// true once a snapshot started after the request has been handed to the
// sink, and false if ctx ends, the collector is closed or its polling ended
// first. A request made mid-tick waits for that tick and then gets a fresh
// one, so the snapshot reflects anything done before the call.
func (c *Collector) PollNow(ctx context.Context) bool {
	done := make(chan struct{})
	select {
	case c.pollNow <- done:
	case <-ctx.Done():
		return false
	case <-c.ctx.Done():
		return false
	case <-c.stopped:
		return false
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-c.ctx.Done():
		return false
	case <-c.stopped:
		return false
	}
}

// Close stops polling and closes the pool. It does not wait for the poller:
// a tick in flight has its requests cancelled and reaches no sink.
func (c *Collector) Close() error {
	c.cancel()
	return c.pool.Close()
}

// run is the poller loop. Ticks run inline and the timer is reset only after
// a tick ends, so a slow tick pushes the next one back instead of overlapping.
func (c *Collector) run() {
	defer close(c.stopped)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var done chan struct{}
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
		case done = <-c.pollNow:
		}
		if !c.tick() {
			return
		}
		if done != nil {
			close(done)
		}
		timer.Reset(c.interval)
	}
}

// tick polls every unit with a deadline of 0.8 × the interval, rates the
// snapshot and hands it to the sink. It reports false, handing over
// nothing, once the collector is closed, and false when the sink fails.
func (c *Collector) tick() bool {
	ctx, cancel := context.WithTimeout(c.ctx, c.interval*8/10)
	defer cancel()
	snap := pollOnce(ctx, c.pool, c.tracker)
	if c.ctx.Err() != nil {
		return false
	}
	c.rater.Rate(snap)
	return c.sink(Tick{Snap: snap}) == nil
}

// pollOnce polls every unit of the pool within ctx, tracking instances
// across ticks in t, and returns the snapshot with At set to when the poll
// started. Every Sample in it is freshly allocated, as Rater.Rate
// requires.
func pollOnce(ctx context.Context, pool *api.Pool, t *tracker) *Snapshot {
	return pollAt(ctx, pool, t, time.Now())
}

// pollAt is pollOnce for a tick that started at now, which dates the
// tracker's sightings, so tests can step past its window. Endpoints that are
// balancers at the start are polled alongside the direct ones, skipping the
// instances the previous tick showed as direct units, so neither waits on the
// other. A direct endpoint whose replies came from more than one instance
// becomes a balancer: its unit is dropped and, time permitting, it gets a
// balancer pass of its own. Balanced units of an instance that is a direct
// unit this tick are dropped.
//
// Every time a poll takes is wall clock, with no monotonic reading, which
// stands still while the machine sleeps and would rate a night's counters
// over a few seconds.
func pollAt(ctx context.Context, pool *api.Pool, t *tracker, now time.Time) *Snapshot {
	now = now.Round(0)
	snap := &Snapshot{At: now}
	var directs, balancers []*api.Client
	for _, c := range pool.Clients() {
		if t.balancer(c.PrimaryEndpoint(), now) {
			balancers = append(balancers, c)
		} else {
			directs = append(directs, c)
		}
	}

	// Until wg.Wait, only the balancer pass uses t.
	var balanced []PodSnapshot
	pods := make([]PodSnapshot, len(directs))
	answered := make([][]string, len(directs))
	var wg sync.WaitGroup
	if len(balancers) > 0 {
		wg.Go(func() { balanced = pollBalanced(ctx, balancers, t, t.direct, now) })
	}
	for i, c := range directs {
		wg.Go(func() { pods[i], answered[i] = pollDirect(ctx, c) })
	}
	wg.Wait()

	direct := make(map[string]bool) // instances shown as a direct unit this tick
	var moved []*api.Client
	for i, c := range directs {
		endpoint := c.PrimaryEndpoint()
		for _, id := range answered[i] {
			t.saw(endpoint, id, now)
		}
		if len(answered[i]) > 1 {
			moved = append(moved, c)
			continue
		}
		pod := pods[i]
		if pod.Instance == "" && pod.Err != "" {
			pod.Instance = t.lastSeen(endpoint, now) // no reply: still that instance's unit
		}
		snap.Pods = append(snap.Pods, pod)
		if pod.Instance != "" {
			direct[pod.Instance] = true
		}
	}
	if len(moved) > 0 && ctx.Err() == nil {
		skip := maps.Clone(direct)
		for _, u := range balanced {
			skip[u.Instance] = true
		}
		balanced = append(balanced, pollBalanced(ctx, moved, t, skip, now)...)
	}
	for _, u := range balanced {
		if !direct[u.Instance] {
			snap.Pods = append(snap.Pods, u)
		}
	}
	for _, c := range slices.Concat(balancers, moved) {
		if snap.Behind == nil {
			snap.Behind = make(map[string]int)
		}
		snap.Behind[c.PrimaryEndpoint()] = t.behind(c.PrimaryEndpoint(), now)
	}
	t.direct = direct

	if t.kubernetes {
		for i := range snap.Pods {
			p := &snap.Pods[i]
			if p.Balanced && p.Instance != "" {
				p.Label = p.Instance
			} else {
				// After the requests: PodName may name the instance that just answered.
				p.Label = pool.PodName(p.Via)
			}
		}
		dedupeLabels(snap.Pods)
		slices.SortFunc(snap.Pods, func(a, b PodSnapshot) int { return strings.Compare(a.Key, b.Key) })
	} else {
		t.label(snap.Pods) // and lists them by number
	}
	t.prune(now)
	return snap
}

// pollDirect lists one endpoint's workers on a new connection, so that a
// balancer picking per connection is seen sending it elsewhere, then fetches
// every worker concurrently. The endpoint is a direct unit: its key and the
// endpoint it was reached through are the same, and its instance is the one
// that answered the list. A failed or timed-out list marks the unit errored.
// A worker that answers 404 was removed since the list and is skipped; any
// other fetch error also marks the unit errored, naming the worker, since its
// state is unknown, not absent. answered is the distinct instances that
// replied to the list or with a sample, sorted.
//
// ponytail: no per-pod request limit; add a semaphore of ~4 if TIME_WAIT
// build-up or tick latency shows up.
func pollDirect(ctx context.Context, c *api.Client) (pod PodSnapshot, answered []string) {
	endpoint := c.PrimaryEndpoint()
	list, instance, err := c.ListWorkersFresh(ctx)
	pod = PodSnapshot{Key: endpoint, Via: endpoint, Instance: instance}
	if instance != "" {
		answered = append(answered, instance)
	}
	if err != nil {
		pod.Err = err.Error()
		return pod, answered
	}

	samples := make([]*Sample, len(list))
	errs := make([]error, len(list))
	var wg sync.WaitGroup
	for i, w := range list {
		wg.Go(func() {
			d, err := c.GetWorkerCtx(ctx, url.PathEscape(w.Name))
			if err != nil {
				errs[i] = err
				return
			}
			samples[i] = &Sample{At: time.Now().Round(0), WorkerDetails: *d}
		})
	}
	wg.Wait()

	pod.Workers = make(map[string]*Sample, len(list))
	for i, w := range list {
		switch {
		case errs[i] == nil:
			pod.Workers[w.Name] = samples[i]
			if samples[i].Instance != "" {
				answered = append(answered, samples[i].Instance)
			}
		case isNotFound(errs[i]):
			// removed since the list: skip
		case pod.Err == "":
			pod.Err = "worker " + w.Name + ": " + errs[i].Error()
		}
	}
	slices.Sort(answered)
	return pod, slices.Compact(answered)
}

// dedupeLabels makes pod labels distinct within a snapshot: pods sharing a
// label fall back to the host:port of the endpoint they were reached through
// (their key when none is set), and pods still sharing one to the full URL.
func dedupeLabels(pods []PodSnapshot) {
	address := func(p PodSnapshot) string { return cmp.Or(p.Via, p.Key) }
	fallbacks := []func(PodSnapshot) string{
		func(p PodSnapshot) string { return hostPort(address(p)) },
		address,
	}
	for _, fallback := range fallbacks {
		count := make(map[string]int, len(pods))
		for _, p := range pods {
			count[p.Label]++
		}
		for i := range pods {
			if count[pods[i].Label] > 1 {
				pods[i].Label = fallback(pods[i])
			}
		}
	}
}

// hostPort returns the endpoint URL's host:port, or the endpoint unchanged
// when it has no host.
func hostPort(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}
