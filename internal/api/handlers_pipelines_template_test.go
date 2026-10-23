package api

import (
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// templateFixtureItemsYAML declares the three job shapes design D12's
// Template row distinguishes: a marker-defined multibranch project, an
// items-declared pipeline bound to a catalog template, and a plain
// Jenkinsfile pipeline with no catalog template at all.
const templateFixtureItemsYAML = `
items:
  - kind: multibranch
    name: svc-marker
    sourcesList:
      - branchSource:
          source:
            git:
              remote: https://github.com/fake-org/svc-marker.git
              credentialsId: git-cred
    projectFactory:
      varroaMarkerProjectFactory: {}
  - kind: pipeline
    name: svc-items
    definition:
      varroaTemplate:
        template: golang-service
        templateVersion: "2"
  - kind: pipeline
    name: svc-jenkinsfile
    definition:
      script: "pipeline {}"
`

// templateFixturePipelineTemplatesYAML delivers two versions of
// golang-service, so an items-declared job's "2" constraint must resolve
// to the highest delivered patch rather than echoing the raw constraint.
const templateFixturePipelineTemplatesYAML = `
templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: golang-service
    spec:
      version: "2.0.0"
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline {}"
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: golang-service
    spec:
      version: "2.3.1"
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline {}"
`

// newTemplateFixture seeds one controller, "ctrl-a" in "test-ns", carrying
// a composed bundle whose items.yaml and pipeline-templates.yaml declare
// the three job shapes above.
func newTemplateFixture(t *testing.T) *fakeResourceClient {
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
		"bundle-a-content": {
			"items.yaml":              templateFixtureItemsYAML,
			"pipeline-templates.yaml": templateFixturePipelineTemplatesYAML,
		},
	}
	return client
}

func getPipelineJobTemplate(t *testing.T, client *fakeResourceClient, reader *fakeRecordReader, jobPath string) *pipelineTemplateInfoJSON {
	t.Helper()
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	srv := pipelinesTestServer(client, authz, reader, nil)
	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/ctrl-a/jobs/"+jobPath,
		map[string]string{"tenant": "ctrl-a", "job": jobPath})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detail.Template
}

// TestHandleGetPipelineJob_MarkerDefinedWithRun asserts the pipelines-api
// delta's "A marker-defined job reports its resolved template" scenario:
// the last run's payload carries the resolved name, version, marker path,
// and marker revision, and the build that resolved it.
func TestHandleGetPipelineJob_MarkerDefinedWithRun(t *testing.T) {
	client := newTemplateFixture(t)
	reader := newFakeRecordReader()
	buildNumber := int64(42)
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "svc-marker/main",
		LastRun: &recordstore.Run{
			RunID:       "run-1",
			BuildNumber: &buildNumber,
			Payload: json.RawMessage(`{"run.started":{"kind":"run.started","payload":` +
				`{"template_name":"go-service","template_version":"2.1.4","marker_path":".varroa/pipeline.yaml","marker_revision":"abc123"}}}`),
		},
	}}

	tmpl := getPipelineJobTemplate(t, client, reader, "svc-marker/main")
	if tmpl == nil {
		t.Fatal("template is nil, want a resolved marker template")
	}
	if tmpl.Source != "marker" {
		t.Errorf("source = %q, want marker", tmpl.Source)
	}
	if tmpl.Name != "go-service" || tmpl.Version != "2.1.4" {
		t.Errorf("name/version = %q/%q, want go-service/2.1.4", tmpl.Name, tmpl.Version)
	}
	if tmpl.MarkerPath != ".varroa/pipeline.yaml" || tmpl.MarkerRevision != "abc123" {
		t.Errorf("markerPath/markerRevision = %q/%q", tmpl.MarkerPath, tmpl.MarkerRevision)
	}
	if tmpl.AsOfBuild != 42 {
		t.Errorf("asOfBuild = %d, want 42", tmpl.AsOfBuild)
	}
}

// TestHandleGetPipelineJob_MarkerDefinedBeforeFirstRun asserts the delta's
// "A marker-defined job with no run reports the marker only" scenario.
func TestHandleGetPipelineJob_MarkerDefinedBeforeFirstRun(t *testing.T) {
	client := newTemplateFixture(t)
	reader := newFakeRecordReader()

	tmpl := getPipelineJobTemplate(t, client, reader, "svc-marker/feature")
	if tmpl == nil {
		t.Fatal("template is nil, want the marker-only shape")
	}
	if tmpl.Source != "marker" {
		t.Errorf("source = %q, want marker", tmpl.Source)
	}
	if tmpl.MarkerPath != ".varroa/pipeline.yaml" {
		t.Errorf("markerPath = %q, want .varroa/pipeline.yaml", tmpl.MarkerPath)
	}
	if tmpl.Name != "" || tmpl.Version != "" || tmpl.AsOfBuild != 0 {
		t.Errorf("template = %+v, want no name, version, or asOfBuild", tmpl)
	}
}

// TestHandleGetPipelineJob_ItemsDeclaredTemplate asserts the delta's
// "For an items-declared job the same object SHALL carry name, version,
// and source: items" rule, resolved against the delivered catalog set
// rather than echoed from the item's raw constraint.
func TestHandleGetPipelineJob_ItemsDeclaredTemplate(t *testing.T) {
	client := newTemplateFixture(t)
	reader := newFakeRecordReader()

	tmpl := getPipelineJobTemplate(t, client, reader, "svc-items")
	if tmpl == nil {
		t.Fatal("template is nil, want the items-declared shape")
	}
	if tmpl.Source != "items" {
		t.Errorf("source = %q, want items", tmpl.Source)
	}
	if tmpl.Name != "golang-service" {
		t.Errorf("name = %q, want golang-service", tmpl.Name)
	}
	if tmpl.Version != "2.3.1" {
		t.Errorf("version = %q, want 2.3.1 (the highest delivered match for constraint \"2\")", tmpl.Version)
	}
	if tmpl.MarkerPath != "" || tmpl.MarkerRevision != "" || tmpl.AsOfBuild != 0 {
		t.Errorf("template = %+v, want no markerPath, markerRevision, or asOfBuild", tmpl)
	}
}

// TestHandleGetPipelineJob_PlacedTenantTemplateFromTenantBundle asserts
// design D13 on the job-detail path: a placed Tenant's items-declared job
// resolves its template version against the Tenant's own tenant bundle
// (spec.pipelines.composedBundleRef), not the cell's, so a version the
// cell bundle does not carry is still found.
func TestHandleGetPipelineJob_PlacedTenantTemplateFromTenantBundle(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-cell"}},
		},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"bundle-cell": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-cell", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "bundle-cell-content"},
		},
		"bundle-tenant": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-tenant", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "bundle-tenant-content"},
		},
	}
	client.configMaps = map[string]map[string]string{
		// The cell's own composed items.yaml carries the cell's items only.
		// The tenant's items and templates come from the tenant bundle; the
		// cell re-roots the items under the Tenant folder when it assembles.
		"bundle-cell-content": {"items.yaml": "items:\n  - kind: pipeline\n    name: hello-varroa\n"},
		"bundle-tenant-content": {
			"items.yaml": `
items:
  - kind: pipeline
    name: svc-items
    definition:
      varroaTemplate:
        template: payments-template
        templateVersion: "1"
`,
			"pipeline-templates.yaml": placedTenantMarkerTemplatesYAML("payments-template", "1.4.0"),
		},
	}
	seedTenant(t, client.crdStore, placedTenantObjectWithBundle("payments", "core", "test-ns", "ctrl-a", "test-ns"))

	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Fsvc-items",
		map[string]string{"tenant": "payments", "job": "payments/svc-items"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Template == nil {
		t.Fatal("template is nil, want the items-declared shape")
	}
	if detail.Template.Name != "payments-template" || detail.Template.Version != "1.4.0" {
		t.Errorf("name/version = %q/%q, want payments-template/1.4.0 (from the tenant bundle, not the cell bundle)",
			detail.Template.Name, detail.Template.Version)
	}
}

// TestHandleGetPipelineJob_JenkinsfileJobHasNoTemplate asserts the delta's
// "A Jenkinsfile job reports no template" scenario.
func TestHandleGetPipelineJob_JenkinsfileJobHasNoTemplate(t *testing.T) {
	client := newTemplateFixture(t)
	reader := newFakeRecordReader()

	tmpl := getPipelineJobTemplate(t, client, reader, "svc-jenkinsfile")
	if tmpl != nil {
		t.Errorf("template = %+v, want nil for a Jenkinsfile job", tmpl)
	}
}

// TestPlacedTenantListsTenantItemsNotCellItems asserts that a placed Tenant's
// pipeline surface reads the tenant's own bundle items.yaml, re-rooted
// under the Tenant folder, and never the cell's bundle: a warm cell carries
// the starter bundle, whose root-level items belong to no tenant.
func TestPlacedTenantListsTenantItemsNotCellItems(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-cell"}},
		},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"bundle-cell": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-cell", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "bundle-cell-content"},
		},
		"bundle-tenant": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-tenant", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "bundle-tenant-content"},
		},
	}
	client.configMaps = map[string]map[string]string{
		"bundle-cell-content":   {"items.yaml": "items:\n  - kind: pipeline\n    name: hello-varroa\n"},
		"bundle-tenant-content": {"items.yaml": "items:\n  - kind: pipeline\n    name: app-x\n    definition:\n      script: \"pipeline {}\"\n    parameters:\n      - string:\n          name: ENV\n          defaultValue: staging\n"},
	}
	seedTenant(t, client.crdStore, placedTenantObjectWithBundle("payments", "core", "test-ns", "ctrl-a", "test-ns"))
	srv := pipelinesTestServer(client, resolverAuthorizer("payments"), newFakeRecordReader(), nil)

	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/payments", map[string]string{"tenant": "payments"})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []pipelineSummaryJSON `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	paths := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		paths = append(paths, it.JobPath)
	}
	if len(paths) != 1 || paths[0] != "payments/app-x" {
		t.Fatalf("job paths = %v, want exactly [payments/app-x]", paths)
	}

	rec = doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/payments/jobs/payments%2Fapp-x",
		map[string]string{"tenant": "payments", "job": "payments/app-x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Parameters) != 1 || detail.Parameters[0].Name != "ENV" {
		t.Fatalf("parameters = %+v, want the tenant item's ENV parameter; the item was not found under its folder", detail.Parameters)
	}
}
