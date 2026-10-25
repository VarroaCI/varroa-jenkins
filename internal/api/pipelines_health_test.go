package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// healthRuns builds a window of finished runs from result strings, newest
// first. Each run gets a RunID so trend points can be told apart.
func healthRuns(results ...string) []recordstore.HealthRun {
	out := make([]recordstore.HealthRun, 0, len(results))
	for i, r := range results {
		out = append(out, recordstore.HealthRun{RunID: string(rune('a' + i)), Result: r})
	}
	return out
}

// TestComputeJobHealth asserts the health rule over the health window:
// SUCCESS passes; FAILURE, UNSTABLE, and ABORTED fail; INTERRUPTED and
// NOT_BUILT are not counted; and the score maps to a weather bucket.
func TestComputeJobHealth(t *testing.T) {
	cases := []struct {
		name       string
		runs       []recordstore.HealthRun
		bucket     string
		score      *int
		counted    int
		passed     int
		wantWindow int
	}{
		{name: "no runs is unknown", runs: nil, bucket: "unknown", counted: 0, passed: 0},
		{
			name: "all interrupted is unknown", bucket: "unknown",
			runs: healthRuns("INTERRUPTED", "INTERRUPTED", "NOT_BUILT"),
		},
		{
			name: "ten of ten passed is sunny", bucket: "sunny", score: intPtr(100), counted: 10, passed: 10,
			runs: healthRuns("SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS"),
		},
		{
			name: "nine of ten passed is sunny", bucket: "sunny", score: intPtr(90), counted: 10, passed: 9,
			runs: healthRuns("SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "FAILURE"),
		},
		{
			name: "eight of ten passed is partly cloudy", bucket: "partlyCloudy", score: intPtr(80), counted: 10, passed: 8,
			runs: healthRuns("SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "FAILURE", "FAILURE"),
		},
		{
			name: "five of ten passed is cloudy", bucket: "cloudy", score: intPtr(50), counted: 10, passed: 5,
			runs: healthRuns("SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "SUCCESS", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE"),
		},
		{
			name: "three of ten passed is rainy", bucket: "rainy", score: intPtr(30), counted: 10, passed: 3,
			runs: healthRuns("SUCCESS", "SUCCESS", "SUCCESS", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE"),
		},
		{
			name: "two of ten passed is stormy", bucket: "stormy", score: intPtr(20), counted: 10, passed: 2,
			runs: healthRuns("SUCCESS", "SUCCESS", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE", "FAILURE"),
		},
		{
			name: "unstable and aborted count as failures", bucket: "partlyCloudy", score: intPtr(66), counted: 3, passed: 2,
			runs: healthRuns("SUCCESS", "UNSTABLE", "SUCCESS"),
		},
		{
			name: "aborted counts as a failure", bucket: "cloudy", score: intPtr(50), counted: 2, passed: 1,
			runs: healthRuns("ABORTED", "SUCCESS"),
		},
		{
			name: "interrupted and not built do not count", bucket: "sunny", score: intPtr(100), counted: 2, passed: 2,
			runs: healthRuns("SUCCESS", "INTERRUPTED", "NOT_BUILT", "SUCCESS"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeJobHealth(tc.runs)
			if got.Bucket != tc.bucket {
				t.Fatalf("bucket = %q, want %q", got.Bucket, tc.bucket)
			}
			if got.Window != recordstore.JobHealthWindow {
				t.Fatalf("window = %d, want %d", got.Window, recordstore.JobHealthWindow)
			}
			if tc.bucket == "unknown" {
				if got.Score != nil || got.Counted != 0 || got.Passed != 0 {
					t.Fatalf("unknown health = %+v, want no score and no counts", got)
				}
				return
			}
			if got.Counted != tc.counted || got.Passed != tc.passed {
				t.Fatalf("counted/passed = %d/%d, want %d/%d", got.Counted, got.Passed, tc.counted, tc.passed)
			}
			if got.Score == nil || *got.Score != *tc.score {
				t.Fatalf("score = %v, want %d", got.Score, *tc.score)
			}
		})
	}
}

func int32Ptr(v int32) *int32 { return &v }

// TestBuildJobTrend asserts the trend keeps the window's order, derives
// passed from the suite counters, leaves tests absent for a run that
// recorded none, and never returns nil.
func TestBuildJobTrend(t *testing.T) {
	if got := buildJobTrend(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty window trend = %#v, want an empty non-nil slice", got)
	}

	runs := []recordstore.HealthRun{
		{RunID: "run-new", BuildNumber: int64Ptr(9), Result: "UNSTABLE", DurationMs: int64Ptr(4200),
			TestsTotal: int32Ptr(10), TestsFailed: int32Ptr(2), TestsSkipped: int32Ptr(1)},
		{RunID: "run-bare", Result: "SUCCESS"},
		{RunID: "run-odd", Result: "FAILURE", TestsTotal: int32Ptr(2), TestsFailed: int32Ptr(3), TestsSkipped: int32Ptr(0)},
	}
	got := buildJobTrend(runs)
	if len(got) != 3 || got[0].RunID != "run-new" || got[1].RunID != "run-bare" || got[2].RunID != "run-odd" {
		t.Fatalf("trend order = %+v, want the window order kept", got)
	}
	first := got[0]
	if first.BuildNumber == nil || *first.BuildNumber != 9 || first.DurationMs == nil || *first.DurationMs != 4200 {
		t.Fatalf("first point = %+v, want build 9 and duration 4200", first)
	}
	if first.Tests == nil || first.Tests.Passed != 7 || first.Tests.Failed != 2 || first.Tests.Skipped != 1 {
		t.Fatalf("first tests = %+v, want passed 7, failed 2, skipped 1", first.Tests)
	}
	if got[1].Tests != nil {
		t.Fatalf("run with no counters carries tests: %+v", got[1].Tests)
	}
	if got[2].Tests == nil || got[2].Tests.Passed != 0 {
		t.Fatalf("passed must floor at zero: %+v", got[2].Tests)
	}

	raw, err := json.Marshal(got[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"buildNumber", "durationMs", "tests"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("bare point JSON carries %q: %s", key, raw)
		}
	}
}

// TestJobHealthService_Unavailable asserts JobHealth answers 503 when the
// server has no record store.
func TestJobHealthService_Unavailable(t *testing.T) {
	unavailable := NewServer(&Dependencies{})
	if _, serr := unavailable.JobHealth(context.Background(), testClaims(), "ctrl-a", "acme/app-a"); serr == nil || serr.Status != http.StatusServiceUnavailable {
		t.Fatalf("no record store: serr = %+v, want 503", serr)
	}
}

// TestHandleGetPipelineJob_HealthAndTrend asserts the job detail carries
// the health object, the newest-first trend, and no durationTrendMs key.
func TestHandleGetPipelineJob_HealthAndTrend(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, true, true),
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-a",
		RecentRuns: []recordstore.HealthRun{
			{RunID: "run-2", BuildNumber: int64Ptr(2), Result: "SUCCESS", DurationMs: int64Ptr(900)},
			{RunID: "run-1", BuildNumber: int64Ptr(1), Result: "FAILURE"},
		},
	}}
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a", map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["durationTrendMs"]; ok {
		t.Fatalf("job detail still carries durationTrendMs: %s", rec.Body.String())
	}
	var detail struct {
		Health JobHealthJSON       `json:"health"`
		Trend  []JobTrendPointJSON `json:"trend"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Health.Bucket != "cloudy" || detail.Health.Window != recordstore.JobHealthWindow || detail.Health.Counted != 2 {
		t.Fatalf("health = %+v, want cloudy over a window of %d", detail.Health, recordstore.JobHealthWindow)
	}
	if len(detail.Trend) != 2 || detail.Trend[0].RunID != "run-2" || detail.Trend[0].DurationMs == nil {
		t.Fatalf("trend = %+v, want two points newest first", detail.Trend)
	}
}
