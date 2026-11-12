package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/collect"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/hub"
	"github.com/goptics/vartop/internal/store"
	"github.com/goptics/vartop/internal/web"
)

// fakeOrdersVarmq is a varmq pod running one worker, "orders".
func fakeOrdersVarmq(t *testing.T) *httptest.Server {
	t.Helper()
	worker := map[string]any{
		"name": "orders", "status": "Running", "concurrency": 2,
		"num_processing": 1, "num_pending": 0, "num_idle_workers": 1,
		"metrics": map[string]int{"submitted": 3, "completed": 3, "successful": 3, "failed": 0},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/workers":
			_ = json.NewEncoder(w).Encode([]any{worker})
		case "/workers/orders":
			_ = json.NewEncoder(w).Encode(worker)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// collectingSource is the web UI's source over a temporary data dir, with
// cfg its config and a collector collecting it in-process until the test
// ends, before the fakes it polls shut down. Start starts nothing.
func collectingSource(t *testing.T, cfg *config.Config) web.Source {
	t.Helper()
	dataDir := isolate(t)
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = collect.Run(ctx, collect.Options{
			DataDir: dataDir,
			Config:  func() (*config.Config, string, error) { return cfg, "", nil },
			Lease:   store.Lease{PID: os.Getpid()},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return web.Source{
		DataDir: dataDir,
		Config:  func() (*config.Config, error) { return cfg, nil },
		Control: ctl,
	}
}

// startServeWeb runs serveWeb over a collector collecting cfg (see
// collectingSource) on a free loopback port in the background. The
// returned channel yields its result; opened yields each URL passed to
// open. Cancel the context to stop it.
func startServeWeb(t *testing.T, cfg *config.Config, key string, out io.Writer, openErr error) (cancel context.CancelFunc, opened <-chan string, result <-chan error) {
	t.Helper()
	src := collectingSource(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	urls := make(chan string, 1)
	res := make(chan error, 1)
	go func() {
		res <- serveWeb(ctx, src, key, "127.0.0.1:0", out, func(u string) error {
			urls <- u
			return openErr
		})
	}()
	return cancel, urls, res
}

// configSource is a source whose config is cfg, over no store: for a
// serveWeb that fails before it serves.
func configSource(cfg *config.Config) web.Source {
	return web.Source{Config: func() (*config.Config, error) { return cfg, nil }}
}

// waitResult returns serveWeb's result, failing the test if it takes over 6s.
func waitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(6 * time.Second):
		t.Fatal("serveWeb did not return within 6s of cancel")
		return nil
	}
}

func waitURL(t *testing.T, opened <-chan string, result <-chan error) string {
	t.Helper()
	select {
	case u := <-opened:
		return u
	case err := <-result:
		t.Fatalf("serveWeb returned before opening the browser: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serveWeb never called open")
	}
	return ""
}

// getUntil polls GET url until the 200 body contains want or 5s pass.
func getUntil(t *testing.T, url, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, status := httpGet(url)
		if status == http.StatusOK && strings.Contains(body, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: status %d, body never contained %q:\n%s", url, status, want, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func httpGet(url string) (string, int) {
	resp, err := http.Get(url)
	if err != nil {
		return err.Error(), 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func TestLoadOrOnboardReturnsExistingConfig(t *testing.T) {
	saveStatic(t, "http://pod:8080/varmq")

	cfg, err := loadOrOnboard()
	if err != nil {
		t.Fatalf("loadOrOnboard() error: %v", err)
	}
	if cfg.Servers["multi"] == nil || cfg.DefaultServer != "multi" {
		t.Fatalf("loaded config = %+v, want the saved server \"multi\"", cfg)
	}
}

// TestLoadOrOnboardRejectsCorruptConfig: an unreadable config is an error,
// never a reason to onboard, which would overwrite the file.
func TestLoadOrOnboardRejectsCorruptConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "vartop", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		cfg, err := loadOrOnboard()
		if cfg != nil {
			t.Errorf("loadOrOnboard() config = %+v, want nil", cfg)
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("loadOrOnboard() error = %v, want a parse error", err)
	}
	if out != "" {
		t.Errorf("loadOrOnboard() printed %q: onboarding started", out)
	}
	if data, _ := os.ReadFile(path); string(data) != "{bad" {
		t.Errorf("config file now holds %q, want it untouched", data)
	}
}

// TestLoadOrOnboardRejectsKubernetesConfig: a legacy kubernetes config is an
// error, never a reason to onboard, which would overwrite the file.
func TestLoadOrOnboardRejectsKubernetesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "vartop", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"servers":{"k":{"name":"k","kubernetes":{"context":"prod","namespace":"jobs","service":"varmq"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		cfg, err := loadOrOnboard()
		if cfg != nil {
			t.Errorf("loadOrOnboard() config = %+v, want nil", cfg)
		}
		return err
	})
	if !errors.Is(err, config.ErrKubernetesRemoved) {
		t.Errorf("loadOrOnboard() error = %v, want ErrKubernetesRemoved", err)
	}
	if out != "" {
		t.Errorf("loadOrOnboard() printed %q: onboarding started", out)
	}
	if data, _ := os.ReadFile(path); string(data) != body {
		t.Errorf("config file now holds %q, want it untouched", data)
	}
}

// TestWebCmdSilencesUsage: a runtime error (port in use, shutdown) is not a
// usage mistake, so it must not print the usage text.
func TestWebCmdSilencesUsage(t *testing.T) {
	if !webCmd.SilenceUsage {
		t.Error("webCmd.SilenceUsage = false, want true")
	}
}

func TestServeWebServesAndShutsDown(t *testing.T) {
	fake := fakeOrdersVarmq(t)
	// The key differs from Server.Name: the session must follow the key.
	cfg := &config.Config{
		DefaultServer: "prod",
		Servers: map[string]*config.Server{
			"prod": {Name: "production", Endpoints: []string{fake.URL}},
		},
	}
	var out bytes.Buffer
	cancel, opened, result := startServeWeb(t, cfg, "prod", &out, nil)

	url := waitURL(t, opened, result)
	if !strings.HasSuffix(url, "/?server=prod") {
		t.Errorf("opened %q, want prod's dashboard", url)
	}
	getUntil(t, url, "orders") // the collector's first snapshot is async
	cancel()

	if err := waitResult(t, result); err != nil {
		t.Fatalf("serveWeb() error: %v", err)
	}
	// out is read only after serveWeb returned, which ends its writes.
	if want := "vartop web: " + url; !strings.Contains(out.String(), want) || !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("out = %q, want it to announce %q (a http://127.0.0.1: URL)", out.String(), want)
	}
	if _, status := httpGet(url); status != 0 {
		t.Fatalf("server still answering after shutdown (status %d)", status)
	}
}

func TestServeWebOpenFailureIsNotFatal(t *testing.T) {
	fake := fakeOrdersVarmq(t)
	cfg := &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{fake.URL}}},
	}
	var out bytes.Buffer
	// No server named: the URL is the server's root, which shows the
	// default server.
	cancel, opened, result := startServeWeb(t, cfg, "", &out, errors.New("no xdg-open"))

	url := waitURL(t, opened, result)
	getUntil(t, url+"/", "orders")
	cancel()

	if err := waitResult(t, result); err != nil {
		t.Fatalf("serveWeb() error: %v, want nil: a failed browser launch is only reported", err)
	}
	if !strings.Contains(out.String(), "no xdg-open") {
		t.Fatalf("out = %q, want the open error printed", out.String())
	}
}

func TestServeWebRejectsNonLoopback(t *testing.T) {
	cfg := &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{"http://pod:8080/varmq"}}},
	}
	opened := false
	var out bytes.Buffer
	err := serveWeb(context.Background(), configSource(cfg), "prod", "0.0.0.0:7070", &out, func(string) error {
		opened = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "0.0.0.0:7070") {
		t.Fatalf("serveWeb() error = %v, want one naming 0.0.0.0:7070", err)
	}
	if opened {
		t.Fatal("open was called for a rejected address")
	}
}

func TestServeWebUnknownServer(t *testing.T) {
	cfg := &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{"http://pod:8080/varmq"}}},
	}
	opened := false
	err := serveWeb(context.Background(), configSource(cfg), "nope", "127.0.0.1:0", io.Discard, func(string) error {
		opened = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("serveWeb() error = %v, want a not-found error naming \"nope\"", err)
	}
	if opened {
		t.Fatal("open was called for a server not configured")
	}
}

// TestWebLogsStoreNotices: vartop web logs what the store gets past — a
// stored snapshot it skips because it cannot be decoded — where it logs
// the rest (store.SetLogger).
func TestWebLogsStoreNotices(t *testing.T) {
	dataDir := isolate(t)
	saveServersConfig(t, "prod", &config.Server{Name: "prod", Endpoints: []string{"http://127.0.0.1:1/varmq"}})
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		store.SetLogger(nil)
	})
	origHost, origPort, origNoOpen := webHost, webPort, webNoOpen
	webHost, webPort, webNoOpen = "127.0.0.1", 0, true
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // serve, then shut down at once
	webCmd.SetContext(ctx)
	var out bytes.Buffer
	webCmd.SetOut(&out)
	t.Cleanup(func() {
		webHost, webPort, webNoOpen = origHost, origPort, origNoOpen
		webCmd.SetContext(context.Background())
		webCmd.SetOut(nil)
	})
	if err := webCmd.RunE(webCmd, nil); err != nil {
		t.Fatalf("vartop web = %v; printed %s", err, out.String())
	}

	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	token, _, err := ctl.Claim(store.Lease{PID: 4242}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.OpenServer(dataDir, "prod", true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := w.Append(token, fleet.Tick{Snap: &fleet.Snapshot{At: time.Now()}})
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.PathFor(dataDir, "prod"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`UPDATE snapshots SET data = x'00ff' WHERE id = ?`, id)
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	r, err := store.OpenServer(dataDir, "prod", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, _, err := r.Window(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "cannot be decoded") {
		t.Errorf("logged %q, want the skipped snapshot reported", logged.String())
	}
}

// TestWebSourceChecksEnv (R31): vartop web checks the collector's lease
// against its own environment — its config file.
func TestWebSourceChecksEnv(t *testing.T) {
	isolate(t)
	src, release, err := webSource()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if src.SameEnv == nil {
		t.Fatal("webSource has no SameEnv: the web would act on another environment's collector's units")
	}
	own := ownLease()
	if !src.SameEnv(own) {
		t.Error("SameEnv refuses this vartop's own environment")
	}
	other := own
	other.ConfigPath = "/elsewhere/vartop/config.json"
	if src.SameEnv(other) {
		t.Error("SameEnv takes a collector reading another config file for this environment's")
	}
}

// TestSameEnv (R31): two leases are of one environment when their config
// path agrees, whatever their pid; vartop web's own lease is of its own.
func TestSameEnv(t *testing.T) {
	isolate(t)
	own := ownLease()
	if own.ConfigPath == "" {
		t.Fatalf("ownLease = %+v, want this environment's config path", own)
	}
	with := func(edit func(*store.Lease)) store.Lease { l := own; edit(&l); return l }
	for _, c := range []struct {
		name string
		l    store.Lease
		want bool
	}{
		{"identical", own, true},
		{"another pid", with(func(l *store.Lease) { l.PID++ }), true},
		{"another config path", with(func(l *store.Lease) { l.ConfigPath = "/elsewhere/vartop/config.json" }), false},
	} {
		if got := sameEnv(c.l, own); got != c.want {
			t.Errorf("%s: sameEnv = %v, want %v", c.name, got, c.want)
		}
	}
	if !sameEnvAsOwn()(own) {
		t.Error("sameEnvAsOwn refuses this vartop's own lease")
	}
}

// lockedBuffer is a bytes.Buffer a logger and a test may share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// runCollectLoop runs collectLoop until ctx ends, failing the test if it
// does not return within 5s of that.
func runCollectLoop(t *testing.T, ctx context.Context, run func(context.Context) (int, error), lease func() (store.Lease, bool, error), retry time.Duration, log *slog.Logger) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		collectLoop(ctx, run, lease, retry, log)
	}()
	<-ctx.Done()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collectLoop did not return within 5s of its context ending")
	}
}

// TestCollectLoopLogsOtherCollectorOnce: while another vartop holds the
// lease, every Run returns without collecting; the loop keeps calling it,
// to collect once that one stops, and says once who collects.
func TestCollectLoopLogsOtherCollectorOnce(t *testing.T) {
	var logs lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	run := func(context.Context) (int, error) {
		if runs.Add(1) == 4 {
			cancel()
		}
		return 0, nil
	}
	other := func() (store.Lease, bool, error) { return store.Lease{Token: "other", PID: 4242}, true, nil }
	runCollectLoop(t, ctx, run, other, time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))

	if n := runs.Load(); n != 4 {
		t.Errorf("Run called %d times, want 4: the loop keeps trying while another vartop collects", n)
	}
	if n := strings.Count(logs.String(), "pid=4242"); n != 1 {
		t.Errorf("logged the other collector %d times, want once:\n%s", n, logs.String())
	}
}

// TestCollectLoopRetriesAfterError: a Run that fails is logged, once while
// it fails the same way, and called again retry after it started — not in
// a tight loop — so that collection resumes once the cause is gone.
func TestCollectLoopRetriesAfterError(t *testing.T) {
	var logs lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const retry = 100 * time.Millisecond
	var mu sync.Mutex
	var starts []time.Time
	run := func(ctx context.Context) (int, error) {
		mu.Lock()
		starts = append(starts, time.Now())
		n := len(starts)
		mu.Unlock()
		if n <= 2 {
			return 1, errors.New("open control.db: disk I/O error")
		}
		cancel() // the third collects, until vartop web stops
		return 0, nil
	}
	none := func() (store.Lease, bool, error) { return store.Lease{}, false, nil }
	runCollectLoop(t, ctx, run, none, retry, slog.New(slog.NewTextHandler(&logs, nil)))

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("Run called %d times, want 3", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < retry {
			t.Errorf("Run %d started %v after the one before, want at least %v", i+1, gap, retry)
		}
	}
	if n := strings.Count(logs.String(), "disk I/O error"); n != 1 {
		t.Errorf("logged the failure %d times, want once:\n%s", n, logs.String())
	}
}

// waitStored waits up to timeout for a snapshot of the server keyed key in
// dataDir, and reports whether one came.
func waitStored(dataDir, key string, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		db, err := store.OpenServer(dataDir, key, false)
		if err != nil {
			continue
		}
		latest, err := db.Latest()
		_ = db.Close()
		if err == nil && latest != nil {
			return true
		}
	}
	return false
}

// startRunWeb saves cfg as the config and runs runWeb on a free loopback
// port in the background; the channel yields its result.
func startRunWeb(t *testing.T, ctx context.Context, cfg *config.Config) <-chan error {
	t.Helper()
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() {
		res <- runWeb(ctx, "", "127.0.0.1:0", hubFlags{}, io.Discard, func(string) error { return nil }, slog.New(slog.DiscardHandler))
	}()
	return res
}

// TestWebCollectsWhileRunning: vartop web collects every configured server
// into the store while it runs, and lets the lease go when it stops, so
// that nothing collects after it.
func TestWebCollectsWhileRunning(t *testing.T) {
	dataDir := isolate(t)
	fake := fakeOrdersVarmq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := startRunWeb(t, ctx, &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{fake.URL}}},
	})

	if !waitStored(dataDir, "prod", 5*time.Second) {
		t.Fatal("no snapshot of prod stored within 5s of vartop web starting")
	}
	cancel()
	if err := waitResult(t, res); err != nil {
		t.Fatalf("runWeb() error: %v", err)
	}
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if l, fresh, err := ctl.Lease(); err != nil || fresh {
		t.Errorf("Lease = %+v, fresh %v, %v; want it released once vartop web stopped", l, fresh, err)
	}
}

// TestSecondWebTakesOver: a vartop web started while another vartop
// collects into the same data dir collects nothing while that one beats,
// and takes over once it lets the lease go.
func TestSecondWebTakesOver(t *testing.T) {
	dataDir := isolate(t)
	fake := fakeOrdersVarmq(t)
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	token, ok, err := ctl.Claim(store.Lease{PID: 4242}, time.Now())
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	stopBeat := make(chan struct{})
	beating := make(chan struct{})
	go func() {
		defer close(beating)
		tick := time.NewTicker(store.BeatEvery / 4)
		defer tick.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-tick.C:
				_ = ctl.Beat(token, time.Now())
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := startRunWeb(t, ctx, &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{fake.URL}}},
	})
	// Longer than one watch of the beat (store.DeadAfter): it moved, so
	// this vartop web did not take over.
	if waitStored(dataDir, "prod", store.DeadAfter+time.Second) {
		t.Fatal("vartop web collected while another vartop held the lease")
	}
	close(stopBeat)
	<-beating
	if err := ctl.Release(token); err != nil {
		t.Fatal(err)
	}
	if !waitStored(dataDir, "prod", store.DeadAfter+2*time.Second) {
		t.Fatal("vartop web did not collect once the other vartop let the lease go")
	}
	cancel()
	if err := waitResult(t, res); err != nil {
		t.Fatalf("runWeb() error: %v", err)
	}
}

// isolate points every directory vartop could use at temporary
// ones, and returns the data dir.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	dataDir := filepath.Join(home, "data")
	t.Setenv("VARTOP_DATA_DIR", dataDir)
	return dataDir
}

// TestWebHostPortFlags: vartop web listens on --host and --port, by default
// 127.0.0.1:7070; --addr is gone.
func TestWebHostPortFlags(t *testing.T) {
	for name, want := range map[string]string{"host": "127.0.0.1", "port": "7070"} {
		if f := webCmd.Flags().Lookup(name); f == nil || f.DefValue != want {
			t.Errorf("--%s = %+v, want a flag defaulting to %s", name, f, want)
		}
	}
	if webCmd.Flags().Lookup("addr") != nil {
		t.Error("vartop web still takes --addr")
	}
}

// TestListenAddr: --host and --port make the address listened on; a port
// out of range is refused, naming it.
func TestListenAddr(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"127.0.0.1", 7070, "127.0.0.1:7070"},
		{"localhost", 0, "localhost:0"},
		{"::1", 8080, "[::1]:8080"},
	} {
		if got, err := listenAddr(c.host, c.port); err != nil || got != c.want {
			t.Errorf("listenAddr(%q, %d) = %q, %v; want %q", c.host, c.port, got, err, c.want)
		}
	}
	for _, port := range []int{-1, 65536, 70000} {
		if _, err := listenAddr("127.0.0.1", port); err == nil || !strings.Contains(err.Error(), strconv.Itoa(port)) {
			t.Errorf("listenAddr(port %d) error = %v, want one naming the port", port, err)
		}
	}
}

// TestWebRefusesNonLoopbackHost: --host must be a loopback address; the
// dashboard has no login.
func TestWebRefusesNonLoopbackHost(t *testing.T) {
	isolate(t)
	saveServersConfig(t, "prod", &config.Server{Name: "prod", Endpoints: []string{"http://127.0.0.1:1/varmq"}})
	origHost, origPort, origNoOpen := webHost, webPort, webNoOpen
	webHost, webPort, webNoOpen = "0.0.0.0", 0, true
	webCmd.SetOut(io.Discard)
	t.Cleanup(func() {
		webHost, webPort, webNoOpen = origHost, origPort, origNoOpen
		webCmd.SetOut(nil)
		store.SetLogger(nil)
	})
	webCmd.SetContext(context.Background())
	err := webCmd.RunE(webCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "0.0.0.0") || !strings.Contains(err.Error(), "loopback only") {
		t.Fatalf("vartop web --host 0.0.0.0 = %v, want a refusal naming 0.0.0.0, loopback only", err)
	}
}

// TestWebStopsWhenDataDirRemoved: with its data directory deleted under
// it, vartop web would read the deleted files while its collector writes
// new ones; it stops instead, saying why.
func TestWebStopsWhenDataDirRemoved(t *testing.T) {
	dataDir := isolate(t)
	fake := fakeOrdersVarmq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := startRunWeb(t, ctx, &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{fake.URL}}},
	})
	if !waitStored(dataDir, "prod", 5*time.Second) {
		t.Fatal("no snapshot of prod stored within 5s of vartop web starting")
	}
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "data directory removed") || !strings.Contains(err.Error(), dataDir) {
			t.Errorf("runWeb() = %v, want an error saying %s was removed", err, dataDir)
		}
	case <-time.After(3 * store.BeatEvery):
		t.Fatal("vartop web still runs over a deleted data directory")
	}
}

// TestReaderWebStopsWhenDataDirRemoved: a vartop web that only shows what
// another collects stops too when the data directory is deleted, rather
// than make it anew, collect into it, and refuse every action for reading
// the deleted control.db.
func TestReaderWebStopsWhenDataDirRemoved(t *testing.T) {
	dataDir := isolate(t)
	fake := fakeOrdersVarmq(t)
	ctl, err := store.OpenControl(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	token, ok, err := ctl.Claim(store.Lease{PID: 4242}, time.Now())
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	stopBeat := make(chan struct{})
	beating := make(chan struct{})
	go func() { // the other vartop, beating on into the deleted file
		defer close(beating)
		tick := time.NewTicker(store.BeatEvery / 4)
		defer tick.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-tick.C:
				_ = ctl.Beat(token, time.Now())
			}
		}
	}()
	defer func() { close(stopBeat); <-beating }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := startRunWeb(t, ctx, &config.Config{
		DefaultServer: "prod",
		Servers:       map[string]*config.Server{"prod": {Name: "prod", Endpoints: []string{fake.URL}}},
	})
	time.Sleep(time.Second) // it watched the other one beat: a reader
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "data directory removed") || !strings.Contains(err.Error(), dataDir) {
			t.Errorf("runWeb() = %v, want an error saying %s was removed", err, dataDir)
		}
	case <-time.After(collectRetry + 3*time.Second):
		t.Fatal("a reader vartop web still runs over a deleted data directory")
	}
	if _, err := os.Stat(dataDir); err == nil {
		t.Error("the reader made the deleted data directory anew")
	}
}

// hubCfg is a config of one hub server, "h", polled every second.
func hubCfg(tok string) *config.Config {
	return &config.Config{DefaultServer: "h", Servers: map[string]*config.Server{
		"h": {Name: "h", Hub: true, Token: tok, Interval: 1},
	}}
}

// latestOf reads the newest snapshot of the server keyed key, nil when
// none can be read yet.
func latestOf(dataDir, key string) *fleet.Snapshot {
	db, err := store.OpenServer(dataDir, key, false)
	if err != nil {
		return nil
	}
	defer db.Close()
	latest, _ := db.Latest()
	return latest
}

// TestWebServesHub: with --hub-listen vartop web prints where instances
// connect; an instance of a hub server connects there and the collector
// stores it as the server's unit; the hub stops with the web.
func TestWebServesHub(t *testing.T) {
	dataDir := isolate(t)
	const tok = "hub-token-0123456789abcdef"
	if err := config.Save(hubCfg(tok)); err != nil {
		t.Fatal(err)
	}
	var out lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() {
		res <- runWeb(ctx, "", "127.0.0.1:0", hubFlags{listen: "127.0.0.1:0"}, &out, func(string) error { return nil }, slog.New(slog.DiscardHandler))
	}()
	addr := regexp.MustCompile(`vartop hub: (http://\S+) `)
	var base string
	fleettest.WaitUntil(t, 5*time.Second, "the hub address", func() bool {
		m := addr.FindStringSubmatch(out.String())
		if m != nil {
			base = m[1]
		}
		return m != nil
	})
	if !strings.Contains(out.String(), "hub servers: h") {
		t.Errorf("output does not name the hub servers:\n%s", out.String())
	}
	if strings.Contains(out.String(), "clear text") {
		t.Errorf("warned of clear text on a loopback hub:\n%s", out.String())
	}

	// No instance yet: an empty, error-free snapshot, and pages that say so.
	fleettest.WaitUntil(t, 5*time.Second, "a snapshot with no instance", func() bool {
		s := latestOf(dataDir, "h")
		return s != nil && len(s.Pods) == 0 && s.Err == ""
	})
	// The dashboard line can print after the hub line: wait for it too.
	dashRe := regexp.MustCompile(`vartop web: (http://\S+)`)
	var dash []string
	fleettest.WaitUntil(t, 5*time.Second, "the dashboard address", func() bool {
		dash = dashRe.FindStringSubmatch(out.String())
		return dash != nil
	})
	for path, want := range map[string]string{"/": "0/0 instances", "/grid": "No instances to show.", "/queues": "0/0 instances", "/workflows": "0/0 instances"} {
		resp, err := http.Get(dash[1] + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s with no instance = %d, want 200 with %q", path, resp.StatusCode, want)
		}
	}

	_, v := fleettest.NewVarmq(t, "pod-1", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 2}})
	inst, code := fleettest.DialHub(t, base, tok, "pod-1", fleettest.HubInstanceOptions{Handler: v.Config.Handler})
	if code != http.StatusOK {
		t.Fatalf("instance stream: %d", code)
	}
	fleettest.WaitUntil(t, 10*time.Second, "a snapshot of the hub instance", func() bool {
		s := latestOf(dataDir, "h")
		return s != nil && len(s.Pods) == 1 && s.Pods[0].Instance == "pod-1" && s.Pods[0].Workers["orders"] != nil
	})
	cancel()
	if err := waitResult(t, res); err != nil {
		t.Fatalf("runWeb() error: %v", err)
	}
	select {
	case <-inst.Done():
	case <-time.After(5 * time.Second):
		t.Error("instance stream still open after vartop web stopped")
	}
}

// TestWebHubOff: a hub server without --hub-listen is said so at start,
// and its snapshots carry why it is not collected.
func TestWebHubOff(t *testing.T) {
	dataDir := isolate(t)
	if err := config.Save(hubCfg("hub-token-0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	var out lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() {
		res <- runWeb(ctx, "", "127.0.0.1:0", hubFlags{}, &out, func(string) error { return nil }, slog.New(slog.DiscardHandler))
	}()
	fleettest.WaitUntil(t, 5*time.Second, "a snapshot saying why", func() bool {
		s := latestOf(dataDir, "h")
		return s != nil && strings.Contains(s.Err, "--hub-listen")
	})
	if want := `server "h" is a hub server, but vartop web runs without --hub-listen`; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
	cancel()
	if err := waitResult(t, res); err != nil {
		t.Fatalf("runWeb() error: %v", err)
	}
}

func TestHubFlags(t *testing.T) {
	for _, c := range []struct {
		f    hubFlags
		want string // "" ok
	}{
		{hubFlags{}, ""},
		{hubFlags{listen: ":7071"}, ""},
		{hubFlags{listen: ":7071", cert: "c", key: "k"}, ""},
		{hubFlags{listen: ":7071", cert: "c"}, "go together"},
		{hubFlags{listen: ":7071", key: "k"}, "go together"},
		{hubFlags{cert: "c", key: "k"}, "need --hub-listen"},
		{hubFlags{listen: ":7071", perIP: -1}, "0 (no limit) or more"},
	} {
		err := c.f.check()
		if (c.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v.check() = %v, want %q", c.f, err, c.want)
		}
	}
	for _, name := range []string{"hub-listen", "hub-tls-cert", "hub-tls-key"} {
		if f := webCmd.Flags().Lookup(name); f == nil || f.DefValue != "" {
			t.Errorf("--%s = %+v, want a flag defaulting to empty", name, f)
		}
	}
	if f := webCmd.Flags().Lookup("hub-max-conns-per-ip"); f == nil || f.DefValue != "64" {
		t.Errorf("--hub-max-conns-per-ip = %+v, want a flag defaulting to 64", f)
	}
	if _, _, err := listenHub(hubFlags{listen: "127.0.0.1:0", cert: "/nonexistent/c", key: "/nonexistent/k"}, io.Discard); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("listenHub with a missing cert = %v, want a TLS error", err)
	}
	var out bytes.Buffer
	ln, _, err := listenHub(hubFlags{listen: "0.0.0.0:0"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if !strings.Contains(out.String(), "clear text") {
		t.Errorf("no clear-text warning for a plain hub on 0.0.0.0:\n%s", out.String())
	}
}

// TestWebHubShutdownOrder: Ctrl+C with a poll in flight. The hub closes
// only after the collector stopped, so the last stored snapshot is never
// spoiled by the hub's "disconnected" error. The pages name a hub unit by
// its instance, never by its internal hub.invalid URL.
func TestWebHubShutdownOrder(t *testing.T) {
	const rounds = 8
	for i := range rounds {
		t.Run("round", func(t *testing.T) {
			dataDir := isolate(t)
			const tok = "hub-token-0123456789abcdef"
			if err := config.Save(hubCfg(tok)); err != nil {
				t.Fatal(err)
			}
			var out lockedBuffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := make(chan error, 1)
			go func() {
				res <- runWeb(ctx, "", "127.0.0.1:0", hubFlags{listen: "127.0.0.1:0"}, &out, func(string) error { return nil }, slog.New(slog.DiscardHandler))
			}()
			addr := regexp.MustCompile(`vartop hub: (http://\S+) `)
			var base string
			fleettest.WaitUntil(t, 5*time.Second, "the hub address", func() bool {
				m := addr.FindStringSubmatch(out.String())
				if m != nil {
					base = m[1]
				}
				return m != nil
			})
			_, v := fleettest.NewVarmq(t, "pod-1", map[string]*fleettest.Worker{"orders": {Status: "Running", Concurrency: 2}})
			var armed atomic.Bool
			inTick := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if armed.Load() && r.URL.Path == "/workers" {
					select {
					case inTick <- struct{}{}:
					default:
					}
					<-release
				}
				v.Config.Handler.ServeHTTP(w, r)
			})
			if _, code := fleettest.DialHub(t, base, tok, "pod-1", fleettest.HubInstanceOptions{Handler: h}); code != http.StatusOK {
				t.Fatal(code)
			}
			fleettest.WaitUntil(t, 10*time.Second, "a good snapshot", func() bool {
				s := latestOf(dataDir, "h")
				return s != nil && len(s.Pods) == 1 && s.Pods[0].Err == "" && s.Pods[0].Workers["orders"] != nil
			})
			if i == 0 {
				dash := regexp.MustCompile(`vartop web: (http://\S+)`).FindStringSubmatch(out.String())
				for _, p := range []string{"/grid", "/workers/orders"} {
					resp, err := http.Get(dash[1] + p)
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if strings.Contains(string(body), "hub.invalid") || strings.Contains(string(body), "hub://") {
						t.Errorf("GET %s shows the internal hub URL", p)
					}
					if !strings.Contains(string(body), "pod-1 via hub") {
						t.Errorf("GET %s does not name the unit %q", p, "pod-1 via hub")
					}
				}
			}
			armed.Store(true)
			select {
			case <-inTick:
			case <-time.After(5 * time.Second):
				t.Fatal("no poll in flight")
			}
			cancel()
			if err := waitResult(t, res); err != nil {
				t.Fatalf("runWeb: %v", err)
			}
			if s := latestOf(dataDir, "h"); s == nil || len(s.Pods) != 1 || s.Pods[0].Err != "" {
				t.Errorf("last stored snapshot spoiled at shutdown: %+v", s)
			}
		})
	}
}

// TestHubSyncWarnsOnce: a config that cannot be read keeps the hub
// servers' last tokens (the old token still connects), and says so once
// while that lasts, and once when it reads again.
func TestHubSyncWarnsOnce(t *testing.T) {
	isolate(t)
	const tok = "hub-token-0123456789abcdef"
	if err := config.Save(hubCfg(tok)); err != nil {
		t.Fatal(err)
	}
	var buf lockedBuffer
	r := hub.NewRouter(hub.Options{})
	ts := httptest.NewServer(r)
	t.Cleanup(func() { r.Close(); ts.Close() })
	s := &hubSync{router: r, log: slog.New(slog.NewTextHandler(&buf, nil))}
	s.sync()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"servers": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.sync()
	s.sync()
	if n := strings.Count(buf.String(), "cannot read the config"); n != 1 {
		t.Errorf("%d warnings for one bad config, want 1:\n%s", n, buf.String())
	}
	if _, code := fleettest.DialHub(t, ts.URL, tok, "i1", fleettest.HubInstanceOptions{}); code != http.StatusOK {
		t.Errorf("old token while the config is bad: %d, want 200", code)
	}
	if err := config.Save(hubCfg(tok)); err != nil {
		t.Fatal(err)
	}
	s.sync()
	s.sync()
	if n := strings.Count(buf.String(), "the config reads again"); n != 1 {
		t.Errorf("%d notes of a good config again, want 1:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), tok) {
		t.Error("the token is in the log")
	}
}
