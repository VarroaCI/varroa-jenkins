package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestCellPoolDeepCopyIsolatesSubObjects mutates a deep copy of a populated
// CellPool and asserts the original stays unchanged. DeepCopy for this kind
// is hand-written, so this round trip is its only acceptance.
func TestCellPoolDeepCopyIsolatesSubObjects(t *testing.T) {
	grace := metav1.Duration{Duration: 20 * 60 * 1000 * 1000 * 1000}
	orig := CellPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "lts-2-555",
			Finalizers: []string{"varroa.dev/cellpool-drain"},
		},
		Spec: CellPoolSpec{
			ProfileRef:         "lts-2-555",
			Warm:               2,
			MaxCells:           6,
			MaxTenantsPerCell:  4,
			UnreadyGracePeriod: &grace,
			CellTemplate: &CellTemplate{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("500m"),
					},
				},
				PodOverrides: &PodOverrides{
					NodeSelector: map[string]string{"pool": "cells"},
				},
			},
		},
		Status: CellPoolStatus{
			ObservedGeneration: 3,
			WarmCells:          2,
			PlacedCells:        1,
			UnreadyCells:       0,
			FreeTenantSlots:    3,
			Tenants:            []string{"alpha", "beta"},
			Conditions: []metav1.Condition{
				{Type: "PoolReady", Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}

	cp := orig.DeepCopy()

	cp.Finalizers[0] = "mutated"
	cp.Spec.Warm = 9
	cp.Spec.UnreadyGracePeriod.Duration = 1
	cp.Spec.CellTemplate.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
	cp.Spec.CellTemplate.PodOverrides.NodeSelector["pool"] = "mutated"
	cp.Status.Tenants[0] = "mutated"
	cp.Status.Conditions[0].Reason = "Mutated"

	if got := orig.Finalizers[0]; got != "varroa.dev/cellpool-drain" {
		t.Fatalf("finalizers shallow-copied: got %q", got)
	}
	if got := orig.Spec.Warm; got != 2 {
		t.Fatalf("warm shallow-copied: got %d, want 2", got)
	}
	if got := orig.Spec.UnreadyGracePeriod.Duration; got != grace.Duration {
		t.Fatalf("unreadyGracePeriod shallow-copied: got %v", got)
	}
	req := orig.Spec.CellTemplate.Resources.Requests[corev1.ResourceCPU]
	if got := req.String(); got != "500m" {
		t.Fatalf("resources shallow-copied: got %q, want 500m", got)
	}
	if got := orig.Spec.CellTemplate.PodOverrides.NodeSelector["pool"]; got != "cells" {
		t.Fatalf("podOverrides shallow-copied: got %q, want cells", got)
	}
	if got := orig.Status.Tenants[0]; got != "alpha" {
		t.Fatalf("tenants shallow-copied: got %q, want alpha", got)
	}
	if got := orig.Status.Conditions[0].Reason; got != "Ready" {
		t.Fatalf("conditions shallow-copied: got %q, want Ready", got)
	}
}

// TestCellPoolDeepCopyObject asserts both new kinds satisfy runtime.Object,
// which addKnownTypes needs.
func TestCellPoolDeepCopyObject(t *testing.T) {
	var _ runtime.Object = &CellPool{}
	var _ runtime.Object = &CellPoolList{}

	list := CellPoolList{Items: []CellPool{{Spec: CellPoolSpec{ProfileRef: "lts-2-555"}}}}
	cp, ok := list.DeepCopyObject().(*CellPoolList)
	if !ok {
		t.Fatal("DeepCopyObject did not return a *CellPoolList")
	}
	cp.Items[0].Spec.ProfileRef = "mutated"
	if got := list.Items[0].Spec.ProfileRef; got != "lts-2-555" {
		t.Fatalf("list items shallow-copied: got %q, want lts-2-555", got)
	}

	var nilPool *CellPool
	if got := nilPool.DeepCopy(); got != nil {
		t.Fatalf("DeepCopy of a nil CellPool = %v, want nil", got)
	}
}
