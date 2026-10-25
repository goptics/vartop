package web

import (
	"sync"
	"testing"
	"time"
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

func TestHistoryCapacityAndRollOff(t *testing.T) {
	h := NewHistory(time.Minute) // 60 slots
	for i := range 70 {
		h.Append(&Snapshot{At: time.Unix(int64(i*60), 0)})
	}
	s := h.Snapshots()
	if len(s) != 60 || s[0].At.Unix() != 600 || h.Latest().At.Unix() != 69*60 {
		t.Fatalf("len=%d first=%v", len(s), s[0].At)
	}
}

func TestHistoryEmptyAndInterval(t *testing.T) {
	h := NewHistory(7 * time.Second)
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
	h := NewHistory(7 * time.Second) // ceil(3600/7) = 515 slots
	for i := range 600 {
		h.Append(&Snapshot{At: time.Unix(int64(i*7), 0)})
	}
	if got := len(h.Snapshots()); got != 515 {
		t.Fatalf("len = %d, want 515", got)
	}
}

func TestSnapshotsReturnsCopy(t *testing.T) {
	h := NewHistory(time.Minute)
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
	h := NewHistory(iv)

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
	// includes the endpoint).
	first := &Snapshot{At: t0, Pods: []PodSnapshot{
		{Endpoint: "p1", Workers: map[string]*Sample{
			"w": sample("w", t0, 100, 0),
			"x": sample("x", t0, 1000, 5),
		}},
	}}
	second := &Snapshot{At: t1, Pods: []PodSnapshot{
		{Endpoint: "p0", Workers: map[string]*Sample{
			"w": sample("w", t1, 5000, 50),
			"x": sample("x", t1, 6000, 60),
		}},
		{Endpoint: "p1", Workers: map[string]*Sample{
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
	appendTo := func(h *History, samples ...*Sample) {
		for _, s := range samples {
			h.Append(&Snapshot{At: s.At, Pods: []PodSnapshot{{Endpoint: "p", Workers: map[string]*Sample{"w": s}}}})
		}
	}

	s0 := sample(0, 100, 0)
	s1 := sample(5*time.Second, 150, 5) // 10/s, 1/s
	early := sample(5008*time.Millisecond, 151, 5)
	s2 := sample(10*time.Second, 250, 15) // against s1: 20/s, 2/s
	appendTo(NewHistory(iv), s0, s1, early, s2)
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
	appendTo(NewHistory(iv), first, soon)
	if soon.RateOK {
		t.Errorf("early poll after an unrated sample has RateOK (completed=%v)", soon.CompletedRate)
	}
	// So is one whose counter went back (a restart), which becomes the base:
	// rated against r1 instead, r2's counter would be back too, a gap.
	r0, r1, reset, r2 := sample(0, 100, 0), sample(5*time.Second, 150, 0), sample(5008*time.Millisecond, 2, 0), sample(10*time.Second, 52, 0)
	appendTo(NewHistory(iv), r0, r1, reset, r2)
	if reset.RateOK || !r2.RateOK {
		t.Errorf("reset: RateOK=%v, next: RateOK=%v; want a gap, then a rate against the reset", reset.RateOK, r2.RateOK)
	}
}

func TestHistoryConcurrentAppendAndRead(t *testing.T) {
	const appends = 1000
	iv := 5 * time.Second
	t0 := time.Unix(1000, 0)
	h := NewHistory(iv)

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
				{Endpoint: "p", Workers: map[string]*Sample{"w": s}},
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
