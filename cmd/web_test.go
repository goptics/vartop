package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/config"
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

// startServeWeb runs serveWeb on a free loopback port in the background. The
// returned channel yields its result; opened yields each URL passed to open.
// Cancel the context to stop it.
func startServeWeb(t *testing.T, cfg *config.Config, key string, out io.Writer, openErr error) (cancel context.CancelFunc, opened <-chan string, result <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	urls := make(chan string, 1)
	res := make(chan error, 1)
	go func() {
		res <- serveWeb(ctx, cfg, key, "127.0.0.1:0", out, func(u string) error {
			urls <- u
			return openErr
		})
	}()
	return cancel, urls, res
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
	getUntil(t, url+"/", "orders") // the first snapshot is async
	cancel()

	if err := waitResult(t, result); err != nil {
		t.Fatalf("serveWeb() error: %v", err)
	}
	// out is read only after serveWeb returned, which ends its writes.
	if want := "vartop web: " + url; !strings.Contains(out.String(), want) || !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("out = %q, want it to announce %q (a http://127.0.0.1: URL)", out.String(), want)
	}
	if _, status := httpGet(url + "/"); status != 0 {
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
	cancel, opened, result := startServeWeb(t, cfg, "prod", &out, errors.New("no xdg-open"))

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
	err := serveWeb(context.Background(), cfg, "prod", "0.0.0.0:7070", &out, func(string) error {
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
	err := serveWeb(context.Background(), cfg, "nope", "127.0.0.1:0", io.Discard, func(string) error {
		opened = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("serveWeb() error = %v, want a not-found error naming \"nope\"", err)
	}
	if opened {
		t.Fatal("open was called although no server was connected")
	}
}
