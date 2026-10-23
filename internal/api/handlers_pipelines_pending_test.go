package api

import (
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// pendingInventory builds a job inventory with the given job and project
// paths and completeness flags.
func pendingInventory(jobs, projects []string, jobsKnown, projectsKnown bool) recordstore.JobInventory {
	inv := recordstore.JobInventory{
		JobsKnown:     jobsKnown,
		ProjectsKnown: projectsKnown,
		Jobs:          map[string]struct{}{},
		Projects:      map[string]struct{}{},
	}
	for _, j := range jobs {
		inv.Jobs[j] = struct{}{}
	}
	for _, p := range projects {
		inv.Projects[p] = struct{}{}
	}
	return inv
}

func pendingOf(t *testing.T, resp pipelineListResponse, jobPath string) bool {
	t.Helper()
	for _, it := range resp.Items {
		if it.JobPath == jobPath {
			return it.Pending
		}
	}
	t.Fatalf("job %q absent from the listing: %+v", jobPath, resp.Items)
	return false
}

func TestDeclaredJobPending(t *testing.T) {
	known := pendingInventory([]string{"acme/app-a"}, []string{"acme/checkout-api"}, true, true)
	unknownJobs := pendingInventory(nil, []string{"acme/checkout-api"}, false, true)
	cases := []struct {
		name string
		inv  recordstore.JobInventory
		path string
		kind string
		want bool
	}{
		{"pipeline lacked by a known inventory", known, "acme/gone", "pipeline", true},
		{"pipeline held by a known inventory", known, "acme/app-a", "pipeline", false},
		{"pipeline with an unknown inventory", unknownJobs, "acme/app-a", "pipeline", false},
		{"multibranch reads Projects, not Jobs", known, "acme/app-a", "multibranch", true},
		{"multibranch held in Projects", known, "acme/checkout-api", "multibranch", false},
		{"multibranch with ProjectsKnown false", pendingInventory(nil, nil, true, false), "acme/checkout-api", "multibranch", false},
		{"unknown kind", known, "acme/gone", "unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaredJobPending(tc.inv, tc.path, tc.kind); got != tc.want {
				t.Fatalf("declaredJobPending = %v, want %v", got, tc.want)
			}
		})
	}
}

// newPendingServer builds the single-controller tenant ctrl-a, which
// declares acme/app-a and acme/checkout-api, with a read grant.
func newPendingServer(reader *fakeRecordReader) *Server {
	client := newMultibranchControllerFixture()
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	return pipelinesTestServer(client, authz, reader, nil)
}

func TestHandleListPipelines_DeclaredJobAbsentFromInventoryIsPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, []string{"acme/checkout-api"}, true, true),
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")
	if !pendingOf(t, resp, "acme/app-a") {
		t.Fatalf("acme/app-a not pending: %+v", resp.Items)
	}
	if pendingOf(t, resp, "acme/checkout-api") {
		t.Fatalf("acme/checkout-api pending although Projects holds it")
	}
}

func TestHandleListPipelines_DeclaredMultibranchAbsentIsPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory([]string{"acme/app-a"}, nil, true, true),
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")
	if !pendingOf(t, resp, "acme/checkout-api") {
		t.Fatalf("acme/checkout-api not pending: %+v", resp.Items)
	}
	if pendingOf(t, resp, "acme/app-a") {
		t.Fatalf("acme/app-a pending although Jobs holds it")
	}
}

func TestHandleListPipelines_ReportedJobIsNotPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory([]string{"acme/app-a"}, []string{"acme/checkout-api"}, true, true),
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")
	if pendingOf(t, resp, "acme/app-a") || pendingOf(t, resp, "acme/checkout-api") {
		t.Fatalf("a reported job is pending: %+v", resp.Items)
	}
}

func TestHandleListPipelines_UnknownInventoryMarksNothingPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, false, false),
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")
	if pendingOf(t, resp, "acme/app-a") || pendingOf(t, resp, "acme/checkout-api") {
		t.Fatalf("an unknown inventory marks a job pending: %+v", resp.Items)
	}
}

func TestHandleListPipelines_JobWithRunIsNeverPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, true, true),
	}
	lastRun := recordstore.Run{JobPath: "acme/app-a", RunID: "run-app-a-1", Status: "COMPLETED"}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/app-a", LastRun: &lastRun},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")
	if pendingOf(t, resp, "acme/app-a") {
		t.Fatalf("a job with a run is pending: %+v", resp.Items)
	}
}

func TestHandleListPipelines_RecordOnlyJobIsNeverPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, true, true),
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "legacy/retired"},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")
	if pendingOf(t, resp, "legacy/retired") {
		t.Fatalf("a record-only job is pending: %+v", resp.Items)
	}
}

func TestHandleListPipelines_InventoryReadErrorMarksNothingPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventoryErr = http.ErrAbortHandler
	rec := doPipelinesRequest(t, newPendingServer(reader).handleListPipelines, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pipelineListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pendingOf(t, resp, "acme/app-a") || pendingOf(t, resp, "acme/checkout-api") {
		t.Fatalf("a failed read marks a job pending: %+v", resp.Items)
	}
}

// pendingTwoTenantServer builds tenant payments over ctrl-pipe (pipeline
// declaration, ns-a) and ctrl-multi (multibranch declaration, ns-b), both
// declaring shared/api. The reader's inventories are set by the caller.
func pendingTwoTenantServer(t *testing.T, reader *fakeRecordReader) *Server {
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
	seedTenant(t, client.crdStore, unplacedTenantObject("payments", "ns-a", "ns-b"))
	return pipelinesTestServer(client, namespaceAuthorizer("ns-a", "ns-b"), reader, nil)
}

// TestHandleListPipelines_AnyTenantNotPendingWins asserts that one tenant
// that does not report a job pending clears the flag, whichever tenant the
// handler reads first.
func TestHandleListPipelines_AnyTenantNotPendingWins(t *testing.T) {
	t.Run("one tenant holds the job", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.jobInventory = map[string]recordstore.JobInventory{
			"core/ns-a/ctrl-pipe":  pendingInventory(nil, nil, true, false),
			"core/ns-b/ctrl-multi": pendingInventory(nil, []string{"shared/api"}, true, true),
		}
		srv := pendingTwoTenantServer(t, reader)
		for i := 0; i < 40; i++ {
			resp := listPipelinesFor(t, srv, "payments")
			if pendingOf(t, resp, "shared/api") {
				t.Fatalf("run %d: shared/api pending although one tenant holds it", i)
			}
		}
	})

	t.Run("both tenants lack the job", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.jobInventory = map[string]recordstore.JobInventory{
			"core/ns-a/ctrl-pipe":  pendingInventory(nil, nil, true, false),
			"core/ns-b/ctrl-multi": pendingInventory(nil, nil, true, true),
		}
		srv := pendingTwoTenantServer(t, reader)
		for i := 0; i < 40; i++ {
			resp := listPipelinesFor(t, srv, "payments")
			if !pendingOf(t, resp, "shared/api") {
				t.Fatalf("run %d: shared/api not pending although both tenants lack it", i)
			}
		}
	})
}

func TestHandleGetPipelineJob_DeclaredJobIsPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, []string{"acme/checkout-api"}, true, true),
	}
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a", map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !detail.Pending {
		t.Fatalf("declared job without a run is not pending: %s", rec.Body.String())
	}
}

func TestHandleGetPipelineJob_JobWithRunIsNotPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, true, true),
	}
	lastRun := recordstore.Run{JobPath: "acme/app-a", RunID: "run-app-a-1", Status: "COMPLETED"}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/app-a", LastRun: &lastRun},
	}
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a", map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Pending {
		t.Fatalf("a job with a run is pending")
	}
}

func TestHandleGetPipelineJob_UnknownInventoryIsNotPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, false, false),
	}
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a", map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Pending {
		t.Fatalf("an unknown inventory marks the job pending")
	}
}

func TestHandleGetPipelineJob_UndeclaredJobIsNotPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, true, true),
	}
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fundeclared", map[string]string{"tenant": "ctrl-a", "job": "acme/undeclared"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Pending {
		t.Fatalf("an undeclared job is pending")
	}
}

// detailPendingOf reads the job detail of shared/api for tenant payments.
func detailPendingOf(t *testing.T, srv *Server) pipelineJobDetailJSON {
	t.Helper()
	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/shared%2Fapi", map[string]string{"tenant": "payments", "job": "shared/api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detail
}

// TestHandleGetPipelineJob_AnyTenantNotPendingWins asserts that the job
// detail applies the cross-tenant rule of the list: one declaring tenant
// that holds the job clears the flag, and only all-lacking tenants keep it.
func TestHandleGetPipelineJob_AnyTenantNotPendingWins(t *testing.T) {
	t.Run("one tenant holds the job", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.jobInventory = map[string]recordstore.JobInventory{
			"core/ns-a/ctrl-pipe":  pendingInventory(nil, nil, true, false),
			"core/ns-b/ctrl-multi": pendingInventory(nil, []string{"shared/api"}, true, true),
		}
		if detailPendingOf(t, pendingTwoTenantServer(t, reader)).Pending {
			t.Fatalf("shared/api pending although one declaring tenant holds it")
		}
	})

	t.Run("both tenants lack the job", func(t *testing.T) {
		reader := newFakeRecordReader()
		reader.jobInventory = map[string]recordstore.JobInventory{
			"core/ns-a/ctrl-pipe":  pendingInventory(nil, nil, true, false),
			"core/ns-b/ctrl-multi": pendingInventory(nil, nil, true, true),
		}
		if !detailPendingOf(t, pendingTwoTenantServer(t, reader)).Pending {
			t.Fatalf("shared/api not pending although both declaring tenants lack it")
		}
	})
}

// TestHandleGetPipelineJob_InventoryReadErrorIsNotPending asserts that a
// failed inventory read answers 200 and marks no job pending.
func TestHandleGetPipelineJob_InventoryReadErrorIsNotPending(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventoryErr = http.ErrAbortHandler
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a", map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Pending {
		t.Fatalf("a failed inventory read marks the job pending")
	}
}

// TestHandleGetPipelineJob_RunOnNonDeclaringTenantIsNotPending asserts that
// a run on a readable tenant that does not declare the job clears pending,
// on the detail and on the list alike. ctrl-pipe declares shared/api and
// lacks it. ctrl-run reads no declaration of it and holds a run.
func TestHandleGetPipelineJob_RunOnNonDeclaringTenantIsNotPending(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-pipe": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-pipe", Namespace: "ns-a"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-pipe"}},
		},
		"ctrl-run": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-run", Namespace: "ns-b"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-run"}},
		},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"bundle-pipe": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-pipe", Namespace: "ns-a"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "content-pipe"},
		},
		"bundle-run": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-run", Namespace: "ns-b"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "content-run"},
		},
	}
	client.configMaps = map[string]map[string]string{
		"content-pipe": {"items.yaml": sharedPathItemsYAML("pipeline")},
		"content-run": {"items.yaml": `
items:
  - kind: folder
    name: other
    items:
      - kind: pipeline
        name: job
        definition:
          script: "pipeline {}"
`},
	}
	seedTenant(t, client.crdStore, unplacedTenantObject("payments", "ns-a", "ns-b"))

	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/ns-a/ctrl-pipe": pendingInventory(nil, nil, true, false),
	}
	run := recordstore.Run{JobPath: "shared/api", RunID: "run-shared-1", Status: "COMPLETED"}
	reader.jobSummaries["core/ns-b/ctrl-run"] = []recordstore.JobSummary{
		{JobPath: "shared/api", LastRun: &run},
	}
	srv := pipelinesTestServer(client, namespaceAuthorizer("ns-a", "ns-b"), reader, nil)

	if detailPendingOf(t, srv).Pending {
		t.Fatalf("detail: shared/api pending although a readable tenant holds a run")
	}
	if pendingOf(t, listPipelinesFor(t, srv, "payments"), "shared/api") {
		t.Fatalf("list: shared/api pending although a readable tenant holds a run")
	}
}
