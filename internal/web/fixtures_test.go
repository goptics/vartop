package web

import (
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// Snapshot fixtures, as fleet's view tests build them.

var viewT0 = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// atTick is the timestamp of the n-th 5s poll after viewT0.
func atTick(n int) time.Time { return viewT0.Add(time.Duration(n) * 5 * time.Second) }

// stamp is the clock time the pages show for s.
func stamp(s *fleet.Snapshot) string { return s.At.Local().Format("15:04:05") }

// smp builds a sample with the fields the views read.
func smp(status string, conc, busy, idle, pending int) *fleet.Sample {
	s := &fleet.Sample{}
	s.Status, s.Concurrency, s.NumProcessing, s.NumIdleWorkers, s.NumPending = status, conc, busy, idle, pending
	return s
}

// okPod is a healthy direct unit whose label is its endpoint.
func okPod(endpoint string, workers map[string]*fleet.Sample) fleet.PodSnapshot {
	return fleet.PodSnapshot{Key: endpoint, Via: endpoint, Label: endpoint, Workers: workers}
}

func snap(at time.Time, pods ...fleet.PodSnapshot) *fleet.Snapshot {
	return &fleet.Snapshot{At: at, Pods: pods}
}

// unitKeys returns the snapshot's unit keys in order.
func unitKeys(s *fleet.Snapshot) []string {
	keys := make([]string, len(s.Pods))
	for i, p := range s.Pods {
		keys[i] = p.Key
	}
	return keys
}

// budget3 is the most attempts an action sends through a server's
// balancers to reach 3 instances (fleet's attemptCap(3)).
const budget3 = 18
