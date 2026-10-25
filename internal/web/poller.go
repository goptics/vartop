package web

import (
	"context"
	"errors"
	"net/http"
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
	snap := pollOnce(ctx, s.pool)
	if s.ctx.Err() != nil {
		return false
	}
	s.History.Append(snap)
	return true
}

// pollOnce polls every pod in the pool concurrently, within ctx, and returns
// the snapshot with At set to when the poll started. Every Sample in it is
// freshly allocated, as History.Append requires.
func pollOnce(ctx context.Context, pool *api.Pool) *Snapshot {
	snap := &Snapshot{At: time.Now()}
	clients := pool.Clients()
	snap.Pods = make([]PodSnapshot, len(clients))

	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() { snap.Pods[i] = pollPod(ctx, c) })
	}
	wg.Wait()

	lb := pool.LoadBalanced()
	for i := range snap.Pods {
		p := &snap.Pods[i]
		// After the requests: PodName may name the instance that just answered.
		p.Label = pool.PodName(p.Endpoint)
		p.LoadBalanced = lb[p.Endpoint]
	}
	dedupeLabels(snap.Pods)
	slices.SortFunc(snap.Pods, func(a, b PodSnapshot) int { return strings.Compare(a.Endpoint, b.Endpoint) })
	return snap
}

// pollPod lists one pod's workers, then fetches every worker concurrently. A
// failed or timed-out list marks the pod errored. A worker that answers 404
// was removed since the list and is skipped; any other fetch error also marks
// the pod errored, since that worker's state is unknown, not absent.
//
// ponytail: no per-pod request limit; add a semaphore of ~4 if TIME_WAIT
// build-up or tick latency shows up.
func pollPod(ctx context.Context, c *api.Client) PodSnapshot {
	pod := PodSnapshot{Endpoint: c.PrimaryEndpoint()}
	list, err := c.ListWorkersCtx(ctx)
	if err != nil {
		pod.Err = err.Error()
		return pod
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
		var apiErr *api.APIError
		switch {
		case errs[i] == nil:
			pod.Workers[w.Name] = samples[i]
		case errors.As(errs[i], &apiErr) && apiErr.StatusCode == http.StatusNotFound:
			// removed since the list: skip
		case pod.Err == "":
			pod.Err = errs[i].Error()
		}
	}
	return pod
}

// dedupeLabels makes pod labels distinct within a snapshot: pods sharing a
// label fall back to their endpoint's host:port, and pods still sharing one
// to the full endpoint URL.
func dedupeLabels(pods []PodSnapshot) {
	fallbacks := []func(endpoint string) string{hostPort, func(endpoint string) string { return endpoint }}
	for _, fallback := range fallbacks {
		count := make(map[string]int, len(pods))
		for _, p := range pods {
			count[p.Label]++
		}
		for i := range pods {
			if count[pods[i].Label] > 1 {
				pods[i].Label = fallback(pods[i].Endpoint)
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
