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
	h := NewHistory(iv)

	sample := func(at time.Time, completed uint64) *Sample {
		s := &Sample{At: at}
		s.Name = "w"
		s.Metrics.Completed = completed
		return s
	}
	// "w" exists on both pods; p2 only shows up in the second snapshot, so the
	// shared worker name must not borrow p1's history.
	first := &Snapshot{At: t0, Pods: []PodSnapshot{
		{Endpoint: "p1", Workers: map[string]*Sample{"w": sample(t0, 0)}},
	}}
	t1 := t0.Add(iv)
	second := &Snapshot{At: t1, Pods: []PodSnapshot{
		{Endpoint: "p1", Workers: map[string]*Sample{"w": sample(t1, 50)}},
		{Endpoint: "p2", Workers: map[string]*Sample{"w": sample(t1, 500)}},
	}}

	h.Append(first)
	if first.Pods[0].Workers["w"].RateOK {
		t.Error("first sample for a worker has RateOK")
	}
	h.Append(second)

	p1 := second.Pods[0].Workers["w"]
	if !p1.RateOK || p1.CompletedRate != 10 || p1.FailedRate != 0 {
		t.Errorf("p1: RateOK=%v completed=%v failed=%v, want true 10 0", p1.RateOK, p1.CompletedRate, p1.FailedRate)
	}
	if p2 := second.Pods[1].Workers["w"]; p2.RateOK {
		t.Errorf("p2 appeared only in the second snapshot but RateOK=true (completed=%v)", p2.CompletedRate)
	}
	if h.Latest() != second {
		t.Error("Latest is not the second snapshot")
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
