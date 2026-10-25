package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

const singlePipelineItemsYAML = `
items:
  - kind: folder
    name: acme
    items:
      - kind: pipeline
        name: app-a
        definition:
          script: "pipeline {}"
      - kind: pipeline
        name: app-b
        definition:
          script: "pipeline {}"
`

func testClaims() *auth.Claims { return &auth.Claims{Subject: "u"} }

// newSingleControllerFixture seeds one controller, "ctrl-a" in
// "test-ns", carrying a composed bundle whose items.yaml declares
// acme/app-a and acme/app-b.
func newSingleControllerFixture(t *testing.T) *fakeResourceClient {
	t.Helper()
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
		"bundle-a-content": {"items.yaml": singlePipelineItemsYAML},
	}
	return client
}

func doPipelinesRequest(t *testing.T, handler func(http.ResponseWriter, *http.Request), path string, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	req = req.WithContext(auth.ContextWithClaims(req.Context(), testClaims()))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// TestCauseSummary asserts causeSummary reads the plain string the
// gateway writes, and falls back to the older object and array shapes.
func TestCauseSummary(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string is the primary shape", `"Started by user nate"`, "Started by user nate"},
		{"empty input yields empty string", "", ""},
		{"object with shortDescription falls back", `{"shortDescription":"Started by timer"}`, "Started by timer"},
		{"array with shortDescription falls back", `[{"shortDescription":"Started by SCM change"}]`, "Started by SCM change"},
		{"malformed JSON yields empty string", `{not valid`, ""},
		{"object without shortDescription yields empty string", `{"other":"field"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := causeSummary(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("causeSummary(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestLatestChangeMessage asserts latestChangeMessage returns the
// newest commit message, the last entry in the oldest-first array.
func TestLatestChangeMessage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty input yields empty string", "", ""},
		{"no entries yields empty string", `[]`, ""},
		{
			"newest entry is the last array element",
			`[{"message":"first commit"},{"message":"second commit"}]`,
			"second commit",
		},
		{"newest entry with no message yields empty string", `[{"message":"first commit"},{"message":""}]`, ""},
		{"malformed JSON yields empty string", `{not valid`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := latestChangeMessage(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("latestChangeMessage(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// --- 3.5: GET /pipelines/{tenant} ---

func TestHandleListPipelines_UnionsCatalogAndRecords_TruncatedFlag(t *testing.T) {
	client := newSingleControllerFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lastRun := recordstore.Run{
		RunID: "run-app-a-1", Cluster: "core", Namespace: "test-ns", Cell: "ctrl-a",
		Tenant: "test-ns/ctrl-a", JobPath: "acme/app-a", Status: "COMPLETED", CreatedAt: base,
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/app-a", RecentRuns: []recordstore.HealthRun{{Result: "SUCCESS"}}, LastRun: &lastRun},
	}
	// 60 QUEUED runs so the running-now page for this tenant is full.
	for i := 0; i < 60; i++ {
		reader.addRun(recordstore.Run{
			RunID: fmt.Sprintf("q-%02d", i), Cluster: "core", Namespace: "test-ns", Cell: "ctrl-a",
			Tenant: "test-ns/ctrl-a", JobPath: "acme/app-a", Status: "QUEUED",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}

	srv := pipelinesTestServer(client, authz, reader, nil)
	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pipelineListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byPath := map[string]pipelineSummaryJSON{}
	for _, it := range resp.Items {
		byPath[it.JobPath] = it
	}
	a, ok := byPath["acme/app-a"]
	if !ok || a.LastBuild == nil {
		t.Fatalf("expected acme/app-a with a lastBuild, got %+v", byPath)
	}
	b, ok := byPath["acme/app-b"]
	if !ok {
		t.Fatalf("expected acme/app-b (catalog-only, never run) in the union, got %+v", byPath)
	}
	if b.LastBuild != nil {
		t.Fatalf("never-run job acme/app-b carries a lastBuild: %+v", b)
	}
	if b.Health.Bucket != "unknown" || b.Health.Score != nil {
		t.Fatalf("never-run job health = %+v, want unknown with no score", b.Health)
	}
	if !resp.Truncated {
		t.Fatalf("expected truncated=true with 60 queued runs on one tenant")
	}
}

func TestHandleListPipelines_RunningRunSurvivesFinishedRunFlood(t *testing.T) {
	client := newSingleControllerFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()

	base := time.Now().Add(-2 * time.Hour)
	running := recordstore.Run{
		RunID: "running-1", Cluster: "core", Namespace: "test-ns", Cell: "ctrl-a",
		Tenant: "test-ns/ctrl-a", JobPath: "acme/app-a", Status: "RUNNING", CreatedAt: base,
	}
	reader.addRun(running)
	// 60 finished runs, all newer than the still-running one.
	for i := 0; i < 60; i++ {
		reader.addRun(recordstore.Run{
			RunID: fmt.Sprintf("done-%02d", i), Cluster: "core", Namespace: "test-ns", Cell: "ctrl-a",
			Tenant: "test-ns/ctrl-a", JobPath: "acme/app-a", Status: "COMPLETED",
			CreatedAt: base.Add(time.Duration(i+1) * time.Minute),
		})
	}

	srv := pipelinesTestServer(client, authz, reader, nil)
	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pipelineListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, r := range resp.RunningNow {
		if r.RunID == "running-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("running-1 missing from runningNow after 60 newer finished runs: %+v", resp.RunningNow)
	}
}

// TestHandleListPipelines_RunningNowCarriesTheCurrentStage asserts each
// Running now tile names the stage in progress of its run, and that a run
// which holds no stage in progress carries no currentStage field.
func TestHandleListPipelines_RunningNowCarriesTheCurrentStage(t *testing.T) {
	client := newSingleControllerFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()

	base := time.Now().Add(-2 * time.Hour)
	startedAt := base.Add(2 * time.Minute)
	reader.addRun(recordstore.Run{
		RunID: "run-in-stage", Cluster: "core", Namespace: "test-ns", Cell: "ctrl-a",
		Tenant: "test-ns/ctrl-a", JobPath: "acme/app-a", Status: "RUNNING", CreatedAt: base,
	})
	reader.addRun(recordstore.Run{
		RunID: "run-before-stage", Cluster: "core", Namespace: "test-ns", Cell: "ctrl-a",
		Tenant: "test-ns/ctrl-a", JobPath: "acme/app-a", Status: "RUNNING", CreatedAt: base.Add(time.Minute),
	})
	reader.stages["run-in-stage"] = []recordstore.Stage{
		{RunID: "run-in-stage", StageID: "stage-1", Name: "Checkout", NodeType: "stage",
			Status: "SUCCESS", CreatedAt: base},
		{RunID: "run-in-stage", StageID: "stage-2", Name: "Build", NodeType: "stage",
			Status: "IN_PROGRESS", CreatedAt: base.Add(time.Minute), StartedAt: &startedAt},
	}

	srv := pipelinesTestServer(client, authz, reader, nil)
	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RunningNow []map[string]any `json:"runningNow"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byRun := map[string]map[string]any{}
	for _, tile := range body.RunningNow {
		runID, _ := tile["runId"].(string)
		byRun[runID] = tile
	}
	if got := byRun["run-in-stage"]["currentStage"]; got != "Build" {
		t.Fatalf("currentStage = %v, want Build", got)
	}
	if _, present := byRun["run-before-stage"]["currentStage"]; present {
		t.Fatalf("a run with no stage in progress carries currentStage: %+v", byRun["run-before-stage"])
	}
}

// --- 3.6: GET /pipelines/{tenant}/jobs/{job} ---

const parameterizedItemYAML = `
items:
  - kind: pipeline
    name: app-a
    definition:
      script: "pipeline {}"
    properties:
      - parameters:
          parameterDefinitions:
            - string:
                name: BRANCH
                defaultValue: main
            - choice:
                name: ENV
                choices: [dev, staging, prod]
            - booleanParam:
                name: SKIP_TESTS
                defaultValue: false
`

func TestHandleGetPipelineJob_ParameterDefinitionsReachResponse(t *testing.T) {
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
		"bundle-a-content": {"items.yaml": parameterizedItemYAML},
	}

	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/ctrl-a/jobs/app-a",
		map[string]string{"tenant": "ctrl-a", "job": "app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Parameters) != 3 {
		t.Fatalf("got %d parameters, want 3: %+v", len(detail.Parameters), detail.Parameters)
	}
	byName := map[string]pipelineParameterJSON{}
	for _, p := range detail.Parameters {
		byName[p.Name] = p
	}
	if p, ok := byName["BRANCH"]; !ok || p.Kind != "string" || p.DefaultValue != "main" {
		t.Errorf("BRANCH parameter = %+v", p)
	}
	if p, ok := byName["ENV"]; !ok || p.Kind != "choice" {
		t.Errorf("ENV parameter = %+v", p)
	}
	if p, ok := byName["SKIP_TESTS"]; !ok || p.Kind != "boolean" {
		t.Errorf("SKIP_TESTS parameter = %+v", p)
	}
	if detail.TenantFolder != "" {
		t.Errorf("tenantFolder = %q, want empty for a root-hosted synthetic tenant", detail.TenantFolder)
	}
	if detail.Tenant != "test-ns/ctrl-a" {
		t.Errorf("tenant = %q, want %q for a synthetic tenant", detail.Tenant, "test-ns/ctrl-a")
	}
}

// TestHandleGetPipelineJob_TenantFolderForPlacedTenant asserts a placed
// Tenant's job detail carries its Jenkins folder prefix, TenantRef.Folder,
// which equals the Tenant name for design D1's placed-Tenant tenant kind.
func TestHandleGetPipelineJob_TenantFolderForPlacedTenant(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Fapp-a",
		map[string]string{"tenant": "payments", "job": "payments/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.TenantFolder != "payments" {
		t.Errorf("tenantFolder = %q, want %q", detail.TenantFolder, "payments")
	}
	if detail.Tenant != "payments" {
		t.Errorf("tenant = %q, want %q for a Tenant tenant", detail.Tenant, "payments")
	}
}

// --- Task 11.1/11.2: the dependency edge (D13) ---

// TestHandleGetPipelineJob_DependenciesBothDirections asserts the job
// detail carries both an upstream and a downstream edge, each granted.
func TestHandleGetPipelineJob_DependenciesBothDirections(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	reader.setPaymentsReleaseDependencies(recordstore.JobDependencies{
		Upstream:   []recordstore.DependencyEdge{{Tenant: "alpha", Job: "build", JobPath: "alpha/build", Result: "SUCCESS", Granted: true}},
		Downstream: []recordstore.DependencyEdge{{Tenant: "search", Job: "rebuild", JobPath: "search/rebuild", Result: "SUCCESS", Granted: true}},
	})
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Frelease",
		map[string]string{"tenant": "payments", "job": "payments/release"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Dependencies.Upstream) != 1 || detail.Dependencies.Upstream[0] != (pipelineDependencyEdgeJSON{
		Tenant: "alpha", Job: "build", JobPath: "alpha/build", Result: "SUCCESS", Granted: true,
	}) {
		t.Errorf("upstream = %+v, want one granted alpha/build edge", detail.Dependencies.Upstream)
	}
	var raw struct {
		Dependencies struct {
			Upstream []map[string]any `json:"upstream"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw.Dependencies.Upstream) != 1 {
		t.Fatalf("raw upstream = %+v, want one edge", raw.Dependencies.Upstream)
	}
	if got, ok := raw.Dependencies.Upstream[0]["jobPath"]; !ok || got != "alpha/build" {
		t.Errorf("raw upstream[0].jobPath = %v (present %t), want alpha/build", got, ok)
	}
	if len(detail.Dependencies.Downstream) != 1 || detail.Dependencies.Downstream[0] != (pipelineDependencyEdgeJSON{
		Tenant: "search", Job: "rebuild", JobPath: "search/rebuild", Result: "SUCCESS", Granted: true,
	}) {
		t.Errorf("downstream = %+v, want one granted search/rebuild edge", detail.Dependencies.Downstream)
	}
}

// TestHandleGetPipelineJob_DependenciesUngrantedEdgeVisible asserts an
// edge whose producer grants nobody still appears, with granted false,
// rather than disappearing from the list.
func TestHandleGetPipelineJob_DependenciesUngrantedEdgeVisible(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	reader.setPaymentsReleaseDependencies(recordstore.JobDependencies{
		Upstream: []recordstore.DependencyEdge{{Tenant: "alpha", Job: "build", Result: "SUCCESS", Granted: false}},
	})
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Frelease",
		map[string]string{"tenant": "payments", "job": "payments/release"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Dependencies.Upstream) != 1 || detail.Dependencies.Upstream[0].Granted {
		t.Fatalf("upstream = %+v, want one ungranted edge, not hidden", detail.Dependencies.Upstream)
	}
}

// TestHandleGetPipelineJob_DependenciesEmptyForNoEdge asserts a job with
// no declared cross-tenant trigger and no subscriber reports both
// dependency arrays as empty, present arrays, never omitted.
func TestHandleGetPipelineJob_DependenciesEmptyForNoEdge(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Flonely",
		map[string]string{"tenant": "payments", "job": "payments/lonely"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Dependencies.Upstream == nil || len(detail.Dependencies.Upstream) != 0 {
		t.Errorf("upstream = %+v, want an empty array", detail.Dependencies.Upstream)
	}
	if detail.Dependencies.Downstream == nil || len(detail.Dependencies.Downstream) != 0 {
		t.Errorf("downstream = %+v, want an empty array", detail.Dependencies.Downstream)
	}
}

// TestHandleGetPipelineJob_NoReadRightReturnsNoDependencyData asserts a
// caller with no read right on the tenant reaches no dependency data.
// The route answers the same 404 "job not found" that tenantForJob
// already returns for any unauthorized job read (tenantresolver.go: "A
// 403 would tell the caller a tenant it may not read exists"); this
// dependency edge introduces no new authorization outcome.
func TestHandleGetPipelineJob_NoReadRightReturnsNoDependencyData(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := resolverAuthorizer() // no tenant granted at all
	reader := newFakeRecordReader()
	reader.setPaymentsReleaseDependencies(recordstore.JobDependencies{
		Upstream: []recordstore.DependencyEdge{{Tenant: "alpha", Job: "build", Result: "SUCCESS", Granted: true}},
	})
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Frelease",
		map[string]string{"tenant": "payments", "job": "payments/release"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alpha") {
		t.Fatalf("body leaked dependency data to an unauthorized caller: %s", rec.Body.String())
	}
}

// --- Ruling (aa): the job page's "Last 30 days" card ---

func TestHandleGetPipelineJob_Last30DaysCard(t *testing.T) {
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
		"bundle-a-content": {"items.yaml": singlePipelineItemsYAML},
	}
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")

	rate := 0.75
	median := int64(4200)
	reader := newFakeRecordReader()
	reader.setWindowStats("core", "test-ns/ctrl-a", "acme/app-a", recordstore.JobWindowStats{
		Builds:           4,
		PassRate:         &rate,
		MedianDurationMs: &median,
		SlowestStage:     &recordstore.StageStat{Name: "Deploy", DurationMs: 9000},
	})

	srv := pipelinesTestServer(client, authz, reader, nil)
	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a",
		map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Last30Days == nil {
		t.Fatalf("last30Days missing, want the seeded card")
	}
	if detail.Last30Days.Builds != 4 {
		t.Errorf("builds = %d, want 4", detail.Last30Days.Builds)
	}
	if detail.Last30Days.PassRate == nil || *detail.Last30Days.PassRate != rate {
		t.Errorf("passRate = %v, want %v", detail.Last30Days.PassRate, rate)
	}
	if detail.Last30Days.MedianDurationMs == nil || *detail.Last30Days.MedianDurationMs != median {
		t.Errorf("medianDurationMs = %v, want %v", detail.Last30Days.MedianDurationMs, median)
	}
	if detail.Last30Days.SlowestStage == nil || detail.Last30Days.SlowestStage.Name != "Deploy" ||
		detail.Last30Days.SlowestStage.DurationMs != 9000 {
		t.Errorf("slowestStage = %+v, want Deploy at 9000ms", detail.Last30Days.SlowestStage)
	}
}

// TestHandleGetPipelineJob_Last30DaysDegradesToNullOnReaderError asserts
// a JobWindowStats failure leaves the rest of the job page intact and
// reports last30Days as null rather than failing the whole request.
func TestHandleGetPipelineJob_Last30DaysDegradesToNullOnReaderError(t *testing.T) {
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
		"bundle-a-content": {"items.yaml": singlePipelineItemsYAML},
	}
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")

	reader := newFakeRecordReader()
	reader.windowStatsErr = fmt.Errorf("read timeout")

	srv := pipelinesTestServer(client, authz, reader, nil)
	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a",
		map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	val, ok := raw["last30Days"]
	if !ok {
		t.Fatalf("last30Days key missing from response: %s", rec.Body.String())
	}
	if string(val) != "null" {
		t.Fatalf("last30Days = %s, want null", val)
	}
}

// --- 3.7: GET /pipelines/{tenant}/jobs/{job}/runs ---

func TestHandleListRuns_CrossTenantMergeNoGapNoRepeat_RevokedTenantContinues(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
	}
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns-a", "ns-b"}},
	}
	crdstore.MustSeed(client.crdStore, tenant)

	reader := newFakeRecordReader()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wantIDs []string
	for i := 0; i < 3; i++ {
		a := recordstore.Run{RunID: fmt.Sprintf("a-%d", i), Cluster: "core", Tenant: "ns-a/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(2*i) * time.Minute)}
		b := recordstore.Run{RunID: fmt.Sprintf("b-%d", i), Cluster: "core", Tenant: "ns-b/ctrl-b", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(2*i+1) * time.Minute)}
		reader.addRun(a)
		reader.addRun(b)
		wantIDs = append(wantIDs, a.RunID, b.RunID)
	}

	authz := pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b")
	srv := pipelinesTestServer(client, authz, reader, nil)

	var gotIDs []string
	cursor := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		rec := doPipelinesRequest(t, srv.handleListRuns, "/pipelines/payments/jobs/svc%2Fmain/runs?"+q.Encode(),
			map[string]string{"tenant": "payments", "job": "svc/main"})
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, body = %s", page, rec.Code, rec.Body.String())
		}
		var resp runListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d: decode: %v", page, err)
		}
		for _, it := range resp.Items {
			gotIDs = append(gotIDs, it.RunID)
		}
		cursor = resp.NextCursor
		if cursor == "" {
			break
		}
	}

	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d run ids %v, want %d %v", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	seen := map[string]bool{}
	for _, id := range gotIDs {
		if seen[id] {
			t.Fatalf("run %q repeated across pages: %v", id, gotIDs)
		}
		seen[id] = true
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Fatalf("run %q missing across pages (a gap): %v", id, gotIDs)
		}
	}

	// Revoke tenant b's read access; paging must continue on tenant a's
	// rows rather than fail.
	narrowed := pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a")
	srv2 := pipelinesTestServer(client, narrowed, reader, nil)
	rec := doPipelinesRequest(t, srv2.handleListRuns, "/pipelines/payments/jobs/svc%2Fmain/runs?limit=10",
		map[string]string{"tenant": "payments", "job": "svc/main"})
	if rec.Code != http.StatusOK {
		t.Fatalf("after revoke: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp2 runListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("after revoke: decode: %v", err)
	}
	if len(resp2.Items) != 3 {
		t.Fatalf("after revoke: got %d items, want tenant a's 3 runs only: %+v", len(resp2.Items), resp2.Items)
	}
	for _, it := range resp2.Items {
		if strings.HasPrefix(it.RunID, "b-") {
			t.Fatalf("after revoke, still saw tenant b's run %q", it.RunID)
		}
	}
}

// tenant A monopolizes page 1 entirely; tenant B's rows
// are all older and untouched on page 1. Verifies no gap, no repeat.
func TestHandleListRuns_CrossTenantMerge_UntouchedTenantCursorNoRepeatNoGap(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
	}
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns-a", "ns-b"}},
	}
	crdstore.MustSeed(client.crdStore, tenant)

	reader := newFakeRecordReader()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wantIDs []string
	// A: a0 newest .. a2 oldest-of-A, all newer than every B row.
	for i := 0; i < 3; i++ {
		a := recordstore.Run{RunID: fmt.Sprintf("a-%d", i), Cluster: "core", Tenant: "ns-a/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(100-i) * time.Minute)}
		reader.addRun(a)
		wantIDs = append(wantIDs, a.RunID)
	}
	// B: b0..b1, all older than every A row.
	for i := 0; i < 2; i++ {
		b := recordstore.Run{RunID: fmt.Sprintf("b-%d", i), Cluster: "core", Tenant: "ns-b/ctrl-b", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(-i) * time.Minute)}
		reader.addRun(b)
		wantIDs = append(wantIDs, b.RunID)
	}

	authz := pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b")
	srv := pipelinesTestServer(client, authz, reader, nil)

	var gotIDs []string
	cursor := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		rec := doPipelinesRequest(t, srv.handleListRuns, "/pipelines/payments/jobs/svc%2Fmain/runs?"+q.Encode(),
			map[string]string{"tenant": "payments", "job": "svc/main"})
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, body = %s", page, rec.Code, rec.Body.String())
		}
		var resp runListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d: decode: %v", page, err)
		}
		for _, it := range resp.Items {
			gotIDs = append(gotIDs, it.RunID)
		}
		cursor = resp.NextCursor
		if cursor == "" {
			break
		}
	}

	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d run ids %v, want %d %v", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	seen := map[string]bool{}
	for _, id := range gotIDs {
		if seen[id] {
			t.Fatalf("run %q repeated across pages: %v", id, gotIDs)
		}
		seen[id] = true
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Fatalf("run %q missing across pages (a gap): %v", id, gotIDs)
		}
	}
}

func TestHandleListRuns_CrossTenantMerge_ExhaustedTenantDoesNotRevive(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
		"ctrl-c": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-c", Namespace: "ns-c"}},
	}
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns-a", "ns-b", "ns-c"}},
	}
	crdstore.MustSeed(client.crdStore, tenant)

	reader := newFakeRecordReader()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantIDs := make([]string, 0, 8)
	for i, m := range []int{100, 99} {
		a := recordstore.Run{RunID: fmt.Sprintf("a-%d", i), Cluster: "core", Tenant: "ns-a/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(m) * time.Minute)}
		reader.addRun(a)
		wantIDs = append(wantIDs, a.RunID)
	}
	for i, m := range []int{50, 49, 48} {
		b := recordstore.Run{RunID: fmt.Sprintf("b-%d", i), Cluster: "core", Tenant: "ns-b/ctrl-b", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(m) * time.Minute)}
		reader.addRun(b)
		wantIDs = append(wantIDs, b.RunID)
	}
	for i, m := range []int{10, 9, 8} {
		c := recordstore.Run{RunID: fmt.Sprintf("c-%d", i), Cluster: "core", Tenant: "ns-c/ctrl-c", JobPath: "svc/main", Status: "COMPLETED", CreatedAt: base.Add(time.Duration(m) * time.Minute)}
		reader.addRun(c)
		wantIDs = append(wantIDs, c.RunID)
	}

	authz := pipelineGrantAuthorizer([]string{"read"}, "ns-a/ctrl-a", "ns-b/ctrl-b", "ns-c/ctrl-c")
	srv := pipelinesTestServer(client, authz, reader, nil)

	var gotIDs []string
	cursor := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		rec := doPipelinesRequest(t, srv.handleListRuns, "/pipelines/payments/jobs/svc%2Fmain/runs?"+q.Encode(),
			map[string]string{"tenant": "payments", "job": "svc/main"})
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, body = %s", page, rec.Code, rec.Body.String())
		}
		var resp runListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d: decode: %v", page, err)
		}
		for _, it := range resp.Items {
			gotIDs = append(gotIDs, it.RunID)
		}
		cursor = resp.NextCursor
		if cursor == "" {
			break
		}
	}

	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d run ids %v, want %d %v", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	seen := map[string]bool{}
	for _, id := range gotIDs {
		if seen[id] {
			t.Fatalf("run %q repeated across pages: %v", id, gotIDs)
		}
		seen[id] = true
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Fatalf("run %q missing across pages (a gap): %v", id, gotIDs)
		}
	}
}

// The job response's provenance fields.

// jenkinsfileItemsYAML declares acme/app-a with an SCM definition, so the
// item renders a jenkinsfile source.
const jenkinsfileItemsYAML = `
items:
  - kind: folder
    name: acme
    items:
      - kind: pipeline
        name: app-a
        definition:
          cpsScmFlowDefinition:
            scm:
              gitSCM:
                userRemoteConfigs:
                  - userRemoteConfig:
                      url: https://github.com/acme/svc
`

// pipelinesJobDetail issues one job-detail request and decodes it.
func pipelinesJobDetail(t *testing.T, srv *Server, tenant, jobPath string) pipelineJobDetailJSON {
	t.Helper()
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal([]byte(pipelinesJobDetailBody(t, srv, tenant, jobPath)), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detail
}

// pipelinesJobDetailBody issues one job-detail request and returns its
// body, so a test can assert a key is absent rather than zero.
func pipelinesJobDetailBody(t *testing.T, srv *Server, tenant, jobPath string) string {
	t.Helper()
	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/"+tenant+"/jobs/"+url.PathEscape(jobPath),
		map[string]string{"tenant": tenant, "job": jobPath})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestHandleGetPipelineJob_DeclaredByAndDefinitionPath asserts a
// git-declared job with a recorded last run carries both fields.
func TestHandleGetPipelineJob_DeclaredByAndDefinitionPath(t *testing.T) {
	client := newSingleControllerFixture(t)
	client.configMaps["bundle-a-content"] = map[string]string{"items.yaml": jenkinsfileItemsYAML}
	client.composedBundles["bundle-a"].Spec.Inputs = []v1alpha1.ComposedInput{
		{GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://github.com/acme/svc", Path: "bundles/tenant"}},
	}
	client.composedBundles["bundle-a"].Status.ObservedRevisions = map[string]string{"0": "sha-git"}

	rev := "sha-checkout"
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-a",
		LastRun: &recordstore.Run{JobPath: "acme/app-a", Source: json.RawMessage(runSourceColumnJSON), SCMRevision: &rev},
	}}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), reader, nil)

	detail := pipelinesJobDetail(t, srv, "ctrl-a", "acme/app-a")
	if detail.Definition == nil {
		t.Fatal("definition is nil, want the job's jenkinsfile definition")
	}
	if detail.Definition.DefinitionPath != "ci/Jenkinsfile" {
		t.Errorf("definitionPath = %q, want the last run's recorded script path", detail.Definition.DefinitionPath)
	}
	if detail.Definition.JenkinsfileCommit != rev {
		t.Errorf("jenkinsfileCommit = %q, want the last run's revision", detail.Definition.JenkinsfileCommit)
	}
	db := detail.Definition.DeclaredBy
	if db == nil {
		t.Fatal("declaredBy is missing, want the bundle's git input")
	}
	if db.Kind != "gitSource" || db.Name != "https://github.com/acme/svc" || db.RepoURL != "https://github.com/acme/svc" {
		t.Errorf("declaredBy = %+v, want the gitSource input's repo URL", db)
	}
	if db.Path != "bundles/tenant" || db.Revision != "sha-git" {
		t.Errorf("path/revision = %q/%q, want the input's path and observed revision", db.Path, db.Revision)
	}
	if want := (bundleRefJSON{Namespace: "test-ns", Name: "bundle-a"}); db.Bundle != want {
		t.Errorf("bundle = %+v, want %+v", db.Bundle, want)
	}
}

// TestHandleGetPipelineJob_NoDefinitionCarriesNoProvenance asserts a job
// with no declared item carries neither field, though its last run
// recorded a script path.
func TestHandleGetPipelineJob_NoDefinitionCarriesNoProvenance(t *testing.T) {
	client := newSingleControllerFixture(t)
	rev := "sha-checkout"
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-z",
		LastRun: &recordstore.Run{JobPath: "acme/app-z", Source: json.RawMessage(runSourceColumnJSON), SCMRevision: &rev},
	}}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), reader, nil)

	body := pipelinesJobDetailBody(t, srv, "ctrl-a", "acme/app-z")
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Definition != nil {
		t.Errorf("definition = %+v, want nil for a job with no declared item", detail.Definition)
	}
	if strings.Contains(body, "declaredBy") || strings.Contains(body, "definitionPath") || strings.Contains(body, "jenkinsfileForge") {
		t.Errorf("provenance leaked for a job with no definition: %s", body)
	}
}

// TestHandleGetPipelineJob_StaleDefinitionPathStaysAbsent asserts the
// jenkinsfile source gate, not omitempty alone, keeps definitionPath
// absent for an inline job whose last run recorded one.
func TestHandleGetPipelineJob_StaleDefinitionPathStaysAbsent(t *testing.T) {
	client := newSingleControllerFixture(t)
	rev := "sha-checkout"
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-a",
		LastRun: &recordstore.Run{JobPath: "acme/app-a", Source: json.RawMessage(runSourceColumnJSON), SCMRevision: &rev},
	}}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), reader, nil)

	detail := pipelinesJobDetail(t, srv, "ctrl-a", "acme/app-a")
	if detail.Definition == nil || detail.Definition.Source != "inline" {
		t.Fatalf("definition = %+v, want the item's inline source", detail.Definition)
	}
	if detail.Definition.DefinitionPath != "" {
		t.Errorf("definitionPath = %q, want it absent for an inline job", detail.Definition.DefinitionPath)
	}
	if detail.Definition.JenkinsfileCommit != "" {
		t.Errorf("jenkinsfileCommit = %q, want it absent for an inline job", detail.Definition.JenkinsfileCommit)
	}
}

// TestHandleGetPipelineJob_UnresolvableInputKeepsDefinition asserts a job
// whose bundle input cannot be resolved keeps its definition and every
// other definition field, and reports no provenance.
func TestHandleGetPipelineJob_UnresolvableInputKeepsDefinition(t *testing.T) {
	client := newSingleControllerFixture(t)
	client.composedBundles["bundle-a"].Spec.Inputs = []v1alpha1.ComposedInput{
		{ItemRef: &v1alpha1.ComposedItemRef{Name: "items-missing"}},
	}
	client.composedBundles["bundle-a"].Status.InputSummary = []v1alpha1.InputSummaryEntry{
		{Kind: "itemRef", Type: "item", Namespace: "test-ns"},
	}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), newFakeRecordReader(), nil)

	detail := pipelinesJobDetail(t, srv, "ctrl-a", "acme/app-a")
	if detail.Definition == nil {
		t.Fatal("definition is nil, want the item's inline definition")
	}
	if want := (&pipelineDefinitionJSON{Source: "inline"}); *detail.Definition != *want {
		t.Fatalf("definition = %+v, want %+v: an unresolvable input changes no other field", *detail.Definition, *want)
	}
}

// --- GET /pipelines/{tenant}/jobs/{job}/tests/{case} ---

// newCaseHistoryFixture builds a Server over the tenant "payments", which
// resolves to the two controller tenants ns-a/ctrl-a and ns-b/ctrl-b, and
// grants read on the tenants named in granted.
func newCaseHistoryFixture(reader *fakeRecordReader, granted ...string) *Server {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a"}},
		"ctrl-b": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "ns-b"}},
	}
	crdstore.MustSeed(client.crdStore, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns-a", "ns-b"}},
	})
	return pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, granted...), reader, nil)
}

func caseHistoryPath(caseKey, query string) string {
	p := "/pipelines/payments/jobs/svc%2Fmain/tests/" + url.PathEscape(caseKey)
	if query != "" {
		p += "?" + query
	}
	return p
}

func caseHistoryValues(caseKey string) map[string]string {
	return map[string]string{"tenant": "payments", "job": "svc/main", "case": caseKey}
}

func TestHandleListJobTestCaseHistory_MergesTenantsNewestFirst(t *testing.T) {
	reader := newFakeRecordReader()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	build := func(n int64) *int64 { return &n }
	className, name := "com.example/FooTest", "bar baz"
	reader.caseHistory[caseHistoryKey("core", "ns-a/ctrl-a", "svc/main", className, name)] = []recordstore.TestCaseOutcome{
		{RunID: "a-2", BuildNumber: build(2), Suite: "unit", Status: "FAILED", CreatedAt: base.Add(2 * time.Minute), Result: strPtr("FAILURE")},
		{RunID: "a-1", BuildNumber: build(1), Suite: "unit", Status: "FAILED", CreatedAt: base, Result: strPtr("FAILURE")},
	}
	reader.caseHistory[caseHistoryKey("core", "ns-b/ctrl-b", "svc/main", className, name)] = []recordstore.TestCaseOutcome{
		{RunID: "b-1", BuildNumber: build(1), Suite: "it", Status: "REGRESSION", CreatedAt: base.Add(time.Minute), ErrorDetails: "boom"},
	}
	srv := newCaseHistoryFixture(reader, "ns-a/ctrl-a", "ns-b/ctrl-b")

	key := TestCaseKey(className, name)
	rec := doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, ""), caseHistoryValues(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp testCaseHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		ids = append(ids, it.RunID)
	}
	if fmt.Sprint(ids) != "[a-2 b-1 a-1]" {
		t.Fatalf("order = %v, want newest first across tenants", ids)
	}
	mid := resp.Items[1]
	if mid.Suite != "it" || mid.Status != "REGRESSION" || mid.ErrorDetails != "boom" || mid.BuildNumber != 1 {
		t.Fatalf("row = %+v", mid)
	}
	// The run link names the resolved tenant's own tenant segment, the rule
	// the fleet lock rows follow.
	if resp.Items[0].Result != "FAILURE" || resp.Items[0].RunLink != "/pipelines/ctrl-a/svc%2Fmain/a-2" {
		t.Fatalf("row = %+v, want the result and the run link", resp.Items[0])
	}
	for _, q := range reader.historyQueries {
		if q.ClassName != className || q.Name != name || q.JobPath != "svc/main" || q.Limit != 50 {
			t.Fatalf("query = %+v, want the decoded key, the decoded job path, and the default limit", q)
		}
	}
}

func TestHandleListJobTestCaseHistory_ReadsAdmittedTenantsAlone(t *testing.T) {
	reader := newFakeRecordReader()
	srv := newCaseHistoryFixture(reader, "ns-a/ctrl-a")

	key := TestCaseKey("C", "n")
	rec := doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, ""), caseHistoryValues(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(reader.historyQueries) != 1 || reader.historyQueries[0].Tenant != "ns-a/ctrl-a" {
		t.Fatalf("queries = %+v, want one query on the admitted tenant", reader.historyQueries)
	}
}

func TestHandleListJobTestCaseHistory_RefusedTenantIs404(t *testing.T) {
	reader := newFakeRecordReader()
	reader.caseHistory[caseHistoryKey("core", "ns-a/ctrl-a", "svc/main", "C", "n")] = []recordstore.TestCaseOutcome{
		{RunID: "a-1", Suite: "unit", Status: "FAILED"},
	}
	srv := newCaseHistoryFixture(reader, "other-ns/ctrl-z")

	key := TestCaseKey("C", "n")
	rec := doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, ""), caseHistoryValues(key))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "a-1") || len(reader.historyQueries) != 0 {
		t.Fatalf("a refused read reached the store: body = %s, queries = %+v", rec.Body.String(), reader.historyQueries)
	}
}

func TestHandleListJobTestCaseHistory_MalformedKeyIs400(t *testing.T) {
	srv := newCaseHistoryFixture(newFakeRecordReader(), "ns-a/ctrl-a")
	for _, key := range []string{"no-separator", "bad%zz/name"} {
		rec := doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, ""), caseHistoryValues(key))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("key %q: status = %d, body = %s, want 400", key, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleListJobTestCaseHistory_EmptyHistoryIsEmptyList(t *testing.T) {
	srv := newCaseHistoryFixture(newFakeRecordReader(), "ns-a/ctrl-a", "ns-b/ctrl-b")
	key := TestCaseKey("C", "never-ran")
	rec := doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, ""), caseHistoryValues(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Fatalf("body = %s, want an empty items list", rec.Body.String())
	}
}

func TestHandleListJobTestCaseHistory_ClampsLimit(t *testing.T) {
	reader := newFakeRecordReader()
	var rows []recordstore.TestCaseOutcome
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		rows = append(rows, recordstore.TestCaseOutcome{
			RunID: fmt.Sprintf("a-%03d", 250-i), Suite: "unit", Status: "FAILED",
			CreatedAt: base.Add(time.Duration(250-i) * time.Minute),
		})
	}
	reader.caseHistory[caseHistoryKey("core", "ns-a/ctrl-a", "svc/main", "C", "n")] = rows
	srv := newCaseHistoryFixture(reader, "ns-a/ctrl-a")
	key := TestCaseKey("C", "n")

	rec := doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, "limit=500"), caseHistoryValues(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp testCaseHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 200 || reader.historyQueries[0].Limit != 200 {
		t.Fatalf("items = %d, query limit = %d, want both clamped to 200", len(resp.Items), reader.historyQueries[0].Limit)
	}

	rec = doPipelinesRequest(t, srv.handleListJobTestCaseHistory, caseHistoryPath(key, "limit=0"), caseHistoryValues(key))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0: status = %d, want 400", rec.Code)
	}
}

// TestListRunsRowCarriesChangeRequest asserts a run-list row and a running
// row carry the change request, and that a branch run carries none.
func TestListRunsRowCarriesChangeRequest(t *testing.T) {
	run := recordstore.Run{
		RunID: "r1", JobPath: "svc/main", Status: "RUNNING",
		Payload: json.RawMessage(`{"run.started":{"payload":{"change_request":{"id":"7","url":"https://forge.example/pr/7"}}}}`),
	}
	row := buildRunSummary(run)
	if row.ChangeRequest == nil || row.ChangeRequest.ID != "7" || row.ChangeRequest.URL != "https://forge.example/pr/7" {
		t.Fatalf("run row changeRequest = %+v", row.ChangeRequest)
	}
	if running := buildRunningRun(run, nil, ""); running.ChangeRequest == nil || running.ChangeRequest.ID != "7" {
		t.Fatalf("running row changeRequest = %+v", running.ChangeRequest)
	}
	branch := buildRunSummary(recordstore.Run{RunID: "r2", Status: "COMPLETED"})
	if branch.ChangeRequest != nil {
		t.Fatalf("branch run row carries changeRequest %+v", branch.ChangeRequest)
	}
}

// giteaItemsYAML declares acme/app-a with a Gitea-hosted repository.
const giteaItemsYAML = `
items:
  - kind: folder
    name: acme
    items:
      - kind: pipeline
        name: app-a
        definition:
          cpsScmFlowDefinition:
            scm:
              gitSCM:
                userRemoteConfigs:
                  - userRemoteConfig:
                      url: https://gitea.example.com/acme/svc
`

// TestHandleGetPipelineJob_ForgeHintsFromLastRun asserts a Gitea host
// takes its forge from the last run's recorded forge.
func TestHandleGetPipelineJob_ForgeHintsFromLastRun(t *testing.T) {
	client := newSingleControllerFixture(t)
	client.configMaps["bundle-a-content"] = map[string]string{"items.yaml": giteaItemsYAML}
	client.composedBundles["bundle-a"].Spec.Inputs = []v1alpha1.ComposedInput{
		{GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://gitea.example.com/acme/bundles"}},
	}
	client.composedBundles["bundle-a"].Status.ObservedRevisions = map[string]string{"0": "sha-git"}

	rev := "sha-checkout"
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-a",
		LastRun: &recordstore.Run{
			JobPath:     "acme/app-a",
			Source:      json.RawMessage(runSourceColumnJSON),
			SCMRevision: &rev,
			Payload:     json.RawMessage(`{"run.started":{"payload":{"scm_url":"https://gitea.example.com/acme/svc.git","scm_forge":"gitea"}}}`),
		},
	}}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), reader, nil)

	var detail pipelineJobDetailJSON
	if err := json.Unmarshal([]byte(pipelinesJobDetailBody(t, srv, "ctrl-a", "acme/app-a")), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Definition == nil {
		t.Fatal("definition is nil, want the job's jenkinsfile definition")
	}
	if got := detail.Definition.JenkinsfileForge; got != "gitea" {
		t.Errorf("jenkinsfileForge = %q, want gitea", got)
	}
	if detail.Definition.DeclaredBy == nil || detail.Definition.DeclaredBy.Forge != "gitea" {
		t.Errorf("declaredBy.forge = %+v, want gitea", detail.Definition.DeclaredBy)
	}
}

// TestHandleGetPipelineJob_UnknownForgeStaysAbsent asserts a job with no
// forge hint carries neither forge field.
func TestHandleGetPipelineJob_UnknownForgeStaysAbsent(t *testing.T) {
	client := newSingleControllerFixture(t)
	client.configMaps["bundle-a-content"] = map[string]string{"items.yaml": jenkinsfileItemsYAML}
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-a",
		LastRun: &recordstore.Run{JobPath: "acme/app-a"},
	}}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), reader, nil)

	body := pipelinesJobDetailBody(t, srv, "ctrl-a", "acme/app-a")
	if strings.Contains(body, "jenkinsfileForge") || strings.Contains(body, `"forge"`) {
		t.Errorf("forge leaked for a job with no forge hint: %s", body)
	}
}
