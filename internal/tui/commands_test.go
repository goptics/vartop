package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/discovery"
)

// fakePod is an httptest varmq pod that records the requests it receives.
type fakePod struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD /path"

	workers      []api.WorkerSummary
	failAll      bool // every request answers 500
	failActions  bool // PATCH requests answer 409
	healthStatus int  // 0 means 200
}

func newFakePod(t *testing.T, workers ...api.WorkerSummary) *fakePod {
	t.Helper()
	p := &fakePod{workers: workers}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePod) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+r.URL.Path)
	failAll, failActions, health := p.failAll, p.failActions, p.healthStatus
	p.mu.Unlock()

	if failAll {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		if health != 0 {
			w.WriteHeader(health)
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/workers":
		_ = json.NewEncoder(w).Encode(p.workers)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/workers/"):
		name := strings.TrimPrefix(r.URL.Path, "/workers/")
		for _, wk := range p.workers {
			if wk.Name == name {
				_ = json.NewEncoder(w).Encode(wk)
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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

func tuiWorker(name string) api.WorkerSummary {
	return api.WorkerSummary{Name: name, Status: "Running", Concurrency: 2, NumProcessing: 1, NumPending: 3, NumIdleWorkers: 1}
}

func poolModel(t *testing.T, pods ...*fakePod) *Model {
	t.Helper()
	m := testModel(t)
	urls := make([]string, len(pods))
	for i, p := range pods {
		urls[i] = p.url()
	}
	m.pool = api.NewPool(discovery.NewStatic(urls), "")
	return m
}

// unscope unwraps the scopedMsg tag that data commands apply, checking that the
// tag carries the model's current pool.
func unscope(t *testing.T, m *Model, msg tea.Msg) tea.Msg {
	t.Helper()
	s, ok := msg.(scopedMsg)
	if !ok {
		t.Fatalf("msg = %T, want scopedMsg", msg)
	}
	if s.pool != m.pool {
		t.Fatalf("scopedMsg.pool does not match the model's pool")
	}
	return s.msg
}

// runCmd executes cmd, flattening tea.BatchMsg, and returns every non-nil
// message (scopedMsg tags removed).
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch v := msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, sub := range v {
			out = append(out, runCmd(sub)...)
		}
		return out
	case scopedMsg:
		return []tea.Msg{v.msg}
	default:
		return []tea.Msg{msg}
	}
}

func originsByName(msg workersMsg) map[string]string {
	out := make(map[string]string, len(msg))
	for _, w := range msg {
		out[w.Name] = w.Origin
	}
	return out
}

func TestFetchWorkersCmdMergesAllPods(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"), tuiWorker("beta"))
	p2 := newFakePod(t, tuiWorker("gamma"))
	m := poolModel(t, p1, p2)

	msg := unscope(t, m, m.fetchWorkersCmd()())
	wm, ok := msg.(workersMsg)
	if !ok {
		t.Fatalf("msg = %T (%v), want workersMsg", msg, msg)
	}
	if len(wm) != 3 {
		t.Fatalf("got %d workers, want 3: %+v", len(wm), wm)
	}
	got := originsByName(wm)
	want := map[string]string{"alpha": p1.url(), "beta": p1.url(), "gamma": p2.url()}
	for name, origin := range want {
		if got[name] != origin {
			t.Errorf("worker %s origin = %q, want %q", name, got[name], origin)
		}
	}
}

func TestFetchWorkersCmdOnePodFailingIsTolerated(t *testing.T) {
	cases := map[string]func(p *fakePod){
		"500":    func(p *fakePod) { p.failAll = true },
		"closed": func(p *fakePod) { p.srv.Close() },
	}
	for name, breakPod := range cases {
		t.Run(name, func(t *testing.T) {
			good := newFakePod(t, tuiWorker("alpha"), tuiWorker("beta"))
			bad := newFakePod(t, tuiWorker("gamma"))
			breakPod(bad)
			m := poolModel(t, good, bad)

			msg := unscope(t, m, m.fetchWorkersCmd()())
			wm, ok := msg.(workersMsg)
			if !ok {
				t.Fatalf("msg = %T (%v), want workersMsg", msg, msg)
			}
			if len(wm) != 2 {
				t.Fatalf("got %d workers, want 2: %+v", len(wm), wm)
			}
			for _, w := range wm {
				if w.Origin != good.url() {
					t.Errorf("worker %s origin = %q, want healthy pod %q", w.Name, w.Origin, good.url())
				}
			}
		})
	}
}

func TestFetchWorkersCmdAllPodsFailingReturnsError(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	p2 := newFakePod(t, tuiWorker("beta"))
	p1.failAll = true
	p2.srv.Close()
	m := poolModel(t, p1, p2)

	msg := unscope(t, m, m.fetchWorkersCmd()())
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("msg = %T (%v), want errMsg", msg, msg)
	}
	if em.err == nil {
		t.Fatalf("errMsg.err is nil")
	}
}

func TestFetchWorkersCmdSelectedPodOnlyQueriesThatPod(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	p2 := newFakePod(t, tuiWorker("beta"), tuiWorker("gamma"))
	m := poolModel(t, p1, p2)
	m.selectedPodOrigin = p2.url()

	msg := unscope(t, m, m.fetchWorkersCmd()())
	wm, ok := msg.(workersMsg)
	if !ok {
		t.Fatalf("msg = %T (%v), want workersMsg", msg, msg)
	}
	if len(wm) != 2 {
		t.Fatalf("got %d workers, want 2: %+v", len(wm), wm)
	}
	for _, w := range wm {
		if w.Origin != p2.url() {
			t.Errorf("worker %s origin = %q, want %q", w.Name, w.Origin, p2.url())
		}
	}
	if n := p1.total(); n != 0 {
		t.Errorf("pod 1 received %d requests, want 0: %v", n, p1.seen())
	}
}

func TestFetchWorkersCmdSelectedPodUnknownOrigin(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1)
	m.selectedPodOrigin = "http://127.0.0.1:1"

	msg := unscope(t, m, m.fetchWorkersCmd()())
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("msg = %T, want errMsg", msg)
	}
	if !strings.Contains(em.err.Error(), "http://127.0.0.1:1") {
		t.Errorf("error %q does not mention the origin", em.err)
	}
}

func TestNilPoolCommands(t *testing.T) {
	m := testModel(t)
	if m.pool != nil {
		t.Fatalf("testModel pool should be nil")
	}

	if msg := m.fetchWorkersCmd()(); msg != nil {
		t.Errorf("fetchWorkersCmd with nil pool = %#v, want nil", msg)
	}

	msg := unscope(t, m, m.checkHealthCmd()())
	hm, ok := msg.(healthMsg)
	if !ok {
		t.Fatalf("msg = %T, want healthMsg", msg)
	}
	if hm.ok {
		t.Errorf("healthMsg.ok = true with nil pool, want false")
	}
}

func TestCheckHealthCmdMixedPods(t *testing.T) {
	healthy := newFakePod(t)
	sick := newFakePod(t)
	sick.healthStatus = http.StatusInternalServerError
	m := poolModel(t, healthy, sick)

	msg := unscope(t, m, m.checkHealthCmd()())
	hm, ok := msg.(healthMsg)
	if !ok {
		t.Fatalf("msg = %T, want healthMsg", msg)
	}
	if !hm.ok {
		t.Errorf("healthMsg.ok = false, want true when one pod is healthy")
	}
	if len(hm.status) != 2 {
		t.Fatalf("status map = %v, want 2 entries", hm.status)
	}
	if v, present := hm.status[healthy.url()]; !present || !v {
		t.Errorf("status[%s] = %v (present %v), want true", healthy.url(), v, present)
	}
	if v, present := hm.status[sick.url()]; !present || v {
		t.Errorf("status[%s] = %v (present %v), want false", sick.url(), v, present)
	}
}

func TestCheckHealthCmdAllUnhealthy(t *testing.T) {
	p1 := newFakePod(t)
	p2 := newFakePod(t)
	p1.healthStatus = http.StatusServiceUnavailable
	p2.srv.Close()
	m := poolModel(t, p1, p2)

	msg := unscope(t, m, m.checkHealthCmd()())
	hm, ok := msg.(healthMsg)
	if !ok {
		t.Fatalf("msg = %T, want healthMsg", msg)
	}
	if hm.ok {
		t.Errorf("healthMsg.ok = true, want false when all pods are unhealthy")
	}
	for _, p := range []*fakePod{p1, p2} {
		if v, present := hm.status[p.url()]; !present || v {
			t.Errorf("status[%s] = %v (present %v), want false", p.url(), v, present)
		}
	}
}

func TestHandleHealthHealthyDashboardFetchesWorkers(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1)

	cmd := m.handleHealth(healthMsg{ok: true, status: map[string]bool{p1.url(): true}})
	if !m.isHealthy {
		t.Errorf("isHealthy = false, want true")
	}
	if !m.podHealth[p1.url()] {
		t.Errorf("podHealth not updated: %v", m.podHealth)
	}

	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs (%v), want 1 workersMsg", len(msgs), msgs)
	}
	wm, ok := msgs[0].(workersMsg)
	if !ok || len(wm) != 1 || wm[0].Name != "alpha" {
		t.Fatalf("msg = %#v, want workersMsg with alpha", msgs[0])
	}
}

func TestHandleHealthUnhealthyDoesNotFetch(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1)

	cmd := m.handleHealth(healthMsg{ok: false, status: map[string]bool{p1.url(): false}})
	if m.isHealthy {
		t.Errorf("isHealthy = true, want false")
	}
	if msgs := runCmd(cmd); len(msgs) != 0 {
		t.Errorf("unhealthy handleHealth produced msgs: %v", msgs)
	}
	if n := p1.total(); n != 0 {
		t.Errorf("pod received %d requests, want 0: %v", n, p1.seen())
	}
}

func TestHandleHealthSwitcherActiveDoesNotFetch(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1)
	m.pushScreen(m.switcher)

	cmd := m.handleHealth(healthMsg{ok: true, status: map[string]bool{p1.url(): true}})
	if msgs := runCmd(cmd); len(msgs) != 0 {
		t.Errorf("handleHealth with switcher active produced msgs: %v", msgs)
	}
}

func TestHandleHealthDetailActiveFetchesDetails(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("w"))
	m := poolModel(t, p1)
	m.pushScreen(newDetailScreen("w", p1.url()))

	cmd := m.handleHealth(healthMsg{ok: true, status: map[string]bool{p1.url(): true}})
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs (%v), want 1 workerDetailsMsg", len(msgs), msgs)
	}
	dm, ok := msgs[0].(workerDetailsMsg)
	if !ok {
		t.Fatalf("msg = %T, want workerDetailsMsg", msgs[0])
	}
	if dm == nil || dm.Name != "w" || dm.Origin != p1.url() {
		t.Errorf("details = %+v, want worker w from %s", dm, p1.url())
	}
	if n := p1.count("GET /workers"); n != 0 {
		t.Errorf("worker list was fetched %d times, want 0", n)
	}
	if n := p1.count("GET /workers/w"); n != 1 {
		t.Errorf("worker details fetched %d times, want 1", n)
	}
}

func TestActionWorkerCmdTargetsOriginPodOnly(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	p2 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1, p2)

	msg := unscope(t, m, m.actionWorkerCmd("alpha", p2.url(), "pause")())
	wm, ok := msg.(workersMsg)
	if !ok {
		t.Fatalf("msg = %T (%v), want workersMsg refresh", msg, msg)
	}
	if len(wm) != 2 {
		t.Errorf("refresh returned %d workers, want 2 (merged view)", len(wm))
	}
	if n := p2.count("PATCH /workers/alpha/actions/pause"); n != 1 {
		t.Errorf("pod 2 PATCH count = %d, want 1: %v", n, p2.seen())
	}
	if n := p1.count("PATCH /workers/alpha/actions/pause"); n != 0 {
		t.Errorf("pod 1 PATCH count = %d, want 0: %v", n, p1.seen())
	}
}

func TestActionWorkerCmdErrors(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		p1 := newFakePod(t, tuiWorker("alpha"))
		p1.failActions = true
		m := poolModel(t, p1)

		msg := unscope(t, m, m.actionWorkerCmd("alpha", p1.url(), "pause")())
		em, ok := msg.(errMsg)
		if !ok {
			t.Fatalf("msg = %T (%v), want errMsg", msg, msg)
		}
		if !strings.Contains(em.err.Error(), "failed to pause worker alpha") {
			t.Errorf("error = %q, want it to contain %q", em.err, "failed to pause worker alpha")
		}
		if n := p1.count("GET /workers"); n != 0 {
			t.Errorf("workers refreshed after failed action (%d fetches)", n)
		}
	})

	t.Run("unknown origin", func(t *testing.T) {
		p1 := newFakePod(t, tuiWorker("alpha"))
		m := poolModel(t, p1)

		msg := unscope(t, m, m.actionWorkerCmd("alpha", "http://127.0.0.1:1", "pause")())
		em, ok := msg.(errMsg)
		if !ok {
			t.Fatalf("msg = %T, want errMsg", msg)
		}
		if !strings.Contains(em.err.Error(), "failed to pause worker alpha") {
			t.Errorf("error = %q, want it to contain %q", em.err, "failed to pause worker alpha")
		}
		if n := p1.total(); n != 0 {
			t.Errorf("pod received %d requests, want 0", n)
		}
	})
}

func TestActionCurrentWorkerCmd(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	p2 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1, p2)

	if cmd := m.actionCurrentWorkerCmd("", p1.url(), "pause"); cmd != nil {
		t.Errorf("empty worker name should yield nil cmd")
	}

	msg := unscope(t, m, m.actionCurrentWorkerCmd("alpha", p2.url(), "resume")())
	dm, ok := msg.(workerDetailsMsg)
	if !ok {
		t.Fatalf("msg = %T (%v), want workerDetailsMsg", msg, msg)
	}
	if dm.Origin != p2.url() {
		t.Errorf("details origin = %q, want %q", dm.Origin, p2.url())
	}
	if n := p2.count("PATCH /workers/alpha/actions/resume"); n != 1 {
		t.Errorf("pod 2 PATCH count = %d, want 1", n)
	}
	if n := p1.total(); n != 0 {
		t.Errorf("pod 1 received %d requests, want 0", n)
	}

	p2.mu.Lock()
	p2.failActions = true
	p2.mu.Unlock()
	msg = unscope(t, m, m.actionCurrentWorkerCmd("alpha", p2.url(), "resume")())
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("msg = %T, want errMsg", msg)
	}
	if !strings.Contains(em.err.Error(), "failed to resume worker") {
		t.Errorf("error = %q, want it to contain %q", em.err, "failed to resume worker")
	}
}

func TestClientFor(t *testing.T) {
	t.Run("unknown origin", func(t *testing.T) {
		p1 := newFakePod(t)
		m := poolModel(t, p1)
		c, err := m.clientFor("http://example.invalid:9")
		if err == nil || c != nil {
			t.Fatalf("clientFor = (%v, %v), want error", c, err)
		}
		if !strings.Contains(err.Error(), "http://example.invalid:9") {
			t.Errorf("error %q does not mention the origin", err)
		}
	})

	t.Run("empty origin without clients", func(t *testing.T) {
		m := testModel(t)
		m.pool = api.NewPool(discovery.NewStatic(nil), "")
		_, err := m.clientFor("")
		if err == nil || !strings.Contains(err.Error(), "no varmq pods discovered") {
			t.Fatalf("err = %v, want 'no varmq pods discovered'", err)
		}
	})

	t.Run("nil pool", func(t *testing.T) {
		m := testModel(t)
		if _, err := m.clientFor(""); err == nil {
			t.Fatalf("expected error for nil pool")
		}
	})

	t.Run("empty origin returns first client", func(t *testing.T) {
		p1 := newFakePod(t)
		p2 := newFakePod(t)
		m := poolModel(t, p1, p2)
		c, err := m.clientFor("")
		if err != nil {
			t.Fatalf("clientFor: %v", err)
		}
		if want := m.pool.Clients()[0]; c != want {
			t.Errorf("clientFor(\"\") = %s, want first client %s", c.PrimaryEndpoint(), want.PrimaryEndpoint())
		}
	})

	t.Run("known origin", func(t *testing.T) {
		p1 := newFakePod(t)
		p2 := newFakePod(t)
		m := poolModel(t, p1, p2)
		for _, p := range []*fakePod{p1, p2} {
			c, err := m.clientFor(p.url())
			if err != nil {
				t.Fatalf("clientFor(%s): %v", p.url(), err)
			}
			if c.PrimaryEndpoint() != p.url() {
				t.Errorf("clientFor(%s) = %s", p.url(), c.PrimaryEndpoint())
			}
		}
	})
}

func TestFetchWorkerDetailsCmdRoutesToPod(t *testing.T) {
	w1 := tuiWorker("w")
	w1.Concurrency = 1
	w2 := tuiWorker("w")
	w2.Concurrency = 9
	p1 := newFakePod(t, w1)
	p2 := newFakePod(t, w2)
	m := poolModel(t, p1, p2)

	msg := unscope(t, m, m.fetchWorkerDetailsCmd("w", p2.url())())
	dm, ok := msg.(workerDetailsMsg)
	if !ok {
		t.Fatalf("msg = %T (%v), want workerDetailsMsg", msg, msg)
	}
	if dm.Origin != p2.url() || dm.Concurrency != 9 {
		t.Errorf("details = %+v, want pod 2's worker (concurrency 9)", dm)
	}
	if n := p1.total(); n != 0 {
		t.Errorf("pod 1 received %d requests, want 0", n)
	}
}

func TestFetchWorkerDetailsCmdErrors(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("w"))
	m := poolModel(t, p1)

	if _, ok := unscope(t, m, m.fetchWorkerDetailsCmd("missing", p1.url())()).(errMsg); !ok {
		t.Errorf("missing worker should yield errMsg")
	}
	if _, ok := unscope(t, m, m.fetchWorkerDetailsCmd("w", "http://127.0.0.1:1")()).(errMsg); !ok {
		t.Errorf("unknown origin should yield errMsg")
	}
}

func TestHandleWorkerDetailsStoresOnDetailScreenAndClearsError(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("w"))
	m := poolModel(t, p1)
	d := newDetailScreen("w", p1.url())
	m.pushScreen(d)
	m.err = http.ErrAbortHandler

	details := &api.WorkerDetails{WorkerSummary: tuiWorker("w")}
	if cmd := m.handleWorkerDetails(workerDetailsMsg(details)); cmd != nil {
		t.Errorf("handleWorkerDetails cmd = non-nil, want nil")
	}
	if d.details != details {
		t.Errorf("details not stored on the detail screen")
	}
	if m.err != nil {
		t.Errorf("m.err = %v, want nil", m.err)
	}
	if m.lastUpdate.IsZero() {
		t.Errorf("lastUpdate not set")
	}
}

func TestHandleWorkerDetailsWithoutDetailScreen(t *testing.T) {
	m := testModel(t)
	m.err = http.ErrAbortHandler
	m.handleWorkerDetails(workerDetailsMsg(&api.WorkerDetails{}))
	if m.err != nil {
		t.Errorf("m.err = %v, want nil", m.err)
	}
}

func TestFetchedDetailsFlowThroughUpdate(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("w"))
	m := poolModel(t, p1)
	d := newDetailScreen("w", p1.url())
	m.pushScreen(d)

	next, _ := m.Update(m.fetchWorkerDetailsCmd("w", p1.url())())
	*m = next.(Model)
	if d.details == nil || d.details.Name != "w" {
		t.Fatalf("detail screen details = %+v, want worker w", d.details)
	}
}

func TestUpdateDropsScopedMsgFromReplacedPool(t *testing.T) {
	p1 := newFakePod(t, tuiWorker("alpha"))
	m := poolModel(t, p1)
	cmd := m.fetchWorkersCmd()

	m.pool = api.NewPool(discovery.NewStatic([]string{p1.url()}), "")
	next, _ := m.Update(cmd())
	got := next.(Model)
	if !got.lastUpdate.IsZero() {
		t.Errorf("stale scoped msg was applied (lastUpdate set)")
	}
}

func TestHandleErrorSetsErr(t *testing.T) {
	m := testModel(t)
	want := http.ErrHandlerTimeout
	if cmd := m.handleError(errMsg{want}); cmd != nil {
		t.Errorf("handleError cmd = non-nil, want nil")
	}
	if m.err != want {
		t.Errorf("m.err = %v, want %v", m.err, want)
	}
}

func TestHandleWindowSize(t *testing.T) {
	m := testModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 {
		t.Errorf("size = %dx%d, want 120x40", m.width, m.height)
	}

	// Detail screen active, with and without details loaded.
	d := newDetailScreen("w", "http://localhost:9000")
	m.pushScreen(d)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.width != 100 || m.height != 30 {
		t.Errorf("size = %dx%d, want 100x30", m.width, m.height)
	}
	d.details = &api.WorkerDetails{WorkerSummary: tuiWorker("w")}
	m.handleWindowSize(tea.WindowSizeMsg{Width: 60, Height: 10})
	if m.width != 60 || m.height != 10 {
		t.Errorf("size = %dx%d, want 60x10", m.width, m.height)
	}

	// Tiny terminal must not panic either.
	m.handleWindowSize(tea.WindowSizeMsg{Width: 1, Height: 1})
}
