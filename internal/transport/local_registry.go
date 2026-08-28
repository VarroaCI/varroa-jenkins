package transport

import (
	"context"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/mite"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// LocalRegistry wraps an in-process *mite.Registry to implement the Transport
// interface. It performs argument-order shimming (ns, name) ↔ (name, ns).
// This is the single-process fallback and the unit-test double.
type LocalRegistry struct {
	reg *mite.Registry
	// classificationsMu guards classifications: the operator writes via
	// PutPluginClassification while the BFF reads via PluginClassification, and
	// in the single-process deployment those are concurrent goroutines. An
	// unguarded map here is a fatal concurrent read/write, not a stale read.
	classificationsMu sync.RWMutex
	classifications   map[string]*ClassifiedInventory // keyed "ns/name"
}

// NewLocalRegistry creates a LocalRegistry that delegates to the given
// in-memory mite registry.
func NewLocalRegistry(reg *mite.Registry) *LocalRegistry {
	return &LocalRegistry{reg: reg, classifications: make(map[string]*ClassifiedInventory)}
}

// Ensure LocalRegistry implements Transport.
var _ Transport = (*LocalRegistry)(nil)

// Send delegates to Registry.Send, ignoring ctx (the in-process send cannot
// be cancelled).
func (l *LocalRegistry) Send(_ context.Context, ns, name string, msg *mitev1.OperatorMessage) error {
	return l.reg.Send(name, ns, msg)
}

// DrainResults delegates to Registry.DrainResults.
func (l *LocalRegistry) DrainResults(ns, name string) []*mitev1.CommandResult {
	return l.reg.DrainResults(name, ns)
}

// Snapshot delegates to Registry.GetSnapshot.
func (l *LocalRegistry) Snapshot(ns, name string) *mitev1.StateSnapshot {
	return l.reg.GetSnapshot(name, ns)
}

// Health delegates to Registry.GetHealth.
func (l *LocalRegistry) Health(ns, name string) string {
	return l.reg.GetHealth(name, ns)
}

// Connected delegates to Registry.IsConnected.
func (l *LocalRegistry) Connected(ns, name string) bool {
	return l.reg.IsConnected(name, ns)
}

// Info delegates to Registry.GetMiteInfo.
func (l *LocalRegistry) Info(ns, name string) (version string, lastHeartbeat, certExpiry time.Time, ok bool) {
	return l.reg.GetMiteInfo(name, ns)
}

// IdleGauges delegates to Registry.GetIdleGauges.
func (l *LocalRegistry) IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool) {
	return l.reg.GetIdleGauges(name, ns)
}

// ConnEpoch returns the connection epoch from the underlying Registry.
func (l *LocalRegistry) ConnEpoch(ns, name string) (int64, bool) {
	return l.reg.GetEpoch(name, ns)
}

// StreamDegraded is always false for the in-process registry: there is no bus
// bridge between the operator and the mite stream to degrade.
func (l *LocalRegistry) StreamDegraded(_, _ string) (string, bool) { return "", false }

// Registry returns the underlying *mite.Registry. This is provided for
// components that still need direct access to the registry (e.g., the
// gRPC server's CommandStream handler that writes into it).
func (l *LocalRegistry) Registry() *mite.Registry {
	return l.reg
}

// SendImperative forwards an imperative command over the in-process stream,
// mirroring Send. Unlike Send (which uses last-value KV), this is direct.
func (l *LocalRegistry) SendImperative(_ context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	return l.reg.Send(name, ns, &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: cmd},
	})
}

// ClearDesired is a no-op for the in-process registry: desired state is never
// persisted (sends go straight to the live stream).
func (l *LocalRegistry) ClearDesired(_ context.Context, _, _ string) error { return nil }

// ObservabilityReport delegates to Registry.GetObservabilityReport.
func (l *LocalRegistry) ObservabilityReport(ns, name string) *mitev1.ObservabilityReport {
	return l.reg.GetObservabilityReport(name, ns)
}

// FetchLastApplied is not supported in the in-process registry — content-fetch
// requires the NATS-backed bus transport.
func (l *LocalRegistry) FetchLastApplied(_ context.Context, _, _ string) (*mitev1.ContentResponse, error) {
	return nil, ErrContentUnavailable
}

// PluginInventory delegates to Registry.GetPluginInventory.
func (l *LocalRegistry) PluginInventory(ns, name string) *mitev1.PluginInventory {
	return l.reg.GetPluginInventory(name, ns)
}

// InstalledPluginsHash delegates to Registry.GetInstalledPluginsHash.
func (l *LocalRegistry) InstalledPluginsHash(ns, name string) (string, bool) {
	return l.reg.GetInstalledPluginsHash(name, ns)
}

// PluginClassification returns the stored classified inventory from the
// in-memory map, written by PutPluginClassification.
func (l *LocalRegistry) PluginClassification(ns, name string) (*ClassifiedInventory, bool) {
	key := ns + "/" + name
	l.classificationsMu.RLock()
	defer l.classificationsMu.RUnlock()
	c, ok := l.classifications[key]
	return c, ok
}

// PutPluginClassification stores the classified inventory in the in-memory map.
func (l *LocalRegistry) PutPluginClassification(_ context.Context, ns, name string, c *ClassifiedInventory) error {
	key := ns + "/" + name
	l.classificationsMu.Lock()
	defer l.classificationsMu.Unlock()
	l.classifications[key] = c
	return nil
}
