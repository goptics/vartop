package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		server  *Server
		wantErr bool
	}{
		{
			name:    "endpoints only",
			server:  &Server{Name: "local", Endpoints: []string{"http://localhost:8080/varmq"}},
			wantErr: false,
		},
		{
			name:    "kubernetes service",
			server:  &Server{Name: "k8s", Kubernetes: &KubernetesRef{Context: "prod", Namespace: "default", Service: "varmq"}},
			wantErr: false,
		},
		{
			name:    "kubernetes selector",
			server:  &Server{Name: "k8s", Kubernetes: &KubernetesRef{Context: "prod", Namespace: "default", Selector: "app=varmq"}},
			wantErr: false,
		},
		{
			name:    "both endpoints and kubernetes",
			server:  &Server{Name: "bad", Endpoints: []string{"http://localhost:8080/varmq"}, Kubernetes: &KubernetesRef{Context: "prod", Namespace: "default", Service: "varmq"}},
			wantErr: true,
		},
		{
			name:    "dns",
			server:  &Server{Name: "dns", DNS: &DNSRef{Host: "varmq-headless", Port: 8080}},
			wantErr: false,
		},
		{
			name:    "dns and endpoints",
			server:  &Server{Name: "bad", Endpoints: []string{"http://a:1/varmq"}, DNS: &DNSRef{Host: "varmq", Port: 8080}},
			wantErr: true,
		},
		{
			name:    "dns and kubernetes",
			server:  &Server{Name: "bad", Kubernetes: &KubernetesRef{Context: "prod", Namespace: "default", Service: "varmq"}, DNS: &DNSRef{Host: "varmq", Port: 8080}},
			wantErr: true,
		},
		{
			name:    "dns missing host",
			server:  &Server{Name: "bad", DNS: &DNSRef{Port: 8080}},
			wantErr: true,
		},
		{
			name:    "dns missing port",
			server:  &Server{Name: "bad", DNS: &DNSRef{Host: "varmq"}},
			wantErr: true,
		},
		{
			name:    "dns port out of range",
			server:  &Server{Name: "bad", DNS: &DNSRef{Host: "varmq", Port: 70000}},
			wantErr: true,
		},
		{
			name:    "neither endpoints nor kubernetes",
			server:  &Server{Name: "bad"},
			wantErr: true,
		},
		{
			name:    "kubernetes missing context",
			server:  &Server{Name: "bad", Kubernetes: &KubernetesRef{Namespace: "default", Service: "varmq"}},
			wantErr: true,
		},
		{
			name:    "kubernetes missing namespace",
			server:  &Server{Name: "bad", Kubernetes: &KubernetesRef{Context: "prod", Service: "varmq"}},
			wantErr: true,
		},
		{
			name:    "kubernetes both service and selector",
			server:  &Server{Name: "bad", Kubernetes: &KubernetesRef{Context: "prod", Namespace: "default", Service: "varmq", Selector: "app=varmq"}},
			wantErr: true,
		},
		{
			name:    "kubernetes neither service nor selector",
			server:  &Server{Name: "bad", Kubernetes: &KubernetesRef{Context: "prod", Namespace: "default"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.server.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg := &Config{
		DefaultServer: "prod",
		Servers: map[string]*Server{
			"prod": {
				Name:      "prod",
				Endpoints: []string{"http://localhost:8080/varmq"},
				Token:     "secret",
				Interval:  10,
			},
			"k8s": {
				Name: "k8s",
				Kubernetes: &KubernetesRef{
					Context:   "prod",
					Namespace: "varmq",
					Service:   "mux",
					Port:      8080,
					Path:      "/varmq",
				},
				Interval: 5,
			},
		},
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.DefaultServer != cfg.DefaultServer {
		t.Errorf("DefaultServer = %q, want %q", loaded.DefaultServer, cfg.DefaultServer)
	}
	if len(loaded.Servers) != len(cfg.Servers) {
		t.Fatalf("Servers count = %d, want %d", len(loaded.Servers), len(cfg.Servers))
	}

	k8s, ok := loaded.Servers["k8s"]
	if !ok {
		t.Fatalf("server k8s not found")
	}
	if k8s.Kubernetes == nil {
		t.Fatalf("k8s Kubernetes ref is nil")
	}
	if k8s.Kubernetes.Context != "prod" {
		t.Errorf("Context = %q, want %q", k8s.Kubernetes.Context, "prod")
	}
	if k8s.Kubernetes.Namespace != "varmq" {
		t.Errorf("Namespace = %q, want %q", k8s.Kubernetes.Namespace, "varmq")
	}
	if k8s.Kubernetes.Service != "mux" {
		t.Errorf("Service = %q, want %q", k8s.Kubernetes.Service, "mux")
	}
	if k8s.Kubernetes.Port != 8080 {
		t.Errorf("Port = %d, want %d", k8s.Kubernetes.Port, 8080)
	}
	if k8s.Kubernetes.Path != "/varmq" {
		t.Errorf("Path = %q, want %q", k8s.Kubernetes.Path, "/varmq")
	}

	prod, ok := loaded.Servers["prod"]
	if !ok {
		t.Fatalf("server prod not found")
	}
	if len(prod.Endpoints) != 1 || prod.Endpoints[0] != "http://localhost:8080/varmq" {
		t.Errorf("prod Endpoints = %v, want [http://localhost:8080/varmq]", prod.Endpoints)
	}
	if prod.Token != "secret" {
		t.Errorf("prod Token = %q, want %q", prod.Token, "secret")
	}
	if prod.Interval != 10 {
		t.Errorf("prod Interval = %d, want %d", prod.Interval, 10)
	}
}

// TestSaveIsAtomic: Save replaces config.json instead of rewriting it, so a
// reader (or a crash) never sees it half-written. A hard link to the old file
// keeps the old content only if Save renamed a new file over it.
func TestSaveIsAtomic(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, configDirName)
	path := filepath.Join(dir, configFileName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	const old = `{"servers":{}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(t.TempDir(), "held.json")
	if err := os.Link(path, held); err != nil {
		t.Skipf("no hard links here: %v", err)
	}

	cfg := &Config{
		DefaultServer: "prod",
		Servers:       map[string]*Server{"prod": {Name: "prod", Endpoints: []string{"http://localhost:8080/varmq"}, Interval: 5}},
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if got, _ := os.ReadFile(held); string(got) != old {
		t.Errorf("the file Save replaced now reads %q, want it untouched: Save wrote in place", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("Save left %v behind", leftovers)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("config.json: %v, %v; want mode 0600", info, err)
	}
	loaded, err := Load()
	if err != nil || loaded.DefaultServer != "prod" || loaded.Servers["prod"] == nil {
		t.Errorf("Load() = %+v, %v; want the saved config", loaded, err)
	}
}

// saveProd saves a config of one server, prod, and returns it.
func saveProd(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		DefaultServer: "prod",
		Servers:       map[string]*Server{"prod": {Name: "prod", Endpoints: []string{"http://localhost:8080/varmq"}, Token: "s3cr3t"}},
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	return cfg
}

// TestSaveThroughSymlink: a config.json that links to a file elsewhere (a
// dotfiles repo, say) stays a link; the file it links to gets the config,
// 0600.
func TestSaveThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, configDirName)
	path := filepath.Join(dir, configFileName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "vartop.json")
	if err := os.WriteFile(target, []byte(`{"servers":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	saveProd(t)

	if mode := modeOf(t, path, os.Lstat); mode&os.ModeSymlink == 0 {
		t.Errorf("config.json is %v after Save, want the symlink kept", mode)
	}
	if mode := modeOf(t, target, os.Stat); mode.Perm() != 0600 {
		t.Errorf("link target is %v after Save, want mode 0600", mode)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), `"default_server": "prod"`) {
		t.Errorf("link target holds %s, want the saved config", data)
	}
	for _, d := range []string{dir, filepath.Dir(target)} {
		if leftovers, _ := filepath.Glob(filepath.Join(d, "*.tmp")); len(leftovers) != 0 {
			t.Errorf("Save left %v behind", leftovers)
		}
	}
}

// TestSaveIgnoresStaleTemp: a config.json.tmp left by an older vartop, of
// another mode, neither lends config.json its mode nor stays behind.
func TestSaveIgnoresStaleTemp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, configDirName)
	path := filepath.Join(dir, configFileName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	saveProd(t)

	if mode := modeOf(t, path, os.Stat); mode.Perm() != 0600 {
		t.Errorf("config.json is %v, want mode 0600", mode)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("Save left %v behind", leftovers)
	}
}

// modeOf is the mode stat (os.Stat or os.Lstat) gives path.
func modeOf(t *testing.T, path string, stat func(string) (os.FileInfo, error)) os.FileMode {
	t.Helper()
	fi, err := stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// TestSaveFailureLeavesNoTemp: a write that fails leaves no temporary
// file (it holds the tokens) behind.
func TestSaveFailureLeavesNoTemp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, configDirName)
	path := filepath.Join(dir, configFileName)
	// A directory in config.json's place: the rename onto it fails.
	if err := os.MkdirAll(filepath.Join(path, "x"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DefaultServer: "prod", Servers: map[string]*Server{"prod": {Name: "prod", Endpoints: []string{"http://localhost:8080/varmq"}}}}
	if err := Save(cfg); err == nil {
		t.Fatal("Save() = nil, want an error")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("Save left %v behind", leftovers)
	}
}

func TestLoadValidates(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg := &Config{
		DefaultServer: "bad",
		Servers: map[string]*Server{
			"bad": {Name: "bad"},
		},
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	_, err := Load()
	if err == nil {
		t.Fatalf("Load() expected validation error, got nil")
	}
}

func TestLoadMissingConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	_, err := Load()
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Load() error = %v, want ErrConfigNotFound", err)
	}
}

// TestLoadNullServer guards the validation boundary against a hand-edited or
// partially-written config: {"servers":{"a":null}} unmarshals to a nil *Server,
// which must produce a clean error rather than a nil-pointer panic.
func TestLoadNullServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"servers":{"a":null}}`), 0600); err != nil {
		t.Fatal(err)
	}

	_, err = Load()
	if err == nil {
		t.Fatal("Load() expected error for null server, got nil")
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("Load() error = %v, want it to name server %q", err, "a")
	}
}

func TestShortOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"http://10.0.0.5:8080/varmq":  "10.0.0.5",
		"https://staging.example.com": "staging.example.com",
		"http://[fd00::1]:8080/varmq": "fd00::1",
		"http://pod-0:8080/varmq":     "pod-0",
		"localhost:8080":              "localhost:8080", // no scheme: no host to parse, shown as-is
		"":                            "",
	} {
		if got := ShortOrigin(in); got != want {
			t.Errorf("ShortOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPollInterval(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want time.Duration
	}{{0, 5 * time.Second}, {-1, 5 * time.Second}, {1, time.Second}, {30, 30 * time.Second}} {
		if got := (&Server{Interval: tc.in}).PollInterval(); got != tc.want {
			t.Errorf("Interval %d: got %v want %v", tc.in, got, tc.want)
		}
	}
}
