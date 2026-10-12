package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	path, err := configPath()
	if err != nil {
		t.Fatalf("configPath() error: %v", err)
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
