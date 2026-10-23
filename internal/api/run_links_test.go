package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// linkedRun returns a run in the readable tenant test-ns/ctrl-a.
func linkedRun(id string, status, result string, buildNumber int64) recordstore.Run {
	n := buildNumber
	run := recordstore.Run{
		RunID: id, Tenant: "test-ns/ctrl-a", JobPath: "test-ns/ctrl-a/svc/main",
		Status: status, BuildNumber: &n, CreatedAt: time.Now().UTC(),
	}
	if result != "" {
		r := result
		run.Result = &r
	}
	return run
}

// TestGetRunCarriesReadableUpstreamLink asserts a run whose upstream run the
// reader may read carries the full upstream link.
func TestGetRunCarriesReadableUpstreamLink(t *testing.T) {
	reader := newFakeRecordReader()
	upstream := uuid.Must(uuid.NewV7())
	tenant, jobPath := "test-ns/ctrl-a", "test-ns/ctrl-a/svc/main"
	number := int64(7)
	child := linkedRun("r1", "RUNNING", "", 1)
	child.UpstreamRunID = &upstream
	child.UpstreamTenant = &tenant
	child.UpstreamJob = &jobPath
	child.UpstreamNumber = &number
	reader.runs["r1"] = child
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := RunLink{Tenant: tenant, JobPath: jobPath, RunID: upstream.String(), BuildNumber: 7}
	if detail.Upstream == nil || *detail.Upstream != want {
		t.Fatalf("upstream = %+v, want %+v", detail.Upstream, want)
	}
}

// TestGetRunRedactsUnreadableUpstreamToTenant asserts an upstream run the
// reader may not read keeps its tenant alone.
func TestGetRunRedactsUnreadableUpstreamToTenant(t *testing.T) {
	reader := newFakeRecordReader()
	upstream := uuid.Must(uuid.NewV7())
	tenant, jobPath := "other-ns/ctrl-b", "other-ns/ctrl-b/svc/main"
	number := int64(3)
	child := linkedRun("r1", "RUNNING", "", 1)
	child.UpstreamRunID = &upstream
	child.UpstreamTenant = &tenant
	child.UpstreamJob = &jobPath
	child.UpstreamNumber = &number
	reader.runs["r1"] = child
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Upstream == nil || *detail.Upstream != (RunLink{Tenant: tenant}) {
		t.Fatalf("upstream = %+v, want the tenant alone", detail.Upstream)
	}
	if strings.Contains(rec.Body.String(), upstream.String()) {
		t.Fatalf("body leaks the unreadable upstream run id: %s", rec.Body.String())
	}
}

// TestGetRunDownstreamFirstPageRedactsPerItem asserts each downstream item
// is redacted on its own: a readable run keeps its status, an unreadable one
// keeps its tenant alone.
func TestGetRunDownstreamFirstPageRedactsPerItem(t *testing.T) {
	reader := newFakeRecordReader()
	upstream := uuid.Must(uuid.NewV7())
	reader.runs["r1"] = linkedRun("r1", "COMPLETED", "SUCCESS", 1)
	readable := linkedRun("c1", "COMPLETED", "FAILURE", 5)
	hidden := recordstore.Run{RunID: "c2", Tenant: "other-ns/ctrl-b", JobPath: "other-ns/ctrl-b/svc/main", Status: "RUNNING", CreatedAt: time.Now().UTC()}
	reader.downstream[upstream.String()] = []recordstore.Run{readable, hidden}
	reader.runs[upstream.String()] = linkedRun(upstream.String(), "COMPLETED", "SUCCESS", 1)
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/"+upstream.String(), map[string]string{"run": upstream.String()}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Downstream == nil || len(detail.Downstream.Items) != 2 {
		t.Fatalf("downstream = %+v, want two items", detail.Downstream)
	}
	items := detail.Downstream.Items
	if items[0].RunID != "c1" || items[0].Status != "COMPLETED" || items[0].Result != "FAILURE" || items[0].BuildNumber != 5 {
		t.Fatalf("readable item = %+v, want the full link", items[0])
	}
	if items[1] != (RunLink{Tenant: "other-ns/ctrl-b"}) {
		t.Fatalf("unreadable item = %+v, want the tenant alone", items[1])
	}
}

// TestGetRunOmitsLinksWhenAbsent asserts a run with no upstream and no
// downstream runs carries neither key.
func TestGetRunOmitsLinksWhenAbsent(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = linkedRun("r1", "COMPLETED", "SUCCESS", 1)
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"upstream"`) || strings.Contains(rec.Body.String(), `"downstream"`) {
		t.Fatalf("body = %s, want no upstream or downstream key", rec.Body.String())
	}
}

// TestListRunDownstream_PagesAndRedacts asserts the downstream route pages
// the runs with a cursor and redacts each item.
func TestListRunDownstream_PagesAndRedacts(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = linkedRun("r1", "COMPLETED", "SUCCESS", 1)
	reader.downstream["r1"] = []recordstore.Run{
		linkedRun("c1", "RUNNING", "", 3),
		{RunID: "c2", Tenant: "other-ns/ctrl-b", JobPath: "other-ns/ctrl-b/svc/main", Status: "RUNNING"},
		linkedRun("c3", "COMPLETED", "SUCCESS", 1),
	}
	srv := newRunFixtureServer(reader, nil)

	first := doRunRequest(t, srv.handleListRunDownstream, "/runs/r1/downstream?limit=2", map[string]string{"run": "r1"}, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	var page RunLinkPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if len(page.Items) != 2 || page.Items[1] != (RunLink{Tenant: "other-ns/ctrl-b"}) || page.NextCursor == "" {
		t.Fatalf("first page = %+v, want two items, the second redacted, and a cursor", page)
	}

	second := doRunRequest(t, srv.handleListRunDownstream, "/runs/r1/downstream?limit=2&cursor="+page.NextCursor,
		map[string]string{"run": "r1"}, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	var rest RunLinkPage
	if err := json.Unmarshal(second.Body.Bytes(), &rest); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(rest.Items) != 1 || rest.Items[0].RunID != "c3" || rest.NextCursor != "" {
		t.Fatalf("second page = %+v, want c3 and no cursor", rest)
	}
}

// TestListRunDownstream_UnreadableRunIs404 asserts a run the caller may not
// read answers 404 on the downstream route too.
func TestListRunDownstream_UnreadableRunIs404(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "other-ns/ctrl-b", JobPath: "svc/main", Status: "COMPLETED"}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleListRunDownstream, "/runs/r1/downstream", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestListRunDownstream_BadLimitIs400 asserts an out-of-range limit and a
// malformed cursor both answer 400.
func TestListRunDownstream_BadLimitIs400(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = linkedRun("r1", "COMPLETED", "SUCCESS", 1)
	srv := newRunFixtureServer(reader, nil)

	for _, path := range []string{"/runs/r1/downstream?limit=0", "/runs/r1/downstream?limit=201", "/runs/r1/downstream?cursor=%25%25"} {
		rec := doRunRequest(t, srv.handleListRunDownstream, path, map[string]string{"run": "r1"}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

// TestListRunDownstream_NilStoreIs503 asserts the route answers 503 when no
// record store is configured.
func TestListRunDownstream_NilStoreIs503(t *testing.T) {
	srv := newRunFixtureServer(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/runs/r1/downstream", nil)
	req.SetPathValue("run", "r1")
	rec := httptest.NewRecorder()
	srv.handleListRunDownstream(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestHandleGetPipelineJob_DependencyLastRunRedacted asserts an edge's last
// run passes the run read check: a run in a tenant the reader may not read
// keeps the tenant alone.
func TestHandleGetPipelineJob_DependencyLastRunRedacted(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	seedTenant(t, client.crdStore, placedTenantObject("payments", "core", "test-ns", "ctrl-a"))

	authz := resolverAuthorizer("payments")
	reader := newFakeRecordReader()
	reader.setPaymentsReleaseDependencies(recordstore.JobDependencies{
		Upstream: []recordstore.DependencyEdge{{
			Tenant: "alpha", Job: "build", JobPath: "alpha/build", Result: "SUCCESS", Granted: true,
			LastRun: &recordstore.RunLinkRow{RunID: "r9", Tenant: "alpha", JobPath: "alpha/build", BuildNumber: int64Ptr(4), Status: "COMPLETED", Result: "SUCCESS"},
		}},
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
	if len(detail.Dependencies.Upstream) != 1 || detail.Dependencies.Upstream[0].LastRun == nil {
		t.Fatalf("upstream = %+v, want one edge with a last run", detail.Dependencies.Upstream)
	}
	if got := *detail.Dependencies.Upstream[0].LastRun; got != (RunLink{Tenant: "alpha"}) {
		t.Fatalf("lastRun = %+v, want the tenant alone for an unreadable run", got)
	}
}

func int64Ptr(v int64) *int64 { return &v }
