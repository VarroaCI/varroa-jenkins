package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakeAllPlacementReader answers a fixed set of every tenant's placement
// rows, or a fixed error. Every other Reader method panics.
type fakeAllPlacementReader struct {
	recordstore.Reader
	rows []recordstore.TenantPlacement
	err  error
}

func (f *fakeAllPlacementReader) ListAllTenantPlacements(context.Context) ([]recordstore.TenantPlacement, error) {
	return f.rows, f.err
}

// globalPipelinesAuthorizer grants pipelines read on every tenant to
// subject "u", through a binding with no scope. The role holds no
// admin rule.
func globalPipelinesAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "pipelines-viewer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"pipelines"}, Verbs: []string{"read"}},
		}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "global"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "pipelines-viewer",
		},
	}}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

// pipelinesTenant builds a local Tenant. A non-empty cell places it.
func pipelinesTenant(name string, pipelines bool, cluster, cellNamespace, cell string) *v1alpha1.Tenant {
	tenant := &v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if pipelines {
		tenant.Spec.Pipelines = &v1alpha1.TenantPipelinesSpec{}
	}
	if cell != "" {
		tenant.Status.Placement = &v1alpha1.TenantPlacementStatus{
			Cluster: cluster, Cell: cell, CellNamespace: cellNamespace, Generation: 1,
		}
	}
	return tenant
}

// getPipelineTenants serves GET /pipelines against a server with the given
// local Tenants and record store, and decodes the body.
func getPipelineTenants(t *testing.T, authorizer *Authorizer, reader recordstore.Reader, tenants ...*v1alpha1.Tenant) pipelineTenantListJSON {
	t.Helper()
	store := crdstore.NewFake()
	for _, tenant := range tenants {
		crdstore.MustSeed(store, tenant)
	}
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Store:       store,
		Client:      client,
		Brood:       newFakeBrood(client),
		Authorizer:  authorizer,
		RecordStore: reader,
		Logger:      slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
	req = req.WithContext(contextWithClaims(req.Context(), resolverClaims))
	rec := httptest.NewRecorder()
	srv.handleListPipelineTenants(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body pipelineTenantListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func assertPipelineTenants(t *testing.T, got []pipelineTenantJSON, want ...pipelineTenantJSON) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListPipelineTenants_LocalPipelinesTenantOnly(t *testing.T) {
	body := getPipelineTenants(t, resolverAuthorizer("pay", "idle", "plain"), &fakeAllPlacementReader{},
		pipelinesTenant("pay", true, "core", "cells", "cell-a"),
		pipelinesTenant("idle", true, "", "", ""),
		pipelinesTenant("plain", false, "", "", ""),
	)
	if !body.RecordStore {
		t.Error("recordStore = false, want true")
	}
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "idle", Cluster: "core"},
		pipelineTenantJSON{Name: "pay", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTenants_MemberRowOnly(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 3},
	}}
	body := getPipelineTenants(t, resolverAuthorizer("member-a"), reader)
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTenants_LocalAndMemberRows(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 1},
		// The local Tenant of the same name hides this row.
		{Cluster: "member-c", Tenant: "pay", Namespace: "cells", Cell: "cell-x", Generation: 1},
	}}
	body := getPipelineTenants(t, resolverAuthorizer("pay", "member-a"), reader,
		pipelinesTenant("pay", true, "core", "cells", "cell-a"))
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
		pipelineTenantJSON{Name: "pay", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTenants_DropsARowTheCallerMayNotRead(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 1},
		{Cluster: "member-c", Tenant: "secret", Namespace: "cells", Cell: "cell-m", Generation: 1},
	}}
	body := getPipelineTenants(t, resolverAuthorizer("member-a"), reader,
		pipelinesTenant("hidden", true, "core", "cells", "cell-a"))
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTenants_NoReadableRowAnswersAnEmptyList(t *testing.T) {
	body := getPipelineTenants(t, resolverAuthorizer(), &fakeAllPlacementReader{},
		pipelinesTenant("pay", true, "core", "cells", "cell-a"))
	if body.Items == nil || len(body.Items) != 0 {
		t.Errorf("items = %+v, want an empty list", body.Items)
	}
}

func TestListPipelineTenants_RecordStoreOffAnswersLocalRows(t *testing.T) {
	tenant := pipelinesTenant("pay", true, "core", "cells", "cell-a")
	want := pipelineTenantJSON{Name: "pay", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true}

	off := getPipelineTenants(t, resolverAuthorizer("pay"), nil, tenant)
	if off.RecordStore {
		t.Error("recordStore = true with no record store, want false")
	}
	assertPipelineTenants(t, off.Items, want)

	old := getPipelineTenants(t, resolverAuthorizer("pay"), &fakeAllPlacementReader{err: recordstore.ErrSchemaTooOld}, tenant)
	if old.RecordStore {
		t.Error("recordStore = true with a schema too old, want false")
	}
	assertPipelineTenants(t, old.Items, want)
}

func TestListPipelineTenants_SkipsLocalEmptyAndTornDownRows(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "core", Tenant: "local-row", Namespace: "cells", Cell: "cell-c", Generation: 1},
		{Cluster: "member-c", Tenant: "no-cell", Namespace: "cells", Cell: "", Generation: 1},
		{Cluster: "member-c", Tenant: "torn-down", Namespace: "cells", Cell: "cell-m", Generation: 0},
		{Cluster: "member-c", Tenant: "ns/ctrl", Namespace: "cells", Cell: "cell-m", Generation: 1},
	}}
	body := getPipelineTenants(t, globalPipelinesAuthorizer(), reader)
	if len(body.Items) != 0 {
		t.Errorf("items = %+v, want none", body.Items)
	}
}

func TestListPipelineTenants_GlobalViewerSeesEveryRow(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 1},
	}}
	body := getPipelineTenants(t, globalPipelinesAuthorizer(), reader,
		pipelinesTenant("north", true, "core", "cells", "cell-a"),
		pipelinesTenant("south", true, "", "", ""),
	)
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
		pipelineTenantJSON{Name: "north", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true},
		pipelineTenantJSON{Name: "south", Cluster: "core"},
	)
}

func TestListPipelineTenants_OneNameOnTwoClustersAnswersTwoRows(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-a", Tenant: "shop", Namespace: "cells", Cell: "cell-a", Generation: 1},
		{Cluster: "member-b", Tenant: "shop", Namespace: "cells", Cell: "cell-b", Generation: 2},
	}}
	body := getPipelineTenants(t, resolverAuthorizer("shop"), reader)
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "shop", Cluster: "member-a", Cell: "cell-a", CellNamespace: "cells", Placed: true},
		pipelineTenantJSON{Name: "shop", Cluster: "member-b", Cell: "cell-b", CellNamespace: "cells", Placed: true},
	)
}

// nsTenant builds a local Tenant without spec.pipelines, naming namespaces.
func nsTenant(name string, namespaces ...string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TenantSpec{Namespaces: namespaces},
	}
}

// getPipelineTenantsWithControllers serves GET /pipelines with the given
// Controllers in the brood, next to the local Tenants.
func getPipelineTenantsWithControllers(t *testing.T, authorizer *Authorizer, controllers []*v1alpha1.Controller, tenants ...*v1alpha1.Tenant) pipelineTenantListJSON {
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
	srv := NewServer(&Dependencies{
		Store:      store,
		Client:     client,
		Brood:      newFakeBrood(client),
		Authorizer: authorizer,
		Logger:     slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
	req = req.WithContext(contextWithClaims(req.Context(), resolverClaims))
	rec := httptest.NewRecorder()
	srv.handleListPipelineTenants(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body pipelineTenantListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func controllerIn(name, namespace string) *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

func TestListPipelineTenants_TenantWithControllersInItsNamespacesIsListed(t *testing.T) {
	body := getPipelineTenantsWithControllers(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay")},
		nsTenant("payments", "team-pay"))
	assertPipelineTenants(t, body.Items,
		pipelineTenantJSON{Name: "payments", Cluster: "core", Placed: false})
}

func TestListPipelineTenants_TenantWithNoControllersIsNotListed(t *testing.T) {
	body := getPipelineTenantsWithControllers(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "other")},
		nsTenant("mt-tenant", "mt-tenant-ns"))
	assertPipelineTenants(t, body.Items)
}

func TestListPipelineTenants_TenantWithUnreadableControllersIsNotListed(t *testing.T) {
	body := getPipelineTenantsWithControllers(t, namespaceAuthorizer("other"),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay")},
		nsTenant("payments", "team-pay"))
	assertPipelineTenants(t, body.Items)
}

func TestListPipelineTenants_TenantPastTheTenantCeilingIsNotListed(t *testing.T) {
	var controllers []*v1alpha1.Controller
	for i := 0; i <= maxTenantTenants; i++ {
		controllers = append(controllers, controllerIn(fmt.Sprintf("svc-%02d", i), "tenant-big"))
	}
	body := getPipelineTenantsWithControllers(t, globalPipelinesAuthorizer(), controllers,
		nsTenant("big", "tenant-big"))
	assertPipelineTenants(t, body.Items)
}
