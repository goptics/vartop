package discovery

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
)

func ips(addrs ...string) []net.IPAddr {
	out := make([]net.IPAddr, len(addrs))
	for i, a := range addrs {
		out[i] = net.IPAddr{IP: net.ParseIP(a)}
	}
	return out
}

func TestDNSSourceURLs(t *testing.T) {
	d := newDNSSource(config.DNSRef{Host: "varmq", Port: 8080}, func(context.Context, string) ([]net.IPAddr, error) {
		return ips("10.0.0.2", "fd00::1", "10.0.0.1", "10.0.0.2"), nil
	})
	if err := d.refresh(); err != nil {
		t.Fatal(err)
	}
	want := []string{"http://10.0.0.1:8080/varmq", "http://10.0.0.2:8080/varmq", "http://[fd00::1]:8080/varmq"}
	if got := d.Endpoints(); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	d = newDNSSource(config.DNSRef{Host: "varmq", Port: 9000, Path: "/api/varmq"}, func(context.Context, string) ([]net.IPAddr, error) {
		return ips("10.0.0.9"), nil
	})
	_ = d.refresh()
	if got := d.Endpoints(); len(got) != 1 || got[0] != "http://10.0.0.9:9000/api/varmq" {
		t.Fatalf("path override: got %v", got)
	}
}

// TestDNSSourceKeepsLastGood covers a DNS blip: a failed or empty lookup must
// not wipe the instances already discovered.
func TestDNSSourceKeepsLastGood(t *testing.T) {
	var fail atomic.Bool
	var empty atomic.Bool
	d := newDNSSource(config.DNSRef{Host: "varmq", Port: 8080}, func(context.Context, string) ([]net.IPAddr, error) {
		if fail.Load() {
			return nil, errors.New("servfail")
		}
		if empty.Load() {
			return nil, nil
		}
		return ips("10.0.0.1"), nil
	})
	if err := d.refresh(); err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	if err := d.refresh(); err == nil {
		t.Fatal("want lookup error")
	}
	fail.Store(false)
	empty.Store(true)
	if err := d.refresh(); err == nil {
		t.Fatal("want error for empty answer")
	}
	if got := d.Endpoints(); len(got) != 1 || got[0] != "http://10.0.0.1:8080/varmq" {
		t.Fatalf("last good set lost: %v", got)
	}
}

// TestDNSSourceRefreshAndClose covers background re-resolution (instances
// scaling out) and that Close stops it; Endpoints must not wait on a lookup.
func TestDNSSourceRefreshAndClose(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})
	d := newDNSSource(config.DNSRef{Host: "varmq", Port: 8080}, func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		switch calls.Add(1) {
		case 1:
			return ips("10.0.0.1"), nil
		case 2:
			return ips("10.0.0.1", "10.0.0.2"), nil
		default:
			<-block // a hung resolver
			return nil, ctx.Err()
		}
	})
	if err := d.refresh(); err != nil {
		t.Fatal(err)
	}
	go d.run(10 * time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for len(d.Endpoints()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("refresh never picked up new instance: %v", d.Endpoints())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A lookup is now hung; Endpoints still answers immediately.
	done := make(chan struct{})
	go func() { _ = d.Endpoints(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Endpoints blocked on a hung lookup")
	}

	close(block)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	_ = d.Close() // idempotent
}

func TestNewDNSUnknownHost(t *testing.T) {
	if _, err := NewDNS(config.DNSRef{Host: "does-not-exist.invalid", Port: 8080}); err == nil {
		t.Fatal("want error for unresolvable host")
	}
}
