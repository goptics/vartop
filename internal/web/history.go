// Package web serves vartop's browser UI: a poller that samples every pod of
// the selected server, an in-memory history of those samples, and the HTTP
// handlers that render them.
package web

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

// PodSnapshot is everything one pod (endpoint) reported in a single poll.
type PodSnapshot struct {
	Endpoint     string             // pod key (Client.PrimaryEndpoint())
	Label        string             // display label captured at poll time
	Err          string             // "" = healthy
	LoadBalanced int                // instances seen behind this endpoint (>1 = LB)
	Workers      map[string]*Sample // by worker name
}

// Snapshot is one full poll across all pods. It is immutable once passed to
// History.Append.
type Snapshot struct {
	At   time.Time
	Pods []PodSnapshot // sorted by Endpoint
}

// History keeps the last hour of snapshots in a ring and derives per-worker
// rates as snapshots arrive. It is safe for one writer and many readers.
type History struct {
	interval time.Duration

	mu    sync.RWMutex
	ring  []*Snapshot
	next  int                // ring index the next snapshot is written to
	count int                // snapshots currently held
	last  map[string]*Sample // latest sample per endpoint+"\x00"+worker
}

// NewHistory sizes the ring to hold an hour of polls: ceil(time.Hour/interval)
// slots. interval must be positive (config.Server.PollInterval always is).
func NewHistory(interval time.Duration) *History {
	return &History{
		interval: interval,
		ring:     make([]*Snapshot, int((time.Hour+interval-1)/interval)),
		last:     make(map[string]*Sample),
	}
}

// Interval is the poll interval the history was sized for.
func (h *History) Interval() time.Duration { return h.interval }

// Append fills the rate fields of every sample in s from the previous sample of
// the same worker on the same pod, then publishes s, evicting the oldest
// snapshot when the ring is full. s must not be modified afterwards.
func (h *History) Append(s *Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, pod := range s.Pods {
		for name, sm := range pod.Workers {
			key := pod.Endpoint + "\x00" + name
			sm.CompletedRate, sm.FailedRate, sm.RateOK = rate(h.last[key], sm, h.interval)
			// ponytail: never pruned; entries for vanished pods are tiny and
			// fall out of rate()'s 3x window anyway.
			h.last[key] = sm
		}
	}

	h.ring[h.next] = s
	h.next = (h.next + 1) % len(h.ring)
	if h.count < len(h.ring) {
		h.count++
	}
}

// Snapshots returns the held snapshots, oldest first. The slice is a copy;
// the snapshots themselves are shared and must be treated as read-only.
func (h *History) Snapshots() []*Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]*Snapshot, h.count)
	start := (h.next - h.count + len(h.ring)) % len(h.ring)
	for i := range out {
		out[i] = h.ring[(start+i)%len(h.ring)]
	}
	return out
}

// Latest returns the newest snapshot, or nil when the history is empty.
func (h *History) Latest() *Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.count == 0 {
		return nil
	}
	return h.ring[(h.next-1+len(h.ring))%len(h.ring)]
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
