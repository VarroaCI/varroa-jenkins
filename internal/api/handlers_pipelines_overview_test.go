package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// overviewFor answers the overview of srv at now, for the caller in claims.
func overviewFor(t *testing.T, srv *Server) PipelinesOverview {
	t.Helper()
	out, serviceErr, err := srv.PipelinesOverview(context.Background(), resolverClaims, time.Now().UTC())
	if serviceErr != nil || err != nil {
		t.Fatalf("PipelinesOverview: service %+v, err %v", serviceErr, err)
	}
	return out
}

func TestPipelinesOverviewRailOrderAndGrouping(t *testing.T) {
	reader := &fakeRecordReader{
		placements: []recordstore.TenantPlacement{
			{Cluster: "member-b", Tenant: "shop", Namespace: "ns", Cell: "c3", Generation: 1},
			{Cluster: "edge", Tenant: "shop", Namespace: "ns", Cell: "c4", Generation: 1},
		},
		overviewScopes: map[string]recordstore.ScopeOverview{
			"core/pay":      {Running: 2},
			"core/ops":      {Runs24h: 3, Failed24h: 3},
			"member-b/shop": {Runs24h: 4},
			"edge/shop":     {Runs24h: 5},
		},
	}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader,
		pipelinesTenant("pay", true, "core", "ns", "c1"),
		pipelinesTenant("ops", true, "core", "ns", "c2"),
		pipelinesTenant("idle", true, "", "", ""))
	out := overviewFor(t, srv)

	names := make([]string, 0, len(out.Tenants))
	byName := map[string]PipelinesOverviewTenant{}
	for _, tenant := range out.Tenants {
		names = append(names, tenant.Name)
		byName[tenant.Name] = tenant
	}
	want := []string{"ops", "pay", "shop", "idle"}
	if !sameNames(names, want) {
		t.Fatalf("rail order = %v; want %v", names, want)
	}
	if shop := byName["shop"]; len(shop.Clusters) != 2 || shop.Clusters[0] != "edge" || shop.Clusters[1] != "member-b" || shop.Runs24h != 9 {
		t.Errorf("shop = %+v; want clusters edge and member-b, and 9 runs", shop)
	}
	if byName["idle"].Placement != placementAwaitingCell {
		t.Errorf("idle placement = %q; want awaitingCell", byName["idle"].Placement)
	}
	if out.Attention.AwaitingCell != 1 {
		t.Errorf("awaitingCell = %d; want 1", out.Attention.AwaitingCell)
	}
	if out.Clusters != 3 {
		t.Errorf("clusters = %d; want 3 (core, member-b, edge)", out.Clusters)
	}
}

func TestPipelinesOverviewPassRate(t *testing.T) {
	// Two of the four counted runs passed. Finished runs that do not count stay out of the rate.
	reader := &fakeRecordReader{overviewTotals: recordstore.RunsOverviewTotals{Passed: 2, Counted: 4, Finished: 6}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if out.Totals.PassRate == nil || *out.Totals.PassRate != 0.5 {
		t.Fatalf("passRate = %v; want 0.5", out.Totals.PassRate)
	}

	// Every finished run is interrupted, so no run counts and the rate is absent.
	reader.overviewTotals = recordstore.RunsOverviewTotals{Finished: 3, Interrupted: 3, Counted: 0}
	out = overviewFor(t, srv)
	if out.Totals.PassRate != nil {
		t.Fatalf("passRate = %v; want nil when no run counts", *out.Totals.PassRate)
	}
}

func TestPipelinesOverviewLocksHeldOnlyReadable(t *testing.T) {
	reader := &fakeRecordReader{allLocks: []recordstore.FleetLock{
		{Cluster: "core", Tenant: "pay", JobPath: "pay/deploy", State: "held"},
		{Cluster: "core", Tenant: "pay", JobPath: "pay/secret", State: "held"},
		{Cluster: "core", Tenant: "other", JobPath: "other/x", State: "held"},
		{Cluster: "core", Tenant: "pay", JobPath: "pay/deploy", State: "waiting"},
	}}
	srv := fleetServer(t, pipelineJobGrantAuthorizer([]string{"read"}, "deploy"), reader,
		pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if out.Attention.LocksHeld != 1 {
		t.Fatalf("locksHeld = %d; want 1", out.Attention.LocksHeld)
	}
}

func TestPipelinesOverviewNoRecordStore(t *testing.T) {
	srv := fleetServer(t, globalPipelinesAuthorizer(), nil, pipelinesTenant("pay", true, "core", "ns", "c1"))
	_, serviceErr, err := srv.PipelinesOverview(context.Background(), resolverClaims, time.Now().UTC())
	if err != nil || serviceErr == nil || serviceErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("service %+v, err %v; want 503", serviceErr, err)
	}
}

// sameNames reports whether two name lists hold the same names in order.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
