package controller

import (
	"fmt"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestApplyUnknownDependencies_CountsAndCaps(t *testing.T) {
	var inv []inventoryEntry
	for i := 60; i >= 1; i-- {
		inv = append(inv, inventoryEntry{Name: fmt.Sprintf("p%02d", i), Version: "1.0", DependenciesKnown: false})
	}
	inv = append(inv, inventoryEntry{Name: "known", Version: "1.0", DependenciesKnown: true})

	status := &v1alpha1.UpdateCenterStatus{}
	r := &UpdateCenterReconciler{}
	r.applyUnknownDependencies(status, inv)

	if status.UnknownDependencyCount != 60 {
		t.Fatalf("count = %d, want 60", status.UnknownDependencyCount)
	}
	if len(status.UnknownDependencies) != maxUnknownDependencies {
		t.Fatalf("list length = %d, want %d", len(status.UnknownDependencies), maxUnknownDependencies)
	}
	for i := 1; i < len(status.UnknownDependencies); i++ {
		prev, cur := status.UnknownDependencies[i-1].Plugin, status.UnknownDependencies[i].Plugin
		if prev >= cur {
			t.Fatalf("list not sorted at %d: %q then %q", i, prev, cur)
		}
	}
	if got := status.UnknownDependencies[0].Plugin; got != "p01" {
		t.Errorf("first entry = %q, want p01", got)
	}
	c := conditionFor(t, status, condTypeDependenciesKnown)
	if c.Status != "False" || c.Reason != reasonUnknownDependencies {
		t.Errorf("condition = %+v, want False/UnknownDependencies", c)
	}
}

func TestApplyUnknownDependencies_AllKnownSetsTrue(t *testing.T) {
	inv := []inventoryEntry{
		{Name: "a", Version: "1.0", DependenciesKnown: true},
		{Name: "b", Version: "2.0", DependenciesKnown: true},
	}
	status := &v1alpha1.UpdateCenterStatus{UnknownDependencyCount: 3,
		UnknownDependencies: []v1alpha1.UpdateCenterPluginRef{{Plugin: "stale", Version: "1"}}}
	r := &UpdateCenterReconciler{}
	r.applyUnknownDependencies(status, inv)

	if status.UnknownDependencyCount != 0 || status.UnknownDependencies != nil {
		t.Fatalf("status = count %d list %+v, want zero and nil", status.UnknownDependencyCount, status.UnknownDependencies)
	}
	c := conditionFor(t, status, condTypeDependenciesKnown)
	if c.Status != "True" || c.Reason != reasonAllDependenciesKnown {
		t.Errorf("condition = %+v, want True/AllDependenciesKnown", c)
	}
}

func TestApplyUnknownDependencies_DoesNotAffectReady(t *testing.T) {
	inv := []inventoryEntry{{Name: "x", Version: "1.0", DependenciesKnown: false}}
	status := &v1alpha1.UpdateCenterStatus{
		Phase: v1alpha1.UpdateCenterPhase("Ready"),
		Conditions: []v1alpha1.UpdateCenterCondition{
			{Type: condTypeReady, Status: "True", Reason: "Ready"},
		},
	}
	r := &UpdateCenterReconciler{}
	r.applyUnknownDependencies(status, inv)

	if status.Phase != "Ready" {
		t.Errorf("phase = %q, want Ready unchanged", status.Phase)
	}
	if c := conditionFor(t, status, condTypeReady); c.Status != "True" {
		t.Errorf("Ready = %+v, want unchanged True", c)
	}
}

// conditionFor returns the condition of the given type, or fails the test.
func conditionFor(t *testing.T, status *v1alpha1.UpdateCenterStatus, typ string) v1alpha1.UpdateCenterCondition {
	t.Helper()
	for _, c := range status.Conditions {
		if c.Type == typ {
			return c
		}
	}
	t.Fatalf("no %s condition in %+v", typ, status.Conditions)
	return v1alpha1.UpdateCenterCondition{}
}
