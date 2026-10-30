package api

import (
	"context"
	"log/slog"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// readableServer builds a Server with the given local Tenants and
// controllers, for a direct call to readablePipelineTenants.
func readableServer(t *testing.T, authorizer *Authorizer, controllers []*v1alpha1.Controller, tenants ...*v1alpha1.Tenant) *Server {
	t.Helper()
	store := crdstore.NewFake()
	for _, tenant := range tenants {
		crdstore.MustSeed(store, tenant)
	}
	client := newFakeResourceClient()
	client.controllers = make(map[string]*v1alpha1.Controller, len(controllers))
	for _, cr := range controllers {
		client.controllers[cr.Name] = cr.DeepCopy()
	}
	return NewServer(&Dependencies{
		Store:      store,
		Client:     client,
		Brood:      newFakeBrood(client),
		Authorizer: authorizer,
		Logger:     slog.Default(),
	})
}

func TestReadablePipelineTenantsPlacement(t *testing.T) {
	srv := readableServer(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay")},
		pipelinesTenant("pay", true, "core", "cell-ns", "c1"),
		pipelinesTenant("idle", true, "", "", ""),
		nsTenant("payments", "team-pay"))
	got, _, err := srv.readablePipelineTenants(context.Background(), resolverClaims)
	if err != nil {
		t.Fatalf("readablePipelineTenants: %v", err)
	}
	want := map[string]struct {
		placement string
		refs      int
	}{
		"pay":      {placementPlaced, 1},
		"idle":     {placementAwaitingCell, 0},
		"payments": {placementControllers, 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tenants; want %d", len(got), len(want))
	}
	for _, tenant := range got {
		w, ok := want[tenant.Row.Name]
		if !ok {
			t.Fatalf("unexpected tenant %q", tenant.Row.Name)
		}
		if tenant.Placement != w.placement || len(tenant.Refs) != w.refs {
			t.Errorf("%s: placement %q with %d refs; want %q with %d",
				tenant.Row.Name, tenant.Placement, len(tenant.Refs), w.placement, w.refs)
		}
	}
}

func TestReadablePipelineTenantsFanOutKeepsEveryReadableController(t *testing.T) {
	srv := readableServer(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay"), controllerIn("svc-b", "team-pay")},
		nsTenant("payments", "team-pay"))
	got, _, err := srv.readablePipelineTenants(context.Background(), resolverClaims)
	if err != nil {
		t.Fatalf("readablePipelineTenants: %v", err)
	}
	if len(got) != 1 || len(got[0].Refs) != 2 {
		t.Fatalf("got %+v; want one tenant with two refs", got)
	}
	seen := map[string]bool{got[0].Refs[0].Tenant: true, got[0].Refs[1].Tenant: true}
	if !seen["team-pay/svc-a"] || !seen["team-pay/svc-b"] {
		t.Fatalf("ref tenants = %v; want team-pay/svc-a and team-pay/svc-b", seen)
	}
}

func TestRunScopesCarryJobPatterns(t *testing.T) {
	srv := &Server{deps: &Dependencies{Authorizer: pipelineJobGrantAuthorizer([]string{"read"}, "release/*")}}
	tenants := []readableTenant{{
		Row:       pipelineTenantJSON{Name: "pay", Cluster: "core", Placed: true},
		Placement: placementPlaced,
		Refs:      []TenantRef{{Cluster: "core", Tenant: "pay", TenantName: "pay", Folder: "pay"}},
	}}
	scopes, names := srv.runScopes(resolverClaims, tenants)
	if len(scopes) != 1 {
		t.Fatalf("scopes = %+v; want one scope", scopes)
	}
	got := scopes[0]
	if got.Cluster != "core" || got.Tenant != "pay" || len(got.JobPatterns) != 1 ||
		got.JobPatterns[0] != "^pay/(?:release/[^/]*)$" {
		t.Fatalf("scope = %+v; want core, pay, and the release pattern", got)
	}
	if len(names) != 1 || len(names["core/pay"]) != 1 || names["core/pay"][0] != "pay" {
		t.Fatalf("names = %v; want map[core/pay:pay]", names)
	}
}
