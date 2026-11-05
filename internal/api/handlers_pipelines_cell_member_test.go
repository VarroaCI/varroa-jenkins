package api

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestGetTenantCellCarriesTheStaleFlag asserts a stale standby reports
// stale=true, the current slot-0 member reports stale=false, and the raw
// body names the flag on every member.
func TestGetTenantCellCarriesTheStaleFlag(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	obj := tenantCellObject("core", v1alpha1.TenantReasonMembersStale)
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{
			"cell": "cell-a", "cellNamespace": "cells", "role": "active",
			"slot": int64(0), "applied": true, "stale": false,
		},
		map[string]any{
			"cell": "cell-c", "cellNamespace": "cells", "role": "standby",
			"applied": true, "stale": true,
		},
	}, "status", "placement", "members")
	seedTenant(t, client.crdStore, obj)
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	if body.State != tenantCellPlaced {
		t.Errorf("state = %q, want placed", body.State)
	}
	if body.Reason != v1alpha1.TenantReasonMembersStale {
		t.Errorf("reason = %q, want MembersStale", body.Reason)
	}
	if len(body.Members) != 2 {
		t.Fatalf("members = %+v, want two", body.Members)
	}
	if body.Members[0].Stale || !body.Members[1].Stale {
		t.Errorf("stale flags = %t/%t, want false/true", body.Members[0].Stale, body.Members[1].Stale)
	}
	if !strings.Contains(rec.Body.String(), `"stale":false`) {
		t.Errorf("body lacks an explicit stale:false: %s", rec.Body.String())
	}
}

// TestGetTenantCellReportsAMemberPlacedTenant asserts a Tenant that only a
// member cluster holds answers its placed cell, not notCellHosted: the
// core finds it through the record-store placement row.
func TestGetTenantCellReportsAMemberPlacedTenant(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	reader.tenantUsage["remote/cells/cell-a/"+tenantCellTenant] = []recordstore.CellJobUsage{
		{JobPath: tenantCellTenant + "/release/eu", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 2, Jobs: 1}},
	}
	placements := &fakePlacementReader{rows: map[string][]recordstore.TenantPlacement{
		tenantCellTenant: {{Cluster: "remote", Namespace: "cells", Cell: "cell-a", Generation: 1}},
	}}
	authorizer := tenantCellAuthorizer([]string{tenantCellTenant}, nil)
	store := storeFromFake(client)
	brood := newFakeBrood(client)
	srv := NewServer(&Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authorizer,
		RecordStore:       reader,
		TenantResolver:    NewTenantTenantResolver(store, brood, authorizer, WithMemberPlacements(placements)),
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	})

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	if body.State != tenantCellPlaced {
		t.Errorf("state = %q, want placed", body.State)
	}
	if body.Cluster != "remote" || body.Namespace != "cells" || body.Cell != "cell-a" {
		t.Errorf("cell coordinates = %q/%q/%q, want remote/cells/cell-a", body.Cluster, body.Namespace, body.Cell)
	}
	if body.RunningNow == nil || *body.RunningNow != 2 {
		t.Errorf("runningNow = %v, want 2", body.RunningNow)
	}
	if len(body.Members) != 1 || body.Members[0].Cell != "cell-a" {
		t.Errorf("members = %+v, want one slot-0 member on cell-a", body.Members)
	}
}
