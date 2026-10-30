package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

func TestRunScopesDedupesOneControllerTenant(t *testing.T) {
	srv := &Server{deps: &Dependencies{Authorizer: pipelineGrantAuthorizer([]string{"read"}, "tenant-pay/svc-a")}}
	ref := TenantRef{Cluster: "core", Tenant: "tenant-pay/svc-a", TenantName: "svc-a", Folder: ""}
	tenants := []readableTenant{
		{Row: pipelineTenantJSON{Name: "payments"}, Refs: []TenantRef{ref}},
		{Row: pipelineTenantJSON{Name: "billing"}, Refs: []TenantRef{ref}},
	}
	scopes, _ := srv.runScopes(resolverClaims, tenants)
	if len(scopes) != 1 {
		t.Fatalf("scopes = %+v; want one scope for the shared controller tenant", scopes)
	}
}

func TestListFleetRunsNilRecordStore(t *testing.T) {
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), nil,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	_, serr, err := srv.ListFleetRuns(context.Background(), resolverClaims, FleetRunsParams{Limit: 50})
	if err != nil || serr == nil || serr.Status != http.StatusServiceUnavailable {
		t.Fatalf("service %+v, err %v; want 503", serr, err)
	}
}

func TestRunScopesKeepEveryRailNameOfASharedScope(t *testing.T) {
	srv := &Server{deps: &Dependencies{Authorizer: pipelineGrantAuthorizer([]string{"read"}, "tenant-pay/svc-a")}}
	ref := TenantRef{Cluster: "core", Tenant: "tenant-pay/svc-a", TenantName: "svc-a", Folder: ""}
	tenants := []readableTenant{
		{Row: pipelineTenantJSON{Name: "payments"}, Refs: []TenantRef{ref}},
		{Row: pipelineTenantJSON{Name: "billing"}, Refs: []TenantRef{ref}},
	}
	_, names := srv.runScopes(resolverClaims, tenants)
	got := names["core/tenant-pay/svc-a"]
	if len(got) != 2 || got[0] != "payments" || got[1] != "billing" {
		t.Fatalf("rail names = %v; want payments and billing", got)
	}
}

func TestListFleetRunsLabelsSharedRowsWithTheRequestedRail(t *testing.T) {
	srv := readableServer(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay")},
		nsTenant("payments", "team-pay"), nsTenant("billing", "team-pay"))
	success := "SUCCESS"
	run := recordstore.Run{
		RunID: "0192f000-0000-7000-8000-000000000009", Cluster: "core", Tenant: "team-pay/svc-a",
		Namespace: "team-pay", Cell: "svc-a", JobPath: "team-pay/svc-a/job", Status: "COMPLETED",
		Result: &success, CreatedAt: time.Now().UTC(),
	}
	srv.deps.RecordStore = &fakeRecordReader{acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{run}}}}
	page, serr, err := srv.ListFleetRuns(context.Background(), resolverClaims, FleetRunsParams{Tenants: []string{"billing"}, Limit: 50})
	if err != nil || serr != nil {
		t.Fatalf("ListFleetRuns: service %+v, err %v", serr, err)
	}
	if len(page.Items) != 1 || page.Items[0].Tenant != "billing" {
		t.Fatalf("items = %+v; want one row labelled billing", page.Items)
	}
}

func TestFleetRunUpstreamRedactedWithoutReadOfUpstream(t *testing.T) {
	upstreamTenant, upstreamJob := "secret", "secret/deploy"
	upstreamNumber := int64(9)
	run := fleetRun("pay/api", "")
	run.UpstreamTenant = &upstreamTenant
	run.UpstreamJob = &upstreamJob
	run.UpstreamNumber = &upstreamNumber
	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{run}}}}
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), reader,
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"))
	page := decodeFleetPage(t, getFleetRuns(t, srv, ""))
	if len(page.Items) != 1 || page.Items[0].Upstream == nil {
		t.Fatalf("items = %+v; want one row with an upstream link", page.Items)
	}
	up := page.Items[0].Upstream
	if up.Tenant != "secret" || up.Job != "" || up.BuildNumber != 0 {
		t.Fatalf("upstream = %+v; want the tenant only", up)
	}
	if page.Items[0].Trigger.Label != "Upstream" {
		t.Fatalf("trigger label = %q; want Upstream without the job", page.Items[0].Trigger.Label)
	}
}
