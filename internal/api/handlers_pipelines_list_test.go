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

// pipelinesTeam builds a local Team. A non-empty cell places it.
func pipelinesTeam(name string, pipelines bool, cluster, cellNamespace, cell string) *v1alpha1.Team {
	team := &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if pipelines {
		team.Spec.Pipelines = &v1alpha1.TeamPipelinesSpec{}
	}
	if cell != "" {
		team.Status.Placement = &v1alpha1.TeamPlacementStatus{
			Cluster: cluster, Cell: cell, CellNamespace: cellNamespace, Generation: 1,
		}
	}
	return team
}

// getPipelineTeams serves GET /pipelines against a server with the given
// local Teams and record store, and decodes the body.
func getPipelineTeams(t *testing.T, authorizer *Authorizer, reader recordstore.Reader, teams ...*v1alpha1.Team) pipelineTeamListJSON {
	t.Helper()
	store := crdstore.NewFake()
	for _, team := range teams {
		crdstore.MustSeed(store, team)
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
	srv.handleListPipelineTeams(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body pipelineTeamListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func assertPipelineTeams(t *testing.T, got []pipelineTeamJSON, want ...pipelineTeamJSON) {
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

func TestListPipelineTeams_LocalPipelinesTeamOnly(t *testing.T) {
	body := getPipelineTeams(t, resolverAuthorizer("pay", "idle", "plain"), &fakeAllPlacementReader{},
		pipelinesTeam("pay", true, "core", "cells", "cell-a"),
		pipelinesTeam("idle", true, "", "", ""),
		pipelinesTeam("plain", false, "", "", ""),
	)
	if !body.RecordStore {
		t.Error("recordStore = false, want true")
	}
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "idle", Cluster: "core"},
		pipelineTeamJSON{Name: "pay", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTeams_MemberRowOnly(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 3},
	}}
	body := getPipelineTeams(t, resolverAuthorizer("member-a"), reader)
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTeams_LocalAndMemberRows(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 1},
		// The local Team of the same name hides this row.
		{Cluster: "member-c", Tenant: "pay", Namespace: "cells", Cell: "cell-x", Generation: 1},
	}}
	body := getPipelineTeams(t, resolverAuthorizer("pay", "member-a"), reader,
		pipelinesTeam("pay", true, "core", "cells", "cell-a"))
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
		pipelineTeamJSON{Name: "pay", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTeams_DropsARowTheCallerMayNotRead(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 1},
		{Cluster: "member-c", Tenant: "secret", Namespace: "cells", Cell: "cell-m", Generation: 1},
	}}
	body := getPipelineTeams(t, resolverAuthorizer("member-a"), reader,
		pipelinesTeam("hidden", true, "core", "cells", "cell-a"))
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
	)
}

func TestListPipelineTeams_NoReadableRowAnswersAnEmptyList(t *testing.T) {
	body := getPipelineTeams(t, resolverAuthorizer(), &fakeAllPlacementReader{},
		pipelinesTeam("pay", true, "core", "cells", "cell-a"))
	if body.Items == nil || len(body.Items) != 0 {
		t.Errorf("items = %+v, want an empty list", body.Items)
	}
}

func TestListPipelineTeams_RecordStoreOffAnswersLocalRows(t *testing.T) {
	team := pipelinesTeam("pay", true, "core", "cells", "cell-a")
	want := pipelineTeamJSON{Name: "pay", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true}

	off := getPipelineTeams(t, resolverAuthorizer("pay"), nil, team)
	if off.RecordStore {
		t.Error("recordStore = true with no record store, want false")
	}
	assertPipelineTeams(t, off.Items, want)

	old := getPipelineTeams(t, resolverAuthorizer("pay"), &fakeAllPlacementReader{err: recordstore.ErrSchemaTooOld}, team)
	if old.RecordStore {
		t.Error("recordStore = true with a schema too old, want false")
	}
	assertPipelineTeams(t, old.Items, want)
}

func TestListPipelineTeams_SkipsLocalEmptyAndTornDownRows(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "core", Tenant: "local-row", Namespace: "cells", Cell: "cell-c", Generation: 1},
		{Cluster: "member-c", Tenant: "no-cell", Namespace: "cells", Cell: "", Generation: 1},
		{Cluster: "member-c", Tenant: "torn-down", Namespace: "cells", Cell: "cell-m", Generation: 0},
		{Cluster: "member-c", Tenant: "ns/ctrl", Namespace: "cells", Cell: "cell-m", Generation: 1},
	}}
	body := getPipelineTeams(t, globalPipelinesAuthorizer(), reader)
	if len(body.Items) != 0 {
		t.Errorf("items = %+v, want none", body.Items)
	}
}

func TestListPipelineTeams_GlobalViewerSeesEveryRow(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-c", Tenant: "member-a", Namespace: "cells", Cell: "cell-m", Generation: 1},
	}}
	body := getPipelineTeams(t, globalPipelinesAuthorizer(), reader,
		pipelinesTeam("north", true, "core", "cells", "cell-a"),
		pipelinesTeam("south", true, "", "", ""),
	)
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "member-a", Cluster: "member-c", Cell: "cell-m", CellNamespace: "cells", Placed: true},
		pipelineTeamJSON{Name: "north", Cluster: "core", Cell: "cell-a", CellNamespace: "cells", Placed: true},
		pipelineTeamJSON{Name: "south", Cluster: "core"},
	)
}

func TestListPipelineTeams_OneNameOnTwoClustersAnswersTwoRows(t *testing.T) {
	reader := &fakeAllPlacementReader{rows: []recordstore.TenantPlacement{
		{Cluster: "member-a", Tenant: "shop", Namespace: "cells", Cell: "cell-a", Generation: 1},
		{Cluster: "member-b", Tenant: "shop", Namespace: "cells", Cell: "cell-b", Generation: 2},
	}}
	body := getPipelineTeams(t, resolverAuthorizer("shop"), reader)
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "shop", Cluster: "member-a", Cell: "cell-a", CellNamespace: "cells", Placed: true},
		pipelineTeamJSON{Name: "shop", Cluster: "member-b", Cell: "cell-b", CellNamespace: "cells", Placed: true},
	)
}

// nsTeam builds a local Team without spec.pipelines, naming namespaces.
func nsTeam(name string, namespaces ...string) *v1alpha1.Team {
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TeamSpec{Namespaces: namespaces},
	}
}

// getPipelineTeamsWithControllers serves GET /pipelines with the given
// Controllers in the brood, next to the local Teams.
func getPipelineTeamsWithControllers(t *testing.T, authorizer *Authorizer, controllers []*v1alpha1.Controller, teams ...*v1alpha1.Team) pipelineTeamListJSON {
	t.Helper()
	store := crdstore.NewFake()
	for _, team := range teams {
		crdstore.MustSeed(store, team)
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
	srv.handleListPipelineTeams(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body pipelineTeamListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func controllerIn(name, namespace string) *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

func TestListPipelineTeams_TeamWithControllersInItsNamespacesIsListed(t *testing.T) {
	body := getPipelineTeamsWithControllers(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay")},
		nsTeam("payments", "team-pay"))
	assertPipelineTeams(t, body.Items,
		pipelineTeamJSON{Name: "payments", Cluster: "core", Placed: false})
}

func TestListPipelineTeams_TeamWithNoControllersIsNotListed(t *testing.T) {
	body := getPipelineTeamsWithControllers(t, globalPipelinesAuthorizer(),
		[]*v1alpha1.Controller{controllerIn("svc-a", "other")},
		nsTeam("mt-tenant", "mt-tenant-ns"))
	assertPipelineTeams(t, body.Items)
}

func TestListPipelineTeams_TeamWithUnreadableControllersIsNotListed(t *testing.T) {
	body := getPipelineTeamsWithControllers(t, namespaceAuthorizer("other"),
		[]*v1alpha1.Controller{controllerIn("svc-a", "team-pay")},
		nsTeam("payments", "team-pay"))
	assertPipelineTeams(t, body.Items)
}

func TestListPipelineTeams_TeamPastTheTenantCeilingIsNotListed(t *testing.T) {
	var controllers []*v1alpha1.Controller
	for i := 0; i <= maxTeamTenants; i++ {
		controllers = append(controllers, controllerIn(fmt.Sprintf("svc-%02d", i), "team-big"))
	}
	body := getPipelineTeamsWithControllers(t, globalPipelinesAuthorizer(), controllers,
		nsTeam("big", "team-big"))
	assertPipelineTeams(t, body.Items)
}
