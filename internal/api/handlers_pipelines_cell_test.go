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

// tenantCellTenant is the tenant every tenant cell test reads, and
// tenantCellNamespace is the namespace of the cell that hosts it.
const (
	tenantCellTenant    = "payments"
	tenantCellNamespace = "cells"
	tenantCellName      = "cell-a"
)

// tenantCellClaims names the caller of every tenant cell test.
var tenantCellClaims = &auth.Claims{Subject: "cell-user"}

// tenantCellAuthorizer grants pipelines:read on the tenants named, and
// controllers:read in the namespaces named. An empty namespace list
// leaves the caller without the controller grant, so canOpenCell is
// false.
func tenantCellAuthorizer(tenants []string, controllerNamespaces []string) *Authorizer {
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

// tenantCellObject builds a placed pipelines Tenant carrying one Placed
// condition reason. Every tenant cell test binds the tenant to the one
// cell tenantCellFixture seeds.
func tenantCellObject(cluster, reason string) *unstructured.Unstructured {
	obj := placedTenantObject(tenantCellTenant, cluster, tenantCellNamespace, tenantCellName)
	if reason != "" {
		_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{
			"type":   v1alpha1.TenantConditionPlaced,
			"status": "True",
			"reason": reason,
		}}, "status", "conditions")
	}
	return obj
}

// unplacedPipelinesTenantObject builds a pipelines Tenant that waits for its
// first cell: spec.pipelines is set and status.placement.cell is empty.
func unplacedPipelinesTenantObject(name, reason string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Tenant",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"namespaces": []any{"ns-a"},
			"pipelines":  map[string]any{},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type":   v1alpha1.TenantConditionPlaced,
				"status": "False",
				"reason": reason,
			}},
		},
	}}
	return obj
}

// tenantCellFixture seeds one cell hosting three tenants, plus the tenant
// that names it.
func tenantCellFixture(t *testing.T, appliedTenants []string) (*fakeResourceClient, *fakeCellReader) {
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
					Tenants:        []string{tenantCellTenant, "acme", "beta"},
					AppliedTenants: appliedTenants,
				},
				MiteStatus: &v1alpha1.MiteStatus{JenkinsHealth: "healthy", JenkinsVersion: "2.531.1"},
			},
		},
	}
	reader := newFakeCellReader()
	reader.tenantUsage["core/cells/cell-a/"+tenantCellTenant] = []recordstore.CellJobUsage{
		{JobPath: tenantCellTenant + "/release/eu", CellUsageCounts: recordstore.CellUsageCounts{
			RunningNow: 1, RunsLast24h: 2, FailedLast24h: 1, Jobs: 1,
		}},
		{JobPath: tenantCellTenant + "/build/nightly", CellUsageCounts: recordstore.CellUsageCounts{
			RunsLast24h: 4, FailedLast24h: 1, Jobs: 3,
		}},
	}
	reader.tenantUsage["core/cells/cell-a/acme"] = []recordstore.CellJobUsage{
		{JobPath: "acme/app", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 9, Jobs: 9}},
	}
	return client, reader
}

// tenantCellServer assembles a *Server wired for the tenant cell read.
func tenantCellServer(client *fakeResourceClient, authorizer *Authorizer, reader recordstore.Reader) *Server {
	store := storeFromFake(client)
	brood := newFakeBrood(client)
	deps := &Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authorizer,
		RecordStore:       reader,
		TenantResolver:    NewTenantTenantResolver(store, brood, authorizer),
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	}
	return NewServer(deps)
}

// doTenantCellRequest calls handleGetTenantCell for one tenant.
func doTenantCellRequest(t *testing.T, srv *Server, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/pipelines/"+tenant+"/cell", nil)
	req.SetPathValue("tenant", tenant)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), tenantCellClaims))
	rec := httptest.NewRecorder()
	srv.handleGetTenantCell(rec, req)
	return rec
}

// decodeTenantCell decodes one tenantCellJSON body.
func decodeTenantCell(t *testing.T, rec *httptest.ResponseRecorder) tenantCellJSON {
	t.Helper()
	var out tenantCellJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode tenant cell: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// TestGetTenantCellCarriesTheWholeBodyWithoutTheControllerGrant asserts a
// caller with the tenant read grant and no controllers:read receives
// every field, with canOpenCell false and no neighbour named.
func TestGetTenantCellCarriesTheWholeBodyWithoutTheControllerGrant(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant, "acme", "beta"})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	if body.State != tenantCellPlaced {
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

// TestGetTenantCellSetsTheHintForAnOperator asserts canOpenCell is true for
// a caller who also holds the controller read grant.
func TestGetTenantCellSetsTheHintForAnOperator(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, []string{"cells"}), reader)

	body := decodeTenantCell(t, doTenantCellRequest(t, srv, tenantCellTenant))
	if !body.CanOpenCell {
		t.Error("canOpenCell = false, want true for a caller holding controllers:read")
	}
}

// TestGetTenantCellRefusesAnUnreadableTenant asserts a caller with no tenant
// grant is not told the tenant exists.
func TestGetTenantCellRefusesAnUnreadableTenant(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{"other"}, nil), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestGetTenantCellDeniesWithoutAuthorizer asserts a nil authorizer denies
// the path.
func TestGetTenantCellDeniesWithoutAuthorizer(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, nil, reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestGetTenantCellWithoutRecordStore asserts the path answers 503 when the
// record store is absent.
func TestGetTenantCellWithoutRecordStore(t *testing.T) {
	client, _ := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), nil)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestGetTenantCellReportsMoving asserts a Placed reason of TenantMoving
// wins over the applied-tenant rule.
func TestGetTenantCellReportsMoving(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantMoving))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	body := decodeTenantCell(t, doTenantCellRequest(t, srv, tenantCellTenant))
	if body.State != tenantCellMoving {
		t.Errorf("state = %q, want moving", body.State)
	}
	if body.Reason != v1alpha1.TenantReasonTenantMoving {
		t.Errorf("reason = %q, want TenantMoving", body.Reason)
	}
}

// TestGetTenantCellReportsRehydrating asserts a placed tenant the cell's
// appliedTenants does not name reports rehydrating.
func TestGetTenantCellReportsRehydrating(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{"acme"})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	body := decodeTenantCell(t, doTenantCellRequest(t, srv, tenantCellTenant))
	if body.State != tenantCellRehydrating {
		t.Errorf("state = %q, want rehydrating", body.State)
	}
}

// TestGetTenantCellReportsUnplaced asserts a pipelines Tenant with no
// placement answers 200 with the Placed condition's reason.
func TestGetTenantCellReportsUnplaced(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, unplacedPipelinesTenantObject(tenantCellTenant, v1alpha1.TenantReasonAwaitingCell))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	if body.State != tenantCellUnplaced {
		t.Errorf("state = %q, want unplaced", body.State)
	}
	if body.Reason != v1alpha1.TenantReasonAwaitingCell {
		t.Errorf("reason = %q, want AwaitingCell", body.Reason)
	}
	if body.Cell != "" || body.FreeSlots != nil || body.RunningNow != nil {
		t.Errorf("unplaced carries state and reason only: %+v", body)
	}
}

// TestGetTenantCellReportsNotCellHosted asserts a single-controller tenant
// answers notCellHosted and no other field.
func TestGetTenantCellReportsNotCellHosted(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	client.controllers["solo"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "ns-a"},
	}
	srv := tenantCellServer(client, soloTenantAuthorizer("ns-a"), reader)

	rec := doTenantCellRequest(t, srv, "solo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	if body.State != tenantCellNotCellHosted {
		t.Errorf("state = %q, want notCellHosted", body.State)
	}
	if body.Reason != "" || body.Cell != "" || body.RunningNow != nil {
		t.Errorf("notCellHosted carries state alone: %+v", body)
	}
}

// TestGetTenantCellOmitsQueueLengthForARemoteCluster asserts a remote cell
// reports no queue length, and every other field keeps its value.
func TestGetTenantCellOmitsQueueLengthForARemoteCluster(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	reader.tenantUsage["remote/cells/cell-a/"+tenantCellTenant] = []recordstore.CellJobUsage{
		{JobPath: tenantCellTenant + "/release/eu", CellUsageCounts: recordstore.CellUsageCounts{RunningNow: 3}},
	}
	seedTenant(t, client.crdStore, tenantCellObject("remote", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)

	body := decodeTenantCell(t, doTenantCellRequest(t, srv, tenantCellTenant))
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

// narrowedTenantCellAuthorizer grants pipelines read on the tenantCellTenant
// tenant, narrowed to the job globs named.
func narrowedTenantCellAuthorizer(globs ...string) *Authorizer {
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
				Tenants: []v1alpha1.TenantScope{{Name: tenantCellTenant, Jobs: globs}},
			},
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

// TestGetTenantCellCountsRespectTheCallerGlobs asserts a caller narrowed to
// one job glob reads the load of the jobs that glob admits, and never the
// tenant's whole load. The tenant check admits the empty job path for a
// narrowed grant, so the counts, not the gate, carry the narrowing.
func TestGetTenantCellCountsRespectTheCallerGlobs(t *testing.T) {
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))
	srv := tenantCellServer(client, narrowedTenantCellAuthorizer("release/*"), reader)

	rec := doTenantCellRequest(t, srv, tenantCellTenant)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeTenantCell(t, rec)
	wantTenantCellCount(t, "runningNow", body.RunningNow, 1)
	wantTenantCellCount(t, "runsLast24h", body.RunsLast24h, 2)
	wantTenantCellCount(t, "failedLast24h", body.FailedLast24h, 1)
	wantTenantCellCount(t, "jobs", body.Jobs, 1)

	// The tenant-level fields stay tenant-level: the globs scope the
	// counts alone.
	if body.OtherTenantCount == nil || *body.OtherTenantCount != 2 {
		t.Errorf("otherTenantCount = %v, want 2", body.OtherTenantCount)
	}
	if strings.Contains(rec.Body.String(), "nightly") {
		t.Errorf("body names the hidden job: %s", rec.Body.String())
	}
}

// wantTenantCellCount compares one optional count of the tenant cell body.
func wantTenantCellCount(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s is absent, want %d", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", name, *got, want)
	}
}
