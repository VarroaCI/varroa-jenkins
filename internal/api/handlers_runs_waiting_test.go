package api

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestGetRun_CarriesWaiting asserts that a queued run held by a tenant cap
// carries its waiting reason in the run detail.
func TestGetRun_CarriesWaiting(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "QUEUED",
	}
	since := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	reader.runWaits = map[string]recordstore.RunWait{
		"r1": {RunID: "r1", Kind: "tenant", Limit: "deploys", Max: 1, Running: 1, Since: since},
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Waiting == nil {
		t.Fatalf("waiting is absent, want the deploys wait; body = %s", rec.Body.String())
	}
	if detail.Waiting.Kind != "tenant" || detail.Waiting.Limit != "deploys" ||
		detail.Waiting.Max != 1 || detail.Waiting.Running != 1 {
		t.Fatalf("waiting = %+v, want the deploys tenant wait", detail.Waiting)
	}
}

// TestGetPipelineJob_ListsWaiting asserts that the job detail lists the
// queued runs of the job that a cap holds.
func TestGetPipelineJob_ListsWaiting(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
	}
	reader := newFakeRecordReader()
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{JobPath: "acme/app-a"}}
	since := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	reader.jobWaits = map[string][]recordstore.RunWait{
		"acme/app-a": {{RunID: "r9", Kind: "cell", Max: 2, Running: 2, Cell: "cell-a", Since: since}},
	}
	srv := pipelinesTestServer(client, pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a"), reader, nil)

	rec := doPipelinesRequest(t, srv.handleGetPipelineJob, "/pipelines/ctrl-a/jobs/"+url.PathEscape("acme/app-a"),
		map[string]string{"team": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail pipelineJobDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(detail.Waiting) != 1 {
		t.Fatalf("waiting = %+v, want one entry", detail.Waiting)
	}
	got := detail.Waiting[0]
	if got.RunID != "r9" || got.Kind != "cell" || got.Cell != "cell-a" || got.Max != 2 {
		t.Fatalf("waiting[0] = %+v, want the r9 cell wait", got)
	}
}
