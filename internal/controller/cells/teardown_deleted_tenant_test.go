package cells

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// capturePurges answers success and records every purge request.
func capturePurges(in *Inputs) *[]PurgeRequest {
	var got []PurgeRequest
	in.RequestPurge = func(_ context.Context, req PurgeRequest) (bool, error) {
		got = append(got, req)
		return true, nil
	}
	return &got
}

// deletedTenant answers a pipelines Tenant under deletion that still holds the
// placement finalizer.
func deletedTenant() *v1alpha1.Tenant {
	tenant := testTenant()
	tenant.UID = "uid-deleted"
	now := metav1.Now()
	tenant.DeletionTimestamp = &now
	tenant.Finalizers = []string{v1alpha1.TenantFinalizerTenantPlacement}
	return tenant
}

// TestPlace_TeardownOfADeletedTenantReportsTheDeletion pins that the purge of
// a drained cell reports a deleted Tenant, so the recorder retires its
// incarnation.
func TestPlace_TeardownOfADeletedTenantReportsTheDeletion(t *testing.T) {
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	got := capturePurges(&in)

	if _, err := Place(context.Background(), in, deletedTenant(), placedStatus(1)); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("purge requests = %d, want 1", len(*got))
	}
	req := (*got)[0]
	if !req.TenantDeleted || req.Incarnation != "uid-deleted" || req.Cell != "" {
		t.Fatalf("purge request = %+v, want a whole-tenant purge that reports the deleted uid-deleted", req)
	}
}

// TestPlace_TeardownWithAnAbsentCellReportsTheDeletion pins that the
// best-effort purge of an absent cell reports a deleted Tenant too.
func TestPlace_TeardownWithAnAbsentCellReportsTheDeletion(t *testing.T) {
	in := testInputs()
	in.Cells = nil
	got := capturePurges(&in)

	if _, err := Place(context.Background(), in, deletedTenant(), placedStatus(1)); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if len(*got) != 1 || !(*got)[0].TenantDeleted {
		t.Fatalf("purge requests = %+v, want one that reports the deleted Tenant", *got)
	}
}

// TestPlace_TeardownAfterDroppingPipelinesReportsNoDeletion pins that a
// Tenant that only drops spec.pipelines reports no deletion. It keeps its
// uid and can add the field again.
func TestPlace_TeardownAfterDroppingPipelinesReportsNoDeletion(t *testing.T) {
	for _, cells := range [][]CellCandidate{{readyCell("cell-a")}, nil} {
		in := testInputs()
		in.Cells = cells
		got := capturePurges(&in)
		tenant := testTenant()
		tenant.Spec.Pipelines = nil
		tenant.Finalizers = []string{v1alpha1.TenantFinalizerTenantPlacement}

		if _, err := Place(context.Background(), in, tenant, placedStatus(1)); err != nil {
			t.Fatalf("Place returned error: %v", err)
		}
		if len(*got) != 1 || (*got)[0].TenantDeleted {
			t.Fatalf("cells %v: purge requests = %+v, want one that reports no deletion", cells, *got)
		}
	}
}
