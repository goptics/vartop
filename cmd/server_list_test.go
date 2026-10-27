package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/goptics/vartop/internal/config"
)

// captureStdout runs fn while os.Stdout is redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	runErr := fn()

	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, runErr
}

func saveServersConfig(t *testing.T, def string, servers ...*config.Server) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &config.Config{DefaultServer: def, Servers: map[string]*config.Server{}}
	for _, s := range servers {
		cfg.Servers[s.Name] = s
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
}

func healthyAndDeadServers(t *testing.T) (healthy, dead string) {
	t.Helper()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/varmq/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ok.Close)

	gone := httptest.NewServer(http.NotFoundHandler())
	deadURL := gone.URL
	gone.Close() // nothing listens here any more

	return ok.URL + "/varmq", deadURL + "/varmq"
}

func TestServerListShowsAllKindsAndStatus(t *testing.T) {
	healthy, dead := healthyAndDeadServers(t)
	saveServersConfig(t, "live",
		&config.Server{Name: "live", Endpoints: []string{healthy}, Token: "super-secret", Interval: 5, History: "7d"},
		&config.Server{Name: "down", Endpoints: []string{dead}, Interval: 3},
		&config.Server{Name: "kube", Kubernetes: &config.KubernetesRef{Context: "ctx", Namespace: "ns", Service: "varmq"}},
		&config.Server{Name: "dnsy", DNS: &config.DNSRef{Host: "varmq.local", Port: 8080}},
	)

	out, err := captureStdout(t, func() error { return serverListCmd.RunE(serverListCmd, nil) })
	if err != nil {
		t.Fatalf("server list error: %v", err)
	}

	for _, want := range []string{
		"Default Server: live",
		"* live [Active]",
		"  down [Inactive]",
		"  kube [kubernetes ctx/ns]",
		"  dnsy [dns varmq.local:8080]",
		"Endpoint: " + healthy,
		"Endpoint: " + dead,
		"Token:    ***",
		"Interval: 5s",
		"Interval: 3s",
		"History:  7d",
		"History:  1d", // unset: the default
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "super-secret") {
		t.Errorf("token leaked in clear text:\n%s", out)
	}
	if n := strings.Count(out, "Token:    ***"); n != 1 {
		t.Errorf("expected exactly one masked token line, got %d", n)
	}
	// Only the default server carries the "* " marker.
	if n := strings.Count(out, "* "); n != 1 {
		t.Errorf("expected exactly one default marker, got %d\n%s", n, out)
	}
}

func TestServerListNoConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := captureStdout(t, func() error { return serverListCmd.RunE(serverListCmd, nil) })
	if err == nil {
		t.Fatalf("expected error when no config exists")
	}
}

func twoStaticServers(t *testing.T) {
	t.Helper()
	saveServersConfig(t, "a",
		&config.Server{Name: "a", Endpoints: []string{"http://a.example/varmq"}},
		&config.Server{Name: "b", Endpoints: []string{"http://b.example/varmq"}},
	)
}

func TestServerUseSetsDefault(t *testing.T) {
	twoStaticServers(t)

	if _, err := captureStdout(t, func() error { return serverUseCmd.RunE(serverUseCmd, []string{"b"}) }); err != nil {
		t.Fatalf("server use error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DefaultServer != "b" {
		t.Fatalf("DefaultServer = %q, want b", cfg.DefaultServer)
	}
}

func TestServerUseUnknownErrors(t *testing.T) {
	twoStaticServers(t)

	_, err := captureStdout(t, func() error { return serverUseCmd.RunE(serverUseCmd, []string{"nope"}) })
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DefaultServer != "a" {
		t.Fatalf("DefaultServer changed to %q after failed use", cfg.DefaultServer)
	}
}

func TestServerRemoveByName(t *testing.T) {
	twoStaticServers(t)

	if _, err := captureStdout(t, func() error { return serverRemoveCmd.RunE(serverRemoveCmd, []string{"a"}) }); err != nil {
		t.Fatalf("server remove error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if _, ok := cfg.Servers["a"]; ok {
		t.Fatalf("server a still present after remove")
	}
	if _, ok := cfg.Servers["b"]; !ok {
		t.Fatalf("server b missing after removing a")
	}
	if cfg.DefaultServer != "b" {
		t.Fatalf("DefaultServer = %q, want reassigned to b", cfg.DefaultServer)
	}
}

// TestServerRemoveLastSaysWhatPrompts: removing the last server says
// which command asks for a new one: vartop web (bare vartop prints help).
func TestServerRemoveLastSaysWhatPrompts(t *testing.T) {
	saveServersConfig(t, "only", &config.Server{Name: "only", Endpoints: []string{"http://pod:8080/varmq"}})
	out, err := captureStdout(t, func() error { return serverRemoveCmd.RunE(serverRemoveCmd, []string{"only"}) })
	if err != nil {
		t.Fatalf("server remove error: %v", err)
	}
	if want := "Removed the last server. vartop web will prompt for a new one, or add one with vartop server add."; !strings.Contains(out, want) {
		t.Errorf("printed %q, want %q", out, want)
	}
}

func TestServerRemoveUnknownAndMissingName(t *testing.T) {
	twoStaticServers(t)

	_, err := captureStdout(t, func() error { return serverRemoveCmd.RunE(serverRemoveCmd, []string{"nope"}) })
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	_, err = captureStdout(t, func() error { return serverRemoveCmd.RunE(serverRemoveCmd, nil) })
	if err == nil {
		t.Fatalf("expected error when no name and no --all")
	}
}

func TestServerRemoveAll(t *testing.T) {
	twoStaticServers(t)

	removeAll = true
	t.Cleanup(func() { removeAll = false })
	if _, err := captureStdout(t, func() error { return serverRemoveCmd.RunE(serverRemoveCmd, nil) }); err != nil {
		t.Fatalf("server remove --all error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Servers) != 0 || cfg.DefaultServer != "" {
		t.Fatalf("expected empty config, got default=%q servers=%d", cfg.DefaultServer, len(cfg.Servers))
	}
}

func TestServerSetIntervalZeroRejected(t *testing.T) {
	twoStaticServers(t)

	_, err := captureStdout(t, func() error { return serverSetCmd.RunE(serverSetCmd, []string{"a", "interval", "0"}) })
	if err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("expected positive-integer error, got %v", err)
	}
	_, err = captureStdout(t, func() error { return serverSetCmd.RunE(serverSetCmd, []string{"a", "interval", "abc"}) })
	if err == nil {
		t.Fatalf("expected error for non-numeric interval")
	}
}

func TestServerSetIntervalValid(t *testing.T) {
	twoStaticServers(t)

	if _, err := captureStdout(t, func() error { return serverSetCmd.RunE(serverSetCmd, []string{"a", "interval", "7"}) }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Servers["a"].Interval != 7 {
		t.Fatalf("Interval = %d, want 7", cfg.Servers["a"].Interval)
	}
}

func TestServerSetEmptyTokenClears(t *testing.T) {
	saveServersConfig(t, "a", &config.Server{Name: "a", Endpoints: []string{"http://a.example/varmq"}, Token: "tok"})

	if _, err := captureStdout(t, func() error { return serverSetCmd.RunE(serverSetCmd, []string{"a", "token", ""}) }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Servers["a"].Token != "" {
		t.Fatalf("Token = %q, want cleared", cfg.Servers["a"].Token)
	}
}

func TestServerSetUnknownKeyAndServer(t *testing.T) {
	twoStaticServers(t)

	_, err := captureStdout(t, func() error { return serverSetCmd.RunE(serverSetCmd, []string{"a", "foo", "x"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
	_, err = captureStdout(t, func() error { return serverSetCmd.RunE(serverSetCmd, []string{"nope", "token", "x"}) })
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestServerSetEndpointOnDNSServerRejected(t *testing.T) {
	saveServersConfig(t, "d", &config.Server{Name: "d", DNS: &config.DNSRef{Host: "varmq.local", Port: 8080}})

	_, err := captureStdout(t, func() error {
		return serverSetCmd.RunE(serverSetCmd, []string{"d", "endpoint", "http://x.example/varmq"})
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one of endpoints") {
		t.Fatalf("expected validation error, got %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Servers["d"].Endpoints) != 0 || cfg.Servers["d"].DNS == nil {
		t.Fatalf("dns server was modified despite validation failure: %+v", cfg.Servers["d"])
	}
}
