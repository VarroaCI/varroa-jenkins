package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// orgFolderItemsYAML declares an organization folder at the root and a
// pipeline in a plain folder. The listing tests read both.
const orgFolderItemsYAML = `
items:
  - kind: organizationFolder
    name: repos
    navigators:
      - giteaNavigator:
          serverUrl: https://git.example.org
          repoOwner: acme
  - kind: folder
    name: svc
    items:
      - kind: pipeline
        name: main
        definition:
          script: "pipeline {}"
`

// newItemsFixture binds the controller ctrl-a to a composed bundle whose
// items.yaml holds itemsYAML.
func newItemsFixture(itemsYAML string) *fakeResourceClient {
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
		"bundle-a-content": {"items.yaml": itemsYAML},
	}
	return client
}

// newItemsServer serves the tenant ctrl-a with a read grant and itemsYAML.
func newItemsServer(reader *fakeRecordReader, itemsYAML string) *Server {
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	return pipelinesTestServer(newItemsFixture(itemsYAML), authz, reader, nil)
}

// listedRow returns the listing row of jobPath, and whether one exists.
func listedRow(resp pipelineListResponse, jobPath string) (pipelineSummaryJSON, bool) {
	for _, it := range resp.Items {
		if it.JobPath == jobPath {
			return it, true
		}
	}
	return pipelineSummaryJSON{}, false
}

// runAt builds a run of jobPath created at when.
func runAt(jobPath string, when time.Time) *recordstore.Run {
	return &recordstore.Run{JobPath: jobPath, RunID: "run-" + jobPath, Status: "COMPLETED", CreatedAt: when}
}

func TestListPipelinesOrganizationFolderRow(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, []string{"repos", "repos/projgen"}, true, true),
	}
	resp := listPipelinesFor(t, newItemsServer(reader, orgFolderItemsYAML), "ctrl-a")

	row, ok := listedRow(resp, "repos")
	if !ok {
		t.Fatalf("organization folder repos absent from the listing: %+v", resp.Items)
	}
	if row.Kind != "organizationFolder" {
		t.Fatalf("repos kind = %q, want organizationFolder", row.Kind)
	}
	child, ok := listedRow(resp, "repos/projgen")
	if !ok || child.Kind != "multibranch" {
		t.Fatalf("repos/projgen = %+v (present %v), want a multibranch row", child, ok)
	}
}

func TestListPipelinesDiscoveredMultibranchFromInventory(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, []string{"acme/new-mb"}, true, true),
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")

	row, ok := listedRow(resp, "acme/new-mb")
	if !ok {
		t.Fatalf("acme/new-mb absent although the inventory reports it: %+v", resp.Items)
	}
	if row.Kind != "multibranch" || row.Health != "unknown" {
		t.Fatalf("acme/new-mb = kind %q health %q, want multibranch unknown", row.Kind, row.Health)
	}
}

func TestListPipelinesBranchKind(t *testing.T) {
	now := time.Now().UTC()
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory([]string{"acme/checkout-api/main"}, []string{"acme/repo-x"}, true, true),
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/checkout-api/main", LastRun: runAt("acme/checkout-api/main", now)},
		{JobPath: "acme/repo-x/main", LastRun: runAt("acme/repo-x/main", now)},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")

	for _, path := range []string{"acme/checkout-api/main", "acme/repo-x/main"} {
		row, ok := listedRow(resp, path)
		if !ok {
			t.Fatalf("%s absent from the listing: %+v", path, resp.Items)
		}
		if row.Kind != "branch" {
			t.Fatalf("%s kind = %q, want branch", path, row.Kind)
		}
	}
}

func TestListPipelinesDropsDeletedJob(t *testing.T) {
	now := time.Now().UTC()
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": {
			JobsKnown:      true,
			ProjectsKnown:  true,
			Jobs:           map[string]struct{}{"acme/app-a": {}},
			Projects:       map[string]struct{}{},
			JobsObservedAt: now.Add(-time.Hour),
		},
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/gone-job", LastRun: runAt("acme/gone-job", now.Add(-48*time.Hour))},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")

	if _, ok := listedRow(resp, "acme/gone-job"); ok {
		t.Fatalf("a deleted job still lists: %+v", resp.Items)
	}
}

func TestListPipelinesKeepsJobRunAfterInventory(t *testing.T) {
	now := time.Now().UTC()
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": {
			JobsKnown:      true,
			ProjectsKnown:  true,
			Jobs:           map[string]struct{}{"acme/app-a": {}},
			Projects:       map[string]struct{}{},
			JobsObservedAt: now.Add(-time.Hour),
		},
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/new-job", LastRun: runAt("acme/new-job", now)},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")

	if _, ok := listedRow(resp, "acme/new-job"); !ok {
		t.Fatalf("a job that ran after the inventory was dropped: %+v", resp.Items)
	}
}

func TestListPipelinesKeepsRowWhenInventoryUnknown(t *testing.T) {
	now := time.Now().UTC()
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": {
			JobsKnown:      false,
			Jobs:           map[string]struct{}{},
			Projects:       map[string]struct{}{},
			JobsObservedAt: now.Add(-time.Hour),
		},
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/old-job", LastRun: runAt("acme/old-job", now.Add(-48*time.Hour))},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")

	if _, ok := listedRow(resp, "acme/old-job"); !ok {
		t.Fatalf("a row dropped on an incomplete inventory: %+v", resp.Items)
	}
}

func TestDeclaredJobPendingOrganizationFolder(t *testing.T) {
	inv := pendingInventory(nil, []string{"repos"}, true, true)
	if declaredJobPending(inv, "repos", "organizationFolder") {
		t.Fatal("organizationFolder held in Projects is pending")
	}
	if !declaredJobPending(inv, "other", "organizationFolder") {
		t.Fatal("organizationFolder absent from a known Projects set is not pending")
	}
}

// scanServerWithItems serves the tenant ci, whose composed bundle declares
// itemsYAML, with the given reader and transport.
func scanServerWithItems(reader recordstore.Reader, mt transport.Transport, itemsYAML string) *Server {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ci": {
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "test-ns"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-ci"}},
		},
	}
	client.composedBundles = map[string]*v1alpha1.ComposedBundle{
		"bundle-ci": {
			ObjectMeta: metav1.ObjectMeta{Name: "bundle-ci", Namespace: "test-ns"},
			Status:     v1alpha1.ComposedBundleStatus{ContentRef: "bundle-ci-content"},
		},
	}
	client.configMaps = map[string]map[string]string{
		"bundle-ci-content": {"items.yaml": itemsYAML},
	}
	authz := pipelineGrantAuthorizer([]string{"read", "write"}, "test-ns/ci")
	srv := pipelinesTestServer(client, authz, reader, nil)
	srv.deps.MiteRegistry = mt
	return srv
}

// postScanOf posts a scan of job on the tenant ci.
func postScanOf(t *testing.T, srv *Server, job string) *httptest.ResponseRecorder {
	t.Helper()
	req := doWriteRequest("/pipelines/ci/jobs/"+strings.ReplaceAll(job, "/", "%2F")+"/scan",
		map[string]string{"tenant": "ci", "job": job}, testClaims(), scanMultibranchRequest{})
	rec := httptest.NewRecorder()
	srv.handleScanMultibranch(rec, req)
	return rec
}

// scanItemsYAML declares the pipeline svc/main on the tenant ci.
const scanItemsYAML = `
items:
  - kind: folder
    name: svc
    items:
      - kind: pipeline
        name: main
        definition:
          script: "pipeline {}"
  - kind: organizationFolder
    name: repos
    navigators:
      - giteaNavigator:
          serverUrl: https://git.example.org
          repoOwner: acme
`

func TestScanRefusesDeclaredPipeline409(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := scanServerWithItems(newFakeRecordReader(), mt, scanItemsYAML)

	rec := postScanOf(t, srv, "svc/main")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands for a pipeline, want 0", len(mt.calls()))
	}
}

func TestScanAcceptsDeclaredOrganizationFolder(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := scanServerWithItems(newFakeRecordReader(), mt, scanItemsYAML)

	rec := postScanOf(t, srv, "repos")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 1 {
		t.Fatalf("published %d commands, want 1", len(mt.calls()))
	}
}

func TestScanAcceptsInventoryProject(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ci": pendingInventory(nil, []string{"found/mb"}, true, true),
	}
	mt := &fakeTriggerTransport{}
	srv := scanServerWithItems(reader, mt, "items: []\n")

	rec := postScanOf(t, srv, "found/mb")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
}

func TestScanRefusesUnknownPathWithKnownInventory(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ci": pendingInventory(nil, []string{"other/mb"}, true, true),
	}
	mt := &fakeTriggerTransport{}
	srv := scanServerWithItems(reader, mt, "items: []\n")

	rec := postScanOf(t, srv, "missing/mb")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands for an unknown path, want 0", len(mt.calls()))
	}
}

func TestScanPassesWhenInventoryUnknown(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ci": pendingInventory(nil, nil, true, false),
	}
	mt := &fakeTriggerTransport{}
	srv := scanServerWithItems(reader, mt, "items: []\n")

	rec := postScanOf(t, srv, "unknown/mb")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202 when the inventory is unknown", rec.Code, rec.Body.String())
	}
}

func TestJobBranchesRepositoryHeads(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))
	authz := NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{
		branchTestBinding("org", "org/*"),
	}), false)
	reader := newFakeRecordReader()
	reader.jobBranches = map[string]recordstore.JobBranchProject{
		branchTestKey("core", "payments", "payments/org"): {
			JobPath:    "payments/org",
			ObservedAt: time.Now().UTC(),
			Heads: []recordstore.JobBranchHead{
				{Name: "repo-b", JobPath: "payments/org/repo-b", Kind: "repository"},
				{Name: "repo-a", JobPath: "payments/org/repo-a", Kind: "repository"},
			},
		},
	}
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListJobBranches, "/pipelines/payments/jobs/payments%2Forg/branches",
		map[string]string{"tenant": "payments", "job": "payments/org"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list JobBranchList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode branch list: %v", err)
	}
	if len(list.Branches) != 0 || len(list.Repositories) != 2 {
		t.Fatalf("branches = %d, repositories = %d, want 0 and 2", len(list.Branches), len(list.Repositories))
	}
	if list.Repositories[0].Name != "repo-a" || list.Repositories[1].Name != "repo-b" {
		t.Fatalf("repositories not sorted by name: %+v", list.Repositories)
	}
}

func TestListPipelinesUndeclaredOrganizationFolderChildIsNotBranch(t *testing.T) {
	now := time.Now().UTC()
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(
			[]string{"acme/org-x/repo-a/main"},
			[]string{"acme/org-x", "acme/org-x/repo-a"}, true, true),
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{
		{JobPath: "acme/org-x/repo-a/main", LastRun: runAt("acme/org-x/repo-a/main", now)},
	}
	resp := listPipelinesFor(t, newPendingServer(reader), "ctrl-a")

	repo, ok := listedRow(resp, "acme/org-x/repo-a")
	if !ok || repo.Kind != "multibranch" {
		t.Fatalf("repository row = %+v (present %v), want kind multibranch", repo, ok)
	}
	branch, ok := listedRow(resp, "acme/org-x/repo-a/main")
	if !ok || branch.Kind != "branch" {
		t.Fatalf("branch row = %+v (present %v), want kind branch", branch, ok)
	}
}
