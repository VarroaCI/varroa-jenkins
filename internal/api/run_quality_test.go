package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

func qualityCounterOf(covered, total int32) *recordstore.CoverageCounter {
	return &recordstore.CoverageCounter{Covered: covered, Total: total}
}

func TestQualityPercentRounding(t *testing.T) {
	cases := []struct {
		name           string
		covered, total int32
		want           *float64
	}{
		{"exact tenth", 812, 1000, floatPtr(81.2)},
		{"rounds to one decimal", 2, 3, floatPtr(66.7)},
		{"zero covered is present", 0, 5, floatPtr(0)},
		{"zero total is absent", 0, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qualityPercent(tc.covered, tc.total)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("qualityPercent(%d, %d) = %v, want %v", tc.covered, tc.total, got, tc.want)
			}
		})
	}
}

func TestBuildRunQualityCoverageDelta(t *testing.T) {
	q := recordstore.RunQuality{Coverage: []recordstore.RunCoverage{
		{
			RunID: "r3", ReportID: "jacoco", Name: "Coverage",
			Line:   qualityCounterOf(812, 1000),
			Branch: qualityCounterOf(140, 200),
			Base: &recordstore.CoverageBase{
				RunID: "r1", BuildNumber: int64Ptr(1),
				Line:   qualityCounterOf(798, 1000),
				Branch: qualityCounterOf(141, 200),
			},
		},
		{
			RunID: "r3", ReportID: "cobertura", Name: "Cobertura",
			Line: qualityCounterOf(10, 20),
			Base: &recordstore.CoverageBase{RunID: "r1", Line: qualityCounterOf(10, 20)},
		},
	}}
	out := buildRunQuality(q)
	if out == nil || len(out.Coverage) != 2 {
		t.Fatalf("quality = %+v, want two coverage reports", out)
	}
	jac := out.Coverage[0]
	if jac.Delta == nil || jac.Delta.BaseRunID != "r1" || jac.Delta.BaseBuildNumber == nil || *jac.Delta.BaseBuildNumber != 1 {
		t.Fatalf("jacoco delta = %+v, want base r1 build 1", jac.Delta)
	}
	if jac.Delta.LinePoints == nil || *jac.Delta.LinePoints != 1.4 {
		t.Fatalf("linePoints = %v, want 1.4", jac.Delta.LinePoints)
	}
	if jac.Delta.BranchPoints == nil || *jac.Delta.BranchPoints != -0.5 {
		t.Fatalf("branchPoints = %v, want -0.5", jac.Delta.BranchPoints)
	}

	cob := out.Coverage[1]
	if cob.Delta == nil || cob.Delta.BranchPoints != nil || cob.Delta.BaseBuildNumber != nil {
		t.Fatalf("cobertura delta = %+v, want no branchPoints and no build number", cob.Delta)
	}
	if cob.Delta.LinePoints == nil || *cob.Delta.LinePoints != 0 {
		t.Fatalf("cobertura linePoints = %v, want 0", cob.Delta.LinePoints)
	}
}

func TestBuildRunQualityZeroDeltaIsPositiveZero(t *testing.T) {
	q := recordstore.RunQuality{Coverage: []recordstore.RunCoverage{{
		RunID: "r2", ReportID: "jacoco", Name: "Coverage",
		Line: qualityCounterOf(50, 100),
		Base: &recordstore.CoverageBase{RunID: "r1", Line: qualityCounterOf(5001, 10000)},
	}}}
	raw, err := json.Marshal(buildRunQuality(q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"linePoints":0`; !strings.Contains(string(raw), want) {
		t.Fatalf("json = %s, want %s", raw, want)
	}
}

func TestBuildRunQualityWarnings(t *testing.T) {
	newTotal := int32(2)
	q := recordstore.RunQuality{Warnings: []recordstore.RunWarnings{
		{
			RunID: "r2", ReportID: "checkstyle", Name: "Checkstyle", Total: 4, Error: 0, High: 0, Normal: 4, Low: 0,
			NewTotal: &newTotal,
			Base:     &recordstore.WarningsBase{RunID: "r1", Total: 7, BuildNumber: int64Ptr(1)},
		},
		{RunID: "r2", ReportID: "pmd", Name: "PMD", Total: 0},
	}}
	out := buildRunQuality(q)
	if out == nil || len(out.Warnings) != 2 {
		t.Fatalf("quality = %+v, want two warnings reports", out)
	}
	cs := out.Warnings[0]
	if cs.Delta == nil || cs.Delta.Total != -3 || cs.New == nil || *cs.New != 2 || cs.Fixed != nil {
		t.Fatalf("checkstyle = %+v, want delta -3, new 2, no fixed", cs)
	}
	pmd := out.Warnings[1]
	if pmd.Total != 0 || pmd.New != nil || pmd.Delta != nil {
		t.Fatalf("pmd = %+v, want zero total kept and no new, no delta", pmd)
	}
}

func TestBuildRunQualityNilWithoutRows(t *testing.T) {
	if out := buildRunQuality(recordstore.RunQuality{}); out != nil {
		t.Fatalf("buildRunQuality(empty) = %+v, want nil", out)
	}
}

func TestBuildQualityTrendGapsAndOrder(t *testing.T) {
	window := []recordstore.HealthRun{
		{RunID: "r3", BuildNumber: int64Ptr(3)},
		{RunID: "r2", BuildNumber: int64Ptr(2)},
		{RunID: "r1", BuildNumber: int64Ptr(1)},
	}
	q := recordstore.RunQuality{
		Coverage: []recordstore.RunCoverage{
			{RunID: "r1", ReportID: "jacoco", Name: "Coverage", Line: qualityCounterOf(800, 1000)},
			{RunID: "r3", ReportID: "jacoco", Name: "Coverage", Line: qualityCounterOf(812, 1000)},
			{RunID: "gone", ReportID: "jacoco", Name: "Coverage", Line: qualityCounterOf(1, 1)},
		},
		Warnings: []recordstore.RunWarnings{
			{RunID: "r3", ReportID: "checkstyle", Name: "Checkstyle", Total: 1, NewTotal: nil},
		},
	}
	out := buildQualityTrend(window, q)
	if out == nil || len(out.Coverage) != 1 || len(out.Warnings) != 1 {
		t.Fatalf("trend = %+v, want one coverage and one warnings series", out)
	}
	series := out.Coverage[0]
	if series.ID != "jacoco" || series.Name != "Coverage" || len(series.Points) != 2 {
		t.Fatalf("coverage series = %+v, want jacoco with two points", series)
	}
	if series.Points[0].RunID != "r3" || series.Points[1].RunID != "r1" {
		t.Fatalf("points = %+v, want newest first r3 then r1", series.Points)
	}
	if series.Points[0].BuildNumber == nil || *series.Points[0].BuildNumber != 3 {
		t.Fatalf("point build number = %v, want 3", series.Points[0].BuildNumber)
	}
	if series.Points[0].LinePercent == nil || *series.Points[0].LinePercent != 81.2 {
		t.Fatalf("linePercent = %v, want 81.2", series.Points[0].LinePercent)
	}
	if out.Warnings[0].ID != "checkstyle" || out.Warnings[0].Points[0].New != nil {
		t.Fatalf("warnings series = %+v, want checkstyle without new", out.Warnings[0])
	}
}

func TestBuildQualityTrendNilWithoutRows(t *testing.T) {
	window := []recordstore.HealthRun{{RunID: "r1"}}
	if out := buildQualityTrend(window, recordstore.RunQuality{}); out != nil {
		t.Fatalf("buildQualityTrend(no rows) = %+v, want nil", out)
	}
}

// TestGetRunCarriesQuality asserts that GET /runs/{run} carries the run's
// quality summary.
func TestGetRunCarriesQuality(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", Result: qualityStr("SUCCESS")}
	reader.quality = map[string]recordstore.RunQuality{"r1": {
		Coverage: []recordstore.RunCoverage{{RunID: "r1", ReportID: "jacoco", Name: "Coverage", Line: qualityCounterOf(812, 1000)}},
	}}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Quality == nil || len(detail.Quality.Coverage) != 1 || detail.Quality.Coverage[0].Line == nil ||
		detail.Quality.Coverage[0].Line.Percent == nil || *detail.Quality.Coverage[0].Line.Percent != 81.2 {
		t.Fatalf("quality = %+v, want the jacoco line at 81.2", detail.Quality)
	}
}

// TestGetRunOmitsQualityWithoutRows asserts that a run with no quality row
// has no quality key.
func TestGetRunOmitsQualityWithoutRows(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", Result: qualityStr("SUCCESS")}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["quality"]; ok {
		t.Fatalf("quality present without rows: %s", rec.Body.String())
	}
}

// TestGetRunQualityReadErrorLeavesRunDetail asserts that a failed quality
// read answers the run with the field left out.
func TestGetRunQualityReadErrorLeavesRunDetail(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED", Result: qualityStr("SUCCESS")}
	reader.qualityErr = errors.New("boom")
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["quality"]; ok {
		t.Fatalf("quality present after a failed read: %s", rec.Body.String())
	}
}

// TestHandleGetPipelineJob_QualityTrend asserts that the job detail carries
// the quality trend over the health window, and that the trend read asks
// for the window's run ids in order.
func TestHandleGetPipelineJob_QualityTrend(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobInventory = map[string]recordstore.JobInventory{
		"core/test-ns/ctrl-a": pendingInventory(nil, nil, true, true),
	}
	reader.jobSummaries["core/test-ns/ctrl-a"] = []recordstore.JobSummary{{
		JobPath: "acme/app-a",
		RecentRuns: []recordstore.HealthRun{
			{RunID: "run-2", BuildNumber: int64Ptr(2), Result: "SUCCESS"},
			{RunID: "run-1", BuildNumber: int64Ptr(1), Result: "FAILURE"},
		},
	}}
	reader.quality = map[string]recordstore.RunQuality{
		"run-2": {Coverage: []recordstore.RunCoverage{{RunID: "run-2", ReportID: "jacoco", Name: "Coverage", Line: qualityCounterOf(900, 1000)}}},
		"run-1": {Coverage: []recordstore.RunCoverage{{RunID: "run-1", ReportID: "jacoco", Name: "Coverage", Line: qualityCounterOf(800, 1000)}}},
	}
	rec := doPipelinesRequest(t, newPendingServer(reader).handleGetPipelineJob, "/pipelines/ctrl-a/jobs/acme%2Fapp-a", map[string]string{"tenant": "ctrl-a", "job": "acme/app-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		QualityTrend *QualityTrendJSON `json:"qualityTrend"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.QualityTrend == nil || len(detail.QualityTrend.Coverage) != 1 || len(detail.QualityTrend.Coverage[0].Points) != 2 {
		t.Fatalf("qualityTrend = %+v, want one jacoco series with two points", detail.QualityTrend)
	}
	if detail.QualityTrend.Coverage[0].Points[0].RunID != "run-2" {
		t.Fatalf("points = %+v, want run-2 first", detail.QualityTrend.Coverage[0].Points)
	}
	if len(reader.trendCalls) != 1 || len(reader.trendCalls[0]) != 2 ||
		reader.trendCalls[0][0] != "run-2" || reader.trendCalls[0][1] != "run-1" {
		t.Fatalf("trendCalls = %v, want one call with [run-2 run-1]", reader.trendCalls)
	}
}

func floatPtr(v float64) *float64 { return &v }

func qualityStr(v string) *string { return &v }

// TestBuildRunQualityCapsReports asserts that a run answers at most
// runQualityMaxReports reports of each kind.
func TestBuildRunQualityCapsReports(t *testing.T) {
	var q recordstore.RunQuality
	for i := 0; i < runQualityMaxReports+5; i++ {
		q.Coverage = append(q.Coverage, recordstore.RunCoverage{ReportID: "c", Line: qualityCounterOf(1, 2)})
		q.Warnings = append(q.Warnings, recordstore.RunWarnings{ReportID: "w", Total: 1})
	}
	out := buildRunQuality(q)
	if out == nil || len(out.Coverage) != runQualityMaxReports || len(out.Warnings) != runQualityMaxReports {
		t.Fatalf("quality = %+v, want %d of each kind", out, runQualityMaxReports)
	}
}
