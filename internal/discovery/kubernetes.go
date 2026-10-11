package discovery

import (
	"context"
	"fmt"
	"net"
	"strconv"
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
	reg       *registry
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
		reg:       newRegistry(),
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
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { k.apply(nil, obj) },
		UpdateFunc: func(oldObj, newObj any) { k.apply(oldObj, newObj) },
		DeleteFunc: func(obj any) { k.apply(obj, nil) },
	})

	// Start the informer, then fail fast if the cache never syncs.
	go informer.Run(k.stopCh)
	syncCtx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		close(k.stopCh)
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

// apply updates the registry for a slice add/update/delete: URLs contributed
// by the old object are removed, URLs contributed by the new object are added.
//
// shortcut: the registry is a set, not reference-counted, so an address
// present in two slices is dropped when either slice is removed even while the
// other still contributes it; upgrade to ref-counting if overlapping slices
// become common.
func (k *k8sSource) apply(oldObj, newObj any) {
	if old := endpointSliceOf(oldObj); old != nil {
		for _, u := range k.urls(old) {
			k.reg.remove(u)
		}
	}
	if cur := endpointSliceOf(newObj); cur != nil {
		for _, u := range k.urls(cur) {
			k.reg.add(u)
		}
	}
}

func endpointSliceOf(obj any) *discoveryv1.EndpointSlice {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}
	return slice
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

func (k *k8sSource) Endpoints() []string { return k.reg.Endpoints() }

func (k *k8sSource) Close() error {
	select {
	case <-k.stopCh: // already closed
	default:
		close(k.stopCh)
	}
	return nil
}
