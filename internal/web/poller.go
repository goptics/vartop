package web

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

// Session polls one server: a single background goroutine fans out to every
// pod in the pool each interval and appends a Snapshot to History. That
// goroutine is the History's only writer, so a session's late tick can never
// land in another session's history.
type Session struct {
	Server   *config.Server
	Interval time.Duration
	History  *History

	pool    *api.Pool
	tracker *tracker        // the poller goroutine's
	ctx     context.Context // cancelled by Close
	cancel  context.CancelFunc
	pollNow chan chan struct{} // the poller closes each received channel once its snapshot is appended
}

// NewSession starts polling srv through pool, which the session now owns.
// The first poll starts immediately; each next one starts Interval after the
// previous one ended.
func NewSession(srv *config.Server, pool *api.Pool) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	interval := srv.PollInterval()
	s := &Session{
		Server:   srv,
		Interval: interval,
		History:  NewHistory(interval),
		pool:     pool,
		tracker:  newTracker(interval, srv.Kubernetes != nil),
		ctx:      ctx,
		cancel:   cancel,
		pollNow:  make(chan chan struct{}),
	}
	go s.run()
	return s
}

// Clients returns the pool's current clients, one per pod endpoint.
func (s *Session) Clients() []*api.Client { return s.pool.Clients() }

// PollNow asks the poller for an immediate poll and waits for it. It returns
// true once a snapshot started after the request has been appended, and false
// if ctx ends or the session is closed first. A request made mid-tick waits
// for that tick and then gets a fresh one, so the snapshot reflects anything
// done before the call.
func (s *Session) PollNow(ctx context.Context) bool {
	done := make(chan struct{})
	select {
	case s.pollNow <- done:
	case <-ctx.Done():
		return false
	case <-s.ctx.Done():
		return false
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-s.ctx.Done():
		return false
	}
}

// Close stops polling and closes the pool. It does not wait for the poller:
// a tick in flight has its requests cancelled.
func (s *Session) Close() error {
	s.cancel()
	return s.pool.Close()
}

// run is the poller loop. Ticks run inline and the timer is reset only after
// a tick ends, so a slow tick pushes the next one back instead of overlapping.
func (s *Session) run() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var done chan struct{}
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		case done = <-s.pollNow:
		}
		if !s.tick() {
			return
		}
		if done != nil {
			close(done)
		}
		timer.Reset(s.Interval)
	}
}

// tick polls every pod with a deadline of 0.8 × Interval and appends the
// snapshot. It reports false, appending nothing, once the session is closed.
func (s *Session) tick() bool {
	ctx, cancel := context.WithTimeout(s.ctx, s.Interval*8/10)
	defer cancel()
	snap := pollOnce(ctx, s.pool, s.tracker)
	if s.ctx.Err() != nil {
		return false
	}
	s.History.Append(snap)
	return true
}

// pollOnce polls every unit of the pool within ctx, tracking instances
// across ticks in t, and returns the snapshot with At set to when the poll
// started. Every Sample in it is freshly allocated, as History.Append
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
func pollAt(ctx context.Context, pool *api.Pool, t *tracker, now time.Time) *Snapshot {
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
			samples[i] = &Sample{At: time.Now(), WorkerDetails: *d}
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
