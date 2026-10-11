package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

const syncTimeout = 10 * time.Second

// k8sSource watches ready EndpointSlice endpoints and exposes their URLs.
type k8sSource struct {
	store     cache.Store // informer cache of EndpointSlices; thread-safe
	path      string
	fixedPort int
	stopCh    chan struct{}
}

// NewKubernetes returns a Source for the varmq mux behind ref, discovered
// through Kubernetes EndpointSlices. It builds a client from the kubeconfig
// (with ref.Context override), starts a discovery/v1 EndpointSlice informer in
// ref.Namespace filtered by service name or ref.Selector, and waits up to 10s
// for the first cache sync. Errors (bad context, sync timeout) return promptly.
func NewKubernetes(ref config.KubernetesRef) (Source, error) {
	selector := ref.Selector
	if ref.Service != "" {
		selector = discoveryv1.LabelServiceName + "=" + ref.Service
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector %q: %w", selector, err)
	}

	client, err := kubeClient(ref.Context)
	if err != nil {
		return nil, fmt.Errorf("building kube client for context %q: %w", ref.Context, err)
	}

	path := ref.Path
	if path == "" {
		path = "/varmq"
	}

	k := &k8sSource{
		path:      path,
		fixedPort: ref.Port,
		stopCh:    make(chan struct{}),
	}

	informer := informers.NewSharedInformerFactoryWithOptions(
		client, 0,
		informers.WithNamespace(ref.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = sel.String()
		}),
	).Discovery().V1().EndpointSlices().Informer()
	k.store = informer.GetStore()

	// Remember the latest list/watch failure (RBAC forbidden, unreachable API
	// server) so a sync timeout can report the real cause.
	var lastErr atomic.Pointer[error]
	_ = informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		lastErr.Store(&err)
	})

	// Start the informer, then fail fast if the cache never syncs.
	go informer.Run(k.stopCh)
	syncCtx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		close(k.stopCh)
		if err := lastErr.Load(); err != nil {
			return nil, fmt.Errorf("EndpointSlice watcher for %q did not sync within %s: %w", selector, syncTimeout, *err)
		}
		return nil, fmt.Errorf("EndpointSlice watcher for %q did not sync within %s", selector, syncTimeout)
	}

	return k, nil
}

func kubeClient(contextName string) (kubernetes.Interface, error) {
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), overrides,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(restConfig)
}

// urls returns the endpoint URLs a slice contributes; nil when the slice has
// no usable port or address type.
func (k *k8sSource) urls(slice *discoveryv1.EndpointSlice) []string {
	if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
		return nil
	}
	port := k.fixedPort
	if port == 0 {
		for _, p := range slice.Ports {
			if p.Port != nil {
				port = int(*p.Port)
				break
			}
		}
	}
	if port == 0 {
		return nil
	}

	var urls []string
	for i := range slice.Endpoints {
		ep := &slice.Endpoints[i]
		if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
			continue
		}
		for _, addr := range ep.Addresses {
			urls = append(urls, "http://"+net.JoinHostPort(addr, strconv.Itoa(port))+k.path)
		}
	}
	return urls
}

// Endpoints recomputes the ready URLs from every cached slice. Deriving the set
// from the store (instead of applying add/remove events) keeps an address that
// moves between slices, or appears in two at once, from being dropped.
func (k *k8sSource) Endpoints() []string {
	seen := make(map[string]struct{})
	for _, obj := range k.store.List() {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			continue
		}
		for _, u := range k.urls(slice) {
			seen[u] = struct{}{}
		}
	}
	urls := make([]string, 0, len(seen))
	for u := range seen {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	return urls
}

func (k *k8sSource) Close() error {
	select {
	case <-k.stopCh: // already closed
	default:
		close(k.stopCh)
	}
	return nil
}
