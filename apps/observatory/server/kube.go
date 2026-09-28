package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Everything the graph is derived from. Kinds that aren't installed are
// skipped, so the same binary works on any edition of the stack.
var watched = []schema.GroupVersionResource{
	{Version: "v1", Resource: "namespaces"},
	{Version: "v1", Resource: "nodes"},
	{Version: "v1", Resource: "pods"},
	{Version: "v1", Resource: "services"},
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "apps", Version: "v1", Resource: "statefulsets"},
	{Group: "apps", Version: "v1", Resource: "daemonsets"},
	{Group: "apps", Version: "v1", Resource: "replicasets"},
	{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
	{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
	{Group: "gateway.kgateway.dev", Version: "v1alpha1", Resource: "gatewayextensions"},
	{Group: "gateway.kgateway.dev", Version: "v1alpha1", Resource: "trafficpolicies"},
	{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaybackends"},
	{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaypolicies"},
	{Group: "kagent.dev", Version: "v1alpha2", Resource: "agents"},
	{Group: "kagent.dev", Version: "v1alpha2", Resource: "sandboxagents"},
	{Group: "kagent.dev", Version: "v1alpha2", Resource: "remotemcpservers"},
	{Group: "kagent.dev", Version: "v1alpha2", Resource: "modelconfigs"},
	{Group: "kagent.dev", Version: "v1alpha1", Resource: "mcpservers"},
	{Group: "ate.dev", Version: "v1alpha1", Resource: "workerpools"},
	{Group: "ate.dev", Version: "v1alpha1", Resource: "actortemplates"},
	{Group: "security.istio.io", Version: "v1", Resource: "authorizationpolicies"},
	{Group: "security.istio.io", Version: "v1", Resource: "peerauthentications"},
	{Group: "security.istio.io", Version: "v1", Resource: "requestauthentications"},
	{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"},
	{Group: "continuity.lab.solo.io", Version: "v1alpha1", Resource: "identitycontinuities"},
}

type Kube struct {
	cfg      *rest.Config
	dyn      dynamic.Interface
	disco    discovery.DiscoveryInterface
	mu       sync.RWMutex
	informer map[schema.GroupVersionResource]cache.SharedIndexInformer
	kinds    map[schema.GroupVersionResource]string // resource -> Kind
	changed  chan struct{}
	onPod    func(kind string, old, cur *unstructured.Unstructured)
}

func NewKube(cfg *rest.Config) (*Kube, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Kube{cfg: cfg, dyn: dyn, disco: dc,
		informer: map[schema.GroupVersionResource]cache.SharedIndexInformer{},
		kinds:    map[schema.GroupVersionResource]string{},
		changed:  make(chan struct{}, 1)}, nil
}

// Start watches every installed kind, and keeps re-discovering so CRDs that
// arrive later (a layer installed after the observatory) are picked up.
func (k *Kube) Start(ctx context.Context) {
	f := dynamicinformer.NewDynamicSharedInformerFactory(k.dyn, 0)
	add := func() {
		for _, gvr := range watched {
			k.mu.RLock()
			_, have := k.informer[gvr]
			k.mu.RUnlock()
			if have {
				continue
			}
			kind := k.lookupKind(gvr)
			if kind == "" {
				continue
			}
			inf := f.ForResource(gvr).Informer()
			g := gvr
			inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc:    func(o any) { k.touch(g, nil, o) },
				UpdateFunc: func(a, b any) { k.touch(g, a, b) },
				DeleteFunc: func(o any) { k.touch(g, o, nil) },
			})
			k.mu.Lock()
			k.informer[gvr], k.kinds[gvr] = inf, kind
			k.mu.Unlock()
			slog.Info("watching", "resource", gvr.String())
		}
		f.Start(ctx.Done())
		f.WaitForCacheSync(ctx.Done())
	}
	add()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				add()
			}
		}
	}()
}

func (k *Kube) lookupKind(gvr schema.GroupVersionResource) string {
	rl, err := k.disco.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return ""
	}
	for _, r := range rl.APIResources {
		if r.Name == gvr.Resource {
			return r.Kind
		}
	}
	return ""
}

func (k *Kube) touch(gvr schema.GroupVersionResource, old, cur any) {
	if gvr.Resource == "pods" && k.onPod != nil {
		k.onPod("pod", asU(old), asU(cur))
	}
	select {
	case k.changed <- struct{}{}:
	default:
	}
}

func asU(o any) *unstructured.Unstructured {
	switch v := o.(type) {
	case *unstructured.Unstructured:
		return v
	case cache.DeletedFinalStateUnknown:
		u, _ := v.Obj.(*unstructured.Unstructured)
		return u
	}
	return nil
}

func (k *Kube) List(resource string) []*unstructured.Unstructured {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for gvr, inf := range k.informer {
		if gvr.Resource != resource {
			continue
		}
		items := inf.GetStore().List()
		out := make([]*unstructured.Unstructured, 0, len(items))
		for _, o := range items {
			if u, ok := o.(*unstructured.Unstructured); ok {
				out = append(out, u)
			}
		}
		sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
		return out
	}
	return nil
}

// GVR resolves an apiVersion/kind pair against what's being watched first,
// then discovery, so the editor can open any object the graph links to.
func (k *Kube) GVR(apiVersion, kind string) (schema.GroupVersionResource, bool) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false
	}
	k.mu.RLock()
	for gvr, kd := range k.kinds {
		if kd == kind && gvr.GroupVersion() == gv {
			k.mu.RUnlock()
			return gvr, true
		}
	}
	k.mu.RUnlock()
	rl, err := k.disco.ServerResourcesForGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false
	}
	for _, r := range rl.APIResources {
		if r.Kind == kind && !strings.Contains(r.Name, "/") {
			return gv.WithResource(r.Name), true
		}
	}
	return schema.GroupVersionResource{}, false
}

func key(u *unstructured.Unstructured) string { return u.GetNamespace() + "/" + u.GetName() }

// Unstructured accessors: absent fields read as zero values.
func str(u map[string]any, path ...string) string {
	v, _, _ := unstructured.NestedString(u, path...)
	return v
}

func slice(u map[string]any, path ...string) []any {
	v, _, _ := unstructured.NestedSlice(u, path...)
	return v
}

func strmap(u map[string]any, path ...string) map[string]string {
	v, _, _ := unstructured.NestedStringMap(u, path...)
	return v
}

func obj(u map[string]any, path ...string) map[string]any {
	v, _, _ := unstructured.NestedMap(u, path...)
	return v
}

func i64(u map[string]any, path ...string) int64 {
	v, _, _ := unstructured.NestedInt64(u, path...)
	return v
}
