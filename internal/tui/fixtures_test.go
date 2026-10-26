package tui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/config"
	"github.com/goptics/vartop/internal/daemon"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/fleet/fleettest"
	"github.com/goptics/vartop/internal/store"
)

// TestMain keeps every test off the user's config: a model reads the config
// again only through Model.load, which the fixture sets to its own.
func TestMain(m *testing.M) {
	loadConfig = func() (*config.Config, error) { return nil, errors.New("tests read no config file") }
	os.Exit(m.Run())
}

// fixture is a store in a temporary data dir for a TUI to read. The test
// writes the snapshots itself under a lease it holds (claim, write), or a
// daemon running in-process collects them (startDaemon).
type fixture struct {
	t   *testing.T
	dir string // the data dir
	ctl *store.Control

	mu       sync.Mutex
	cfg      *config.Config // what the daemon and the TUI read; replaced whole, never changed
	token    string         // the lease the test holds, "" if none
	starts   int            // calls of the model's start
	startErr error          // what they return
	run      *daemonRun     // the daemon running, nil if none
}

// newFixture is a fresh data dir for cfg, its servers named by their keys.
// Nothing collects yet.
func newFixture(t *testing.T, cfg *config.Config) *fixture {
	t.Helper()
	for name, s := range cfg.Servers {
		s.Name = name
	}
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), "data"), cfg: cfg}
	ctl, err := store.OpenControl(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	f.ctl = ctl
	return f
}

func (f *fixture) config() (*config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, nil
}

// setConfig replaces the config the daemon and the TUI read.
func (f *fixture) setConfig(cfg *config.Config) {
	for name, s := range cfg.Servers {
		s.Name = name
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
}

// start is the model's start: it counts its calls.
func (f *fixture) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return f.startErr
}

func (f *fixture) started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fixture) failStart(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr = err
}

// model is a 120×40 TUI showing the server keyed key, its feed synced once.
// Its feeds are closed when the test ends.
func (f *fixture) model(key string) *Model {
	f.t.Helper()
	return f.modelFor(f.t, key)
}

// modelFor is model for test t, which may be a subtest of the fixture's.
func (f *fixture) modelFor(t *testing.T, key string) *Model {
	t.Helper()
	cfg, _ := f.config()
	m := NewModel(cfg, key, f.dir, f.ctl, f.start)
	m.load = f.config
	t.Cleanup(func() { _ = m.Close() })
	deliver(&m, tea.WindowSizeMsg{Width: 120, Height: 40})
	syncNow(t, &m)
	return &m
}

// claim takes the lease, as a daemon does, so that the test writes the
// snapshots (see write).
func (f *fixture) claim() {
	f.t.Helper()
	token, ok, err := f.ctl.Claim(store.Lease{PID: os.Getpid(), Build: "test"}, time.Now())
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

// write stores s as the newest snapshot of the server keyed key, under the
// lease the test holds.
func (f *fixture) write(key string, s *fleet.Snapshot) {
	f.t.Helper()
	f.mu.Lock()
	token := f.token
	f.mu.Unlock()
	db, err := store.OpenServer(f.dir, key, true)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
		f.t.Fatalf("write %s: %v", key, err)
	}
}

// pokes is how many times the server keyed key was poked.
func (f *fixture) pokes(key string) int64 {
	f.t.Helper()
	pokes, err := f.ctl.Pokes()
	if err != nil {
		f.t.Fatal(err)
	}
	return pokes[key]
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
		DataDir: f.dir,
		Config:  func() (*config.Config, string, error) { cfg, err := f.config(); return cfg, "", err },
		Lease:   store.Lease{PID: os.Getpid(), Build: "test"},
		Log:     slog.New(slog.DiscardHandler),
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

// deliver hands msg to the model and keeps the model Update returns, as
// Bubble Tea does (Update has a value receiver).
func deliver(m *Model, msg tea.Msg) tea.Cmd {
	next, cmd := m.Update(msg)
	*m = next.(Model)
	return cmd
}

// press delivers a key.
func press(m *Model, key tea.KeyMsg) tea.Cmd { return deliver(m, key) }

// runeKey builds a printable-rune key message such as "j" or "q".
func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// isQuit reports whether cmd is tea.Quit.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// syncNow syncs the shown server's feed, as a tick does, and delivers the
// result.
func syncNow(t *testing.T, m *Model) {
	t.Helper()
	cmd := m.syncCmd(m.feed, m.gen)
	if cmd == nil {
		return // no server to sync
	}
	msg, ok := cmd().(syncedMsg)
	if !ok {
		t.Fatalf("syncCmd produced %T, want syncedMsg", msg)
	}
	deliver(m, msg)
}

// reloadConfig reads the config again, as a tick does, delivers it, and
// runs what that returns (none of it waits for a timer).
func reloadConfig(t *testing.T, m *Model) {
	t.Helper()
	if msg := m.configCmd()(); msg != nil {
		runAll(m, deliver(m, msg))
	}
}

// runAll runs cmd, a batch's commands each, and delivers what they return,
// recursively. It is only for commands that wait for no timer.
func runAll(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			runAll(m, c)
		}
	default:
		runAll(m, deliver(m, msg))
	}
}

// waitRows syncs m until its dashboard lists want rows.
func waitRows(t *testing.T, m *Model, want int) {
	t.Helper()
	fleettest.WaitUntil(t, 5*time.Second, "rows", func() bool {
		syncNow(t, m)
		return len(m.dashboardScreen().table.Rows()) == want
	})
}

// rowsOf is the dashboard's rows, each its cells joined by "|", trimmed.
func rowsOf(m *Model) []string {
	var rows []string
	for _, r := range m.dashboardScreen().table.Rows() {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = strings.TrimSpace(c)
		}
		rows = append(rows, strings.Join(cells, "|"))
	}
	return rows
}

// Snapshot fixtures.

// smp builds a sample with the fields the TUI shows.
func smp(status string, conc, busy, idle, pending int) *fleet.Sample {
	s := &fleet.Sample{At: time.Now()}
	s.Status, s.Concurrency, s.NumProcessing, s.NumIdleWorkers, s.NumPending = status, conc, busy, idle, pending
	return s
}

// unit is a healthy direct unit at key, labelled label.
func unit(label, key string, workers map[string]*fleet.Sample) fleet.PodSnapshot {
	return fleet.PodSnapshot{Key: key, Via: key, Label: label, Workers: workers}
}

// downUnit is a unit at key, labelled label, that answered nothing.
func downUnit(label, key, err string) fleet.PodSnapshot {
	return fleet.PodSnapshot{Key: key, Via: key, Label: label, Err: err}
}

// snapAt is a snapshot taken at at.
func snapAt(at time.Time, pods ...fleet.PodSnapshot) *fleet.Snapshot {
	return &fleet.Snapshot{At: at.Round(0), Pods: pods}
}

// fresh is a snapshot taken now.
func fresh(pods ...fleet.PodSnapshot) *fleet.Snapshot { return snapAt(time.Now(), pods...) }

// fakePod is an httptest varmq pod that records the requests it receives.
type fakePod struct {
	srv *httptest.Server

	mu          sync.Mutex
	requests    []string // "METHOD /path"
	auth        []string // each request's Authorization header, same order
	failActions bool     // PATCH requests answer 409
}

func newFakePod(t *testing.T) *fakePod {
	t.Helper()
	p := &fakePod{}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePod) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+r.URL.Path)
	p.auth = append(p.auth, r.Header.Get("Authorization"))
	failActions := p.failActions
	p.mu.Unlock()

	switch {
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/actions/"):
		if failActions {
			http.Error(w, `{"error":"action rejected"}`, http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (p *fakePod) url() string { return p.srv.URL }

func (p *fakePod) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func (p *fakePod) count(req string) int {
	n := 0
	for _, r := range p.seen() {
		if r == req {
			n++
		}
	}
	return n
}

func (p *fakePod) total() int { return len(p.seen()) }

// auths is the Authorization header of each request received.
func (p *fakePod) auths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auth...)
}

func (p *fakePod) rejectActions() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failActions = true
}
