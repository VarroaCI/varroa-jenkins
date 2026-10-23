package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// validateMarker posts marker to /pipelines/{tenant}/markers/validate for
// tenant "ctrl-a" and returns the recorded response.
func validateMarker(t *testing.T, srv *Server, marker string) *httptest.ResponseRecorder {
	t.Helper()
	req := doWriteRequest("/pipelines/ctrl-a/markers/validate", map[string]string{"tenant": "ctrl-a"}, testClaims(),
		map[string]string{"marker": marker})
	rec := httptest.NewRecorder()
	srv.handleValidatePipelineMarker(rec, req)
	return rec
}

const validMarkerNoValues = `
apiVersion: varroa.dev/v1
kind: Pipeline
template: golang-service
templateVersion: "2"
`

const markerWithUnknownValue = `
apiVersion: varroa.dev/v1
kind: Pipeline
template: golang-service
templateVersion: "2"
values:
  extra: true
`

// TestHandleValidatePipelineMarker_AuthorizedGetsFindings asserts task
// 7.2a's "an authorized caller gets findings" case: a marker whose values
// the resolved template's schema rejects reports one finding and no
// resolved template name.
func TestHandleValidatePipelineMarker_AuthorizedGetsFindings(t *testing.T) {
	client := newTemplateFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := validateMarker(t, srv, markerWithUnknownValue)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp markerValidateResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Findings) == 0 {
		t.Fatal("findings is empty, want at least one for an unknown value")
	}
	if resp.TemplateName != "" || resp.TemplateVersion != "" {
		t.Errorf("templateName/templateVersion = %q/%q, want empty when findings are non-empty", resp.TemplateName, resp.TemplateVersion)
	}
}

// TestHandleValidatePipelineMarker_ValidMarkerResolvesTemplate asserts a
// marker whose values satisfy the resolved template's schema reports no
// finding and names the resolved template.
func TestHandleValidatePipelineMarker_ValidMarkerResolvesTemplate(t *testing.T) {
	client := newTemplateFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := validateMarker(t, srv, validMarkerNoValues)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp markerValidateResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Findings) != 0 {
		t.Fatalf("findings = %+v, want none for a valid marker", resp.Findings)
	}
	if resp.TemplateName != "golang-service" || resp.TemplateVersion != "2.3.1" {
		t.Errorf("templateName/templateVersion = %q/%q, want golang-service/2.3.1 (the highest delivered match for \"2\")", resp.TemplateName, resp.TemplateVersion)
	}
}

// TestHandleValidatePipelineMarker_UnauthorizedGetsNoFindings asserts
// task 7.2a's "an unauthorized caller gets ... no findings and no
// template name" case: a caller with no pipelines:read grant on the
// tenant gets the same error response the tenant's other pipeline reads
// answer for an unreadable tenant (design D1's anti-enumeration 404, per
// TenantResolver.Resolve's own contract), never the findings shape.
func TestHandleValidatePipelineMarker_UnauthorizedGetsNoFindings(t *testing.T) {
	client := newTemplateFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "other-ns/other-ctrl")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := validateMarker(t, srv, validMarkerNoValues)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want an error status for an unauthorized caller; body = %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["findings"]; ok {
		t.Errorf("response carries a findings field: %v", resp)
	}
	if _, ok := resp["templateName"]; ok {
		t.Errorf("response carries a templateName field: %v", resp)
	}
}

// placedTenantMarkerTemplatesYAML delivers one PipelineTemplate document,
// used as either the cell's own bundle content or the tenant's own
// bundle content in the placed-Tenant tests below.
func placedTenantMarkerTemplatesYAML(name, version string) string {
	return `
templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: ` + name + `
    spec:
      version: "` + version + `"
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "pipeline {}"
`
}

// newPlacedTenantMarkerFixture seeds a placed Tenant "payments" on cell
// "ctrl-a" (namespace "test-ns"): the cell's own bundle delivers
// "cell-template", and the Tenant's own tenant bundle
// (spec.pipelines.composedBundleRef) delivers "payments-template".
// Design D13: the marker validate endpoint reads the tenant bundle only.
func newPlacedTenantMarkerFixture(t *testing.T) *fakeResourceClient {
	t.Helper()
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
		"bundle-cell-content":   {"pipeline-templates.yaml": placedTenantMarkerTemplatesYAML("cell-template", "1.0.0")},
		"bundle-tenant-content": {"pipeline-templates.yaml": placedTenantMarkerTemplatesYAML("payments-template", "1.0.0")},
	}
	seedTenant(t, client.crdStore, placedTenantObjectWithBundle("payments", "core", "test-ns", "ctrl-a", "test-ns"))
	return client
}

// TestHandleValidatePipelineMarker_PlacedTenantUsesTenantBundle asserts
// design D13: a placed Tenant's marker validation reads the tenant's own
// bundle, not the cell's, so a template the tenant bundle delivers
// resolves even though the cell bundle does not carry it.
func TestHandleValidatePipelineMarker_PlacedTenantUsesTenantBundle(t *testing.T) {
	client := newPlacedTenantMarkerFixture(t)
	authz := resolverAuthorizer("payments")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	req := doWriteRequest("/pipelines/payments/markers/validate", map[string]string{"tenant": "payments"}, testClaims(),
		map[string]string{"marker": "apiVersion: varroa.dev/v1\nkind: Pipeline\ntemplate: payments-template\ntemplateVersion: \"1\"\n"})
	rec := httptest.NewRecorder()
	srv.handleValidatePipelineMarker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp markerValidateResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Findings) != 0 {
		t.Fatalf("findings = %+v, want none for a template the tenant bundle delivers", resp.Findings)
	}
	if resp.TemplateName != "payments-template" {
		t.Errorf("templateName = %q, want payments-template", resp.TemplateName)
	}
}

// TestHandleValidatePipelineMarker_CellBundleOnlyTemplateNotVisible
// asserts the flip side: a template that exists only in the cell's own
// bundle, and that the tenant's own bundle never delivered, is invisible
// to the tenant's marker validation.
func TestHandleValidatePipelineMarker_CellBundleOnlyTemplateNotVisible(t *testing.T) {
	client := newPlacedTenantMarkerFixture(t)
	authz := resolverAuthorizer("payments")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	req := doWriteRequest("/pipelines/payments/markers/validate", map[string]string{"tenant": "payments"}, testClaims(),
		map[string]string{"marker": "apiVersion: varroa.dev/v1\nkind: Pipeline\ntemplate: cell-template\ntemplateVersion: \"1\"\n"})
	rec := httptest.NewRecorder()
	srv.handleValidatePipelineMarker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp markerValidateResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Findings) == 0 {
		t.Fatal("findings is empty, want a finding for a template only the cell bundle delivers")
	}
	if resp.TemplateName != "" {
		t.Errorf("templateName = %q, want empty: a cell-bundle-only template must not resolve for the tenant", resp.TemplateName)
	}
}

// TestHandleValidatePipelineMarker_NoBundleReportsOneFinding asserts the
// varroa-cli delta's "A tenant with no bundle produces a finding" case: a
// controller with no composed bundle at all delivers no template, so
// validation reports one finding naming the tenant.
func TestHandleValidatePipelineMarker_NoBundleReportsOneFinding(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := validateMarker(t, srv, validMarkerNoValues)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp markerValidateResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one", resp.Findings)
	}
	if resp.TemplateName != "" {
		t.Errorf("templateName = %q, want empty", resp.TemplateName)
	}
}
