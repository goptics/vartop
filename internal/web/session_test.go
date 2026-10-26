package web

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// recordingSource is a discovery.Source with no endpoints that counts its
// Close calls, so a test can see when a pool built on it is closed.
type recordingSource struct{ closes atomic.Int32 }

func (*recordingSource) Endpoints() []string { return nil }

func (s *recordingSource) Close() error {
	s.closes.Add(1)
	return nil
}

// gatedBuilds is a Manager build func whose builds block until the test
// releases them, one gate per server name. Every build of a server returns
// a pool over that server's recordingSource.
type gatedBuilds struct {
	t       *testing.T
	gates   map[string]chan error
	sources map[string]*recordingSource
}

func (g *gatedBuilds) build(srv *config.Server) (*api.Pool, error) {
	if err := <-g.gates[srv.Name]; err != nil {
		return nil, err
	}
	return api.NewPool(g.sources[srv.Name], ""), nil
}

// release lets the build of name in flight return err; it fails the test if
// no build of name is waiting.
func (g *gatedBuilds) release(name string, err error) {
	g.t.Helper()
	select {
	case g.gates[name] <- err:
	case <-time.After(3 * time.Second):
		g.t.Fatalf("no build of %s in flight", name)
	}
}

// expectNoBuild fails the test if a build of name is waiting.
func (g *gatedBuilds) expectNoBuild(name string) {
	g.t.Helper()
	select {
	case g.gates[name] <- nil:
		g.t.Errorf("a build of %s was started", name)
	case <-time.After(100 * time.Millisecond):
	}
}

func (g *gatedBuilds) closes(name string) int32 { return g.sources[name].closes.Load() }

// testConfig has one static server per name, keyed by its name.
func testConfig(names ...string) *config.Config {
	cfg := &config.Config{Servers: make(map[string]*config.Server, len(names))}
	for _, n := range names {
		cfg.Servers[n] = &config.Server{Name: n, Interval: 1, Endpoints: []string{"http://" + n + ".invalid"}}
	}
	return cfg
}

// newGatedManager returns a Manager over the named servers whose builds are
// gated. Builds still blocked when the test ends are released after Close.
func newGatedManager(t *testing.T, names ...string) (*Manager, *gatedBuilds) {
	t.Helper()
	g := &gatedBuilds{t: t, gates: make(map[string]chan error), sources: make(map[string]*recordingSource)}
	for _, n := range names {
		g.gates[n] = make(chan error)
		g.sources[n] = &recordingSource{}
	}
	m := NewManager(testConfig(names...), g.build)
	t.Cleanup(func() {
		_ = m.Close()
		for _, gate := range g.gates {
			close(gate)
		}
	})
	return m, g
}

// activate switches m to name, lets its build succeed and waits for it.
func activate(t *testing.T, m *Manager, g *gatedBuilds, name string) *Session {
	t.Helper()
	if err := m.Switch(name); err != nil {
		t.Fatalf("Switch(%q): %v", name, err)
	}
	g.release(name, nil)
	waitForServer(t, m, name)
	return m.Current()
}

func waitForServer(t *testing.T, m *Manager, name string) {
	t.Helper()
	waitUntil(t, 3*time.Second, "server "+name+" active", func() bool {
		s := m.Current()
		return s != nil && s.Server.Name == name
	})
}

func TestSwitchSuccessReplacesAndClosesOld(t *testing.T) {
	m, g := newGatedManager(t, "a", "b")

	if err := m.Switch("a"); err != nil {
		t.Fatalf("Switch(a): %v", err)
	}
	if m.Pending() != "a" || m.Current() != nil {
		t.Fatalf("while building: Pending() = %q, Current() = %v; want a, nil", m.Pending(), m.Current())
	}
	g.release("a", nil)
	waitForServer(t, m, "a")
	a := m.Current()
	if m.Pending() != "" || m.Err() != nil {
		t.Errorf("after a: Pending() = %q, Err() = %v; want none", m.Pending(), m.Err())
	}

	if err := m.Switch("b"); err != nil {
		t.Fatalf("Switch(b): %v", err)
	}
	if m.Pending() != "b" || m.Current() != a {
		t.Errorf("while building b: Pending() = %q, Current() = %v; want b and a's session", m.Pending(), m.Current())
	}
	g.release("b", nil)
	waitForServer(t, m, "b")

	waitUntil(t, 3*time.Second, "a's pool closed", func() bool { return g.closes("a") == 1 })
	if a.ctx.Err() == nil {
		t.Error("a's session still polling after the switch")
	}
	if g.closes("b") != 0 {
		t.Errorf("b's pool closed %d times, want open", g.closes("b"))
	}
	if m.Pending() != "" || m.Err() != nil {
		t.Errorf("after b: Pending() = %q, Err() = %v; want none", m.Pending(), m.Err())
	}
}

func TestSwitchFailureKeepsOld(t *testing.T) {
	m, g := newGatedManager(t, "a", "b")
	a := activate(t, m, g, "a")

	if err := m.Switch("b"); err != nil {
		t.Fatalf("Switch(b): %v", err)
	}
	g.release("b", errors.New("b unreachable"))
	waitUntil(t, 3*time.Second, "build error", func() bool { return m.Err() != nil })

	if !strings.Contains(m.Err().Error(), "b unreachable") {
		t.Errorf("Err() = %v, want the build error", m.Err())
	}
	if m.Current() != a || m.Pending() != "" {
		t.Errorf("Current() = %v, Pending() = %q; want a's session, nothing pending", m.Current(), m.Pending())
	}
	if g.closes("a") != 0 || a.ctx.Err() != nil {
		t.Error("a's session was closed by a failed switch; want it still polling")
	}

	// A later successful build clears the error.
	activate(t, m, g, "b")
	if m.Err() != nil {
		t.Errorf("Err() = %v after a successful switch, want nil", m.Err())
	}
}

func TestFailedNamesLastFailedBuild(t *testing.T) {
	m, g := newGatedManager(t, "a", "b")
	if m.Failed() != "" {
		t.Fatalf("Failed() = %q before any build, want none", m.Failed())
	}

	if err := m.Switch("a"); err != nil {
		t.Fatal(err)
	}
	g.release("a", errors.New("a unreachable"))
	waitUntil(t, 3*time.Second, "build error", func() bool { return m.Err() != nil })
	if m.Failed() != "a" {
		t.Errorf("Failed() = %q after a's build failed, want a", m.Failed())
	}

	activate(t, m, g, "b")
	if m.Failed() != "" {
		t.Errorf("Failed() = %q after a successful build, want none", m.Failed())
	}
}

func TestActiveNamesConfigKey(t *testing.T) {
	src := &recordingSource{}
	cfg := &config.Config{Servers: map[string]*config.Server{"prod-eu": {Name: "Production EU", Interval: 1}}}
	m := NewManager(cfg, func(*config.Server) (*api.Pool, error) { return api.NewPool(src, ""), nil })
	t.Cleanup(func() { _ = m.Close() })
	if name, s := m.Active(); name != "" || s != nil {
		t.Fatalf("Active() = %q, %v before any build, want none", name, s)
	}

	if err := m.Switch("prod-eu"); err != nil {
		t.Fatal(err)
	}
	waitForServer(t, m, "Production EU")
	if name, s := m.Active(); name != "prod-eu" || s != m.Current() {
		t.Errorf("Active() = %q, %v; want the key prod-eu and the current session", name, s)
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if name, s := m.Active(); name != "" || s != nil {
		t.Errorf("Active() = %q, %v after Close, want none", name, s)
	}
}

func TestSupersededBuildIsClosed(t *testing.T) {
	t.Run("newer finishes first", func(t *testing.T) {
		m, g := newGatedManager(t, "b", "c")
		_ = m.Switch("b")
		_ = m.Switch("c")
		if m.Pending() != "c" {
			t.Fatalf("Pending() = %q, want c", m.Pending())
		}

		g.release("c", nil)
		waitForServer(t, m, "c")
		g.release("b", nil)
		waitUntil(t, 3*time.Second, "b's pool closed", func() bool { return g.closes("b") == 1 })

		if s := m.Current(); s == nil || s.Server.Name != "c" {
			t.Errorf("Current() = %v, want c", s)
		}
		if g.closes("c") != 0 || m.Pending() != "" {
			t.Errorf("c closes = %d, Pending() = %q; want c open, nothing pending", g.closes("c"), m.Pending())
		}
	})

	t.Run("older finishes first", func(t *testing.T) {
		m, g := newGatedManager(t, "b", "c")
		_ = m.Switch("b")
		_ = m.Switch("c")

		g.release("b", nil)
		waitUntil(t, 3*time.Second, "b's pool closed", func() bool { return g.closes("b") == 1 })
		if m.Current() != nil || m.Pending() != "c" {
			t.Errorf("after b: Current() = %v, Pending() = %q; want nil, c", m.Current(), m.Pending())
		}

		g.release("c", nil)
		waitForServer(t, m, "c")
	})
}

func TestSwitchUnknownServer(t *testing.T) {
	m, g := newGatedManager(t, "a")

	err := m.Switch("nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Switch(nope) = %v, want an error naming it", err)
	}
	if m.Pending() != "" || m.Current() != nil || m.Err() != nil {
		t.Errorf("Pending() = %q, Current() = %v, Err() = %v; want nothing", m.Pending(), m.Current(), m.Err())
	}

	a := activate(t, m, g, "a")
	if err := m.Switch("nope"); err == nil {
		t.Error("Switch(nope) with a active = nil, want an error")
	}
	if m.Current() != a || m.Pending() != "" {
		t.Errorf("Current() = %v, Pending() = %q; want a kept, nothing pending", m.Current(), m.Pending())
	}
}

func TestSwitchToActiveOrPendingIsNoop(t *testing.T) {
	m, g := newGatedManager(t, "a", "b")
	activate(t, m, g, "a")

	if err := m.Switch("a"); err != nil {
		t.Fatalf("Switch(active) = %v", err)
	}
	if m.Pending() != "" {
		t.Errorf("Switch(active): Pending() = %q, want no build", m.Pending())
	}
	g.expectNoBuild("a")

	_ = m.Switch("b")
	if err := m.Switch("b"); err != nil {
		t.Fatalf("Switch(pending) = %v", err)
	}
	g.release("b", nil)
	waitForServer(t, m, "b")
	g.expectNoBuild("b")
	waitUntil(t, 3*time.Second, "a's pool closed", func() bool { return g.closes("a") == 1 })
	b := m.Current()

	// Back to the active server while another switch is building: that
	// build is dropped and the active session kept.
	_ = m.Switch("a")
	if err := m.Switch("b"); err != nil {
		t.Fatalf("Switch(active, a pending) = %v", err)
	}
	if m.Pending() != "" || m.Current() != b {
		t.Errorf("Pending() = %q, Current() = %v; want nothing pending, b kept", m.Pending(), m.Current())
	}
	g.release("a", nil)
	waitUntil(t, 3*time.Second, "dropped a pool closed", func() bool { return g.closes("a") == 2 })
	if m.Current() != b || g.closes("b") != 0 {
		t.Errorf("Current() = %v, b closes = %d; want b kept open", m.Current(), g.closes("b"))
	}
}

func TestManagerClose(t *testing.T) {
	m, g := newGatedManager(t, "a", "b")
	a := activate(t, m, g, "a")
	_ = m.Switch("b")

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if g.closes("a") != 1 || a.ctx.Err() == nil {
		t.Error("Close left the current session open")
	}
	if m.Current() != nil || m.Pending() != "" {
		t.Errorf("after Close: Current() = %v, Pending() = %q; want none", m.Current(), m.Pending())
	}

	// A build finishing after Close is closed and dropped.
	g.release("b", nil)
	waitUntil(t, 3*time.Second, "late b pool closed", func() bool { return g.closes("b") == 1 })
	if m.Current() != nil {
		t.Errorf("Current() = %v after a late build, want nil", m.Current())
	}

	if err := m.Close(); err != nil || g.closes("a") != 1 {
		t.Errorf("second Close = %v, a closes = %d; want nil, 1", err, g.closes("a"))
	}
	if err := m.Switch("a"); err == nil {
		t.Error("Switch after Close = nil, want an error")
	}
}

func TestServersSorted(t *testing.T) {
	m := NewManager(testConfig("prod", "dev", "stage"), api.PoolFor)
	if got, want := m.Servers(), []string{"dev", "prod", "stage"}; !slices.Equal(got, want) {
		t.Errorf("Servers() = %v, want %v", got, want)
	}
}

// TestConcurrentSwitches races many Switch calls and reads; once settled,
// exactly one pool may be open (the current session's) and every other pool
// built must be closed exactly once.
func TestConcurrentSwitches(t *testing.T) {
	names := []string{"a", "b", "c"}
	var mu sync.Mutex
	sources := make(map[*api.Pool]*recordingSource)
	build := func(*config.Server) (*api.Pool, error) {
		time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
		src := &recordingSource{}
		pool := api.NewPool(src, "")
		mu.Lock()
		sources[pool] = src
		mu.Unlock()
		return pool, nil
	}
	m := NewManager(testConfig(names...), build)
	t.Cleanup(func() { _ = m.Close() })

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 50 {
				if err := m.Switch(names[(i+j)%len(names)]); err != nil {
					t.Errorf("Switch: %v", err)
				}
				_, _, _ = m.Current(), m.Pending(), m.Err()
			}
		})
	}
	wg.Wait()

	settled := func() bool {
		cur := m.Current()
		if cur == nil || m.Pending() != "" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		for pool, src := range sources {
			want := int32(1)
			if pool == cur.pool {
				want = 0
			}
			if src.closes.Load() != want {
				return false
			}
		}
		return true
	}
	waitUntil(t, 5*time.Second, "one open pool, the current session's", settled)

	time.Sleep(100 * time.Millisecond) // nothing may close twice or reopen
	if !settled() {
		t.Error("state changed after settling")
	}
	if m.Err() != nil {
		t.Errorf("Err() = %v, want nil", m.Err())
	}
}

func TestOldTickNeverReachesNewHistory(t *testing.T) {
	// a answers its worker list only once released, so its first tick is in
	// flight when the switch happens.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	aSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workers":
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			_, _ = w.Write([]byte(`[{"name":"wa"}]`))
		default:
			_, _ = w.Write([]byte(`{"name":"wa","status":"Running"}`))
		}
	}))
	t.Cleanup(aSrv.Close)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // before aSrv.Close, which waits for the handler
	_, bSrv := newFakeVarmq(t, "b", map[string]*fakeWorker{"wb": {Status: "Running"}})

	cfg := &config.Config{Servers: map[string]*config.Server{
		"a": {Name: "a", Interval: 1, Endpoints: []string{aSrv.URL}},
		"b": {Name: "b", Interval: 1, Endpoints: []string{bSrv.URL}},
	}}
	m := NewManager(cfg, api.PoolFor)
	t.Cleanup(func() { _ = m.Close() })

	if err := m.Switch("a"); err != nil {
		t.Fatalf("Switch(a): %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("a's first poll never reached the server")
	}
	if err := m.Switch("b"); err != nil {
		t.Fatalf("Switch(b): %v", err)
	}
	waitForServer(t, m, "b")
	b := m.Current()

	unblock()
	time.Sleep(200 * time.Millisecond) // let a's cancelled tick wind down
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !b.PollNow(ctx) {
		t.Fatal("PollNow on b = false")
	}

	snaps := b.History.Snapshots()
	if len(snaps) == 0 {
		t.Fatal("b's history is empty")
	}
	for i, s := range snaps {
		var eps []string
		for _, p := range s.Pods {
			eps = append(eps, p.Key)
		}
		if !slices.Equal(eps, []string{bSrv.URL}) {
			t.Errorf("b snapshot %d has pods %v, want only b's %s", i, eps, bSrv.URL)
		}
	}
}

func TestConnectionLabel(t *testing.T) {
	tests := []struct {
		name string
		srv  *config.Server
		want string
	}{
		{"kubernetes", &config.Server{Kubernetes: &config.KubernetesRef{Context: "prod", Namespace: "jobs", Service: "varmq"}}, "prod/jobs"},
		{"dns", &config.Server{DNS: &config.DNSRef{Host: "varmq-headless", Port: 8080}}, "dns:varmq-headless:8080"},
		{"single endpoint", &config.Server{Endpoints: []string{"http://localhost:8080/varmq"}}, "http://localhost:8080/varmq"},
		{"several endpoints", &config.Server{Endpoints: []string{"http://a:1/varmq", "http://b:1/varmq"}}, "2 instances"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ConnectionLabel(tt.srv); got != tt.want {
				t.Errorf("ConnectionLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
