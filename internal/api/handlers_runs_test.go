package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// newRunFixtureServer builds a Server over one resolvable controller
// tenant, "test-ns/ctrl-a" (URL segment "ctrl-a"), granted read access.
// A controller tenant's Folder is always empty, so a run's job path
// needs no folder prefix to authorize.
func newRunFixtureServer(reader recordstore.Reader, blobStore blob.Store) *Server {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	return pipelinesTestServer(client, authz, reader, blobStore)
}

func doRunRequest(t *testing.T, handler func(http.ResponseWriter, *http.Request), path string, pathValues map[string]string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	if header != nil {
		req.Header = header
	}
	req = req.WithContext(auth.ContextWithClaims(context.Background(), testClaims()))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// --- 3.8: GET /runs/{run} — 404 not 403; a run survives a deleted cell ---

func TestHandleGetRun_UnreadableRunIs404Not403(t *testing.T) {
	reader := newFakeRecordReader()
	// A different namespace, not just a different controller: the
	// controller-tenant grant is namespace-scoped (rbac.Resolver's
	// effectiveControllerTenantGrants reads sc.Namespaces, not a
	// per-controller selector), so this is the case that must deny.
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "other-ns/ctrl-b", JobPath: "svc/main", Status: "COMPLETED"}
	srv := newRunFixtureServer(reader, nil) // grants test-ns/ctrl-a only; r1 belongs to other-ns/ctrl-b

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (never 403, so the surface does not confirm the record exists)", rec.Code)
	}
}

func TestHandleGetRun_SurvivesDeletedCell(t *testing.T) {
	reader := newFakeRecordReader()
	// cell is empty: the run's cluster/cell no longer identify a live
	// controller, but tenant/job_path alone still authorize the read.
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Cluster: "core", Cell: "", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for a run whose cell is gone", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.RunID != "r1" {
		t.Fatalf("runId = %q, want r1", detail.RunID)
	}
	if detail.TenantFolder != "" {
		t.Fatalf("tenantFolder = %q, want empty for a controller tenant", detail.TenantFolder)
	}
	if detail.Tenant != "test-ns/ctrl-a" {
		t.Fatalf("tenant = %q, want %q for a synthetic tenant", detail.Tenant, "test-ns/ctrl-a")
	}
}

// TestHandleGetRun_TenantFolderForPlacedTenant asserts a run stored under
// a placed Tenant's tenant (no "/", per rbac.ParseTenant) carries that
// Tenant name as tenantFolder.
func TestHandleGetRun_TenantFolderForPlacedTenant(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Cluster: "core", Tenant: "payments", JobPath: "payments/app-a", Status: "COMPLETED"}
	client := newFakeResourceClient()
	authz := resolverAuthorizer("payments")
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.TenantFolder != "payments" {
		t.Fatalf("tenantFolder = %q, want %q", detail.TenantFolder, "payments")
	}
	if detail.Tenant != "payments" {
		t.Fatalf("tenant = %q, want %q for a Tenant tenant", detail.Tenant, "payments")
	}
}

// --- 7.6: the run's source column and its response object ---

// runSourceColumnJSON is the snake_case JSON the recorder's merge writes
// into the runs.source column: the store's key names, never the camelCase
// names the response carries.
const runSourceColumnJSON = `{"definition_path":"ci/Jenkinsfile","libraries":[{"name":"acme","version":"1.2.3","revision":"abcdef1234567890"}],"pod_templates":[{"name":"maven","label":"maven","image":"maven:3.9.6"}]}`

// TestHandleGetRun_CarriesSource seeds the column with the recorder's own
// shape and asserts the response answers all three camelCase fields. A
// direct unmarshal of the column into the response type would leave
// definitionPath and podTemplates empty, because the column spells those
// two keys with snake_case.
func TestHandleGetRun_CarriesSource(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Source: json.RawMessage(runSourceColumnJSON),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Source == nil {
		t.Fatalf("source is absent, want the record's source object")
	}
	if detail.Source.DefinitionPath != "ci/Jenkinsfile" {
		t.Errorf("definitionPath = %q, want %q", detail.Source.DefinitionPath, "ci/Jenkinsfile")
	}
	if len(detail.Source.Libraries) != 1 {
		t.Fatalf("libraries = %+v, want one entry", detail.Source.Libraries)
	}
	if got := detail.Source.Libraries[0]; got.Name != "acme" || got.Version != "1.2.3" || got.Revision != "abcdef1234567890" {
		t.Errorf("libraries[0] = %+v, want acme, 1.2.3, abcdef1234567890", got)
	}
	if len(detail.Source.PodTemplates) != 1 {
		t.Fatalf("podTemplates = %+v, want one entry", detail.Source.PodTemplates)
	}
	if got := detail.Source.PodTemplates[0]; got.Name != "maven" || got.Label != "maven" || got.Image != "maven:3.9.6" {
		t.Errorf("podTemplates[0] = %+v, want maven, maven, maven:3.9.6", got)
	}
	if !strings.Contains(rec.Body.String(), `"definitionPath"`) {
		t.Errorf("body = %s, want the camelCase key definitionPath", rec.Body.String())
	}
}

// TestHandleGetRun_OmitsSourceWhenColumnCarriesNone covers the three
// empty cases: a NULL column, an empty object, and a column that fails to
// decode. Each one still answers the run.
func TestHandleGetRun_OmitsSourceWhenColumnCarriesNone(t *testing.T) {
	cases := []struct {
		name   string
		source json.RawMessage
	}{
		{"null column", nil},
		{"empty object", json.RawMessage(`{}`)},
		{"undecodable column", json.RawMessage(`{"definition_path":`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := newFakeRecordReader()
			reader.runs["r1"] = recordstore.Run{
				RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
				Source: c.source,
			}
			srv := newRunFixtureServer(reader, nil)

			rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
			}
			var detail runDetailJSON
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if detail.RunID != "r1" {
				t.Fatalf("runId = %q, want r1: one unreadable column must not hide the run", detail.RunID)
			}
			if detail.Source != nil {
				t.Fatalf("source = %+v, want it absent", detail.Source)
			}
			if strings.Contains(rec.Body.String(), `"source"`) {
				t.Fatalf("body = %s, want no source key", rec.Body.String())
			}
		})
	}
}

// --- the run's inputs column and its response array ---

// runInputsColumnJSON is the snake_case JSON the recorder's input write
// stores in the runs.inputs column: the store's key names, never the
// camelCase names the response carries.
const runInputsColumnJSON = `[` +
	`{"id":"in-1","message":"Deploy?","ok":"Ship it","submitter":"deployers","submitter_parameter":"APPROVER",` +
	`"parameters":[{"name":"ENV","type":"ChoiceParameterDefinition","description":"target environment","default_value":"staging","choices":["staging","prod"]}]},` +
	`{"id":"in-2","message":"Resume?","parameters":[{"name":"NOTE","type":"StringParameterDefinition"}]}]`

// TestHandleGetRun_CarriesInputs seeds the column with the recorder's own
// shape and asserts the response answers both entries with the camelCase
// key names.
func TestHandleGetRun_CarriesInputs(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "RUNNING",
		Inputs: json.RawMessage(runInputsColumnJSON),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range []string{`"inputs"`, `"submitterParameter"`, `"defaultValue"`} {
		if !strings.Contains(body, key) {
			t.Errorf("body = %s, want the camelCase key %s", body, key)
		}
	}
	if strings.Contains(body, `"default_value"`) || strings.Contains(body, `"submitter_parameter"`) {
		t.Errorf("body = %s, want no snake_case column key", body)
	}
	if strings.Contains(body, `"description":""`) {
		t.Errorf("body = %s, want an omitted value absent rather than an empty string", body)
	}

	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Inputs) != 2 {
		t.Fatalf("inputs = %+v, want two entries", detail.Inputs)
	}
	first := detail.Inputs[0]
	if first.ID != "in-1" || first.Message != "Deploy?" || first.OK != "Ship it" {
		t.Errorf("inputs[0] = %+v, want in-1, Deploy?, Ship it", first)
	}
	if first.Submitter != "deployers" || first.SubmitterParameter != "APPROVER" {
		t.Errorf("inputs[0] submitter = %q/%q, want deployers/APPROVER", first.Submitter, first.SubmitterParameter)
	}
	if len(first.Parameters) != 1 {
		t.Fatalf("inputs[0] parameters = %+v, want one entry", first.Parameters)
	}
	p := first.Parameters[0]
	if p.Name != "ENV" || p.Type != "ChoiceParameterDefinition" || p.Description != "target environment" {
		t.Errorf("parameter = %+v, want ENV, ChoiceParameterDefinition, target environment", p)
	}
	if p.DefaultValue != "staging" || len(p.Choices) != 2 || p.Choices[1] != "prod" {
		t.Errorf("parameter default/choices = %q/%v, want staging and [staging prod]", p.DefaultValue, p.Choices)
	}
	if len(detail.Inputs[1].Parameters) != 1 || detail.Inputs[1].Parameters[0].Name != "NOTE" {
		t.Errorf("inputs[1] = %+v, want one NOTE parameter", detail.Inputs[1])
	}
}

// TestHandleGetRun_OmitsInputsWhenColumnCarriesNone covers the three
// empty cases: a NULL column, an empty array, and a column that fails to
// decode. Each one still answers the run.
func TestHandleGetRun_OmitsInputsWhenColumnCarriesNone(t *testing.T) {
	cases := []struct {
		name   string
		inputs json.RawMessage
	}{
		{"null column", nil},
		{"empty array", json.RawMessage(`[]`)},
		{"undecodable column", json.RawMessage(`[{"id":`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := newFakeRecordReader()
			reader.runs["r1"] = recordstore.Run{
				RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "RUNNING",
				Inputs: c.inputs,
			}
			srv := newRunFixtureServer(reader, nil)

			rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
			}
			var detail runDetailJSON
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if detail.RunID != "r1" {
				t.Fatalf("runId = %q, want r1: one unreadable column must not hide the run", detail.RunID)
			}
			if detail.Inputs != nil {
				t.Fatalf("inputs = %+v, want it absent", detail.Inputs)
			}
			if strings.Contains(rec.Body.String(), `"inputs"`) {
				t.Fatalf("body = %s, want no inputs key", rec.Body.String())
			}
		})
	}
}

// --- 3.9: nil RecordStore / nil BlobStore / ErrSchemaTooOld -> 503 ---

func newFakeRecordReaderWithRun() *fakeRecordReader {
	r := newFakeRecordReader()
	r.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	r.artifacts["r1"] = []recordstore.Artifact{{RunID: "r1", Path: "a.txt", SizeBytes: 3, BlobKey: "logs/artifacts/r1/a.txt"}}
	return r
}

func TestPipelinesPaths_Answer503(t *testing.T) {
	schemaTooOldReader := newFakeRecordReader()
	schemaTooOldReader.schemaTooOld = true

	cases := []struct {
		name       string
		reader     recordstore.Reader
		blob       blob.Store
		handlerOf  func(*Server) func(http.ResponseWriter, *http.Request)
		path       string
		values     map[string]string
		wantStatus int
	}{
		{"listPipelines-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListPipelines }, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"}, http.StatusServiceUnavailable},
		{"getPipelineJob-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetPipelineJob }, "/pipelines/ctrl-a/jobs/j", map[string]string{"tenant": "ctrl-a", "job": "j"}, http.StatusServiceUnavailable},
		{"listRuns-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRuns }, "/pipelines/ctrl-a/jobs/j/runs", map[string]string{"tenant": "ctrl-a", "job": "j"}, http.StatusServiceUnavailable},
		{"getRun-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRun }, "/runs/r1", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listRunArtifacts-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRunArtifacts }, "/runs/r1/artifacts", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listRunTests-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRunTests }, "/runs/r1/tests", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listRunChanges-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRunChanges }, "/runs/r1/changes", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listJobTestCaseHistory-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListJobTestCaseHistory }, "/pipelines/ctrl-a/jobs/j/tests/C%2Fn", map[string]string{"tenant": "ctrl-a", "job": "j", "case": "C/n"}, http.StatusServiceUnavailable},
		{"downloadRunArtifact-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleDownloadRunArtifact }, "/runs/r1/artifacts/a.txt", map[string]string{"run": "r1", "path": "a.txt"}, http.StatusServiceUnavailable},

		// A plain run read needs no blob store: a nil store only drops the
		// digest's log tail, so the run detail still answers.
		{"getRun-nilBlobStoreStillOK", newFakeRecordReaderWithRun(), nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRun }, "/runs/r1", map[string]string{"run": "r1"}, http.StatusOK},
		// The script read needs both the reader and the blob store.
		{"getRunScript-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRunScript }, "/runs/r1/script", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"getRunScript-nilBlobStore", newFakeRecordReaderWithRun(), nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRunScript }, "/runs/r1/script", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		// The artifact download needs one.
		{"downloadRunArtifact-nilBlobStore", newFakeRecordReaderWithRun(), nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleDownloadRunArtifact }, "/runs/r1/artifacts/a.txt", map[string]string{"run": "r1", "path": "a.txt"}, http.StatusServiceUnavailable},

		{"listPipelines-schemaTooOld", schemaTooOldReader, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListPipelines }, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"}, http.StatusServiceUnavailable},
		{"listJobTestCaseHistory-schemaTooOld", schemaTooOldReader, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListJobTestCaseHistory }, "/pipelines/ctrl-a/jobs/j/tests/C%2Fn", map[string]string{"tenant": "ctrl-a", "job": "j", "case": "C/n"}, http.StatusServiceUnavailable},
		{"getRun-schemaTooOld", schemaTooOldReader, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRun }, "/runs/r1", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newRunFixtureServer(c.reader, c.blob)
			rec := doRunRequest(t, c.handlerOf(srv), c.path, c.values, nil)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), c.wantStatus)
			}
		})
	}
}

// --- 3.10: GET /runs/{run}/artifacts/{path...} ---

func TestHandleDownloadRunArtifact_HappyPath_Range_EmptyKey(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	reader.artifacts["r1"] = []recordstore.Artifact{
		{RunID: "r1", Path: "build/report.txt", SizeBytes: 11, BlobKey: "artifacts/r1/build/report.txt"},
		{RunID: "r1", Path: "local-only.txt", SizeBytes: 0, BlobKey: ""},
	}
	blobStore := newFakeBlobStore()
	blobStore.objects["artifacts/r1/build/report.txt"] = []byte("hello world")

	srv := newRunFixtureServer(reader, blobStore)

	// Happy path: the complete artifact.
	rec := doRunRequest(t, srv.handleDownloadRunArtifact, "/runs/r1/artifacts/build/report.txt",
		map[string]string{"run": "r1", "path": "build/report.txt"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello world" {
		t.Fatalf("happy path body = %q, want %q", got, "hello world")
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Errorf("missing Content-Disposition header")
	}

	// A Range request returns 206 with only the requested bytes.
	rec = doRunRequest(t, srv.handleDownloadRunArtifact, "/runs/r1/artifacts/build/report.txt",
		map[string]string{"run": "r1", "path": "build/report.txt"}, http.Header{"Range": []string{"bytes=6-10"}})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "world" {
		t.Fatalf("range body = %q, want %q", got, "world")
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 6-10/11" {
		t.Fatalf("Content-Range = %q, want %q", cr, "bytes 6-10/11")
	}

	// An empty blob key (Jenkins stored the artifact locally) is 409,
	// naming the blob-mode chart value.
	rec = doRunRequest(t, srv.handleDownloadRunArtifact, "/runs/r1/artifacts/local-only.txt",
		map[string]string{"run": "r1", "path": "local-only.txt"}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("empty-key status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recordStore.blobs.mode") {
		t.Fatalf("empty-key body = %q, want it to name recordStore.blobs.mode", rec.Body.String())
	}
}

// TestHandleDownloadRunArtifact_PluginWrittenObjectNoPart proves the
// download reads a plugin-shaped object — one whole object at its own
// key, with no numbered chunk part — through the native-object read,
// never the chunked range reader design D6 forbids here.
func TestHandleDownloadRunArtifact_PluginWrittenObjectNoPart(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	reader.artifacts["r1"] = []recordstore.Artifact{
		{RunID: "r1", Path: "dist/app.jar", SizeBytes: 4, BlobKey: "artifacts/r1/dist/app.jar"},
	}
	blobStore := newFakeBlobStore()
	// fakeBlobStore.objects models exactly one native object at its key,
	// with no numbered part — the shape the artifact plugin writes.
	blobStore.objects["artifacts/r1/dist/app.jar"] = []byte("byte")

	srv := newRunFixtureServer(reader, blobStore)
	rec := doRunRequest(t, srv.handleDownloadRunArtifact, "/runs/r1/artifacts/dist/app.jar",
		map[string]string{"run": "r1", "path": "dist/app.jar"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "byte" {
		t.Fatalf("body = %q, want %q", got, "byte")
	}
}

// --- the run's summary line and its digest object ---

// TestHandleGetRun_CarriesSummaryAndDigest seeds a failed run with a
// summary line, a failed stage, that stage's log lines, and test counts,
// then asserts the response carries all four.
func TestHandleGetRun_CarriesSummaryAndDigest(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	failed, skipped, total := int32(2), int32(1), int32(11)
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: strPtr("FAILURE"), StartedAt: &started, Summary: strPtr("shipping a hotfix"),
		TestsFailed: &failed, TestsSkipped: &skipped, TestsTotal: &total,
	}
	reader.stages["r1"] = []recordstore.Stage{
		{RunID: "r1", StageID: "s1", Name: "Build", Status: "SUCCESS"},
		{RunID: "r1", StageID: "s2", Name: "Deploy", Status: "FAILURE", Error: strPtr("deploy refused")},
	}

	store := newRunLogTestStore(t)
	putRunLogConsole(t, store, "r1", []byte("prepare\ndeploy refused\n"))
	putRunLogIndex(t, store, "r1", []runLogIndexEntry{
		{Seq: 0, StageID: "s1", Offset: 0, Len: 8},
		{Seq: 1, StageID: "s2", Offset: 8, Len: 15, Final: true},
	})

	srv := newRunFixtureServer(reader, store)
	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Summary != "shipping a hotfix" {
		t.Errorf("summary = %q, want %q", detail.Summary, "shipping a hotfix")
	}
	if detail.Digest == nil {
		t.Fatalf("digest is absent, body = %s", rec.Body.String())
	}
	if detail.Digest.Stage != "Deploy" || detail.Digest.Error != "deploy refused" {
		t.Errorf("digest stage/error = %q/%q, want Deploy/deploy refused", detail.Digest.Stage, detail.Digest.Error)
	}
	if detail.Digest.LogTail != "deploy refused" {
		t.Errorf("digest logTail = %q, want %q", detail.Digest.LogTail, "deploy refused")
	}
	wantTests := digestFailedTestsJSON{Failed: 2, Skipped: 1, Total: 11}
	if detail.Digest.FailedTests == nil || !reflect.DeepEqual(*detail.Digest.FailedTests, wantTests) {
		t.Errorf("digest failedTests = %+v, want %+v", detail.Digest.FailedTests, wantTests)
	}
	for _, key := range []string{`"summary"`, `"digest"`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("body = %s, want the key %s", rec.Body.String(), key)
		}
	}
}

// TestHandleGetRun_OmitsSummaryAndDigestWhenNothingSetsThem proves a
// successful run with no summary column answers neither key.
func TestHandleGetRun_OmitsSummaryAndDigestWhenNothingSetsThem(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Result: strPtr("SUCCESS"),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	for _, key := range []string{`"summary"`, `"digest"`} {
		if strings.Contains(rec.Body.String(), key) {
			t.Errorf("body = %s, want no %s key", rec.Body.String(), key)
		}
	}
}

// TestHandleGetRun_DigestHardReadFailure proves the digest survives a
// hard log read failure: a nil blob store and an undecodable log index
// both answer the run, with the failed stage and no log tail.
//
// A 500 "encode run detail failed" stays out of reach from the HTTP
// path: a run detail holds only strings, numbers, and time values, so
// the re-encoding convertJSON performs cannot fail. No case covers it.
func TestHandleGetRun_DigestHardReadFailure(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	newFailedReader := func() *fakeRecordReader {
		reader := newFakeRecordReader()
		reader.runs["r1"] = recordstore.Run{
			RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
			Result: strPtr("FAILURE"), StartedAt: &started,
		}
		reader.stages["r1"] = []recordstore.Stage{
			{RunID: "r1", StageID: "s2", Name: "Deploy", Status: "FAILURE", Error: strPtr("deploy refused")},
		}
		return reader
	}

	t.Run("nilBlobStore", func(t *testing.T) {
		assertStageWithoutLogTail(t, newRunFixtureServer(newFailedReader(), nil))
	})

	t.Run("undecodableLogIndex", func(t *testing.T) {
		store := newRunLogTestStore(t)
		putRunLogConsole(t, store, "r1", []byte("deploy refused\n"))
		putRawRunLogObject(t, store, blob.LogIndexKey("r1"), []byte("{not-json}\n"))
		assertStageWithoutLogTail(t, newRunFixtureServer(newFailedReader(), store))
	})
}

// assertStageWithoutLogTail asserts one GET /runs/{run} answer carries a
// digest naming the failed stage, with no log tail and no 503.
func assertStageWithoutLogTail(t *testing.T, srv *Server) {
	t.Helper()
	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200: a hard log read must not fail the run read", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Digest == nil {
		t.Fatalf("digest is absent, body = %s", rec.Body.String())
	}
	if detail.Digest.Stage != "Deploy" || detail.Digest.Error != "deploy refused" {
		t.Errorf("digest stage/error = %q/%q, want Deploy/deploy refused", detail.Digest.Stage, detail.Digest.Error)
	}
	if strings.Contains(rec.Body.String(), `"logTail"`) {
		t.Errorf("body = %s, want no logTail key", rec.Body.String())
	}
}

// --- The run's test cases and its test report ---

// newTestCaseReader seeds run r1 with one suite, two failed cases and one
// skipped case in the reader's own order, and a stored report whose blob
// key is blobKey. A nil blobKey stores the report with no key.
func newTestCaseReader(blobKey *string) *fakeRecordReader {
	reader := newFakeRecordReader()
	duration := int64(1250)
	totals, failed, skipped := int32(7), int32(2), int32(1)
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Payload:       json.RawMessage(`{"log_summary":{"finalized":true},"test_report":{"size_bytes":2048,"case_count":7}}`),
		TestReportKey: blobKey,
		TestsTotal:    &totals, TestsFailed: &failed, TestsSkipped: &skipped,
	}
	reader.tests["r1"] = []recordstore.TestSuite{
		{RunID: "r1", Suite: "unit", Total: 5, Failed: 2, Skipped: 0},
		{RunID: "r1", Suite: "it", Total: 2, Failed: 0, Skipped: 1},
	}
	reader.testCases["r1"] = []recordstore.TestCase{
		{Suite: "unit", ClassName: "com.example.FooTest", Name: "bar", Status: "FAILED", DurationMs: &duration, ErrorDetails: "boom"},
		{Suite: "unit", ClassName: "com.example.FooTest", Name: "baz", Status: "REGRESSION"},
		{Suite: "unit", ClassName: "com.example.FooTest", Name: "qux", Status: "SKIPPED"},
	}
	return reader
}

func TestHandleListRunTests_CarriesCasesAndReport(t *testing.T) {
	srv := newRunFixtureServer(newTestCaseReader(strPtr("artifacts/core/svc/main/1/artifacts/.varroa/tests.json")), nil)
	rec := doRunRequest(t, srv.handleListRunTests, "/runs/r1/tests", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp runTestListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	suites := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		suites = append(suites, it.Suite)
	}
	if strings.Join(suites, ",") != "unit,it" {
		t.Fatalf("items = %v, want the reader's own order", suites)
	}
	if resp.Totals == nil || *resp.Totals != (runTestTotalsJSON{Total: 7, Failed: 2, Skipped: 1}) {
		t.Fatalf("totals = %+v, want {7 2 1}", resp.Totals)
	}
	names := make([]string, 0, len(resp.Cases))
	for _, c := range resp.Cases {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "bar,baz,qux" {
		t.Fatalf("case order = %v, want the reader's own order", names)
	}
	first := resp.Cases[0]
	if first.Suite != "unit" || first.ClassName != "com.example.FooTest" || first.Status != "FAILED" ||
		first.DurationMs != 1250 || first.ErrorDetails != "boom" || first.CaseKey != TestCaseKey("com.example.FooTest", "bar") {
		t.Fatalf("first case = %+v", first)
	}
	want := runTestReportJSON{Path: ".varroa/tests.json", SizeBytes: 2048, CaseCount: 7, Downloadable: true}
	if resp.Report == nil || *resp.Report != want {
		t.Fatalf("report = %+v, want %+v", resp.Report, want)
	}
}

func TestHandleListRunTests_OmitsTotalsWithoutCounters(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	srv := newRunFixtureServer(reader, nil)
	rec := doRunRequest(t, srv.handleListRunTests, "/runs/r1/tests", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"totals"`) {
		t.Fatalf("body = %s, want no totals for a run with no counters", rec.Body.String())
	}
}

func TestHandleListRunTests_LocalReportIsNotDownloadable(t *testing.T) {
	reader := newTestCaseReader(nil)
	reader.runs["r1"] = func(run recordstore.Run) recordstore.Run {
		run.Payload = json.RawMessage(`{"test_report":{"size_bytes":10,"case_count":0}}`)
		return run
	}(reader.runs["r1"])
	srv := newRunFixtureServer(reader, nil)
	rec := doRunRequest(t, srv.handleListRunTests, "/runs/r1/tests", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp runTestListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := runTestReportJSON{Path: testReportPath, SizeBytes: 10, CaseCount: 0, Downloadable: false}
	if resp.Report == nil || *resp.Report != want {
		t.Fatalf("report = %+v, want %+v", resp.Report, want)
	}
	if !strings.Contains(rec.Body.String(), `"caseCount":0`) || !strings.Contains(rec.Body.String(), `"downloadable":false`) {
		t.Fatalf("body = %s, want caseCount zero and downloadable false present", rec.Body.String())
	}
}

func TestHandleListRunTests_NoCaseAndNoReport(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Payload: json.RawMessage(`{"log_summary":{"finalized":true}}`)}
	srv := newRunFixtureServer(reader, nil)
	rec := doRunRequest(t, srv.handleListRunTests, "/runs/r1/tests", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"cases":[]`) {
		t.Fatalf("body = %s, want an empty cases array", body)
	}
	if strings.Contains(body, `"report"`) {
		t.Fatalf("body = %s, want no report", body)
	}
}

func TestHandleGetRun_CarriesTestReportAndCases(t *testing.T) {
	srv := newRunFixtureServer(newTestCaseReader(strPtr("artifacts/key")), nil)
	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := runTestReportDetailJSON{BlobKey: "artifacts/key", SizeBytes: 2048, CaseCount: 7}
	if detail.TestReport == nil || *detail.TestReport != want {
		t.Fatalf("testReport = %+v, want %+v", detail.TestReport, want)
	}
	if len(detail.Cases) != 3 || detail.Cases[0].Name != "bar" || detail.Cases[2].Status != "SKIPPED" {
		t.Fatalf("cases = %+v, want the three stored cases in order", detail.Cases)
	}
	if !detail.LogFinalized {
		t.Fatal("logFinalized = false, want the report merge to leave the other payload keys readable")
	}
}

func TestHandleGetRun_OmitsTestReportAndCasesWhenNoneStored(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Payload: json.RawMessage(`{}`)}
	srv := newRunFixtureServer(reader, nil)
	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{`"testReport"`, `"cases"`} {
		if strings.Contains(rec.Body.String(), key) {
			t.Fatalf("body = %s, want no %s", rec.Body.String(), key)
		}
	}
}

func TestTestCaseKey_RoundTripsSeparators(t *testing.T) {
	className := "com.example/Foo%Test Bar"
	name := "case/with%odd name"
	key := TestCaseKey(className, name)
	if strings.Count(key, "/") != 1 {
		t.Fatalf("key = %q, want exactly one separator", key)
	}
	gotClass, gotName, err := ParseTestCaseKey(key)
	if err != nil {
		t.Fatalf("ParseTestCaseKey(%q): %v", key, err)
	}
	if gotClass != className || gotName != name {
		t.Fatalf("round trip = (%q, %q), want (%q, %q)", gotClass, gotName, className, name)
	}

	for _, bad := range []string{"no-separator", "bad%zz/name", "class/bad%zz"} {
		if _, _, err := ParseTestCaseKey(bad); err == nil {
			t.Fatalf("ParseTestCaseKey(%q) answered no error", bad)
		}
	}
}

// TestRunArtifactsHideTheInternalReportButServeItsDownload asserts the
// artifacts list omits the internal test report, and the download route
// still serves it by its path.
func TestRunArtifactsHideTheInternalReportButServeItsDownload(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED"}
	reader.artifacts["r1"] = []recordstore.Artifact{
		{RunID: "r1", Path: "dist/app.jar", SizeBytes: 4, BlobKey: "artifacts/r1/dist/app.jar"},
		{RunID: "r1", Path: recordstore.TestReportPath, SizeBytes: 2, BlobKey: "artifacts/r1/.varroa/tests.json"},
	}
	blobStore := newFakeBlobStore()
	blobStore.objects["artifacts/r1/.varroa/tests.json"] = []byte("{}")
	srv := newRunFixtureServer(reader, blobStore)

	rec := doRunRequest(t, srv.handleListRunArtifacts, "/runs/r1/artifacts", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list runArtifactListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Path != "dist/app.jar" {
		t.Fatalf("list items = %+v, want dist/app.jar alone", list.Items)
	}

	rec = doRunRequest(t, srv.handleDownloadRunArtifact, "/runs/r1/artifacts/.varroa/tests.json",
		map[string]string{"run": "r1", "path": recordstore.TestReportPath}, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "{}" {
		t.Fatalf("report download status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// TestGetRunReturnsCommandRefusal asserts the run detail carries the
// refusal notice the recorder stored in the run's payload.
func TestGetRunReturnsCommandRefusal(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "RUNNING",
		Payload: json.RawMessage(`{"command_refusal":{"command":"cancel","actor":"alice",` +
			`"http_status":404,"error":"Not Found","refused_at":"2026-09-29T12:03:00.5+00:00"}}`),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := detail.CommandRefusal
	if got == nil {
		t.Fatalf("commandRefusal is absent: %s", rec.Body.String())
	}
	want := time.Date(2026, 9, 29, 12, 3, 0, 500_000_000, time.UTC)
	if got.Command != "cancel" || got.Actor != "alice" || got.HTTPStatus != 404 ||
		got.Error != "Not Found" || !got.RefusedAt.Equal(want) {
		t.Fatalf("commandRefusal = %+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"httpStatus":404`) {
		t.Fatalf("body %s names no httpStatus", rec.Body.String())
	}
}

// TestGetRunOmitsCommandRefusalWhenAbsent asserts a run with no notice
// carries no commandRefusal field.
func TestGetRunOmitsCommandRefusalWhenAbsent(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "RUNNING",
		Payload: json.RawMessage(`{"log_summary":{"finalized":false}}`),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["commandRefusal"]; ok {
		t.Fatal("the response carries commandRefusal for a run that holds none")
	}
}

// TestGetRunCarriesChangeRequest asserts GET /runs/{run} returns the change
// request of a multibranch pull-request run.
func TestGetRunCarriesChangeRequest(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Payload: json.RawMessage(`{"run.started":{"payload":{"change_request":{"id":"12",` +
			`"title":"Fix the cache","source_branch":"fix/cache","target_branch":"main"}}}}`),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.ChangeRequest == nil || detail.ChangeRequest.ID != "12" ||
		detail.ChangeRequest.SourceBranch != "fix/cache" || detail.ChangeRequest.TargetBranch != "main" {
		t.Fatalf("changeRequest = %+v", detail.ChangeRequest)
	}
}
