package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestPipelinesOverviewStopsWhenPlacementsFail asserts that a degraded
// placement read stops the overview before its run read. The store error
// returns to the caller, and no service error is set.
func TestPipelinesOverviewStopsWhenPlacementsFail(t *testing.T) {
	reader := &fakeRecordReader{placementsErr: fmt.Errorf("%w: x", recordstore.ErrUnavailable)}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	_, serr, err := srv.PipelinesOverview(context.Background(), resolverClaims, time.Now().UTC())
	if serr != nil {
		t.Fatalf("service error = %+v, want none", serr)
	}
	if !errors.Is(err, recordstore.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if len(reader.overviewQueries) != 0 {
		t.Fatalf("RunsOverview calls = %d, want none", len(reader.overviewQueries))
	}
}

// overviewFor answers the overview of srv at now, for the caller in claims.
func overviewFor(t *testing.T, srv *Server) PipelinesOverview {
	t.Helper()
	out, serviceErr, err := srv.PipelinesOverview(context.Background(), resolverClaims, time.Now().UTC())
	if serviceErr != nil || err != nil {
		t.Fatalf("PipelinesOverview: service %+v, err %v", serviceErr, err)
	}
	return out
}

func TestPipelinesOverviewRailOrderAndGrouping(t *testing.T) {
	reader := &fakeRecordReader{
		placements: []recordstore.TenantPlacement{
			{Cluster: "member-b", Tenant: "shop", Namespace: "ns", Cell: "c3", Generation: 1},
			{Cluster: "edge", Tenant: "shop", Namespace: "ns", Cell: "c4", Generation: 1},
		},
		overviewScopes: map[string]recordstore.ScopeOverview{
			"core/pay":      {Running: 2},
			"core/ops":      {Runs24h: 3, Failed24h: 3},
			"member-b/shop": {Runs24h: 4},
			"edge/shop":     {Runs24h: 5},
		},
	}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader,
		pipelinesTenant("pay", true, "core", "ns", "c1"),
		pipelinesTenant("ops", true, "core", "ns", "c2"),
		pipelinesTenant("idle", true, "", "", ""))
	out := overviewFor(t, srv)

	names := make([]string, 0, len(out.Tenants))
	byName := map[string]PipelinesOverviewTenant{}
	for _, tenant := range out.Tenants {
		names = append(names, tenant.Name)
		byName[tenant.Name] = tenant
	}
	want := []string{"ops", "pay", "shop", "idle"}
	if !sameNames(names, want) {
		t.Fatalf("rail order = %v; want %v", names, want)
	}
	if shop := byName["shop"]; len(shop.Clusters) != 2 || shop.Clusters[0] != "edge" || shop.Clusters[1] != "member-b" || shop.Runs24h != 9 {
		t.Errorf("shop = %+v; want clusters edge and member-b, and 9 runs", shop)
	}
	if byName["idle"].Placement != placementAwaitingCell {
		t.Errorf("idle placement = %q; want awaitingCell", byName["idle"].Placement)
	}
	if out.Attention.AwaitingCell != 1 {
		t.Errorf("awaitingCell = %d; want 1", out.Attention.AwaitingCell)
	}
	if out.Clusters != 3 {
		t.Errorf("clusters = %d; want 3 (core, member-b, edge)", out.Clusters)
	}
}

func TestPipelinesOverviewPassRate(t *testing.T) {
	// Two of the four counted runs passed. Finished runs that do not count stay out of the rate.
	reader := &fakeRecordReader{overviewTotals: recordstore.RunsOverviewTotals{Passed: 2, Counted: 4, Finished: 6}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if out.Totals.PassRate == nil || *out.Totals.PassRate != 0.5 {
		t.Fatalf("passRate = %v; want 0.5", out.Totals.PassRate)
	}

	// Every finished run is interrupted, so no run counts and the rate is absent.
	reader.overviewTotals = recordstore.RunsOverviewTotals{Finished: 3, Interrupted: 3, Counted: 0}
	out = overviewFor(t, srv)
	if out.Totals.PassRate != nil {
		t.Fatalf("passRate = %v; want nil when no run counts", *out.Totals.PassRate)
	}
}

func TestPipelinesOverviewLocksHeldOnlyReadable(t *testing.T) {
	reader := &fakeRecordReader{allLocks: []recordstore.FleetLock{
		{Cluster: "core", Tenant: "pay", JobPath: "pay/deploy", State: "held"},
		{Cluster: "core", Tenant: "pay", JobPath: "pay/secret", State: "held"},
		{Cluster: "core", Tenant: "other", JobPath: "other/x", State: "held"},
		{Cluster: "core", Tenant: "pay", JobPath: "pay/deploy", State: "waiting"},
	}}
	srv := fleetServer(t, pipelineJobGrantAuthorizer([]string{"read"}, "deploy"), reader,
		pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if out.Attention.LocksHeld != 1 {
		t.Fatalf("locksHeld = %d; want 1", out.Attention.LocksHeld)
	}
}

func TestPipelinesOverviewNoRecordStore(t *testing.T) {
	srv := fleetServer(t, globalPipelinesAuthorizer(), nil, pipelinesTenant("pay", true, "core", "ns", "c1"))
	_, serviceErr, err := srv.PipelinesOverview(context.Background(), resolverClaims, time.Now().UTC())
	if err != nil || serviceErr == nil || serviceErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("service %+v, err %v; want 503", serviceErr, err)
	}
}

// sameNames reports whether two name lists hold the same names in order.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// attentionRun builds a finished or live run of the core cluster, tenant pay.
func attentionRun(runID, status, jobPath string, created time.Time) recordstore.Run {
	return recordstore.Run{
		RunID: runID, Status: status, CreatedAt: created,
		Cluster: "core", Namespace: "ns", Cell: "c1", Tenant: "pay", JobPath: jobPath,
	}
}

// TestPipelinesOverviewAnswersTwentyFourHours asserts the overview answers 24
// hourly buckets, oldest first, from the store buckets.
func TestPipelinesOverviewAnswersTwentyFourHours(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	reader := &fakeRecordReader{hourly: []recordstore.HourCount{{Runs: 2, Failed: 1}}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out, serviceErr, err := srv.PipelinesOverview(t.Context(), resolverClaims, now)
	if serviceErr != nil || err != nil {
		t.Fatalf("PipelinesOverview: service %+v, err %v", serviceErr, err)
	}
	if len(out.Hourly) != recordstore.OverviewHours {
		t.Fatalf("hourly = %d buckets, want %d", len(out.Hourly), recordstore.OverviewHours)
	}
	if first := out.Hourly[0]; first.Runs != 2 || first.Failed != 1 {
		t.Fatalf("first bucket = %+v, want 2 runs and 1 failure", first)
	}
	if want := now.Add(-24 * time.Hour); !out.Hourly[0].Start.Equal(want) {
		t.Fatalf("first start = %v, want %v", out.Hourly[0].Start, want)
	}
	if out.Hourly[23].Runs != 0 {
		t.Fatalf("newest bucket runs = %d, want 0", out.Hourly[23].Runs)
	}
}

// TestPipelinesOverviewWaitingInputsOldestFirst asserts a waiting run carries
// its first input, the input count, and the parameter count, and that the
// waiting read runs before the running read.
func TestPipelinesOverviewWaitingInputsOldestFirst(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	inputs := json.RawMessage(`[{"id":"approve","message":"Deploy?","ok":"Deploy","submitter":"ana","parameters":[{"name":"env"}]},{"id":"second"}]`)
	waiting := attentionRun("00000000-0000-4000-8000-000000000001", "RUNNING", "pay/deploy", now.Add(-time.Hour))
	waiting.Inputs = inputs
	reader := &fakeRecordReader{
		acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{waiting}}, {}},
	}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out, serviceErr, err := srv.PipelinesOverview(t.Context(), resolverClaims, now)
	if serviceErr != nil || err != nil {
		t.Fatalf("PipelinesOverview: service %+v, err %v", serviceErr, err)
	}
	if len(reader.acrossQueries) < 2 || reader.acrossQueries[0].Status != recordstore.RunStatusWaitingInput {
		t.Fatalf("first across query = %+v, want the waiting read", reader.acrossQueries)
	}
	if !reader.acrossQueries[0].Ascending || reader.acrossQueries[0].Limit != 20 {
		t.Fatalf("waiting query = %+v, want ascending with limit 20", reader.acrossQueries[0])
	}
	if len(out.WaitingInputs) != 1 {
		t.Fatalf("waiting inputs = %d, want 1", len(out.WaitingInputs))
	}
	got := out.WaitingInputs[0]
	if got.Input.ID != "approve" || got.Input.Message != "Deploy?" || got.Input.Submitter != "ana" {
		t.Fatalf("input = %+v, want the first input", got.Input)
	}
	if got.Input.ParameterCount != 1 || got.InputCount != 2 {
		t.Fatalf("parameter count = %d, input count = %d; want 1 and 2", got.Input.ParameterCount, got.InputCount)
	}
	if got.Run.Tenant != "pay" {
		t.Fatalf("rail = %q, want pay", got.Run.Tenant)
	}
}

// TestPipelinesOverviewFailingJobsStageAndReason asserts a failing job names its
// failed stage, the first line of the stage error without escapes, and the tests
// that failed.
func TestPipelinesOverviewFailingJobsStageAndReason(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	failedAt := now.Add(-time.Hour)
	stageErr := "\x1b[31mboom\x1b[0m\nsecond line"
	tests := int32(3)
	failed := attentionRun("00000000-0000-4000-8000-000000000002", "COMPLETED", "pay/build", failedAt)
	failed.TestsFailed = &tests
	reader := &fakeRecordReader{
		failingJobs: []recordstore.FailingJob{{
			Failed: failed,
			FailedStages: []recordstore.Stage{{
				StageID: "s1", Name: "Test", NodeType: "stage", Status: "FAILURE", Error: &stageErr,
			}},
			FailuresSincePass: 2,
		}},
	}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out, serviceErr, err := srv.PipelinesOverview(t.Context(), resolverClaims, now)
	if serviceErr != nil || err != nil {
		t.Fatalf("PipelinesOverview: service %+v, err %v", serviceErr, err)
	}
	if len(out.FailingJobs) != 1 {
		t.Fatalf("failing jobs = %d, want 1", len(out.FailingJobs))
	}
	job := out.FailingJobs[0]
	if job.Stage != "Test" || job.StageError != "boom" || job.TestsFailed != 3 || job.FailuresSincePass != 2 {
		t.Fatalf("failing job = %+v, want stage Test, error boom, 3 tests, 2 failures", job)
	}
	if job.Run.Tenant != "pay" {
		t.Fatalf("rail = %q, want pay", job.Run.Tenant)
	}
	if reader.failingQueries[0].Limit != 20 || !reader.failingQueries[0].Since.Equal(now.Add(-recordstore.FailingJobsWindow)) {
		t.Fatalf("failing query = %+v, want limit 20 and the 7-day window", reader.failingQueries[0])
	}
}

// TestPipelinesOverviewFirstErrorLineCaps asserts a long first line is cut at
// the rune cap with an ellipsis.
func TestPipelinesOverviewFirstErrorLineCaps(t *testing.T) {
	long := strings.Repeat("e", overviewStageErrorRunes+10)
	got := firstErrorLine(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != overviewStageErrorRunes+1 {
		t.Fatalf("capped line has %d runes, want %d and an ellipsis", len([]rune(got)), overviewStageErrorRunes+1)
	}
}

// TestPipelinesOverviewNextScheduled asserts the next cron run names the rail
// tenant and its folder.
func TestPipelinesOverviewNextScheduled(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := now.Add(time.Hour)
	reader := &fakeRecordReader{nextCron: recordstore.ScheduledRun{Cluster: "core", Tenant: "pay", JobPath: "pay/nightly", At: at}, nextCronOK: true}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out, serviceErr, err := srv.PipelinesOverview(t.Context(), resolverClaims, now)
	if serviceErr != nil || err != nil {
		t.Fatalf("PipelinesOverview: service %+v, err %v", serviceErr, err)
	}
	if out.NextScheduled == nil || out.NextScheduled.Tenant != "pay" || out.NextScheduled.JobPath != "pay/nightly" || !out.NextScheduled.At.Equal(at) {
		t.Fatalf("next scheduled = %+v, want pay/nightly at +1h", out.NextScheduled)
	}
}

// TestPipelinesOverviewWaitingInputsCarryWrite asserts a waiting run carries the
// tenant write check of the caller: true with write access, false with read only.
func TestPipelinesOverviewWaitingInputsCarryWrite(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	inputs := json.RawMessage(`[{"id":"approve"}]`)
	build := func() recordstore.Run {
		run := attentionRun("00000000-0000-4000-8000-000000000011", "RUNNING", "pay/deploy", now.Add(-time.Hour))
		run.Inputs = inputs
		return run
	}

	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{build()}}, {}}}
	srv := fleetServer(t, pipelineJobGrantAuthorizer([]string{"read", "write"}, "deploy"), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if len(out.WaitingInputs) != 1 || !out.WaitingInputs[0].Run.CanWrite {
		t.Fatalf("waiting inputs = %+v, want one row with canWrite", out.WaitingInputs)
	}

	reader = &fakeRecordReader{acrossPages: []recordstore.RunPage{{Runs: []recordstore.Run{build()}}, {}}}
	srv = fleetServer(t, pipelineJobGrantAuthorizer([]string{"read"}, "deploy"), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out = overviewFor(t, srv)
	if len(out.WaitingInputs) != 1 || out.WaitingInputs[0].Run.CanWrite {
		t.Fatalf("waiting inputs = %+v, want one row without canWrite", out.WaitingInputs)
	}
}

// TestPipelinesOverviewMoreWaitingInputs asserts a waiting read that has a next
// cursor sets moreWaitingInputs.
func TestPipelinesOverviewMoreWaitingInputs(t *testing.T) {
	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{{NextCursor: "more"}, {}}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if !out.MoreWaitingInputs {
		t.Fatal("moreWaitingInputs = false; want true when the read has a next cursor")
	}
}

// TestPipelinesOverviewRunningSkipsWaitingRuns asserts a run that waits for input
// stays out of the running list.
func TestPipelinesOverviewRunningSkipsWaitingRuns(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	waiting := attentionRun("00000000-0000-4000-8000-000000000021", "RUNNING", "pay/deploy", now.Add(-time.Hour))
	waiting.Inputs = json.RawMessage(`[{"id":"approve"}]`)
	plain := attentionRun("00000000-0000-4000-8000-000000000022", "RUNNING", "pay/build", now.Add(-time.Hour))
	reader := &fakeRecordReader{acrossPages: []recordstore.RunPage{{}, {Runs: []recordstore.Run{waiting, plain}}}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if len(out.Running) != 1 || out.Running[0].Run.RunID != plain.RunID {
		t.Fatalf("running = %+v, want only the run that waits for no input", out.Running)
	}
}

// TestPipelinesOverviewRunningCarriesStage asserts a running row names the
// top-level stage in progress.
func TestPipelinesOverviewRunningCarriesStage(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	run := attentionRun("00000000-0000-4000-8000-000000000031", "RUNNING", "pay/build", now.Add(-time.Hour))
	reader := &fakeRecordReader{
		acrossPages: []recordstore.RunPage{{}, {Runs: []recordstore.Run{run}}},
		stages: map[string][]recordstore.Stage{run.RunID: {{
			RunID: run.RunID, StageID: "s1", Name: "Build", NodeType: "stage", Status: "IN_PROGRESS",
		}}},
	}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if len(out.Running) != 1 || out.Running[0].Stage != "Build" {
		t.Fatalf("running = %+v, want one run with stage Build", out.Running)
	}
}

// TestPipelinesOverviewFailingJobsUseRailNames asserts a failing job names its
// tenant by the rail name of the caller's tenant.
func TestPipelinesOverviewFailingJobsUseRailNames(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	failed := attentionRun("00000000-0000-4000-8000-000000000041", "COMPLETED", "pay/build", now.Add(-time.Hour))
	reader := &fakeRecordReader{failingJobs: []recordstore.FailingJob{{Failed: failed}}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	out := overviewFor(t, srv)
	if len(out.FailingJobs) != 1 || out.FailingJobs[0].Run.Tenant != "pay" || out.FailingJobs[0].Run.Cluster != "core" {
		t.Fatalf("failing jobs = %+v, want one row named pay on core", out.FailingJobs)
	}
}

// TestPipelinesOverviewFailingJobsErrorFailsTheRead asserts a failing-jobs read
// error fails the whole overview, so no attention item is dropped silently.
func TestPipelinesOverviewFailingJobsErrorFailsTheRead(t *testing.T) {
	reader := &fakeRecordReader{failingErr: errors.New("store down")}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	_, serviceErr, err := srv.PipelinesOverview(t.Context(), resolverClaims, time.Now().UTC())
	if err == nil && serviceErr == nil {
		t.Fatal("PipelinesOverview answered without an error after the failing-jobs read failed")
	}
}

// TestPipelinesOverviewNoScopeReadsNoList asserts a caller with no readable
// tenant reads no attention list, and still gets 24 hourly buckets.
func TestPipelinesOverviewNoScopeReadsNoList(t *testing.T) {
	reader := &fakeRecordReader{}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader)
	out := overviewFor(t, srv)
	if len(reader.acrossQueries) != 0 || len(reader.failingQueries) != 0 {
		t.Fatalf("reads = %d across, %d failing; want none", len(reader.acrossQueries), len(reader.failingQueries))
	}
	if len(out.Hourly) != recordstore.OverviewHours {
		t.Fatalf("hourly = %d buckets, want %d", len(out.Hourly), recordstore.OverviewHours)
	}
	if out.WaitingInputs == nil || out.FailingJobs == nil || out.Running == nil {
		t.Fatal("attention lists are nil; want empty lists")
	}
}
