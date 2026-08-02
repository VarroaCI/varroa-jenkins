package api

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/plugininv"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// ---------------------------------------------------------------------------
// stub transport for reader tests
// ---------------------------------------------------------------------------

// stubClassifiedTransport implements transport.Transport only for
// PluginClassification, which is all the reader adapter calls.
type stubClassifiedTransport struct {
	transport.Transport // embedded nil — panics if anything else is called

	data map[types.NamespacedName]*transport.ClassifiedInventory
}

func newStubClassifiedTransport(entries map[types.NamespacedName]*transport.ClassifiedInventory) *stubClassifiedTransport {
	s := &stubClassifiedTransport{data: make(map[types.NamespacedName]*transport.ClassifiedInventory)}
	for k, v := range entries {
		s.data[k] = v
	}
	return s
}

func (s *stubClassifiedTransport) PluginClassification(ns, name string) (*transport.ClassifiedInventory, bool) {
	ci, ok := s.data[types.NamespacedName{Namespace: ns, Name: name}]
	return ci, ok
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestFleetInventoryReader_PluginMappingFidelity(t *testing.T) {
	collectedAt := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	stub := newStubClassifiedTransport(map[types.NamespacedName]*transport.ClassifiedInventory{
		nn("ctrl-a"): {
			Envelope: transport.ClassifiedEnvelope{
				Hash:                 "abc123",
				Source:               "jenkins-api",
				Stale:                true,
				Degraded:             true,
				Truncated:            true,
				OptionalEdgesDropped: true,
				BootstrapApproximate: true,
				DriftTruncated:       true,
				CollectedAt:          collectedAt,
				Total:                2,
				Counts:               map[string]int{"declared": 1, "dependency": 1},
			},
			Plugins: []transport.ClassifiedPlugin{
				{Name: "git-client", Version: "4.0.0", Class: plugininv.ClassDeclared},
				{Name: "workflow-api", Version: "2.0", Class: plugininv.ClassDependency},
			},
		},
	})

	reader := NewFleetInventoryReader(stub)
	result := reader.List([]types.NamespacedName{nn("ctrl-a")})

	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}

	inv := result[nn("ctrl-a")]
	if len(inv.Plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(inv.Plugins))
	}

	// Check plugin 1.
	p0 := inv.Plugins[0]
	if p0.Name != "git-client" || p0.Version != "4.0.0" || p0.Class != "declared" {
		t.Errorf("plugin[0] = %+v, want git-client 4.0.0 declared", p0)
	}

	// Check plugin 2.
	p1 := inv.Plugins[1]
	if p1.Name != "workflow-api" || p1.Version != "2.0" || p1.Class != "dependency" {
		t.Errorf("plugin[1] = %+v, want workflow-api 2.0 dependency", p1)
	}

	// Check envelope.
	env := inv.Envelope
	if env.Hash != "abc123" {
		t.Errorf("Hash = %q, want abc123", env.Hash)
	}
	if env.Source != "jenkins-api" {
		t.Errorf("Source = %q, want jenkins-api", env.Source)
	}
	if !env.Stale || !env.Degraded || !env.Truncated || !env.OptionalEdgesDropped || !env.BootstrapApproximate || !env.DriftTruncated {
		t.Errorf("expected all envelope booleans true, got %+v", env)
	}

	// Check CollectedAt.
	if !inv.CollectedAt.Equal(collectedAt) {
		t.Errorf("CollectedAt = %v, want %v", inv.CollectedAt, collectedAt)
	}
}

func TestFleetInventoryReader_FalseFoundOmitsKey(t *testing.T) {
	collectedAt := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	stub := newStubClassifiedTransport(map[types.NamespacedName]*transport.ClassifiedInventory{
		nn("has-inv"): {
			Envelope: transport.ClassifiedEnvelope{
				Hash:        "abc",
				CollectedAt: collectedAt,
			},
			Plugins: []transport.ClassifiedPlugin{
				{Name: "p", Version: "1", Class: plugininv.ClassUnmanaged},
			},
		},
		// "no-inv" is simply absent from the map.
	})

	reader := NewFleetInventoryReader(stub)
	result := reader.List([]types.NamespacedName{
		nn("has-inv"),
		nn("no-inv"),
	})

	// has-inv should be present.
	if _, ok := result[nn("has-inv")]; !ok {
		t.Error("has-inv should be present")
	}
	// no-inv should be absent (false found → omit key).
	if _, ok := result[nn("no-inv")]; ok {
		t.Error("no-inv should be absent (false found → omit key)")
	}
}

func TestFleetInventoryReader_ZeroPluginsDistinguishableFromAbsent(t *testing.T) {
	collectedAt := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	stub := newStubClassifiedTransport(map[types.NamespacedName]*transport.ClassifiedInventory{
		nn("zero-plugins"): {
			Envelope: transport.ClassifiedEnvelope{
				Hash:        "abc",
				CollectedAt: collectedAt,
			},
			Plugins: []transport.ClassifiedPlugin{}, // zero plugins
		},
	})

	reader := NewFleetInventoryReader(stub)
	result := reader.List([]types.NamespacedName{
		nn("zero-plugins"),
		nn("no-inv"),
	})

	// zero-plugins should be present with empty slice.
	zp, ok := result[nn("zero-plugins")]
	if !ok {
		t.Fatal("zero-plugins should be present (observed empty inventory)")
	}
	if len(zp.Plugins) != 0 {
		t.Errorf("expected 0 plugins in zero-plugins entry, got %d", len(zp.Plugins))
	}

	// no-inv should be absent.
	if _, ok := result[nn("no-inv")]; ok {
		t.Error("no-inv should be absent (no observed inventory)")
	}
}
