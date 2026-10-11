package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// TestK8sSourceEndpointsFromStore covers EndpointSlice churn: an address that
// sits in two slices (controller rebalancing, rolling updates) must survive the
// deletion of either slice, and reads stay consistent under concurrent writes.
func TestK8sSourceEndpointsFromStore(t *testing.T) {
	ready := true
	slice := func(name string, addrs ...string) *discoveryv1.EndpointSlice {
		eps := make([]discoveryv1.Endpoint, len(addrs))
		for i, a := range addrs {
			eps[i] = discoveryv1.Endpoint{Addresses: []string{a}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}
		}
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "default"},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   eps,
		}
	}

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	k := &k8sSource{store: store, path: "/varmq", fixedPort: 8080}

	a := slice("a", "10.0.0.2", "10.0.0.1")
	b := slice("b", "10.0.0.2", "10.0.0.3")
	_ = store.Add(a)
	_ = store.Add(b)

	want := []string{"http://10.0.0.1:8080/varmq", "http://10.0.0.2:8080/varmq", "http://10.0.0.3:8080/varmq"}
	if got := k.Endpoints(); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	_ = store.Delete(a)
	want = []string{"http://10.0.0.2:8080/varmq", "http://10.0.0.3:8080/varmq"}
	if got := k.Endpoints(); !slices.Equal(got, want) {
		t.Fatalf("after deleting overlapping slice: got %v, want %v", got, want)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := slice(fmt.Sprintf("s%d", i), fmt.Sprintf("10.1.0.%d", i))
			_ = store.Add(s)
			_ = k.Endpoints() // read concurrently with writers (race coverage)
			_ = store.Delete(s)
		}(i)
	}
	wg.Wait()
	if got := k.Endpoints(); !slices.Equal(got, want) {
		t.Fatalf("after churn: got %v, want %v", got, want)
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

func TestK8sSourceURLsPathAndAddressType(t *testing.T) {
	ready := true
	slice := &discoveryv1.EndpointSlice{
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}

	// The "/varmq" default is applied by NewKubernetes; an explicit ref.Path
	// override is used verbatim.
	k := &k8sSource{path: "/custom", fixedPort: 8080}
	if got := k.urls(slice); len(got) != 1 || got[0] != "http://10.0.0.5:8080/custom" {
		t.Fatalf("path override: got %v", got)
	}

	// FQDN address types are excluded; only IPv4/IPv6 are dialed.
	fqdn := *slice
	fqdn.AddressType = discoveryv1.AddressTypeFQDN
	if got := k.urls(&fqdn); len(got) != 0 {
		t.Fatalf("FQDN exclusion: got %v", got)
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
