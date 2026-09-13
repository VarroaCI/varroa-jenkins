package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// teamCellTenant is the team every team cell test reads, and
// teamCellNamespace is the namespace of the cell that hosts it.
const (
	teamCellTenant    = "payments"
	teamCellNamespace = "cells"
	teamCellName      = "cell-a"
)

// teamCellClaims names the caller of every team cell test.
var teamCellClaims = &auth.Claims{Subject: "cell-user"}

// teamCellAuthorizer grants pipelines:read on the tenants named, and
// controllers:read in the namespaces named. An empty namespace list
// leaves the caller without the controller grant, so canOpenCell is
// false.
func teamCellAuthorizer(tenants []string, controllerNamespaces []string) *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cell-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	var bindings []*v1alpha1.VarroaRoleBinding
	if len(tenants) > 0 {
		scope := &v1alpha1.VarroaRoleBindingScope{}
		for _, tenant := range tenants {
			scope.Tenants = append(scope.Tenants, v1alpha1.TenantScope{Name: tenant})
		}
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-user"}},
				RoleRef:  "pipelines-viewer",
				Scope:    scope,
			},
		})
	}
	if len(controllerNamespaces) > 0 {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "controller-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-user"}},
				RoleRef:  "cell-viewer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: controllerNamespaces},
			},
		})
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

// soloTenantAuthorizer grants pipelines:read on every controller tenant
// in namespace. A single-controller tenant is admitted by a
// namespace-scoped binding, never by a tenant-scoped one.
func soloTenantAuthorizer(namespace string) *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "namespace-pipelines-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-user"}},
			RoleRef:  "pipelines-viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{namespace}},
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

// teamCellObject builds a placed pipelines Team carrying one Placed
// condition reason. Every team cell test binds the tenant to the one
// cell teamCellFixture seeds.
func teamCellObject(cluster, reason string) *unstructured.Unstructured {
	obj := placedTeamObject(teamCellTenant, cluster, teamCellNamespace, teamCellName)
	if reason != "" {
		_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{
			"type":   v1alpha1.TeamConditionPlaced,
			"status": "True",
			"reason": reason,
		}}, "status", "conditions")
	}
	return obj
}

// unplacedPipelinesTeamObject builds a pipelines Team that waits for its
// first cell: spec.pipelines is set and status.placement.cell is empty.
func unplacedPipelinesTeamObject(name, reason string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Team",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"namespaces": []any{"ns-a"},
			"pipelines":  map[string]any{},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type":   v1alpha1.TeamConditionPlaced,
				"status": "False",
				"reason": reason,
			}},
		},
	}}
	return obj
}

// teamCellFixture seeds one cell hosting three tenants, plus the team
// that names it.
func teamCellFixture(t *testing.T, appliedTenants []string) (*fakeResourceClient, *fakeCellReader) {
	t.Helper()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"cell-a": {
			ObjectMeta: metav1.ObjectMeta{
				Name: "cell-a", Namespace: "cells",
				Labels: map[string]string{cells.LabelCellPool: "pool-a", cells.LabelCellProfile: "lts-2-531"},
			},
			Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 5}},
			Status: v1alpha1.ControllerStatus{
				Cell: &v1alpha1.CellStatus{
					Tenants:        []string{teamCellTenant, "acme", "beta"},
					AppliedTenants: appliedTenants,
				},
				MiteStatus: &v1alpha1.MiteStatus{JenkinsHealth: "healthy", JenkinsVersion: "2.531.1"},
			},
		},
	}
	reader := newFakeCellReader()
	reader.tenantUsage["core/cells/cell-a/"+teamCellTenant] = []recordstore.CellJobUsage{
		{JobPath: teamCellTenant + "/release/eu", CellUsageCounts: recordstore.CellUsageCounts{
			RunningNow: 1, RunsLast24h: 2, FailedLast24h: 1, Jobs: 1,
		}},
		{JobPath: teamCellTenant + "/build/nightly", CellUsageCounts: recordstore.CellUsageCounts{
			RunsLast24h: 4, FailedLast24h: 1, Jobs: 3,
		}},
	}
	reader.tenantUsage["core/cells/cell-a/acme"] = []recordstore.CellJobUsage{
		{JobPath: "acme/app", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 9, Jobs: 9}},
	}
	return client, reader
}

// teamCellServer assembles a *Server wired for the team cell read.
func teamCellServer(client *fakeResourceClient, authorizer *Authorizer, reader recordstore.Reader) *Server {
	store := storeFromFake(client)
	brood := newFakeBrood(client)
	deps := &Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authorizer,
		RecordStore:       reader,
		TenantResolver:    NewTeamTenantResolver(store, brood, authorizer),
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	}
	return NewServer(deps)
}

// doTeamCellRequest calls handleGetTeamCell for one team.
func doTeamCellRequest(t *testing.T, srv *Server, team string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/pipelines/"+team+"/cell", nil)
	req.SetPathValue("team", team)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), teamCellClaims))
	rec := httptest.NewRecorder()
	srv.handleGetTeamCell(rec, req)
	return rec
}

// decodeTeamCell decodes one teamCellJSON body.
func decodeTeamCell(t *testing.T, rec *httptest.ResponseRecorder) teamCellJSON {
	t.Helper()
	var out teamCellJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode team cell: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// TestGetTeamCellCarriesTheWholeBodyWithoutTheControllerGrant asserts a
// caller with the tenant read grant and no controllers:read receives
// every field, with canOpenCell false and no neighbour named.
func TestGetTeamCellCarriesTheWholeBodyWithoutTheControllerGrant(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant, "acme", "beta"})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTeamCell(t, rec)
	if body.State != teamCellPlaced {
		t.Errorf("state = %q, want placed", body.State)
	}
	if body.CanOpenCell {
		t.Error("canOpenCell = true, want false without the controller grant")
	}
	if body.Cell != "cell-a" || body.Namespace != "cells" || body.Cluster != "core" {
		t.Errorf("cell coordinates = %q/%q/%q", body.Cluster, body.Namespace, body.Cell)
	}
	if body.CellPool != "pool-a" || body.Profile != "lts-2-531" {
		t.Errorf("cellPool = %q, profile = %q", body.CellPool, body.Profile)
	}
	if body.RunningNow == nil || *body.RunningNow != 1 {
		t.Errorf("runningNow = %v, want 1", body.RunningNow)
	}
	if body.RunsLast24h == nil || *body.RunsLast24h != 6 {
		t.Errorf("runsLast24h = %v, want 6", body.RunsLast24h)
	}
	if body.FailedLast24h == nil || *body.FailedLast24h != 2 {
		t.Errorf("failedLast24h = %v, want 2", body.FailedLast24h)
	}
	if body.Jobs == nil || *body.Jobs != 4 {
		t.Errorf("jobs = %v, want 4", body.Jobs)
	}
	if body.OtherTenantCount == nil || *body.OtherTenantCount != 2 {
		t.Errorf("otherTenantCount = %v, want 2", body.OtherTenantCount)
	}
	if body.FreeSlots == nil || *body.FreeSlots != 2 {
		t.Errorf("freeSlots = %v, want 2", body.FreeSlots)
	}
	for _, neighbour := range []string{"acme", "beta"} {
		if strings.Contains(rec.Body.String(), neighbour) {
			t.Errorf("body names the neighbour %q: %s", neighbour, rec.Body.String())
		}
	}
}

// TestGetTeamCellSetsTheHintForAnOperator asserts canOpenCell is true for
// a caller who also holds the controller read grant.
func TestGetTeamCellSetsTheHintForAnOperator(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, []string{"cells"}), reader)

	body := decodeTeamCell(t, doTeamCellRequest(t, srv, teamCellTenant))
	if !body.CanOpenCell {
		t.Error("canOpenCell = false, want true for a caller holding controllers:read")
	}
}

// TestGetTeamCellRefusesAnUnreadableTeam asserts a caller with no tenant
// grant is not told the team exists.
func TestGetTeamCellRefusesAnUnreadableTeam(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, teamCellAuthorizer([]string{"other"}, nil), reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestGetTeamCellDeniesWithoutAuthorizer asserts a nil authorizer denies
// the path.
func TestGetTeamCellDeniesWithoutAuthorizer(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, nil, reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestGetTeamCellWithoutRecordStore asserts the path answers 503 when the
// record store is absent.
func TestGetTeamCellWithoutRecordStore(t *testing.T) {
	client, _ := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), nil)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestGetTeamCellReportsMoving asserts a Placed reason of TenantMoving
// wins over the applied-tenant rule.
func TestGetTeamCellReportsMoving(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantMoving))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	body := decodeTeamCell(t, doTeamCellRequest(t, srv, teamCellTenant))
	if body.State != teamCellMoving {
		t.Errorf("state = %q, want moving", body.State)
	}
	if body.Reason != v1alpha1.TeamReasonTenantMoving {
		t.Errorf("reason = %q, want TenantMoving", body.Reason)
	}
}

// TestGetTeamCellReportsRehydrating asserts a placed team the cell's
// appliedTenants does not name reports rehydrating.
func TestGetTeamCellReportsRehydrating(t *testing.T) {
	client, reader := teamCellFixture(t, []string{"acme"})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	body := decodeTeamCell(t, doTeamCellRequest(t, srv, teamCellTenant))
	if body.State != teamCellRehydrating {
		t.Errorf("state = %q, want rehydrating", body.State)
	}
}

// TestGetTeamCellReportsUnplaced asserts a pipelines Team with no
// placement answers 200 with the Placed condition's reason.
func TestGetTeamCellReportsUnplaced(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, unplacedPipelinesTeamObject(teamCellTenant, v1alpha1.TeamReasonAwaitingCell))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTeamCell(t, rec)
	if body.State != teamCellUnplaced {
		t.Errorf("state = %q, want unplaced", body.State)
	}
	if body.Reason != v1alpha1.TeamReasonAwaitingCell {
		t.Errorf("reason = %q, want AwaitingCell", body.Reason)
	}
	if body.Cell != "" || body.FreeSlots != nil || body.RunningNow != nil {
		t.Errorf("unplaced carries state and reason only: %+v", body)
	}
}

// TestGetTeamCellReportsNotCellHosted asserts a single-controller tenant
// answers notCellHosted and no other field.
func TestGetTeamCellReportsNotCellHosted(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	client.controllers["solo"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "ns-a"},
	}
	srv := teamCellServer(client, soloTenantAuthorizer("ns-a"), reader)

	rec := doTeamCellRequest(t, srv, "solo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTeamCell(t, rec)
	if body.State != teamCellNotCellHosted {
		t.Errorf("state = %q, want notCellHosted", body.State)
	}
	if body.Reason != "" || body.Cell != "" || body.RunningNow != nil {
		t.Errorf("notCellHosted carries state alone: %+v", body)
	}
}

// TestGetTeamCellOmitsQueueLengthForARemoteCluster asserts a remote cell
// reports no queue length, and every other field keeps its value.
func TestGetTeamCellOmitsQueueLengthForARemoteCluster(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	reader.tenantUsage["remote/cells/cell-a/"+teamCellTenant] = []recordstore.CellJobUsage{
		{JobPath: teamCellTenant + "/release/eu", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 3}},
	}
	seedTeam(t, client.crdStore, teamCellObject("remote", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, teamCellAuthorizer([]string{teamCellTenant}, nil), reader)

	body := decodeTeamCell(t, doTeamCellRequest(t, srv, teamCellTenant))
	if body.QueueLength != nil {
		t.Errorf("queueLength = %v, want absent", *body.QueueLength)
	}
	if body.Cluster != "remote" {
		t.Errorf("cluster = %q, want remote", body.Cluster)
	}
	if body.RunningNow == nil || *body.RunningNow != 3 {
		t.Errorf("runningNow = %v, want 3", body.RunningNow)
	}
}

// narrowedTeamCellAuthorizer grants pipelines read on the teamCellTenant
// tenant, narrowed to the job globs named.
func narrowedTeamCellAuthorizer(globs ...string) *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "narrow"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cell-user"}},
			RoleRef:  "pipelines-viewer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: teamCellTenant, Jobs: globs}},
			},
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

// TestGetTeamCellCountsRespectTheCallerGlobs asserts a caller narrowed to
// one job glob reads the load of the jobs that glob admits, and never the
// tenant's whole load. The tenant check admits the empty job path for a
// narrowed grant, so the counts, not the gate, carry the narrowing.
func TestGetTeamCellCountsRespectTheCallerGlobs(t *testing.T) {
	client, reader := teamCellFixture(t, []string{teamCellTenant})
	seedTeam(t, client.crdStore, teamCellObject("core", v1alpha1.TeamReasonTenantPlaced))
	srv := teamCellServer(client, narrowedTeamCellAuthorizer("release/*"), reader)

	rec := doTeamCellRequest(t, srv, teamCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTeamCell(t, rec)
	wantTeamCellCount(t, "runningNow", body.RunningNow, 1)
	wantTeamCellCount(t, "runsLast24h", body.RunsLast24h, 2)
	wantTeamCellCount(t, "failedLast24h", body.FailedLast24h, 1)
	wantTeamCellCount(t, "jobs", body.Jobs, 1)

	// The tenant-level fields stay tenant-level: the globs scope the
	// counts alone.
	if body.OtherTenantCount == nil || *body.OtherTenantCount != 2 {
		t.Errorf("otherTenantCount = %v, want 2", body.OtherTenantCount)
	}
	if strings.Contains(rec.Body.String(), "nightly") {
		t.Errorf("body names the hidden job: %s", rec.Body.String())
	}
}

// wantTeamCellCount compares one optional count of the team cell body.
func wantTeamCellCount(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s is absent, want %d", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", name, *got, want)
	}
}
