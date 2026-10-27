// Package fleet is vartop's model of a varmq fleet, with no HTTP of its own:
// a collector that polls every unit of a server (its direct endpoints and
// the instances behind its load balancers) into snapshots, the per-worker
// rates derived from them, the view models vartop web renders, and the action
// core that sends a worker action to the units in scope.
package fleet

import (
	"sync"
	"time"

	"github.com/goptics/vartop/internal/api"
)

// Sample is one worker as seen by one pod at one poll.
type Sample struct {
	At                time.Time // when this worker's GetWorker response arrived
	api.WorkerDetails           // includes Instance
	CompletedRate     float64   // per second; valid only if RateOK
	FailedRate        float64
	RateOK            bool
}

// PodSnapshot is everything one unit reported in a single poll. A unit is one
// varmq instance: a direct endpoint, or an instance found behind a balancer.
type PodSnapshot struct {
	Key       string             // unit key: the endpoint URL of a direct unit, "#"+instance ID of a balanced one
	Via       string             // endpoint the unit was reached through (Client.PrimaryEndpoint())
	Instance  string             // varmq instance ID that answered, "" when none did
	Label     string             // display label captured at poll time
	Balanced  bool               // an instance found behind a load balancer
	Err       string             // "" = healthy
	Workers   map[string]*Sample // by worker name
	Unreached []string           // sorted names of workers on this unit's list that got no answer
}

// Tip is the hover text of the unit's label: the instance and the address it
// was reached through when both are known, else whichever is, else its key.
func (p PodSnapshot) Tip() string {
	switch {
	case p.Instance != "" && p.Via != "":
		return p.Instance + " via " + p.Via
	case p.Via != "":
		return p.Via
	}
	return p.Key
}

// Snapshot is one full poll across all units. It is immutable once
// published (see Ring.Append).
type Snapshot struct {
	At time.Time
	// Pods are in the order the pages list them: by label number
	// (instance-N) off Kubernetes, by Key on it.
	Pods []PodSnapshot
	// Behind maps each balancer endpoint to the distinct instances that
	// answered through it within the lifecycle window (tracker.window), not
	// only this tick: one gone less than a window ago still counts.
	Behind map[string]int
	// Err says why the server could not be polled at all (its units not yet
	// discovered, discovery failing); "" for a poll that ran, whatever its
	// units' errors.
	Err string
	// Source is SourceOf the server's config it was polled under: an action
	// goes out only on a snapshot of the config it reads, never to units
	// the server's previous endpoints found.
	Source string
}

// Rater derives per-worker rates as snapshots arrive. It belongs to the one
// goroutine that rates, and keeps the samples it rates against.
type Rater struct {
	interval time.Duration
	last     map[string]*Sample // latest sample per unit key+"\x00"+worker
}

// NewRater returns a rater for a server polled every interval.
func NewRater(interval time.Duration) *Rater {
	return &Rater{interval: interval, last: make(map[string]*Sample)}
}

// Rate fills the rate fields of every sample in s from the previous sample
// of the same worker on the same unit. s must not be published yet: its
// samples are written.
//
// A sample that comes early (see early), as PollNow's after an action does,
// repeats the previous sample's rate instead of being a gap, and does not
// replace it as the base: the next regular tick is rated against it.
func (r *Rater) Rate(s *Snapshot) {
	for _, pod := range s.Pods {
		for name, sm := range pod.Workers {
			key := pod.Key + "\x00" + name
			prev := r.last[key]
			if early(prev, sm, r.interval) {
				sm.CompletedRate, sm.FailedRate, sm.RateOK = prev.CompletedRate, prev.FailedRate, true
				continue
			}
			sm.CompletedRate, sm.FailedRate, sm.RateOK = rate(prev, sm, r.interval)
			// ponytail: never pruned; entries for vanished pods are tiny and
			// fall out of rate()'s 3x window anyway.
			r.last[key] = sm
		}
	}
}

// Seed makes the samples of s, a snapshot rated before (the latest one
// stored, after a restart), the ones the next Rate rates against, without
// rating them. Its samples are only read.
//
// s becomes the base even when it came early (see early), which an
// unbroken run would have rated against the regular tick before it: the
// first rate after a restart within 3 intervals of an early snapshot can
// then be over a slightly different window than the unbroken run's.
func (r *Rater) Seed(s *Snapshot) {
	for _, pod := range s.Pods {
		for name, sm := range pod.Workers {
			r.last[pod.Key+"\x00"+name] = sm
		}
	}
}

// Ring keeps the last hour of snapshots, as they come: it rates nothing. It
// is safe for one writer and many readers.
type Ring struct {
	interval time.Duration

	mu    sync.RWMutex
	ring  []*Snapshot
	next  int // ring index the next snapshot is written to
	count int // snapshots currently held
}

// NewRing sizes the ring to hold an hour of polls: ceil(time.Hour/interval)
// slots. interval must be positive (config.Server.PollInterval always is).
func NewRing(interval time.Duration) *Ring {
	return &Ring{
		interval: interval,
		ring:     make([]*Snapshot, int((time.Hour+interval-1)/interval)),
	}
}

// Interval is the poll interval the ring was sized for.
func (r *Ring) Interval() time.Duration { return r.interval }

// Append publishes s, evicting the oldest snapshot when the ring is full. s
// must not be modified afterwards.
func (r *Ring) Append(s *Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ring[r.next] = s
	r.next = (r.next + 1) % len(r.ring)
	if r.count < len(r.ring) {
		r.count++
	}
}

// Snapshots returns the held snapshots, oldest first. The slice is a copy;
// the snapshots themselves are shared and must be treated as read-only.
func (r *Ring) Snapshots() []*Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Snapshot, r.count)
	start := (r.next - r.count + len(r.ring)) % len(r.ring)
	for i := range out {
		out[i] = r.ring[(start+i)%len(r.ring)]
	}
	return out
}

// Latest returns the newest snapshot, or nil when the ring is empty.
func (r *Ring) Latest() *Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.count == 0 {
		return nil
	}
	return r.ring[(r.next-1+len(r.ring))%len(r.ring)]
}

// early reports whether cur came under half an interval after prev, which
// has a rate to repeat, from the same instance with no counter gone back.
// A restart or another instance is never early: rate makes it a gap.
func early(prev, cur *Sample, interval time.Duration) bool {
	return prev != nil && prev.RateOK && cur.At.Sub(prev.At) < interval/2 &&
		cur.Instance == prev.Instance &&
		cur.Metrics.Completed >= prev.Metrics.Completed && cur.Metrics.Failed >= prev.Metrics.Failed
}

// rate derives per-second completed and failed rates between two samples of
// the same worker on the same pod. ok is false when the pair can't be compared
// reliably: no previous sample, a counter went backwards (restart), a different
// varmq instance answered, or the gap is under half or over three intervals.
func rate(prev, cur *Sample, interval time.Duration) (completed, failed float64, ok bool) {
	if prev == nil {
		return 0, 0, false
	}
	if cur.Metrics.Completed < prev.Metrics.Completed || cur.Metrics.Failed < prev.Metrics.Failed {
		return 0, 0, false
	}
	if cur.Instance != prev.Instance {
		return 0, 0, false
	}
	dt := cur.At.Sub(prev.At)
	if dt < interval/2 || dt > 3*interval {
		return 0, 0, false
	}
	secs := dt.Seconds()
	return float64(cur.Metrics.Completed-prev.Metrics.Completed) / secs,
		float64(cur.Metrics.Failed-prev.Metrics.Failed) / secs,
		true
}
