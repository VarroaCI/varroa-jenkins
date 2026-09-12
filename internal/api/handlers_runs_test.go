package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestHandleGetRun_TenantFolderForPlacedTeam asserts a run stored under
// a placed Team's tenant (no "/", per rbac.ParseTenant) carries that
// Team name as tenantFolder.
func TestHandleGetRun_TenantFolderForPlacedTeam(t *testing.T) {
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
		t.Fatalf("tenant = %q, want %q for a Team tenant", detail.Tenant, "payments")
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
		{"listPipelines-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListPipelines }, "/pipelines/ctrl-a", map[string]string{"team": "ctrl-a"}, http.StatusServiceUnavailable},
		{"getPipelineJob-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetPipelineJob }, "/pipelines/ctrl-a/jobs/j", map[string]string{"team": "ctrl-a", "job": "j"}, http.StatusServiceUnavailable},
		{"listRuns-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRuns }, "/pipelines/ctrl-a/jobs/j/runs", map[string]string{"team": "ctrl-a", "job": "j"}, http.StatusServiceUnavailable},
		{"getRun-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRun }, "/runs/r1", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listRunArtifacts-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRunArtifacts }, "/runs/r1/artifacts", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listRunTests-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRunTests }, "/runs/r1/tests", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"listRunChanges-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListRunChanges }, "/runs/r1/changes", map[string]string{"run": "r1"}, http.StatusServiceUnavailable},
		{"downloadRunArtifact-nilReader", nil, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleDownloadRunArtifact }, "/runs/r1/artifacts/a.txt", map[string]string{"run": "r1", "path": "a.txt"}, http.StatusServiceUnavailable},

		// A plain run read needs no blob store.
		{"getRun-nilBlobStoreStillOK", newFakeRecordReaderWithRun(), nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleGetRun }, "/runs/r1", map[string]string{"run": "r1"}, http.StatusOK},
		// The artifact download needs one.
		{"downloadRunArtifact-nilBlobStore", newFakeRecordReaderWithRun(), nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleDownloadRunArtifact }, "/runs/r1/artifacts/a.txt", map[string]string{"run": "r1", "path": "a.txt"}, http.StatusServiceUnavailable},

		{"listPipelines-schemaTooOld", schemaTooOldReader, nil, func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleListPipelines }, "/pipelines/ctrl-a", map[string]string{"team": "ctrl-a"}, http.StatusServiceUnavailable},
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
