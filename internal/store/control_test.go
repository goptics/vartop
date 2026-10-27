package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openControl(t *testing.T, dataDir string) *Control {
	t.Helper()
	ctl, err := OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	return ctl
}

func TestLeaseClaimBeatRelease(t *testing.T) {
	dir := t.TempDir()
	a, b := openControl(t, dir), openControl(t, dir) // two daemons
	now := time.Now().Round(0)

	if _, fresh, err := a.Lease(); fresh || err != nil {
		t.Fatalf("Lease before any claim: fresh %v, %v; want none", fresh, err)
	}
	want := Lease{PID: 41, Build: "b1", ConfigPath: "/c.json", Kubeconfig: "/k", PathEnv: "/bin"}
	old, ok, err := a.Claim(want, now)
	if err != nil || !ok || old == "" {
		t.Fatalf("first Claim = %q, %v, %v; want a token", old, ok, err)
	}
	got, fresh, err := b.Lease()
	if err != nil || !fresh || got.Token != old || got.PID != 41 || got.Build != "b1" || got.ConfigPath != "/c.json" ||
		got.Kubeconfig != "/k" || got.PathEnv != "/bin" || !got.Started.Equal(now.Truncate(time.Millisecond)) || got.Stop != 0 {
		t.Errorf("Lease = %+v, fresh %v, %v; want %+v claimed at %v", got, fresh, err, want, now)
	}

	if tok, ok, err := b.Claim(Lease{PID: 42}, now.Add(LeaseTTL-time.Second)); ok || tok != "" || err != nil {
		t.Errorf("second Claim while fresh = %q, %v, %v; want refused", tok, ok, err)
	}
	if stop, err := a.Beat(old, now.Add(BeatEvery)); stop || err != nil {
		t.Errorf("Beat = %v, %v; want no stop", stop, err)
	}

	// The holder stops beating; once the lease is LeaseTTL old another
	// daemon takes over, and the old token is worthless.
	late := now.Add(BeatEvery + LeaseTTL + time.Millisecond)
	next, ok, err := b.Claim(Lease{PID: 42, Build: "b2"}, late)
	if err != nil || !ok || next == "" || next == old {
		t.Fatalf("Claim after LeaseTTL = %q, %v, %v; want a new token", next, ok, err)
	}
	if _, err := a.Beat(old, late); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("old token's Beat = %v, want ErrLeaseLost", err)
	}
	db := newWriter(t, dir, "prod")
	if _, err := db.Append(old, tick(late, 1)); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("old token's Append = %v, want ErrLeaseLost", err)
	}
	if s, _ := db.Latest(); s != nil {
		t.Errorf("old token's Append wrote %v", s.At)
	}
	if _, err := db.Append(next, tick(late, 1)); err != nil {
		t.Errorf("new token's Append = %v", err)
	}

	if err := a.RequestStop(); err != nil {
		t.Fatal(err)
	}
	if stop, err := b.Beat(next, late.Add(BeatEvery)); !stop || err != nil {
		t.Errorf("Beat after RequestStop = %v, %v; want stop", stop, err)
	}

	if err := b.Release(old); err != nil {
		t.Fatal(err)
	}
	// (Not Lease's fresh: the test's clock runs ahead of the real one.)
	if l, _, _ := a.Lease(); l.Token != next {
		t.Error("Release with an old token released the current lease")
	}
	if err := b.Release(next); err != nil {
		t.Fatal(err)
	}
	if l, fresh, err := a.Lease(); fresh || err != nil || l.PID != 42 {
		t.Errorf("Lease after Release = %+v, fresh %v, %v; want the last daemon's, not fresh", l, fresh, err)
	}
	if _, err := b.Beat(next, late.Add(2*BeatEvery)); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Beat after Release = %v, want ErrLeaseLost", err)
	}
	// Released: the next daemon claims at once, with no stop pending.
	tok, ok, err := a.Claim(Lease{PID: 43}, late.Add(2*BeatEvery))
	if err != nil || !ok {
		t.Fatalf("Claim after Release = %q, %v, %v", tok, ok, err)
	}
	if stop, err := a.Beat(tok, late.Add(3*BeatEvery)); stop || err != nil {
		t.Errorf("Beat of a new lease = %v, %v; want no stop carried over", stop, err)
	}
}

func TestPokes(t *testing.T) {
	ctl := openControl(t, t.TempDir())
	before := time.Now()
	at, err := ctl.Poke("prod")
	if err != nil || at.Before(before.Round(0).Truncate(time.Millisecond)) || at.After(time.Now()) {
		t.Fatalf("Poke = %v, %v; want the time of the poke", at, err)
	}
	if _, err := ctl.Poke("prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Poke("staging"); err != nil {
		t.Fatal(err)
	}
	pokes, err := ctl.Pokes()
	if err != nil || pokes["prod"] != 2 || pokes["staging"] != 1 {
		t.Errorf("Pokes = %v, %v; want prod 2, staging 1", pokes, err)
	}
}

func TestServerErrs(t *testing.T) {
	ctl := openControl(t, t.TempDir())
	if _, err := ctl.Poke("ok"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetServerErr("prod", "discovery failed: boom"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetServerErr("staging", "x"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetServerErr("staging", ""); err != nil {
		t.Fatal(err)
	}
	errs, err := ctl.ServerErrs()
	if err != nil || len(errs) != 1 || errs["prod"] != "discovery failed: boom" {
		t.Errorf("ServerErrs = %v, %v; want only prod's", errs, err)
	}
	if pokes, _ := ctl.Pokes(); pokes["prod"] != 0 || pokes["ok"] != 1 {
		t.Errorf("SetServerErr touched pokes: %v", pokes)
	}
}

// TestFreshBounds: a lease is fresh from a beat up to 2·BeatEvery in the
// future (clocks a little apart) to one under LeaseTTL old; a beat further
// ahead, as a dead daemon's reads after the clock stepped back, is not.
func TestFreshBounds(t *testing.T) {
	now := time.UnixMilli(time.Now().UnixMilli())
	for _, c := range []struct {
		name string
		beat time.Time
		want bool
	}{
		{"now", now, true},
		{"just under LeaseTTL old", now.Add(-LeaseTTL + time.Millisecond), true},
		{"LeaseTTL old", now.Add(-LeaseTTL), false},
		{"2·BeatEvery ahead", now.Add(2 * BeatEvery), true},
		{"past 2·BeatEvery ahead", now.Add(2*BeatEvery + time.Millisecond), false},
		{"an hour ahead", now.Add(time.Hour), false},
	} {
		if got := fresh("t", c.beat.UnixMilli(), now); got != c.want {
			t.Errorf("%s: fresh = %v, want %v", c.name, got, c.want)
		}
	}
	if fresh("", now.UnixMilli(), now) {
		t.Error("a released lease is fresh")
	}

	// Through the API: a lease beaten an hour ahead of the clock is no
	// lease Claim must respect, nor one Lease calls fresh.
	dir := t.TempDir()
	a, b := openControl(t, dir), openControl(t, dir)
	if _, ok, err := a.Claim(Lease{PID: 1}, time.Now().Add(time.Hour)); !ok || err != nil {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	if _, fresh, err := b.Lease(); fresh || err != nil {
		t.Errorf("Lease of a beat an hour ahead: fresh %v, %v; want not fresh", fresh, err)
	}
	if _, ok, err := b.Claim(Lease{PID: 2}, time.Now()); !ok || err != nil {
		t.Errorf("Claim over a beat an hour ahead = %v, %v; want it taken", ok, err)
	}
}

// beatClock is a clock whose sleeps pass at once, for BeatAdvances: each
// sleep runs onSleep, if set, after moving the clock on.
type beatClock struct {
	t       time.Time
	slept   time.Duration
	onSleep func()
}

func (c *beatClock) now() time.Time { return c.t }

func (c *beatClock) sleep(d time.Duration) {
	c.t = c.t.Add(d)
	c.slept += d
	if c.onSleep != nil {
		c.onSleep()
	}
}

func TestBeatAdvances(t *testing.T) {
	if DeadAfter != 3*BeatEvery {
		t.Errorf("DeadAfter = %v, want 3·BeatEvery", DeadAfter)
	}
	setup := func(t *testing.T) (holder, watcher *Control, token string, clock *beatClock) {
		dir := t.TempDir()
		holder, watcher = openControl(t, dir), openControl(t, dir)
		clock = &beatClock{t: time.Now().Round(0)}
		token, ok, err := holder.Claim(Lease{PID: 1}, clock.t)
		if !ok || err != nil {
			t.Fatalf("Claim = %v, %v", ok, err)
		}
		return holder, watcher, token, clock
	}

	t.Run("dead holder", func(t *testing.T) {
		_, w, token, clock := setup(t)
		advanced, err := w.BeatAdvances(context.Background(), token, DeadAfter, clock.now, clock.sleep)
		if advanced || err != nil {
			t.Errorf("BeatAdvances = %v, %v; want false", advanced, err)
		}
		if clock.slept < DeadAfter || clock.slept > DeadAfter+time.Second {
			t.Errorf("watched %v, want DeadAfter (%v)", clock.slept, DeadAfter)
		}
	})
	t.Run("live holder", func(t *testing.T) {
		h, w, token, clock := setup(t)
		beaten := false
		clock.onSleep = func() {
			if !beaten && clock.slept >= BeatEvery { // one beat, BeatEvery in
				beaten = true
				if _, err := h.Beat(token, clock.t); err != nil {
					t.Error(err)
				}
			}
		}
		advanced, err := w.BeatAdvances(context.Background(), token, DeadAfter, clock.now, clock.sleep)
		if !advanced || err != nil {
			t.Errorf("BeatAdvances = %v, %v; want true", advanced, err)
		}
		if clock.slept > BeatEvery+time.Second {
			t.Errorf("watched %v after the beat, want it to answer at once", clock.slept)
		}
	})
	t.Run("live holder, clock stepped back", func(t *testing.T) {
		h, w, token, clock := setup(t)
		clock.onSleep = func() {
			if _, err := h.Beat(token, clock.t.Add(-time.Minute)); err != nil {
				t.Error(err)
			}
		}
		if advanced, err := w.BeatAdvances(context.Background(), token, DeadAfter, clock.now, clock.sleep); !advanced || err != nil {
			t.Errorf("BeatAdvances = %v, %v; want true: a beat renewed is a live holder", advanced, err)
		}
	})
	t.Run("token gone", func(t *testing.T) {
		h, w, token, clock := setup(t)
		if err := h.Release(token); err != nil {
			t.Fatal(err)
		}
		if advanced, err := w.BeatAdvances(context.Background(), token, DeadAfter, clock.now, clock.sleep); advanced || err != nil {
			t.Errorf("BeatAdvances of a released lease = %v, %v; want false", advanced, err)
		}
		if clock.slept != 0 {
			t.Errorf("watched a released lease for %v, want no wait", clock.slept)
		}
	})
	t.Run("taken over meanwhile", func(t *testing.T) {
		_, w, token, clock := setup(t)
		clock.onSleep = func() {
			if _, ok, err := w.TakeOver(token, Lease{PID: 3}, clock.t); !ok || err != nil {
				t.Errorf("TakeOver = %v, %v", ok, err)
			}
			clock.onSleep = nil
		}
		if advanced, err := w.BeatAdvances(context.Background(), token, DeadAfter, clock.now, clock.sleep); advanced || err != nil {
			t.Errorf("BeatAdvances after a takeover = %v, %v; want false", advanced, err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		_, w, token, clock := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		clock.onSleep = cancel
		advanced, err := w.BeatAdvances(ctx, token, DeadAfter, clock.now, clock.sleep)
		if advanced || !errors.Is(err, context.Canceled) {
			t.Errorf("BeatAdvances cancelled = %v, %v; want false, context.Canceled", advanced, err)
		}
		if clock.slept > time.Second {
			t.Errorf("watched %v after the cancel", clock.slept)
		}
	})
}

// TestTakeOver: a lease whose holder is dead (fresh as it looks) goes to
// the first daemon to take it over; one more watcher of the same dead
// holder finds it taken and gets nothing.
func TestTakeOver(t *testing.T) {
	dir := t.TempDir()
	dead := openControl(t, dir)
	now := time.Now().Add(-DeadAfter).Round(0) // so that the takeover is now, by Lease's clock
	token, _, err := dead.Claim(Lease{PID: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := dead.Claim(Lease{PID: 2}, now.Add(DeadAfter)); ok || err != nil {
		t.Fatalf("Claim over a fresh lease = %v, %v; want refused", ok, err)
	}

	const watchers = 4
	var wg sync.WaitGroup
	won := make(chan Lease, watchers)
	for i := range watchers {
		ctl := openControl(t, dir)
		wg.Go(func() {
			tok, ok, err := ctl.TakeOver(token, Lease{PID: 10 + i}, now.Add(DeadAfter))
			if err != nil {
				t.Error(err)
			}
			if ok {
				won <- Lease{Token: tok, PID: 10 + i}
			}
		})
	}
	wg.Wait()
	close(won)
	var winners []Lease
	for l := range won {
		winners = append(winners, l)
	}
	if len(winners) != 1 {
		t.Fatalf("%d watchers took the lease over, want exactly one", len(winners))
	}
	l, fresh, err := dead.Lease()
	if err != nil || !fresh || l.Token != winners[0].Token || l.PID != winners[0].PID || l.Stop != 0 {
		t.Errorf("Lease = %+v, fresh %v, %v; want the winner's %+v", l, fresh, err, winners[0])
	}
	if _, err := dead.Beat(token, now.Add(DeadAfter)); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("the dead holder's Beat = %v, want ErrLeaseLost", err)
	}

	// A takeover of a lease no longer held (released, or stale) is a Claim.
	if err := dead.Release(l.Token); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := dead.TakeOver("someone", Lease{PID: 20}, now.Add(DeadAfter)); !ok || err != nil {
		t.Errorf("TakeOver of a released lease = %v, %v; want it taken", ok, err)
	}
}

// TestControlReplaced: control.db removed, or another file put in its
// place (the data dir deleted and made anew), is noticed.
func TestControlReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctl := openControl(t, dir)
	if gone, err := ctl.Replaced(); gone || err != nil {
		t.Fatalf("Replaced = %v, %v; want false", gone, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if gone, err := ctl.Replaced(); !gone || err != nil {
		t.Errorf("Replaced after the data dir was removed = %v, %v; want true", gone, err)
	}
	openControl(t, dir) // a UI makes it anew
	if gone, err := ctl.Replaced(); !gone || err != nil {
		t.Errorf("Replaced once made anew = %v, %v; want true", gone, err)
	}
}
