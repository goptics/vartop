package cmd

import (
	"strings"
	"testing"

	"github.com/goptics/vartop/internal/config"
)

func TestServerSetEndpointOnKubernetesServerRejected(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg := &config.Config{
		DefaultServer: "k8s",
		Servers: map[string]*config.Server{
			"k8s": {
				Name: "k8s",
				Kubernetes: &config.KubernetesRef{
					Context:   "prod",
					Namespace: "default",
					Service:   "varmq",
				},
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	err := serverSetCmd.RunE(serverSetCmd, []string{"k8s", "endpoint", "http://localhost:8080/varmq"})
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one of endpoints, kubernetes or dns") {
		t.Fatalf("error %q does not mention endpoints/kubernetes conflict", err.Error())
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	srv, ok := loaded.Servers["k8s"]
	if !ok {
		t.Fatalf("server k8s not found after failed set")
	}
	if len(srv.Endpoints) != 0 {
		t.Fatalf("Endpoints was saved despite validation failure: %v", srv.Endpoints)
	}
	if srv.Kubernetes == nil {
		t.Fatalf("Kubernetes block missing after failed set")
	}
}

func TestServerSetTokenOnKubernetesServerAccepted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cfg := &config.Config{
		DefaultServer: "k8s",
		Servers: map[string]*config.Server{
			"k8s": {
				Name: "k8s",
				Kubernetes: &config.KubernetesRef{
					Context:   "prod",
					Namespace: "default",
					Service:   "varmq",
				},
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	err := serverSetCmd.RunE(serverSetCmd, []string{"k8s", "token", "new-token"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.Servers["k8s"].Token != "new-token" {
		t.Fatalf("Token = %q, want %q", loaded.Servers["k8s"].Token, "new-token")
	}
}

func TestServerSetHistory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &config.Config{DefaultServer: "prod", Servers: map[string]*config.Server{
		"prod": {Name: "prod", Endpoints: []string{"http://localhost:8080/varmq"}},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := serverSetCmd.RunE(serverSetCmd, []string{"prod", "history", "7d"}); err != nil {
		t.Fatalf("set history 7d: %v", err)
	}
	if err := serverSetCmd.RunE(serverSetCmd, []string{"prod", "history", "90d"}); err == nil || !strings.Contains(err.Error(), "30d") {
		t.Fatalf("set history 90d: err %v, want one saying the most is 30d", err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Servers["prod"].History; got != "7d" {
		t.Fatalf("History = %q, want 7d (the refused 90d not saved)", got)
	}
}
