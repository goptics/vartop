package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetActiveServerResolution(t *testing.T) {
	a := &Server{Name: "a", Endpoints: []string{"http://a/varmq"}}
	b := &Server{Name: "b", Endpoints: []string{"http://b/varmq"}}
	cfg := &Config{DefaultServer: "a", Servers: map[string]*Server{"a": a, "b": b}}

	got, err := cfg.GetActiveServer("b")
	if err != nil || got != b {
		t.Fatalf("explicit name: got %v, %v; want b", got, err)
	}
	got, err = cfg.GetActiveServer("")
	if err != nil || got != a {
		t.Fatalf("default: got %v, %v; want a", got, err)
	}
	if _, err = cfg.GetActiveServer("nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown name: got %v, want not-found error", err)
	}

	noDefault := &Config{Servers: map[string]*Server{"a": a}}
	if _, err = noDefault.GetActiveServer(""); err == nil || !strings.Contains(err.Error(), "no default") {
		t.Fatalf("missing default: got %v, want no-default error", err)
	}
	// An explicit name still works without a default.
	if got, err = noDefault.GetActiveServer("a"); err != nil || got != a {
		t.Fatalf("explicit name without default: got %v, %v", got, err)
	}

	danglingDefault := &Config{DefaultServer: "gone", Servers: map[string]*Server{"a": a}}
	if _, err = danglingDefault.GetActiveServer(""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("dangling default: got %v, want not-found error", err)
	}
}

func TestDisplayEndpointCounts(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []string
		want      string
	}{
		{"none", nil, "(no endpoints)"},
		{"one", []string{"http://a:8080/varmq"}, "http://a:8080/varmq"},
		{"three", []string{"http://a", "http://b", "http://c"}, "3 pods"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{Name: "s", Endpoints: tt.endpoints}
			if got := s.DisplayEndpoint(); got != tt.want {
				t.Fatalf("DisplayEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSaveCreatesDirAndFileWithMode0600(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg := &Config{
		DefaultServer: "a",
		Servers:       map[string]*Server{"a": {Name: "a", Endpoints: []string{"http://a/varmq"}, Token: "t"}},
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	dir := filepath.Join(tmp, "vartop")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("config dir not created: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file mode = %o, want 600", perm)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vartop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load()
	if err == nil {
		t.Fatalf("expected parse error, got config %+v", cfg)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error %q should mention parsing", err.Error())
	}
}

func TestLoadRoundTripsDNSServerBlock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	in := &Config{
		DefaultServer: "d",
		Servers: map[string]*Server{
			"d": {Name: "d", DNS: &DNSRef{Host: "varmq.svc", Port: 9090, Path: "/custom"}, Token: "tok", Interval: 4},
		},
	}
	if err := Save(in); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	out, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := out.Servers["d"]
	if got == nil || got.DNS == nil {
		t.Fatalf("dns block lost: %+v", got)
	}
	if *got.DNS != *in.Servers["d"].DNS {
		t.Fatalf("DNS = %+v, want %+v", *got.DNS, *in.Servers["d"].DNS)
	}
	if got.Token != "tok" || got.Interval != 4 || len(got.Endpoints) != 0 || got.Kubernetes != nil {
		t.Fatalf("unexpected server after round trip: %+v", got)
	}
	if out.DefaultServer != "d" {
		t.Fatalf("DefaultServer = %q, want d", out.DefaultServer)
	}
}
