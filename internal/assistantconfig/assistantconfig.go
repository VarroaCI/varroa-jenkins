// Package assistantconfig turns the AssistantConfig object into the runtime
// configuration of the assistant. The BFF and the Tenant agent runner read the
// object through a Live value. Each request or run reads one Snapshot, so a
// change reaches the next request or run with no restart.
package assistantconfig

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

// Snapshot is the runtime state that one object version builds. It is never
// changed after it is built.
type Snapshot struct {
	Config provider.Config
	// Registry holds the adapters of Config. It is never nil.
	Registry *provider.Registry
	// ResourceVersion is the version of the object the snapshot was built from.
	// It is empty when the object does not exist.
	ResourceVersion string
	// Err is set when the object exists and does not pass validation. Such a
	// snapshot holds no providers.
	Err error
}

// FromSpec maps the spec of the object to the runtime configuration. A zero
// budget, an empty sharing level, and an empty expiry take the default. The
// result passes provider.Validate.
func FromSpec(s v1alpha1.AssistantConfigSpec) (provider.Config, error) {
	c := provider.Config{
		DefaultProvider: s.DefaultProvider,
		DefaultModel:    s.DefaultModel,
	}
	for _, p := range s.Providers {
		c.Providers = append(c.Providers, provider.Endpoint{
			Name:        p.Name,
			Kind:        agent.ProviderKind(p.Kind),
			BaseURL:     p.BaseURL,
			Models:      append([]string(nil), p.Models...),
			Trust:       agent.ProviderTrust(p.Trust),
			Default:     p.Name == s.DefaultProvider,
			KeyRequired: p.Auth != v1alpha1.AssistantAuthNone,
		})
	}
	c.ExternalDataSharing = agent.DataSharing(s.ExternalDataSharing)
	if c.ExternalDataSharing == "" {
		c.ExternalDataSharing = agent.DataSharing(v1alpha1.AssistantDefaultSharing)
	}
	c.Budgets = provider.Budgets{
		Session:   valueOr(s.Budgets.Session, v1alpha1.AssistantDefaultSessionBudget),
		UserDay:   valueOr(s.Budgets.UserDay, v1alpha1.AssistantDefaultUserDayBudget),
		TenantDay: valueOr(s.Budgets.TenantDay, v1alpha1.AssistantDefaultTenantDayBudget),
		Steps:     int(valueOr(int64(s.Budgets.Steps), int64(v1alpha1.AssistantDefaultSteps))),
	}
	expiry := s.ApprovalExpiry
	if expiry == "" {
		expiry = v1alpha1.AssistantDefaultApprovalExpiry
	}
	d, err := time.ParseDuration(expiry)
	if err != nil {
		return provider.Config{}, fmt.Errorf("assistant config: approvalExpiry %q: %w", expiry, err)
	}
	c.ApprovalExpiry = d
	if err := provider.Validate(&c); err != nil {
		return provider.Config{}, err
	}
	return c, nil
}

func valueOr(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// Build returns the snapshot of one object version. A nil object means the
// object does not exist: the snapshot holds no providers and the default
// limits. An object that does not pass validation gives the same empty
// providers and sets Err.
func Build(obj *v1alpha1.AssistantConfig, factories map[agent.ProviderKind]provider.Factory, keyDir string) *Snapshot {
	spec := v1alpha1.DefaultAssistantConfigSpec()
	snap := &Snapshot{}
	if obj != nil {
		spec = obj.Spec
		snap.ResourceVersion = obj.ResourceVersion
	}
	cfg, err := FromSpec(spec)
	if err != nil {
		snap.Err = err
		cfg, _ = FromSpec(v1alpha1.DefaultAssistantConfigSpec())
	}
	reg, err := provider.NewRegistry(cfg, factories, keyDir)
	if err != nil {
		snap.Err = err
		cfg, _ = FromSpec(v1alpha1.DefaultAssistantConfigSpec())
		// The default config holds no providers, so this registry never fails.
		// The fallback keeps snap.Registry non-nil for every caller.
		reg, _ = provider.NewRegistry(cfg, nil, "")
	}
	snap.Config = cfg
	snap.Registry = reg
	return snap
}

// Live holds the current snapshot. Load is safe for concurrent use and never
// blocks. The change hooks run on the goroutine that stores a snapshot, so a
// hook must not block.
type Live struct {
	cur    atomic.Pointer[Snapshot]
	mu     sync.Mutex
	hooks  []func()
	ready  chan struct{}
	signal sync.Once
}

// NewLive returns a Live that holds the snapshot of an absent object.
func NewLive() *Live {
	l := &Live{ready: make(chan struct{})}
	l.cur.Store(Build(nil, nil, ""))
	return l
}

// NewLiveWith returns a Live that holds snap and is already synced. A caller
// that builds its snapshot itself, such as a test, uses it in place of Watch.
func NewLiveWith(snap *Snapshot) *Live {
	l := NewLive()
	l.cur.Store(snap)
	l.markSynced()
	return l
}

// Load returns the current snapshot.
func (l *Live) Load() *Snapshot { return l.cur.Load() }

// Store replaces the current snapshot and then runs each change hook.
func (l *Live) Store(s *Snapshot) {
	l.cur.Store(s)
	l.mu.Lock()
	hooks := append([]func(){}, l.hooks...)
	l.mu.Unlock()
	for _, h := range hooks {
		h()
	}
}

// OnChange adds a hook that runs after each Store.
func (l *Live) OnChange(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hooks = append(l.hooks, fn)
}

// Synced returns a channel that closes when the first list of the object has
// completed.
func (l *Live) Synced() <-chan struct{} { return l.ready }

func (l *Live) markSynced() {
	l.signal.Do(func() { close(l.ready) })
}
