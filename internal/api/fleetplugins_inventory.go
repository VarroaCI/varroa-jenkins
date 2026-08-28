package api

import (
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// FleetPluginInventory reads the CLASSIFIED inventory for local-cluster
// controllers from T2.1's `invc/` read model (R27). v1 is local-only per R22;
// the named cross-cluster follow-up widens this seam without changing anything
// above it.
//
// The BFF never classifies. It reads the label and carries it.
// Re-deriving one here would put a second classifier in the tree that could
// disagree with T2.1's, and inferring one from detached/bundled is the same
// defect in cheaper clothing — R20 measured bundled: true on zero of 84 plugins
// on smoke-mcp.
type FleetPluginInventory interface {
	// List reads the CLASSIFIED inventory for exactly the given LOCAL-cluster
	// controllers from T2.1's invc/ read model (R27) via
	// Transport.PluginClassification. The caller supplies the keys
	// because RBAC filtering has already run; the reader never widens the set.
	// v1 is local-only (R22); the named cross-cluster follow-up widens this
	// seam without changing anything above it.
	//
	// A key absent from the result has no observed inventory — distinct from a
	// present entry reporting zero plugins. There is no error return and no
	// per-controller failure channel, because the underlying accessor has
	// neither: PluginClassification(ns, name) (*ClassifiedInventory, bool)
	// takes no context and returns no error — a value and a found bool. T2.2
	// must not invent a distinction the contract cannot express.
	List(keys []types.NamespacedName) map[types.NamespacedName]ControllerInventory
}

// ControllerInventory is the classified plugin inventory for one controller,
// as read from T2.1's invc/ read model.
type ControllerInventory struct {
	Plugins     []InstalledPlugin
	CollectedAt time.Time
	// Envelope is the guard projection of T2.1's classified record envelope:
	// its hash plus every quality boolean. Compared against
	// Controller.status.pluginInventory, never trusted. Dropping any included
	// flag would let T2.2 report detailStale: false where T2.1's own route
	// discloses a mismatch.
	Envelope ClassifiedEnvelope
}

// InstalledPlugin is one installed plugin from the classified inventory.
type InstalledPlugin struct {
	Name    string
	Version string
	// Class is the T2.1 provenance class LABEL (R19, R27) — carried verbatim
	// from the classified record. Never enumerated, never ranked, never an
	// ordinal, and never re-derived here.
	Class string
}

// ClassifiedEnvelope is the GUARD PROJECTION of T2.1's envelope: its hash plus
// every quality boolean. It is deliberately not the whole envelope — T2.1's
// also carries collectedAt, observedAt, total and counts, none of which the
// cross-check needs. What matters is that no compared field is dropped:
// omitting one would let T2.2 report detailStale: false where T2.1's own route
// discloses a mismatch, and the two surfaces would disagree about the same
// controller.
type ClassifiedEnvelope struct {
	Hash                 string
	Source               string
	Stale                bool
	Degraded             bool
	Truncated            bool
	OptionalEdgesDropped bool
	BootstrapApproximate bool
	DriftTruncated       bool
}

// fakeFleetInventory is a test fake keyed by (namespace, name) with an explicit
// set of absent keys, so "no observed inventory" and "observed an empty
// inventory" stay distinguishable.
type fakeFleetInventory struct {
	data   map[types.NamespacedName]ControllerInventory
	absent map[types.NamespacedName]bool
}

// newFakeFleetInventory creates a fake with the given data entries.
// Keys in absent are explicitly absent (no observed inventory), distinct from
// a present entry that happens to have zero plugins.
func newFakeFleetInventory(data map[types.NamespacedName]ControllerInventory, absent []types.NamespacedName) *fakeFleetInventory {
	fi := &fakeFleetInventory{
		data:   make(map[types.NamespacedName]ControllerInventory, len(data)),
		absent: make(map[types.NamespacedName]bool, len(absent)),
	}
	for k, v := range data {
		fi.data[k] = v
	}
	for _, k := range absent {
		fi.absent[k] = true
	}
	return fi
}

// List implements FleetPluginInventory.
func (f *fakeFleetInventory) List(keys []types.NamespacedName) map[types.NamespacedName]ControllerInventory {
	result := make(map[types.NamespacedName]ControllerInventory)
	for _, k := range keys {
		if f.absent[k] {
			continue // explicitly absent — no observed inventory
		}
		if v, ok := f.data[k]; ok {
			result[k] = v
		}
		// else: not found in either map → also no observed inventory
	}
	return result
}

// ---------------------------------------------------------------------------
// Concrete adapter over transport.Transport
// ---------------------------------------------------------------------------

// fleetInventoryReader is the concrete adapter satisfying FleetPluginInventory
// by calling T2.1's Transport.PluginClassification once per key. It is a
// straight loop — no worker pool, no per-call timeout, no goroutines, no error
// channel, because the underlying accessor has none.
//
// The BFF never classifies (R23/R27). This adapter reads the class label from
// the classified record and carries it verbatim. No class is computed,
// re-derived, or inferred from detached/bundled — R20 measured bundled: true on
// zero of 84 plugins on smoke-mcp, so inference fails silently.
type fleetInventoryReader struct {
	reg transport.Transport
}

// NewFleetInventoryReader creates a FleetPluginInventory backed by the
// transport's PluginClassification accessor.
func NewFleetInventoryReader(reg transport.Transport) FleetPluginInventory {
	return &fleetInventoryReader{reg: reg}
}

// List implements FleetPluginInventory by calling
// Transport.PluginClassification(ns, name) once per key.
func (r *fleetInventoryReader) List(keys []types.NamespacedName) map[types.NamespacedName]ControllerInventory {
	result := make(map[types.NamespacedName]ControllerInventory, len(keys))
	for _, k := range keys {
		ci, ok := r.reg.PluginClassification(k.Namespace, k.Name)
		if !ok || ci == nil {
			continue // no observed inventory for this controller
		}
		plugins := make([]InstalledPlugin, len(ci.Plugins))
		for i, p := range ci.Plugins {
			plugins[i] = InstalledPlugin{
				Name:    p.Name,
				Version: p.Version,
				Class:   p.Class, // label carried verbatim (R19, R27)
			}
		}
		result[k] = ControllerInventory{
			Plugins:     plugins,
			CollectedAt: ci.Envelope.CollectedAt,
			Envelope: ClassifiedEnvelope{
				Hash:                 ci.Envelope.Hash,
				Source:               ci.Envelope.Source,
				Stale:                ci.Envelope.Stale,
				Degraded:             ci.Envelope.Degraded,
				Truncated:            ci.Envelope.Truncated,
				OptionalEdgesDropped: ci.Envelope.OptionalEdgesDropped,
				BootstrapApproximate: ci.Envelope.BootstrapApproximate,
				DriftTruncated:       ci.Envelope.DriftTruncated,
			},
		}
	}
	return result
}
