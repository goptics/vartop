package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goptics/vartop/internal/config"
)

// recorder is a fake varmq pod that records request paths and answers with
// the given status.
type recorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *recorder) serve(t *testing.T, status int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.Method+" "+req.URL.Path)
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func saveStatic(t *testing.T, endpoints ...string) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &config.Config{
		DefaultServer: "multi",
		Servers: map[string]*config.Server{
			"multi": {Name: "multi", Endpoints: endpoints},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
}

func runCmd(t *testing.T, args []string, origin string) (string, error) {
	t.Helper()
	actionOrigin = origin
	t.Cleanup(func() { actionOrigin = "" })
	var out bytes.Buffer
	actionCmd.SetOut(&out)
	t.Cleanup(func() { actionCmd.SetOut(nil) })
	err := actionCmd.RunE(actionCmd, args)
	return out.String(), err
}

func TestActionFansOutToAllStaticPods(t *testing.T) {
	var a, b recorder
	srvA, srvB := a.serve(t, http.StatusOK), b.serve(t, http.StatusOK)
	saveStatic(t, srvA.URL, srvB.URL)

	out, err := runCmd(t, []string{"emails", "pause"}, "")
	if err != nil {
		t.Fatalf("action error: %v\n%s", err, out)
	}
	for _, r := range []*recorder{&a, &b} {
		if len(r.paths) != 1 || r.paths[0] != "PATCH /workers/emails/actions/pause" {
			t.Fatalf("pod paths = %v, want one pause action", r.paths)
		}
	}
	if got := strings.Count(out, ": ok"); got != 2 {
		t.Fatalf("want 2 per-pod ok lines, got %d:\n%s", got, out)
	}
}

func TestActionOriginNarrowsToOnePod(t *testing.T) {
	var a, b recorder
	srvA, srvB := a.serve(t, http.StatusOK), b.serve(t, http.StatusOK)
	saveStatic(t, srvA.URL, srvB.URL)

	if out, err := runCmd(t, []string{"emails", "stop"}, srvB.URL); err != nil {
		t.Fatalf("action error: %v\n%s", err, out)
	}
	if len(a.paths) != 0 || len(b.paths) != 1 {
		t.Fatalf("a=%v b=%v, want only b hit", a.paths, b.paths)
	}

	if _, err := runCmd(t, []string{"emails", "stop"}, "http://nope:1"); err == nil {
		t.Fatalf("expected error for unknown origin")
	}
}

func TestActionReportsPartialFailure(t *testing.T) {
	var a, b recorder
	srvA, srvB := a.serve(t, http.StatusOK), b.serve(t, http.StatusInternalServerError)
	saveStatic(t, srvA.URL, srvB.URL)

	out, err := runCmd(t, []string{"emails", "resume"}, "")
	if err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Fatalf("err = %v, want 1 of 2 failed", err)
	}
	if !strings.Contains(out, ": ok") || !strings.Contains(out, ": failed") {
		t.Fatalf("missing per-pod lines:\n%s", out)
	}
	if len(a.paths) != 1 || len(b.paths) != 1 {
		t.Fatalf("a=%v b=%v, want both hit", a.paths, b.paths)
	}
}
