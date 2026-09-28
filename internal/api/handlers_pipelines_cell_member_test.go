package api

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestGetTeamCellReportsAMemberPlacedTeam asserts a Team that only a
// member cluster holds answers its placed cell, not notCellHosted: the
// core finds it through the record-store placement row.
func TestGetTeamCellReportsAMemberPlacedTeam(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	reader.tenantUsage["remote/cells/cell-a/"+teamCellTenant] = []recordstore.CellJobUsage{
		{JobPath: teamCellTenant + "/release/eu", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 2, Jobs: 1}},
	}
	placements := &fakePlacementReader{rows: map[string][]recordstore.TenantPlacement{
		teamCellTenant: {{Cluster: "remote", Namespace: "cells", Cell: "cell-a"}},
	}}
	authorizer := teamCellAuthorizer([]string{teamCellTenant}, nil)
	store := storeFromFake(client)
	brood := newFakeBrood(client)
	srv := NewServer(&Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authorizer,
		RecordStore:       reader,
		TenantResolver:    NewTeamTenantResolver(store, brood, authorizer, WithMemberPlacements(placements)),
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	})

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTeamCell(t, rec)
	if body.State != teamCellPlaced {
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
