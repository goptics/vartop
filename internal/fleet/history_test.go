package fleet

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/fleet/fleettest"
)

func TestRate(t *testing.T) {
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	mk := func(at time.Duration, inst string, completed, failed uint64) *Sample {
		s := &Sample{At: t0.Add(at)}
		s.Instance = inst
		s.Metrics.Completed, s.Metrics.Failed = completed, failed
		return s
	}
	cases := []struct {
		name         string
		prev, cur    *Sample
		wantC, wantF float64
		ok           bool
	}{
		{"normal", mk(0, "a", 100, 4), mk(5*time.Second, "a", 150, 9), 10, 1, true},
		{"no prev", nil, mk(5*time.Second, "a", 1, 0), 0, 0, false},
		{"counter reset", mk(0, "a", 100, 4), mk(5*time.Second, "a", 3, 0), 0, 0, false},
		{"failed counter reset", mk(0, "a", 100, 4), mk(5*time.Second, "a", 150, 1), 0, 0, false},
		{"instance changed", mk(0, "a", 100, 4), mk(5*time.Second, "b", 150, 4), 0, 0, false},
		{"dt too small", mk(0, "a", 100, 4), mk(2*time.Second, "a", 110, 4), 0, 0, false},
		{"prev stale", mk(0, "a", 100, 4), mk(16*time.Second, "a", 200, 4), 0, 0, false},
		{"dt at lower bound", mk(0, "a", 100, 4), mk(2500*time.Millisecond, "a", 105, 4), 2, 0, true},
		{"dt at upper bound", mk(0, "a", 100, 4), mk(15*time.Second, "a", 130, 4), 2, 0, true},
	}
	for _, tc := range cases {
		c, f, ok := rate(tc.prev, tc.cur, iv)
		if ok != tc.ok || (ok && (c != tc.wantC || f != tc.wantF)) {
			t.Errorf("%s: got (%v,%v,%v) want (%v,%v,%v)", tc.name, c, f, ok, tc.wantC, tc.wantF, tc.ok)
		}
	}
}

// TestRateAcrossSleepIsGap: the monotonic clock stands still while a laptop
// sleeps, so a pair of poll times that kept it would be ~5s apart across 8
// hours and rate a huge spike. Wall-clock times (see TestPollTimesAreWallClock)
// are 8h apart: a gap, never a rate, and never "early".
func TestRateAcrossSleepIsGap(t *testing.T) {
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	prev := &Sample{At: t0, RateOK: true}
	cur := &Sample{At: t0.Add(8 * time.Hour)}
	cur.Metrics.Completed = 1_000_000
	if c, f, ok := rate(prev, cur, iv); ok {
		t.Errorf("rate across an 8h sleep = (%v, %v, true), want a gap", c, f)
	}
	if early(prev, cur, iv) {
		t.Error("a sample 8h after the previous one is early, want a regular one")
	}
}

// TestPollTimesAreWallClock: Go's monotonic reading does not advance during
// suspend, so every time the poller takes (the tick and each sample, direct
// or behind a balancer) must be wall clock, with no monotonic reading.
func TestPollTimesAreWallClock(t *testing.T) {
	_, direct := fleettest.NewVarmq(t, "d", map[string]*fleettest.Worker{"w": {Status: "Running", Concurrency: 1}})
	_, srvs := fleettest.NewInstances(t, "a", "b")
	lb := fleettest.NewRoundRobinLB(t, srvs...)
	dpool := fleettest.StaticPool(t, direct.URL)
	bpool, btr := lbPoll(t, lb.URL)

	samples := 0
	check := func(what string, s *Snapshot) {
		t.Helper()
		if s.At != s.At.Round(0) {
			t.Errorf("%s: Snapshot.At %v carries a monotonic reading", what, s.At)
		}
		for _, p := range s.Pods {
			for name, sm := range p.Workers {
				samples++
				if sm.At != sm.At.Round(0) {
					t.Errorf("%s: %s/%s Sample.At %v carries a monotonic reading", what, p.Key, name, sm.At)
				}
			}
		}
	}
	check("direct", pollOnce(context.Background(), dpool, newTracker(time.Second, false)))
	check("direct pollAt", pollAt(context.Background(), dpool, newTracker(time.Second, false), time.Now()))
	check("balanced", pollUntil(t, bpool, btr, 5, "both units", hasKeys("#a", "#b")))
	if samples != 4 {
		t.Errorf("checked %d samples, want 4: the direct unit's in two polls and both balanced units'", samples)
	}
}

func TestHistoryCapacityAndRollOff(t *testing.T) {
	h := newTestHistory(time.Minute) // 60 slots
	for i := range 70 {
		h.Append(&Snapshot{At: time.Unix(int64(i*60), 0)})
	}
	s := h.Snapshots()
	if len(s) != 60 || s[0].At.Unix() != 600 || h.Latest().At.Unix() != 69*60 {
		t.Fatalf("len=%d first=%v", len(s), s[0].At)
	}
}

func TestHistoryEmptyAndInterval(t *testing.T) {
	h := newTestHistory(7 * time.Second)
	if h.Latest() != nil {
		t.Errorf("Latest on empty history = %v, want nil", h.Latest())
	}
	if got := h.Snapshots(); len(got) != 0 {
		t.Errorf("Snapshots on empty history has %d entries", len(got))
	}
	if h.Interval() != 7*time.Second {
		t.Errorf("Interval = %v, want 7s", h.Interval())
	}
}

func TestHistoryCapacityRoundsUp(t *testing.T) {
	h := newTestHistory(7 * time.Second) // ceil(3600/7) = 515 slots
	for i := range 600 {
		h.Append(&Snapshot{At: time.Unix(int64(i*7), 0)})
	}
	if got := len(h.Snapshots()); got != 515 {
		t.Fatalf("len = %d, want 515", got)
	}
}

func TestSnapshotsReturnsCopy(t *testing.T) {
	h := newTestHistory(time.Minute)
	h.Append(&Snapshot{At: time.Unix(1, 0)})
	h.Append(&Snapshot{At: time.Unix(2, 0)})
	got := h.Snapshots()
	got[0] = nil
	if again := h.Snapshots(); again[0] == nil || again[0].At.Unix() != 1 {
		t.Fatalf("mutating the returned slice changed the history: %v", again[0])
	}
}

func TestAppendFillsRatesAcrossSnapshots(t *testing.T) {
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(iv)
	h := newTestHistory(iv)

	sample := func(name string, at time.Time, completed, failed uint64) *Sample {
		s := &Sample{At: at}
		s.Name = name
		s.Metrics.Completed, s.Metrics.Failed = completed, failed
		return s
	}
	// p1 runs workers "w" and "x" with different counters, so each must get its
	// own rate (the key includes the worker name). p0 shows up only in the
	// second snapshot with the same worker names, and is listed before p1 so that
	// when Append reaches it p1's t0 samples are still the stored previous ones:
	// borrowing them would give a valid 5s gap and RateOK==true (the key
	// includes the unit key).
	first := &Snapshot{At: t0, Pods: []PodSnapshot{
		{Key: "p1", Workers: map[string]*Sample{
			"w": sample("w", t0, 100, 0),
			"x": sample("x", t0, 1000, 5),
		}},
	}}
	second := &Snapshot{At: t1, Pods: []PodSnapshot{
		{Key: "p0", Workers: map[string]*Sample{
			"w": sample("w", t1, 5000, 50),
			"x": sample("x", t1, 6000, 60),
		}},
		{Key: "p1", Workers: map[string]*Sample{
			"w": sample("w", t1, 150, 10),
			"x": sample("x", t1, 1020, 5),
		}},
	}}

	h.Append(first)
	for name, s := range first.Pods[0].Workers {
		if s.RateOK {
			t.Errorf("first sample of p1/%s has RateOK", name)
		}
	}
	h.Append(second)

	for _, tc := range []struct {
		worker       string
		wantC, wantF float64
	}{{"w", 10, 2}, {"x", 4, 0}} {
		s := second.Pods[1].Workers[tc.worker]
		if !s.RateOK || s.CompletedRate != tc.wantC || s.FailedRate != tc.wantF {
			t.Errorf("p1/%s: RateOK=%v completed=%v failed=%v, want true %v %v",
				tc.worker, s.RateOK, s.CompletedRate, s.FailedRate, tc.wantC, tc.wantF)
		}
	}
	for name, s := range second.Pods[0].Workers {
		if s.RateOK {
			t.Errorf("p0/%s appeared only in the second snapshot but RateOK=true (completed=%v)", name, s.CompletedRate)
		}
	}
	if h.Latest() != second {
		t.Error("Latest is not the second snapshot")
	}
}

// TestAppendCarriesRateOverEarlyPoll: a poll under half an interval after
// the last one (PollNow after an action) repeats the last rate instead of
// leaving a gap, and the next regular tick is rated against the sample
// before it.
func TestAppendCarriesRateOverEarlyPoll(t *testing.T) {
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	sample := func(at time.Duration, completed, failed uint64) *Sample {
		s := &Sample{At: t0.Add(at)}
		s.Instance = "i"
		s.Metrics.Completed, s.Metrics.Failed = completed, failed
		return s
	}
	appendTo := func(h *testHistory, samples ...*Sample) {
		for _, s := range samples {
			h.Append(&Snapshot{At: s.At, Pods: []PodSnapshot{{Key: "p", Workers: map[string]*Sample{"w": s}}}})
		}
	}

	s0 := sample(0, 100, 0)
	s1 := sample(5*time.Second, 150, 5) // 10/s, 1/s
	early := sample(5008*time.Millisecond, 151, 5)
	s2 := sample(10*time.Second, 250, 15) // against s1: 20/s, 2/s
	appendTo(newTestHistory(iv), s0, s1, early, s2)
	for _, c := range []struct {
		name         string
		s            *Sample
		wantC, wantF float64
	}{{"t0+5s", s1, 10, 1}, {"early t0+5.008s", early, 10, 1}, {"t0+10s", s2, 20, 2}} {
		if !c.s.RateOK || c.s.CompletedRate != c.wantC || c.s.FailedRate != c.wantF {
			t.Errorf("%s: RateOK=%v completed=%v failed=%v, want true %v %v",
				c.name, c.s.RateOK, c.s.CompletedRate, c.s.FailedRate, c.wantC, c.wantF)
		}
	}

	// With no rate to repeat, an early poll is still a gap.
	first, soon := sample(0, 100, 0), sample(8*time.Millisecond, 101, 0)
	appendTo(newTestHistory(iv), first, soon)
	if soon.RateOK {
		t.Errorf("early poll after an unrated sample has RateOK (completed=%v)", soon.CompletedRate)
	}
	// So is one whose counter went back (a restart), which becomes the base:
	// rated against r1 instead, r2's counter would be back too, a gap.
	r0, r1, reset, r2 := sample(0, 100, 0), sample(5*time.Second, 150, 0), sample(5008*time.Millisecond, 2, 0), sample(10*time.Second, 52, 0)
	appendTo(newTestHistory(iv), r0, r1, reset, r2)
	if reset.RateOK || !r2.RateOK {
		t.Errorf("reset: RateOK=%v, next: RateOK=%v; want a gap, then a rate against the reset", reset.RateOK, r2.RateOK)
	}
}

func TestHistoryConcurrentAppendAndRead(t *testing.T) {
	const appends = 1000
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	h := newTestHistory(iv)

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := range appends {
			at := t0.Add(time.Duration(i) * iv)
			s := &Sample{At: at}
			s.Name = "w"
			s.Metrics.Completed = uint64(i * 10)
			h.Append(&Snapshot{At: at, Pods: []PodSnapshot{
				{Key: "p", Workers: map[string]*Sample{"w": s}},
			}})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var lastSeen time.Time
		check := func() {
			snaps := h.Snapshots()
			for i := 1; i < len(snaps); i++ {
				if !snaps[i].At.After(snaps[i-1].At) {
					t.Errorf("snapshots out of order at %d: %v then %v", i, snaps[i-1].At, snaps[i].At)
					return
				}
			}
			for _, sn := range snaps {
				// Published samples are read while the appender keeps going.
				if s := sn.Pods[0].Workers["w"]; s.RateOK && s.CompletedRate != 2 {
					t.Errorf("CompletedRate = %v, want 2", s.CompletedRate)
					return
				}
			}
			if latest := h.Latest(); latest != nil {
				if latest.At.Before(lastSeen) {
					t.Errorf("Latest went backwards: %v after %v", latest.At, lastSeen)
					return
				}
				lastSeen = latest.At
			}
		}
		for {
			select {
			case <-done:
				return
			default:
				check()
			}
		}
	}()

	wg.Wait()

	snaps := h.Snapshots()
	if want := int(time.Hour / iv); len(snaps) != want {
		t.Fatalf("len = %d, want %d", len(snaps), want)
	}
	if want := t0.Add((appends - 1) * iv); !h.Latest().At.Equal(want) {
		t.Fatalf("Latest = %v, want %v", h.Latest().At, want)
	}
	if got := snaps[len(snaps)-1].Pods[0].Workers["w"]; !got.RateOK {
		t.Error("last sample missing a rate after 1000 evenly spaced appends")
	}
}

// Units reached through one balancer share a Via; their series must still be
// each one's own.
func TestAppendKeysRatesByUnitKey(t *testing.T) {
	iv := 5 * time.Second
	h := newTestHistory(iv)
	t0 := time.Unix(1000, 0)
	unit := func(key string, at time.Time, completed uint64) PodSnapshot {
		sm := &Sample{At: at}
		sm.Instance, sm.Metrics.Completed = key[1:], completed
		return PodSnapshot{Key: key, Via: "http://lb", Instance: key[1:], Balanced: true, Workers: map[string]*Sample{"w": sm}}
	}
	first := &Snapshot{At: t0, Pods: []PodSnapshot{unit("#a", t0, 10), unit("#b", t0, 100)}}
	second := &Snapshot{At: t0.Add(iv), Pods: []PodSnapshot{unit("#a", t0.Add(iv), 20), unit("#b", t0.Add(iv), 130)}}
	h.Append(first)
	h.Append(second)

	for i, want := range []float64{10 / iv.Seconds(), 30 / iv.Seconds()} {
		sm := second.Pods[i].Workers["w"]
		if !sm.RateOK || sm.CompletedRate != want {
			t.Errorf("%s: RateOK = %v, CompletedRate = %v, want %v", second.Pods[i].Key, sm.RateOK, sm.CompletedRate, want)
		}
	}
}

// rateRun is a run of polls that exercises every rating rule: two units with
// two workers each, a regular tick, an early poll, a counter reset, another
// instance answering, a unit that shows up late and a tick after a gap.
// Each call builds fresh samples, so runs never share any.
func rateRun() []*Snapshot {
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	sample := func(at time.Duration, inst string, completed, failed uint64) *Sample {
		s := &Sample{At: t0.Add(at)}
		s.Instance = inst
		s.Metrics.Completed, s.Metrics.Failed = completed, failed
		return s
	}
	type w = map[string]*Sample
	snap := func(at time.Duration, pods ...PodSnapshot) *Snapshot {
		return &Snapshot{At: t0.Add(at), Pods: pods}
	}
	return []*Snapshot{
		snap(0,
			PodSnapshot{Key: "p", Workers: w{"a": sample(0, "i", 100, 0), "b": sample(0, "i", 10, 1)}}),
		snap(iv,
			PodSnapshot{Key: "p", Workers: w{"a": sample(iv, "i", 150, 5), "b": sample(iv, "i", 20, 1)}}),
		snap(iv+8*time.Millisecond, // early: PollNow after an action
			PodSnapshot{Key: "p", Workers: w{"a": sample(iv+8*time.Millisecond, "i", 151, 5), "b": sample(iv+8*time.Millisecond, "i", 3, 0)}}),
		snap(2*iv,
			PodSnapshot{Key: "p", Workers: w{"a": sample(2*iv, "j", 160, 5), "b": sample(2*iv, "i", 13, 0)}},
			PodSnapshot{Key: "q", Workers: w{"a": sample(2*iv, "k", 7, 0)}}),
		snap(3*iv,
			PodSnapshot{Key: "p", Workers: w{"a": sample(3*iv, "j", 210, 6), "b": sample(3*iv, "i", 33, 0)}},
			PodSnapshot{Key: "q", Workers: w{"a": sample(3*iv, "k", 17, 0)}}),
		snap(7*iv, // after a gap of more than three intervals
			PodSnapshot{Key: "p", Workers: w{"a": sample(7*iv, "j", 400, 6), "b": sample(7*iv, "i", 40, 0)}}),
		snap(8*iv,
			PodSnapshot{Key: "p", Workers: w{"a": sample(8*iv, "j", 450, 7), "b": sample(8*iv, "i", 45, 0)}}),
	}
}

// rates lists the rate fields of every sample of snaps, in a stable order.
func rates(snaps []*Snapshot) []string {
	var out []string
	for i, s := range snaps {
		for _, p := range s.Pods {
			for _, name := range slices.Sorted(maps.Keys(p.Workers)) {
				sm := p.Workers[name]
				out = append(out, fmt.Sprintf("%d %s/%s: %v %v %v", i, p.Key, name, sm.RateOK, sm.CompletedRate, sm.FailedRate))
			}
		}
	}
	return out
}

// TestRaterSeed: a rater seeded with a snapshot rated before (as a restart
// seeds it with the latest stored one) rates the next snapshot as a rater
// that saw every snapshot does, early polls included.
func TestRaterSeed(t *testing.T) {
	iv := 5 * time.Second
	for _, seedAt := range []int{0, 1, 3, 4} {
		unbroken := rateRun()
		r := NewRater(iv)
		for _, s := range unbroken {
			r.Rate(s)
		}

		resumed := rateRun()
		seeded := NewRater(iv)
		seeded.Seed(unbroken[seedAt]) // as stored: rated, never rated again
		for _, s := range resumed[seedAt+1:] {
			seeded.Rate(s)
		}

		want, got := rates(unbroken[seedAt+1:]), rates(resumed[seedAt+1:])
		if !slices.Equal(got, want) {
			t.Errorf("seeded with snapshot %d, rates:\n%s\nwant an unbroken run's:\n%s", seedAt, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	// Seeding rates nothing.
	s := rateRun()[1]
	NewRater(iv).Seed(s)
	for _, line := range rates([]*Snapshot{s}) {
		if strings.Contains(line, "true") {
			t.Errorf("Seed rated a sample: %s", line)
		}
	}
}

// TestRingStoresWithoutRating: a Ring keeps the rates a snapshot comes with.
func TestRingStoresWithoutRating(t *testing.T) {
	iv := 5 * time.Second
	run := rateRun()
	ring := NewRing(iv)
	for _, s := range run {
		ring.Append(s)
	}
	for _, line := range rates(ring.Snapshots()) {
		if strings.Contains(line, "true") {
			t.Errorf("Ring.Append rated a sample: %s", line)
		}
	}
	if ring.Latest() != run[len(run)-1] || ring.Interval() != iv {
		t.Errorf("Latest = %p, Interval = %v; want the last appended and %v", ring.Latest(), ring.Interval(), iv)
	}
}

// testHistory is a Ring that rates what it stores, as a collector rates
// each snapshot before its sink keeps it.
type testHistory struct {
	*Ring
	rater *Rater
}

func newTestHistory(interval time.Duration) *testHistory {
	return &testHistory{Ring: NewRing(interval), rater: NewRater(interval)}
}

// Append rates s, then publishes it.
func (h *testHistory) Append(s *Snapshot) {
	h.rater.Rate(s)
	h.Ring.Append(s)
}
