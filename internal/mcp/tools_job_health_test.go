package mcp

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestGetJobHealthToolReturnsHealthAndTrend proves get_job_health answers
// the job's health bucket, its window, and its newest-first trend.
func TestGetJobHealthToolReturnsHealthAndTrend(t *testing.T) {
	reader := newFakePipelineReader()
	build := int64(7)
	reader.summaries["team-a/app-a"] = recordstore.JobSummary{
		JobPath: "team-a/app-a",
		RecentRuns: []recordstore.HealthRun{
			{RunID: "run-7", BuildNumber: &build, Result: "SUCCESS"},
			{RunID: "run-6", Result: "ABORTED"},
		},
	}
	deps := &api.Dependencies{
		RecordStore:    reader,
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}}},
		Authorizer:     pipelinesTestAuthorizer("read"),
	}

	tr := callPipelineTool(t, deps, "get_job_health", map[string]interface{}{"tenant": "team-a", "job": "team-a/app-a"})
	if tr.IsError {
		t.Fatalf("get_job_health returned error: %v", tr.Content)
	}
	var payload struct {
		JobPath string `json:"jobPath"`
		Health  struct {
			Bucket  string `json:"bucket"`
			Window  int    `json:"window"`
			Counted int    `json:"counted"`
			Passed  int    `json:"passed"`
		} `json:"health"`
		Trend []struct {
			RunID  string `json:"runId"`
			Result string `json:"result"`
		} `json:"trend"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	if payload.JobPath != "team-a/app-a" || payload.Health.Bucket != "cloudy" || payload.Health.Window != recordstore.JobHealthWindow {
		t.Fatalf("health = %+v (job %q), want cloudy over the window", payload.Health, payload.JobPath)
	}
	if payload.Health.Counted != 2 || payload.Health.Passed != 1 {
		t.Fatalf("counted/passed = %d/%d, want 2/1", payload.Health.Counted, payload.Health.Passed)
	}
	if len(payload.Trend) != 2 || payload.Trend[0].RunID != "run-7" || payload.Trend[1].Result != "ABORTED" {
		t.Fatalf("trend = %+v, want two points newest first", payload.Trend)
	}
}

// TestGetJobHealthToolRequiresTenantAndJob proves the tool rejects a call
// that names no job.
func TestGetJobHealthToolRequiresTenantAndJob(t *testing.T) {
	deps := &api.Dependencies{
		RecordStore:    newFakePipelineReader(),
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}}},
		Authorizer:     pipelinesTestAuthorizer("read"),
	}

	tr := callPipelineTool(t, deps, "get_job_health", map[string]interface{}{"tenant": "team-a"})
	if !tr.IsError {
		t.Fatal("expected get_job_health to reject a call without a job")
	}
}

// TestGetJobHealthToolRefusesWithoutJobRead proves the tool denies a
// caller whose grant reads no pipeline job.
func TestGetJobHealthToolRefusesWithoutJobRead(t *testing.T) {
	deps := &api.Dependencies{
		RecordStore:    newFakePipelineReader(),
		TenantResolver: &fakePipelineTenantResolver{refs: []api.TenantRef{{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}}},
		Authorizer:     pipelinesTestAuthorizer("write"),
	}

	tr := callPipelineTool(t, deps, "get_job_health", map[string]interface{}{"tenant": "team-a", "job": "team-a/app-a"})
	if !tr.IsError {
		t.Fatal("expected get_job_health to deny a caller without the job read grant")
	}
	if got := toolText(tr); got != "job not found" {
		t.Errorf("error text = %q, want %q", got, "job not found")
	}
}
