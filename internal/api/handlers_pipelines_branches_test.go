package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// branchTestKey keys fakeRecordReader.jobBranches.
func branchTestKey(cluster, tenant, jobPath string) string {
	return cluster + "/" + tenant + "/" + jobPath
}

// GetJobBranches answers the seeded inventory row of one job, or false.
func (f *fakeRecordReader) GetJobBranches(_ context.Context, cluster, tenant, jobPath string) (recordstore.JobBranchProject, bool, error) {
	project, ok := f.jobBranches[branchTestKey(cluster, tenant, jobPath)]
	return project, ok, nil
}

// LatestRunsForJobs answers the newest seeded run of each listed job path.
func (f *fakeRecordReader) LatestRunsForJobs(_ context.Context, cluster, tenant string, jobPaths []string) (map[string]recordstore.Run, error) {
	want := map[string]bool{}
	for _, p := range jobPaths {
		want[p] = true
	}
	out := map[string]recordstore.Run{}
	for _, run := range f.runsByTenant[cluster+"/"+tenant] {
		if !want[run.JobPath] {
			continue
		}
		if prev, ok := out[run.JobPath]; !ok || run.CreatedAt.After(prev.CreatedAt) {
			out[run.JobPath] = run
		}
	}
	return out, nil
}

// branchTestBinding grants pipelines read on the "payments" tenant, narrowed
// to the job globs. An empty globs list leaves the grant unnarrowed.
func branchTestBinding(globs ...string) *v1alpha1.VarroaRoleBinding {
	b := pipelineBinding("b", "payments")
	if len(globs) > 0 {
		b.Spec.Scope.Tenants[0].Jobs = globs
	}
	return b
}

// seedBranchInventory stores a multibranch job with a branch, a change
// request, and a head under another job path.
func seedBranchInventory(reader *fakeRecordReader, now time.Time) {
	reader.jobBranches = map[string]recordstore.JobBranchProject{
		branchTestKey("core", "payments", "payments/mb"): {
			JobPath:    "payments/mb",
			ObservedAt: now,
			Heads: []recordstore.JobBranchHead{
				{Name: "main", JobPath: "payments/mb/main", Kind: "branch", Revision: "abc"},
				{Name: "PR-9", JobPath: "payments/mb/PR-9", Kind: "changeRequest",
					ChangeRequest: &recordstore.ChangeRequestBranch{ID: "9", Title: "Newer"}},
				{Name: "PR-10", JobPath: "payments/mb/PR-10", Kind: "changeRequest", Orphaned: true,
					ChangeRequest: &recordstore.ChangeRequestBranch{ID: "10", Title: "Newest"}},
				{Name: "other", JobPath: "payments/other/x", Kind: "branch"},
			},
		},
	}
}

// strPtrBranch answers a pointer to s, for optional run fields.
func strPtrBranch(s string) *string { return &s }

// decodeBranchList decodes a branch-list response body.
func decodeBranchList(t *testing.T, body []byte) JobBranchList {
	t.Helper()
	var list JobBranchList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode branch list: %v", err)
	}
	return list
}

// TestListJobBranchesFiltersByJobGlobs asserts a head the caller may not
// read never reaches the response. The grant admits the parent job and
// the heads under mb/, and the other head is dropped.
func TestListJobBranchesFiltersByJobGlobs(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{
		branchTestBinding("mb", "mb/*"),
	}), false)
	reader := newFakeRecordReader()
	seedBranchInventory(reader, time.Now().UTC())
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListJobBranches, "/pipelines/payments/jobs/payments%2Fmb/branches",
		map[string]string{"tenant": "payments", "job": "payments/mb"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	list := decodeBranchList(t, rec.Body.Bytes())
	if len(list.Branches) != 1 || list.Branches[0].Name != "main" {
		t.Fatalf("branches = %+v, want only main", list.Branches)
	}
	if len(list.ChangeRequests) != 2 {
		t.Fatalf("changeRequests = %d, want 2", len(list.ChangeRequests))
	}
}

// TestListJobBranchesShowsADeletedOrphanedHead asserts a head that the last
// scan deleted is named when the caller reads its job, and hidden when the
// caller does not read it. The head is no longer in the inventory.
func TestListJobBranchesShowsADeletedOrphanedHead(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{
		branchTestBinding("mb", "mb/*"),
	}), false)
	reader := newFakeRecordReader()
	seedBranchInventory(reader, time.Now().UTC())
	project := reader.jobBranches[branchTestKey("core", "payments", "payments/mb")]
	project.LastScan = &recordstore.BranchScan{
		ScanID: "scan-1",
		State:  "completed",
		OrphanedHeadRefs: []recordstore.BranchScanHead{
			{Name: "gone", JobPath: "payments/mb/gone"},
			{Name: "secret", JobPath: "payments/other/secret"},
		},
	}
	reader.jobBranches[branchTestKey("core", "payments", "payments/mb")] = project
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListJobBranches, "/pipelines/payments/jobs/payments%2Fmb/branches",
		map[string]string{"tenant": "payments", "job": "payments/mb"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		LastScan *struct {
			OrphanedHeads []string `json:"orphanedHeads"`
		} `json:"lastScan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode branch list: %v", err)
	}
	if body.LastScan == nil {
		t.Fatalf("lastScan missing from %s", rec.Body.String())
	}
	if len(body.LastScan.OrphanedHeads) != 1 || body.LastScan.OrphanedHeads[0] != "gone" {
		t.Fatalf("orphanedHeads = %v, want [gone]", body.LastScan.OrphanedHeads)
	}
}

// TestListJobBranchesNoInventoryAnswers200Empty asserts a job with no
// inventory row answers an empty list with no observedAt, not a 404.
func TestListJobBranchesNoInventoryAnswers200Empty(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))
	authz := resolverAuthorizer("payments")
	srv := pipelinesTestServer(client, authz, newFakeRecordReader(), nil)

	rec := doPipelinesRequest(t, srv.handleListJobBranches, "/pipelines/payments/jobs/payments%2Fmb/branches",
		map[string]string{"tenant": "payments", "job": "payments/mb"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	list := decodeBranchList(t, rec.Body.Bytes())
	if list.ObservedAt != nil || len(list.Branches) != 0 || len(list.ChangeRequests) != 0 {
		t.Fatalf("list = %+v, want an empty inventory with no observedAt", list)
	}
}

// TestListJobBranchesAttachesLatestRun asserts each head carries its own
// newest run, and that change requests sort by numeric id, newest first.
func TestListJobBranchesAttachesLatestRun(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))
	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	seedBranchInventory(reader, time.Now().UTC())
	reader.runsByTenant = map[string][]recordstore.Run{
		"core/payments": {{
			RunID: uuid.New().String(), JobPath: "payments/mb/main", Status: "COMPLETED", Result: strPtrBranch("SUCCESS"),
			CreatedAt: time.Now().UTC(),
		}},
	}
	srv := pipelinesTestServer(client, authz, reader, nil)

	rec := doPipelinesRequest(t, srv.handleListJobBranches, "/pipelines/payments/jobs/payments%2Fmb/branches",
		map[string]string{"tenant": "payments", "job": "payments/mb"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	list := decodeBranchList(t, rec.Body.Bytes())
	if list.Branches[0].LastRun == nil {
		t.Fatalf("main has no lastRun, want its seeded run")
	}
	if len(list.ChangeRequests) != 2 || list.ChangeRequests[0].ChangeRequest.ID != "10" {
		t.Fatalf("changeRequests = %+v, want id 10 first", list.ChangeRequests)
	}
	if !list.ChangeRequests[0].Orphaned {
		t.Fatalf("PR-10 orphaned = false, want true")
	}
}

// TestListJobBranchesNilReader503 asserts the route answers 503 when the
// install has no record store.
func TestListJobBranchesNilReader503(t *testing.T) {
	client := newFakeResourceClient()
	srv := pipelinesTestServer(client, resolverAuthorizer("payments"), nil, nil)

	rec := doPipelinesRequest(t, srv.handleListJobBranches, "/pipelines/payments/jobs/payments%2Fmb/branches",
		map[string]string{"tenant": "payments", "job": "payments/mb"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestGetPipelineJobKind covers each source of the job kind, in the order
// the job page relies on: a declared item, then the parent, then the
// inventory, then a run.
func TestGetPipelineJobKind(t *testing.T) {
	cases := []struct {
		name           string
		declared       string
		parentMultiple bool
		hasBranches    bool
		hasRun         bool
		want           string
	}{
		{"declared pipeline wins", "pipeline", true, true, true, "pipeline"},
		{"declared multibranch wins", "multibranch", false, false, true, "multibranch"},
		{"declared organization folder wins", "organizationFolder", true, true, true, "organizationFolder"},
		{"a child of a multibranch parent is a branch", "", true, true, true, "branch"},
		{"inventory marks multibranch", "", false, true, true, "multibranch"},
		{"a run marks pipeline", "", false, false, true, "pipeline"},
		{"nothing marks unknown", "", false, false, false, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pipelineJobKind(tc.declared, tc.parentMultiple, tc.hasBranches, tc.hasRun); got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}
