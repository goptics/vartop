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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/collect"
	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/fleet"
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
// cfg its config and a daemon collecting it in-process until the test
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
			Lease:   store.Lease{PID: os.Getpid(), Build: "test"},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return web.Source{
		DataDir: dataDir,
		Config:  func() (*config.Config, error) { return cfg, nil },
		Control: ctl,
		Start:   func() error { return nil },
	}
}

// startServeWeb runs serveWeb over a daemon collecting cfg (see
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
	getUntil(t, url, "orders") // the daemon's first snapshot is async
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
	origAddr, origNoOpen := webAddr, webNoOpen
	webAddr, webNoOpen = "127.0.0.1:0", true
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // serve, then shut down at once
	webCmd.SetContext(ctx)
	var out bytes.Buffer
	webCmd.SetOut(&out)
	t.Cleanup(func() {
		webAddr, webNoOpen = origAddr, origNoOpen
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

// TestWebSourceChecksEnv (R31): vartop web checks the daemon's lease
// against its own environment — its config file and KUBECONFIG.
func TestWebSourceChecksEnv(t *testing.T) {
	isolate(t)
	src, release, err := webSource()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if src.SameEnv == nil {
		t.Fatal("webSource has no SameEnv: the web would act on another environment's daemon's units")
	}
	own := ownLease()
	if !src.SameEnv(own) {
		t.Error("SameEnv refuses this vartop's own environment")
	}
	other := own
	other.ConfigPath = "/elsewhere/vartop/config.json"
	if src.SameEnv(other) {
		t.Error("SameEnv takes a daemon reading another config file for this environment's")
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
		res <- runWeb(ctx, "", "127.0.0.1:0", io.Discard, func(string) error { return nil }, slog.New(slog.DiscardHandler))
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
				_, _ = ctl.Beat(token, time.Now())
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
