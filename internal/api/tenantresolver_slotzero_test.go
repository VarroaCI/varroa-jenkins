package api

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestPlacedTenantReadsSlotZeroForASingleMemberTenant asserts the tenant
// resolver keeps reading the scalar placement, and that a tenant with one
// active slot therefore resolves exactly as it did before slots existed.
//
// The reference is an authorization identity. A multi-slot tenant's
// consequences are stated in the docs: an external runner attaches to slot
// 0, and a webhook replay reaches slot 0.
func TestPlacedTenantReadsSlotZeroForASingleMemberTenant(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"pipelines": map[string]any{"profile": "lts"},
		},
		"status": map[string]any{
			"placement": map[string]any{
				"cluster":       "core",
				"cell":          "cell-a",
				"cellNamespace": "cells-ns",
				"members": []any{
					map[string]any{
						"cell": "cell-a", "cellNamespace": "cells-ns",
						"role": "active", "slot": int64(0), "applied": true,
					},
				},
			},
		},
	}}

	ref, ok := placedTenant("payments", obj)
	if !ok {
		t.Fatal("a placed tenant did not resolve")
	}
	if ref.Controller != "cell-a" || ref.Namespace != "cells-ns" || ref.Cluster != "core" {
		t.Fatalf("ref = %+v, want slot 0's cell-a in cells-ns", ref)
	}
	if ref.Tenant != "payments" || ref.Folder != "payments" {
		t.Fatalf("ref = %+v, want the tenant and the folder payments", ref)
	}
}

// TestPlacedTenantIgnoresTheMemberListForTheReference asserts the
// reference names slot 0's cell even for a tenant with two slots, which is
// the stated slot-0 limitation.
func TestPlacedTenantIgnoresTheMemberListForTheReference(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"pipelines": map[string]any{"profile": "lts"},
		},
		"status": map[string]any{
			"placement": map[string]any{
				"cluster":       "core",
				"cell":          "cell-a",
				"cellNamespace": "cells-ns",
				"members": []any{
					map[string]any{"cell": "cell-a", "cellNamespace": "cells-ns", "role": "active", "slot": int64(0)},
					map[string]any{"cell": "cell-b", "cellNamespace": "cells-ns", "role": "active", "slot": int64(1)},
				},
			},
		},
	}}

	ref, ok := placedTenant("payments", obj)
	if !ok {
		t.Fatal("a placed tenant did not resolve")
	}
	if ref.Controller != "cell-a" {
		t.Fatalf("ref names %q, want slot 0's cell-a", ref.Controller)
	}
}
