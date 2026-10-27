package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/daemon"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
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

// testToken is the token of every newTestServer server; no response may carry it.
const testToken = "s3cr3t-tok"

// fixture serves the web UI over a store in a temporary data dir. A daemon
// running in-process fills it (startDaemon), or the test writes the
// snapshots itself under a lease it holds (claim, write).
type fixture struct {
	t   *testing.T
	dir string // the data dir
	ctl *store.Control
	h   *handler
	srv *httptest.Server

	// build makes the daemon's pools; api.PoolFor when nil. Set it before
	// startDaemon.
	build func(*config.Server) (*api.Pool, error)

	// leaseConfig is the config path in the lease of the daemon
	// startDaemon runs, or that claim takes. Set it before either.
	leaseConfig string

	mu       sync.Mutex
	cfg      *config.Config // what the daemon and the UI read; replaced whole, never changed
	startErr error          // what Source.Start returns
	starts   int            // Source.Start calls
	run      *daemonRun     // the daemon running, nil if none
	token    string         // the lease the test holds, "" if none
	feeds    map[string]*store.Feed
	envCheck func(store.Lease) bool // Source.SameEnv's answer; every lease is of this environment when nil
	envCalls int                    // Source.SameEnv calls
}

// newFixture serves the web UI over a fresh data dir for cfg, its servers
// named by their keys. Nothing collects yet.
func newFixture(t *testing.T, cfg *config.Config) *fixture {
	t.Helper()
	for name, s := range cfg.Servers {
		s.Name = name
	}
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), "data"), cfg: cfg, feeds: make(map[string]*store.Feed)}
	ctl, err := store.OpenControl(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	f.ctl = ctl

	h, err := newHandler(Source{DataDir: f.dir, Config: f.config, Control: ctl, Start: f.start, SameEnv: f.sameEnv})
	if err != nil {
		t.Fatal(err)
	}
	h.logf = func(string, ...any) {} // a recovered panic is the test's to report
	t.Cleanup(h.close)
	f.h = h

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := httptest.NewUnstartedServer(h.routes(AllowedHosts(ln)))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f.srv = srv
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, feed := range f.feeds {
			_ = feed.Close()
		}
	})
	return f
}

// startLive serves cfg with a daemon collecting it, once the server keyed
// name has its first snapshot.
func startLive(t *testing.T, cfg *config.Config, name string) *fixture {
	t.Helper()
	f := newFixture(t, cfg)
	f.startDaemon()
	f.waitFirst(name)
	return f
}

func (f *fixture) config() (*config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, nil
}

// setConfig replaces the config the daemon and the UI read.
func (f *fixture) setConfig(cfg *config.Config) {
	for name, s := range cfg.Servers {
		s.Name = name
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
}

func (f *fixture) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return f.startErr
}

// sameEnv is Source.SameEnv: what the test set (see setEnvCheck).
func (f *fixture) sameEnv(l store.Lease) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envCalls++
	return f.envCheck == nil || f.envCheck(l)
}

// setEnvCheck sets what Source.SameEnv answers.
func (f *fixture) setEnvCheck(check func(store.Lease) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envCheck = check
}

// envChecks is how many times Source.SameEnv was called.
func (f *fixture) envChecks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.envCalls
}

// lease is the lease of the daemon the fixture runs, or of the test.
func (f *fixture) lease() store.Lease {
	return store.Lease{PID: os.Getpid(), Build: "test", ConfigPath: f.leaseConfig}
}

// started is how many times Source.Start was called.
func (f *fixture) started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// daemonRun is a daemon.Run in progress.
type daemonRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startDaemon runs a daemon in-process over the fixture's data dir and
// config; it is stopped when the test ends, before the fakes it polls.
func (f *fixture) startDaemon() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &daemonRun{cancel: cancel, done: make(chan struct{})}
	o := daemon.Options{
		DataDir:   f.dir,
		Config:    func() (*config.Config, string, error) { cfg, err := f.config(); return cfg, "", err },
		BuildPool: f.build,
		Lease:     f.lease(),
		Log:       slog.New(slog.DiscardHandler),
	}
	go func() {
		defer close(r.done)
		if code, err := daemon.Run(ctx, o); code != 0 || err != nil {
			f.t.Errorf("daemon.Run = %d, %v", code, err)
		}
	}()
	f.mu.Lock()
	f.run = r
	f.mu.Unlock()
	f.t.Cleanup(f.stopDaemon)
}

// stopDaemon stops the daemon and waits for it to let the lease go.
func (f *fixture) stopDaemon() {
	f.mu.Lock()
	r := f.run
	f.run = nil
	f.mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		f.t.Error("daemon still running 10s after it was stopped")
	}
}

// claim takes the lease, as a daemon does, so that the test writes the
// snapshots (see write).
func (f *fixture) claim() {
	f.t.Helper()
	f.claimAt(time.Now())
}

// claimAt is claim, as a daemon claiming at at: the snapshots it took
// since, and only those, are its own (R44). Its beat is at's too, so the
// lease holds until LeaseTTL after at.
func (f *fixture) claimAt(at time.Time) {
	f.t.Helper()
	token, ok, err := f.ctl.Claim(f.lease(), at)
	if err != nil || !ok {
		f.t.Fatalf("Claim = %v, %v", ok, err)
	}
	f.mu.Lock()
	f.token = token
	f.mu.Unlock()
}

// release lets the lease the test holds go, as a daemon stopping does.
func (f *fixture) release() {
	f.t.Helper()
	f.mu.Lock()
	token := f.token
	f.token = ""
	f.mu.Unlock()
	if err := f.ctl.Release(token); err != nil {
		f.t.Fatal(err)
	}
}

// write stores s as the newest snapshot of the server keyed name, under
// the lease the test holds or else the running daemon's.
func (f *fixture) write(name string, s *fleet.Snapshot) {
	f.t.Helper()
	f.mu.Lock()
	token := f.token
	f.mu.Unlock()
	if token == "" {
		l, _, err := f.ctl.Lease()
		if err != nil {
			f.t.Fatal(err)
		}
		token = l.Token
	}
	db, err := store.OpenServer(f.dir, name, true)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

// latest is the newest snapshot stored for the server keyed name, nil if
// none.
func (f *fixture) latest(name string) *fleet.Snapshot {
	f.t.Helper()
	f.mu.Lock()
	feed := f.feeds[name]
	if feed == nil {
		feed = store.NewFeed(f.dir, name, f.cfg.Servers[name].PollInterval())
		f.feeds[name] = feed
	}
	f.mu.Unlock()
	if err := feed.Sync(); err != nil {
		f.t.Fatalf("sync %s: %v", name, err)
	}
	return feed.Latest()
}

// waitFirst waits for the first snapshot of the server keyed name.
func (f *fixture) waitFirst(name string) {
	f.t.Helper()
	fleettest.WaitUntil(f.t, 5*time.Second, name+"'s first snapshot", func() bool { return f.latest(name) != nil })
}

// waitFor waits for a snapshot of the server keyed name for which cond
// holds, and returns it.
func (f *fixture) waitFor(name, what string, cond func(*fleet.Snapshot) bool) *fleet.Snapshot {
	f.t.Helper()
	var s *fleet.Snapshot
	fleettest.WaitUntil(f.t, 5*time.Second, what, func() bool {
		s = f.latest(name)
		return s != nil && cond(s)
	})
	return s
}

// pokes is how many times the server keyed name was poked.
func (f *fixture) pokes(name string) int64 {
	f.t.Helper()
	pokes, err := f.ctl.Pokes()
	if err != nil {
		f.t.Fatal(err)
	}
	return pokes[name]
}

// newTestServer serves the web UI over a daemon collecting "multi", the
// default server, which polls the given fake varmqs every second with
// testToken. A second configured server, "b", points at an unreachable
// endpoint. It returns once multi's first snapshot is in.
func newTestServer(t *testing.T, fakes ...*httptest.Server) (*httptest.Server, *fixture) {
	t.Helper()
	f := startLive(t, &config.Config{DefaultServer: "multi", Servers: map[string]*config.Server{
		"multi": {Interval: 1, Endpoints: urlsOf(fakes...), Token: testToken},
		"b":     {Interval: 1, Endpoints: []string{"http://b.invalid"}, Token: testToken},
	}}, "multi")
	return f.srv, f
}

func urlsOf(fakes ...*httptest.Server) []string {
	urls := make([]string, len(fakes))
	for i, f := range fakes {
		urls[i] = f.URL
	}
	return urls
}

// twoFakes starts two fake pods: orders runs on both, emails only on a.
func twoFakes(t *testing.T) (fa, fb *fleettest.Varmq, aSrv, bSrv *httptest.Server) {
	t.Helper()
	fa, aSrv = fleettest.NewVarmq(t, "a", map[string]*fleettest.Worker{
		"orders": {Status: "Running", Concurrency: 4, Busy: 1},
		"emails": {Status: "Paused", Concurrency: 2},
	})
	fb, bSrv = fleettest.NewVarmq(t, "b", map[string]*fleettest.Worker{
		"orders": {Status: "Running", Concurrency: 4, Busy: 2},
	})
	return fa, fb, aSrv, bSrv
}

// threeFakes starts three fake varmqs, instances a, b and c: orders
// (concurrency 4) on each, emails on a only.
func threeFakes(t *testing.T) []*httptest.Server {
	t.Helper()
	_, fakes := threeVarmqs(t)
	return fakes
}

// threeVarmqs is threeFakes with the fakes' handles.
func threeVarmqs(t *testing.T) ([]*fleettest.Varmq, []*httptest.Server) {
	t.Helper()
	var varmqs []*fleettest.Varmq
	var fakes []*httptest.Server
	for i, instance := range []string{"a", "b", "c"} {
		workers := map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 4, Busy: i}}
		if i == 0 {
			workers["emails"] = &fleettest.Worker{Status: "Paused", Concurrency: 2}
		}
		v, f := fleettest.NewVarmq(t, instance, workers)
		varmqs, fakes = append(varmqs, v), append(fakes, f)
	}
	return varmqs, fakes
}

// kubernetesServer is discovered through Kubernetes, the one kind of server
// whose units are pods. unitsFixture builds its pool, so no cluster is
// reached; its pods are labelled by the instance that answered.
var kubernetesServer = config.Server{Kubernetes: &config.KubernetesRef{Context: "prod", Namespace: "jobs", Service: "varmq"}}

// unitsFixture serves the web UI over a daemon whose only server, "multi",
// is a copy of server polling src's endpoints every second. It returns once
// the first snapshot is in.
func unitsFixture(t *testing.T, server config.Server, src *mutableSource) *fixture {
	t.Helper()
	server.Interval = 1
	f := newFixture(t, &config.Config{Servers: map[string]*config.Server{"multi": &server}})
	f.build = func(*config.Server) (*api.Pool, error) { return api.NewPool(src, ""), nil }
	f.startDaemon()
	f.waitFirst("multi")
	return f
}

// serveUnits is unitsFixture over the fakes, which stay.
func serveUnits(t *testing.T, server config.Server, fakes ...*httptest.Server) *httptest.Server {
	t.Helper()
	return unitsFixture(t, server, &mutableSource{eps: urlsOf(fakes...)}).srv
}

// serveBalanced serves the web UI over a daemon whose only server, "multi",
// polls endpoints every second, once its latest snapshot has exactly the
// unit keys want, in any order.
func serveBalanced(t *testing.T, endpoints []string, want ...string) (*httptest.Server, *fixture) {
	t.Helper()
	f := startLive(t, &config.Config{Servers: map[string]*config.Server{
		"multi": {Interval: 1, Endpoints: endpoints},
	}}, "multi")
	want = slices.Sorted(slices.Values(want))
	f.waitFor("multi", "units "+strings.Join(want, " "), func(s *fleet.Snapshot) bool {
		return slices.Equal(slices.Sorted(slices.Values(unitKeys(s))), want)
	})
	return f.srv, f
}

// mutableSource is a discovery.Source whose endpoints a test changes.
type mutableSource struct {
	mu  sync.Mutex
	eps []string
}

func (s *mutableSource) Endpoints() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.eps)
}

func (s *mutableSource) set(eps ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eps = eps
}

func (*mutableSource) Close() error { return nil }

// send performs req against srv and returns the response and its body.
func send(t *testing.T, srv *httptest.Server, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", req.Method, req.URL, err)
	}
	return resp, string(body)
}

// fetch GETs path from srv, as htmx does when hx is set, and returns the
// status and body.
func fetch(t *testing.T, srv *httptest.Server, path string, hx bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	resp, body := send(t, srv, req)
	return resp.StatusCode, body
}

func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// visibleText is body without its tags, so attribute values (the pod form
// field, class names, URLs) are left out.
func visibleText(body string) string { return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(body, "") }

// checkPages fetches each path of pages from srv and requires its body to
// contain its wants and its visible text to lack the other noun.
func checkPages(t *testing.T, srv *httptest.Server, other string, pages map[string][]string) {
	t.Helper()
	for path, wants := range pages {
		status, body := fetch(t, srv, path, false)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, status)
			continue
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s lacks %q", path, want)
			}
		}
		if text := strings.ToLower(visibleText(body)); strings.Contains(text, other) {
			t.Errorf("GET %s says %q:\n%s", path, other, text)
		}
	}
}

// errStart is what a failing Source.Start says.
var errStart = errors.New("vartop daemon did not start; see /data/daemon.log")
