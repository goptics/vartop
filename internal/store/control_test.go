package store

import (
	"errors"
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
	if _, fresh, _ := a.Lease(); !fresh {
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
