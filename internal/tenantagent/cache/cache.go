// Package cache keeps the TenantAgent, Tenant, and CatalogSource objects that the
// Tenant agent runner reads, in memory, from dynamic informers. The runner reads
// the cache for every decision, so a decision never waits on the API server.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/accept"
)

var (
	gvrTenantAgents   = v1alpha1.SchemeGroupVersion.WithResource("tenantagents")
	gvrTenants        = v1alpha1.SchemeGroupVersion.WithResource("tenants")
	gvrCatalogSources = v1alpha1.SchemeGroupVersion.WithResource("catalogsources")
)

// Cache holds the informers and their stores. Its methods are safe for
// concurrent use.
type Cache struct {
	dyn        dynamic.Interface
	releaseNS  string
	logger     *slog.Logger
	factory    dynamicinformer.DynamicSharedInformerFactory
	taInformer cache.SharedIndexInformer
	tenantInf  cache.SharedIndexInformer
	csInformer cache.SharedIndexInformer

	mu        sync.RWMutex
	observers []func()
}

// New returns a cache over dyn. releaseNamespace is the namespace of the core
// install, which the namespace rule of the acceptance check needs.
func New(dyn dynamic.Interface, releaseNamespace string, logger *slog.Logger) *Cache {
	if logger == nil {
		logger = slog.Default()
	}
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	c := &Cache{
		dyn:        dyn,
		releaseNS:  releaseNamespace,
		logger:     logger,
		factory:    factory,
		taInformer: factory.ForResource(gvrTenantAgents).Informer(),
		tenantInf:  factory.ForResource(gvrTenants).Informer(),
		csInformer: factory.ForResource(gvrCatalogSources).Informer(),
	}
	for _, inf := range []cache.SharedIndexInformer{c.taInformer, c.tenantInf, c.csInformer} {
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { c.notify() },
			UpdateFunc: func(any, any) { c.notify() },
			DeleteFunc: func(any) { c.notify() },
		})
	}
	return c
}

// Run starts the informers and blocks until ctx ends. It is safe to call Run
// once. The informers stop when ctx ends.
func (c *Cache) Run(ctx context.Context) {
	c.factory.Start(ctx.Done())
	<-ctx.Done()
	c.factory.Shutdown()
}

// Synced reports whether every informer has listed its resource once.
func (c *Cache) Synced() bool {
	return c.taInformer.HasSynced() && c.tenantInf.HasSynced() && c.csInformer.HasSynced()
}

// OnChange registers fn to run after any cached object changes. fn runs on
// the informer goroutine, so it must not block.
func (c *Cache) OnChange(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observers = append(c.observers, fn)
}

func (c *Cache) notify() {
	c.mu.RLock()
	obs := append([]func(){}, c.observers...)
	c.mu.RUnlock()
	for _, fn := range obs {
		fn()
	}
}

// List returns every TenantAgent in the cache. An object that fails to decode is
// logged and left out.
func (c *Cache) List() []v1alpha1.TenantAgent {
	out := make([]v1alpha1.TenantAgent, 0)
	for _, raw := range c.taInformer.GetStore().List() {
		var ta v1alpha1.TenantAgent
		if err := decode(raw, &ta); err != nil {
			c.logger.Warn("tenant agent cache: decode", "error", err.Error())
			continue
		}
		out = append(out, ta)
	}
	return out
}

// ByUID returns the TenantAgent with the given UID.
func (c *Cache) ByUID(uid string) (*v1alpha1.TenantAgent, bool) {
	for _, ta := range c.List() {
		if string(ta.UID) == uid {
			return &ta, true
		}
	}
	return nil, false
}

// AcceptInput returns the input of the acceptance check for the current cache.
func (c *Cache) AcceptInput() accept.Input {
	tenants := map[string]*v1alpha1.Tenant{}
	for _, raw := range c.tenantInf.GetStore().List() {
		var t v1alpha1.Tenant
		if err := decode(raw, &t); err != nil {
			c.logger.Warn("tenant agent cache: decode tenant", "error", err.Error())
			continue
		}
		tenants[t.Name] = &t
	}
	return accept.Input{TenantAgents: c.List(), Tenants: tenants, ReleaseNamespace: c.releaseNS}
}

// CatalogSource returns the CatalogSource named ns/name. An empty ns reads a
// cluster-scoped object by its name.
func (c *Cache) CatalogSource(ns, name string) (*v1alpha1.CatalogSource, bool) {
	key := name
	if ns != "" {
		key = ns + "/" + name
	}
	raw, ok, err := c.csInformer.GetStore().GetByKey(key)
	if err != nil || !ok {
		return nil, false
	}
	var cs v1alpha1.CatalogSource
	if err := decode(raw, &cs); err != nil {
		c.logger.Warn("tenant agent cache: decode catalog source", "error", err.Error())
		return nil, false
	}
	return &cs, true
}

// decode converts one cached unstructured object into a typed object.
func decode(raw any, out any) error {
	u, ok := raw.(runtimeUnstructured)
	if !ok {
		return fmt.Errorf("cached object is %T, not unstructured", raw)
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), out)
}

// runtimeUnstructured is the part of *unstructured.Unstructured that decode reads.
type runtimeUnstructured interface {
	UnstructuredContent() map[string]any
}

// GVRs names the resources the cache watches, in the order it starts them.
func GVRs() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{gvrTenantAgents, gvrTenants, gvrCatalogSources}
}
