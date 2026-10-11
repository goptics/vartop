package discovery

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

func TestRegistryConcurrent(t *testing.T) {
	r := newRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%26))
			r.add(key)
			r.remove(key)
		}(i)
	}
	wg.Wait()

	if got := r.Endpoints(); len(got) != 0 {
		t.Fatalf("want empty after churn, got %v", got)
	}

	r.add("http://b:8080/varmq")
	r.add("http://a:8080/varmq")
	r.add("http://c:8080/varmq")
	r.remove("http://b:8080/varmq")

	want := []string{"http://a:8080/varmq", "http://c:8080/varmq"}
	endpoints := r.Endpoints()
	if len(endpoints) != len(want) {
		t.Fatalf("got %v, want %v", endpoints, want)
	}
	for i := range want {
		if endpoints[i] != want[i] {
			t.Fatalf("not sorted: got %v, want %v", endpoints, want)
		}
	}

	// Snapshot must not alias internal map.
	before := r.Endpoints()
	r.add("http://a:8080/varmq") // no-op duplicate
	r.remove("http://a:8080/varmq")
	if before[0] != "http://a:8080/varmq" {
		t.Fatal("snapshot aliased internal map")
	}
}

func TestNewStatic(t *testing.T) {
	s := NewStatic([]string{"http://z:1/varmq", "http://a:1/varmq", "http://m:1/varmq"})
	defer s.Close()
	got := s.Endpoints()
	want := []string{"http://a:1/varmq", "http://m:1/varmq", "http://z:1/varmq"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("static Close: %v", err)
	}
}

func TestK8sSourceURLs(t *testing.T) {
	ready := true
	slice := &discoveryv1.EndpointSlice{
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{}, // no ports: fixed port must win
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.5", "fe62:db8::1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			{Addresses: []string{"10.0.0.6"}},
		},
	}

	k := &k8sSource{path: "/varmq", fixedPort: 0}
	if got := k.urls(slice); len(got) != 0 {
		t.Fatalf("want nil, got %v", got)
	}

	k = &k8sSource{path: "/varmq", fixedPort: 8080}
	got := k.urls(slice)
	want := []string{"http://10.0.0.5:8080/varmq", "http://[fe62:db8::1]:8080/varmq", "http://10.0.0.6:8080/varmq"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Not-ready endpoints are skipped; nil Ready counts as ready.
	notReady := false
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.7"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}}}
	if got := k.urls(slice); len(got) != 0 {
		t.Fatalf("want nil, got %v", got)
	}

	// Slice-provided port used when ref.Port == 0.
	k = &k8sSource{path: "/varmq"}
	port := int32(9090)
	slice.Ports = []discoveryv1.EndpointPort{{Port: &port}}
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.5"}}}
	if got := k.urls(slice); len(got) != 1 || got[0] != "http://10.0.0.5:9090/varmq" {
		t.Fatalf("got %v", got)
	}
}

func TestNewKubernetesBadContext(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\ncontexts: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	done := make(chan error, 1)
	go func() {
		_, err := NewKubernetes(config.KubernetesRef{
			Context:   "does-not-exist",
			Namespace: "default",
			Service:   "varmq",
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error for nonexistent context, got nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("NewKubernetes hung on nonexistent context")
	}
}
