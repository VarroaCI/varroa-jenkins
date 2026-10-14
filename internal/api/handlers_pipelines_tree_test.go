package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// --- fixtures ---

// multibranchItemsYAML declares one multibranch item and one pipeline item
// under the folder "acme", so a listing reports two different kinds.
const multibranchItemsYAML = `
items:
  - kind: folder
    name: acme
    items:
      - kind: multibranch
        name: checkout-api
        sourcesList:
          - branchSource:
              source:
                git:
                  remote: https://example.invalid/checkout-api.git
                  credentialsId: git-cred
      - kind: pipeline
        name: app-a
        definition:
          script: "pipeline {}"
`

// teamItemsYAML declares jobs relative to a tenant bundle root. The
// manifest is re-rooted under the Team folder before the listing reads it.
const teamItemsYAML = `
items:
  - kind: folder
    name: release
    items:
      - kind: pipeline
        name: eu
        definition:
          script: "pipeline {}"
  - kind: folder
    name: build
    items:
      - kind: pipeline
        name: nightly
        definition:
          script: "pipeline {}"
`

// sharedPathItemsYAML declares one job path with the kind the caller
// names, so two tenants can declare the same path with different kinds.
func sharedPathItemsYAML(kind string) string {
	if kind == "multibranch" {
		return `
items:
  - kind: folder
    name: shared
    items:
      - kind: multibranch
        name: api
        sourcesList:
          - branchSource:
              source:
                git:
                  remote: https://example.invalid/api.git
                  credentialsId: git-cred
`
	}
	return `
items:
  - kind: folder
    name: shared
    items:
      - kind: pipeline
        name: api
        definition:
          script: "pipeline {}"
`
}

// newMultibranchControllerFixture seeds one controller whose bundle
// declares acme/checkout-api as a multibranch item and acme/app-a as a
// pipeline item.
func newMultibranchControllerFixture() *fakeResourceClient {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-a"}},
		},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"bundle-a": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-a", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "bundle-a-content"},
		},
	}
	client.configMaps = map[string]map[string]string{
		"bundle-a-content": {"items.yaml": multibranchItemsYAML},
	}
	return client
}

// newPlacedTeamFixture seeds the placed Team "payments" on the cell
// "cell-1", with a tenant bundle declaring release/eu and build/nightly.
func newPlacedTeamFixture(t *testing.T) *fakeResourceClient {
	t.Helper()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"cell-1": {ObjectMeta: metav1.ObjectMeta{Name: "cell-1", Namespace: "test-ns"}},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"payments-bundle": {
			ObjectMeta: metav1.ObjectMeta{Name: "payments-bundle", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "payments-content"},
		},
	}
	client.configMaps = map[string]map[string]string{
		"payments-content": {"items.yaml": teamItemsYAML},
	}
	obj := placedTeamObject("payments", "core", "test-ns", "cell-1")
	if err := unstructured.SetNestedMap(obj.Object, map[string]any{
		"name":      "payments-bundle",
		"namespace": "test-ns",
	}, "spec", "pipelines", "composedBundleRef"); err != nil {
		t.Fatalf("set composedBundleRef: %v", err)
	}
	seedTeam(t, client.crdStore, obj)
	return client
}

// listPipelinesFor issues GET /pipelines/{team} and decodes the envelope.
func listPipelinesFor(t *testing.T, srv *Server, team string) pipelineListResponse {
	t.Helper()
	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/"+team, map[string]string{"team": team})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pipelineListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func kindOf(t *testing.T, resp pipelineListResponse, jobPath string) string {
	t.Helper()
	for _, it := range resp.Items {
		if it.JobPath == jobPath {
			return it.Kind
		}
	}
	t.Fatalf("job %q absent from the listing: %+v", jobPath, resp.Items)
	return ""
}

// --- Task 2.2: the item kind ---

// TestHandleListPipelines_DeclaredMultibranchReportsItsKind asserts the
// scenario "A declared multibranch reports its kind".
func TestHandleListPipelines_DeclaredMultibranchReportsItsKind(t *testing.T) {
	client := newMultibranchControllerFixture()
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	resp := listPipelinesFor(t, srv, "ctrl-a")
	if got := kindOf(t, resp, "acme/checkout-api"); got != "multibranch" {
		t.Errorf("kind = %q, want multibranch", got)
	}
	if got := kindOf(t, resp, "acme/app-a"); got != "pipeline" {
		t.Errorf("kind = %q, want pipeline", got)
	}
}

// TestHandleListPipelines_RecordOnlyJobReportsUnknownKind asserts the
// scenario "A record-only job reports an unknown kind".
func TestHandleListPipelines_RecordOnlyJobReportsUnknownKind(t *testing.T) {
	client := newMultibranchControllerFixture()
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/undeclared", RecentResults: []string{"SUCCESS"}},
	}
	srv := pipelinesTestServer(client, authz, reader, nil)

	resp := listPipelinesFor(t, srv, "ctrl-a")
	if got := kindOf(t, resp, "acme/undeclared"); got != "unknown" {
		t.Errorf("kind = %q, want unknown", got)
	}
}

// TestHandleListPipelines_RecordRowDoesNotClearDeclaredKind asserts the
// scenario "A record row does not clear a declared kind". The record
// summary replaces the whole row, so the kind is merged in its own map.
func TestHandleListPipelines_RecordRowDoesNotClearDeclaredKind(t *testing.T) {
	client := newMultibranchControllerFixture()
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/checkout-api", RecentResults: []string{"SUCCESS"}},
	}
	srv := pipelinesTestServer(client, authz, reader, nil)

	resp := listPipelinesFor(t, srv, "ctrl-a")
	if got := kindOf(t, resp, "acme/checkout-api"); got != "multibranch" {
		t.Errorf("kind = %q, want multibranch", got)
	}
}

// TestHandleListPipelines_MultibranchDeclarationWinsOverPipeline asserts
// the scenario "A multibranch declaration wins over a pipeline
// declaration". Two controller tenants of one team declare shared/api with
// different kinds. The fan-out order is the map order of the fake
// controller set, so the request runs many times and the result must not
// change.
func TestHandleListPipelines_MultibranchDeclarationWinsOverPipeline(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-pipe": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-pipe", Namespace: "ns-a"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-pipe"}},
		},
		"ctrl-multi": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-multi", Namespace: "ns-b"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-multi"}},
		},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"bundle-pipe": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-pipe", Namespace: "ns-a"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "content-pipe"},
		},
		"bundle-multi": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-multi", Namespace: "ns-b"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "content-multi"},
		},
	}
	client.configMaps = map[string]map[string]string{
		"content-pipe":  {"items.yaml": sharedPathItemsYAML("pipeline")},
		"content-multi": {"items.yaml": sharedPathItemsYAML("multibranch")},
	}
	seedTeam(t, client.crdStore, unplacedTeamObject("payments", "ns-a", "ns-b"))

	authz := namespaceAuthorizer("ns-a", "ns-b")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	for i := 0; i < 40; i++ {
		resp := listPipelinesFor(t, srv, "payments")
		if got := kindOf(t, resp, "shared/api"); got != "multibranch" {
			t.Fatalf("run %d: kind = %q, want multibranch whichever tenant is read first", i, got)
		}
	}
}

// --- Task 2.3: the tenant folder ---

// TestHandleListPipelines_PlacedTeamReportsItsFolder asserts the scenario
// "A placed team reports its folder".
func TestHandleListPipelines_PlacedTeamReportsItsFolder(t *testing.T) {
	client := newPlacedTeamFixture(t)
	authz := resolverAuthorizer("payments")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	resp := listPipelinesFor(t, srv, "payments")
	if resp.TenantFolder != "payments" {
		t.Fatalf("tenantFolder = %q, want payments", resp.TenantFolder)
	}
}

// TestHandleListPipelines_ControllerTenantReportsEmptyFolder asserts the
// scenario "A controller tenant reports an empty folder". The field is
// present and empty, never absent.
func TestHandleListPipelines_ControllerTenantReportsEmptyFolder(t *testing.T) {
	client := newSingleControllerFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/ctrl-a", map[string]string{"team": "ctrl-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	value, present := raw["tenantFolder"]
	if !present {
		t.Fatalf("tenantFolder absent from the envelope: %s", rec.Body.String())
	}
	if value != "" {
		t.Fatalf("tenantFolder = %v, want the empty string", value)
	}
}

// --- Task 3.2: the nil-authorizer rule at the new seam ---

// TestTenantGrants_NilAuthorizerDenies asserts that a nil *Authorizer
// denies before the method reads a field, so a listing that resolves
// grants once keeps the canonical nil-authorizer rule.
func TestTenantGrants_NilAuthorizerDenies(t *testing.T) {
	var authz *Authorizer
	grants, ok := authz.tenantGrants(testClaims(), "payments", "payments")
	if ok {
		t.Fatalf("tenantGrants on a nil authorizer reported ok")
	}
	if grants != nil {
		t.Fatalf("tenantGrants on a nil authorizer returned %+v, want nil", grants)
	}
}

// --- Task 4.3: the glob filter on the listing ---

// narrowedPaymentsAuthorizer grants pipelines read on the placed Team
// tenant "payments", narrowed to the job globs named.
func narrowedPaymentsAuthorizer(globs ...string) *Authorizer {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pl"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "narrow"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "pl",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: globs}},
			},
		},
	}
	return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
}

// TestHandleListPipelines_NarrowedCallerReadsOnlyItsOwnJobs asserts the
// scenario "A narrowed caller reads only its own jobs". The refused job's
// name must be absent from the whole response body, not merely from the
// decoded item list.
func TestHandleListPipelines_NarrowedCallerReadsOnlyItsOwnJobs(t *testing.T) {
	client := newPlacedTeamFixture(t)
	authz := narrowedPaymentsAuthorizer("release/*")
	reader := newFakeRecordReader()
	reader.jobSummaries["core/payments"] = []recordstore.JobSummary{
		{JobPath: "payments/release/eu", RecentResults: []string{"SUCCESS"}},
		{JobPath: "payments/build/nightly", RecentResults: []string{"SUCCESS"}},
	}
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/payments", map[string]string{"team": "payments"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "payments/release/eu") {
		t.Fatalf("the admitted job is missing from the body: %s", body)
	}
	if strings.Contains(body, "nightly") {
		t.Fatalf("the refused job name reached the response body: %s", body)
	}
}

// TestHandleListPipelines_RunningNowObeysTheSameGlobs asserts the scenario
// "The running-now list obeys the same globs".
func TestHandleListPipelines_RunningNowObeysTheSameGlobs(t *testing.T) {
	client := newPlacedTeamFixture(t)
	authz := narrowedPaymentsAuthorizer("release/*")
	reader := newFakeRecordReader()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reader.addRun(recordstore.Run{
		RunID: "run-hidden", Cluster: "core", Namespace: "test-ns", Cell: "cell-1",
		Tenant: "payments", JobPath: "payments/build/nightly", Status: "RUNNING", CreatedAt: base,
	})
	reader.addRun(recordstore.Run{
		RunID: "run-shown", Cluster: "core", Namespace: "test-ns", Cell: "cell-1",
		Tenant: "payments", JobPath: "payments/release/eu", Status: "RUNNING", CreatedAt: base,
	})
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/payments", map[string]string{"team": "payments"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "run-hidden") || strings.Contains(body, "nightly") {
		t.Fatalf("a refused running build reached the response body: %s", body)
	}
	var resp pipelineListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.RunningNow) != 1 || resp.RunningNow[0].RunID != "run-shown" {
		t.Fatalf("runningNow = %+v, want only run-shown", resp.RunningNow)
	}
}

// TestHandleListPipelines_FilteredStripKeepsTruncated asserts the scenario
// "A filtered strip does not change the truncated flag". truncated reports
// the page the record store answered, not the filter.
func TestHandleListPipelines_FilteredStripKeepsTruncated(t *testing.T) {
	client := newPlacedTeamFixture(t)
	authz := narrowedPaymentsAuthorizer("release/*")
	reader := newFakeRecordReader()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		reader.addRun(recordstore.Run{
			RunID: fmt.Sprintf("q-%02d", i), Cluster: "core", Namespace: "test-ns", Cell: "cell-1",
			Tenant: "payments", JobPath: "payments/build/nightly", Status: "QUEUED",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	reader.addRun(recordstore.Run{
		RunID: "run-shown", Cluster: "core", Namespace: "test-ns", Cell: "cell-1",
		Tenant: "payments", JobPath: "payments/release/eu", Status: "RUNNING", CreatedAt: base,
	})
	srv := pipelinesTestServer(client, authz, reader, nil)

	resp := listPipelinesFor(t, srv, "payments")
	if !resp.Truncated {
		t.Fatalf("truncated = false, want true: the page the store answered was full")
	}
	if len(resp.RunningNow) != 1 {
		t.Fatalf("runningNow holds %d tiles, want only the admitted one", len(resp.RunningNow))
	}
}

// TestHandleListPipelines_NarrowedCallerStillReadsTheTeamPage asserts the
// scenario "A narrowed caller still reads the team page": the empty job
// path admits the tenant, so the page answers 200.
func TestHandleListPipelines_NarrowedCallerStillReadsTheTeamPage(t *testing.T) {
	client := newPlacedTeamFixture(t)
	authz := narrowedPaymentsAuthorizer("release/*")
	reader := newFakeRecordReader()
	reader.jobSummaries["core/payments"] = []recordstore.JobSummary{
		{JobPath: "payments/build/nightly", RecentResults: []string{"SUCCESS"}},
	}
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/payments", map[string]string{"team": "payments"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// TestHandleListPipelines_HeldTenantListsNoDeclaredJobs asserts a placed
// Team whose items the cell holds shows no declared job, while its run
// history still lists.
func TestHandleListPipelines_HeldTenantListsNoDeclaredJobs(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantUnknownKeyItems}
	reader := newFakeRecordReader()
	reader.jobSummaries["core/payments"] = []recordstore.JobSummary{
		{JobPath: "payments/release/eu", RecentResults: []string{"SUCCESS"}},
	}
	srv := pipelinesTestServer(client, resolverAuthorizer("payments"), reader, nil)

	resp := listPipelinesFor(t, srv, "payments")
	if got := kindOf(t, resp, "payments/release/eu"); got != "unknown" {
		t.Errorf("kind = %q, want unknown: a held tenant declares no job", got)
	}
}
