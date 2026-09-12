package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedRehydrationRun inserts one COMPLETED varroa.runs row directly,
// with the given job path, result, and creation time, so a test can
// build a rehydration fixture without going through the ledger and the
// ingester. buildNumber may be nil, matching a run the claim gate never
// numbered.
func seedRehydrationRun(t *testing.T, db *sql.DB, jobPath string, buildNumber *int64, createdAt time.Time, result string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, build_number, status, result)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, $8, 'COMPLETED', $9)
	`, runID, createdAt, "manual:rehydration-"+runID.String(), testCluster, testNamespace, testTenant, jobPath, buildNumber, sqlNullableResult(result)); err != nil {
		t.Fatalf("seed run for %s: %v", jobPath, err)
	}
	return runID
}

// sqlNullableResult turns an empty string into a nil driver value, so a
// QUEUED or RUNNING fixture row carries no result.
func sqlNullableResult(result string) any {
	if result == "" {
		return nil
	}
	return result
}

// seedRehydrationRunFull inserts one varroa.runs row with the full field
// set ListRehydrationRecords reads: params, changes, cause, and a
// run.started payload carrying masked parameter names.
func seedRehydrationRunFull(t *testing.T, db *sql.DB, jobPath string, buildNumber int64, createdAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	params := `{"BRANCH":"main","TOKEN":"<masked>"}`
	changes := `[{"commitId":"a1"},{"commitId":"a2"}]`
	cause := `"Started by user tester"`
	payload := `{"run.started":{"payload":{"masked_params":["TOKEN"]}}}`
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			build_number, status, result, params, changes, cause, payload
		) VALUES (
			$1, $2, $3, $4, $5, 'cell-a', $6, $7, $8, 'COMPLETED', 'SUCCESS', $9::jsonb, $10::jsonb, $11::jsonb, $12::jsonb
		)
	`, runID, createdAt, "manual:rehydration-"+runID.String(), testCluster, testNamespace, testTenant, jobPath,
		buildNumber, params, changes, cause, payload); err != nil {
		t.Fatalf("seed full run for %s: %v", jobPath, err)
	}
	return runID
}

// TestListRehydrationRecordsNewestFirst seeds twelve completed runs of
// one job and asserts a per-job limit of five returns the five newest,
// newest first.
func TestListRehydrationRecordsNewestFirst(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	var want []int64
	for i := int64(1); i <= 12; i++ {
		n := i
		seedRehydrationRun(t, db, "acme/app", &n, base.Add(time.Duration(i)*time.Minute), "SUCCESS")
	}
	for i := int64(12); i >= 8; i-- {
		want = append(want, i)
	}

	got, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{"acme/app"}, 5)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one job path answer, got %d", len(got))
	}
	if len(got[0].Records) != 5 {
		t.Fatalf("want 5 records, got %d", len(got[0].Records))
	}
	for i, rec := range got[0].Records {
		if rec.BuildNumber != want[i] {
			t.Fatalf("record %d: want build number %d, got %d", i, want[i], rec.BuildNumber)
		}
	}
}

// TestListRehydrationRecordsEnumeratesJobPaths asserts an empty
// job-path list enumerates every job path holding a job_numbers row or
// a completed run, including a multibranch child job.
func TestListRehydrationRecordsEnumeratesJobPaths(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	n := int64(1)
	seedRehydrationRun(t, db, "acme/app", &n, base, "SUCCESS")
	seedRehydrationRun(t, db, "acme/multi/main", &n, base, "SUCCESS")
	if err := raiseWatermark(ctx, pool, testCluster, testTenant, "acme/numbered-only", 5); err != nil {
		t.Fatalf("raise watermark: %v", err)
	}

	got, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, nil, 10)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}

	byPath := map[string]RehydrationJobRecords{}
	for _, jr := range got {
		byPath[jr.JobPath] = jr
	}
	for _, want := range []string{"acme/app", "acme/multi/main", "acme/numbered-only"} {
		jr, ok := byPath[want]
		if !ok {
			t.Fatalf("want job path %q enumerated, got %v", want, byPath)
		}
		if len(jr.Records) != 0 {
			t.Fatalf("job path %q: want no record in an enumeration, got %d", want, len(jr.Records))
		}
	}
	if byPath["acme/numbered-only"].NextBuildNumber != 5 {
		t.Fatalf("acme/numbered-only next build number: want 5, got %d", byPath["acme/numbered-only"].NextBuildNumber)
	}
}

// TestListRehydrationRecordsReturnsLastSuccessBeyondTheLimit seeds one
// successful run followed by twenty failures and asserts a request for
// ten records still carries the successful run.
func TestListRehydrationRecordsReturnsLastSuccessBeyondTheLimit(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	successNum := int64(1)
	seedRehydrationRun(t, db, "acme/flaky", &successNum, base, "SUCCESS")
	for i := int64(2); i <= 21; i++ {
		n := i
		seedRehydrationRun(t, db, "acme/flaky", &n, base.Add(time.Duration(i)*time.Minute), "FAILURE")
	}

	got, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{"acme/flaky"}, 10)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one job path answer, got %d", len(got))
	}
	records := got[0].Records
	if len(records) != 11 {
		t.Fatalf("want 10 newest plus the one successful run, got %d records", len(records))
	}
	var foundSuccess bool
	for _, rec := range records {
		if rec.BuildNumber == successNum {
			foundSuccess = true
			if rec.Result != "SUCCESS" {
				t.Fatalf("build %d: want result SUCCESS, got %q", successNum, rec.Result)
			}
		}
	}
	if !foundSuccess {
		t.Fatalf("want build %d present among the records, got %v", successNum, records)
	}
}

// TestListRehydrationRecordsZeroLimitStillReturnsLastSuccess asserts a
// per-job limit of zero still returns the job's one successful run.
func TestListRehydrationRecordsZeroLimitStillReturnsLastSuccess(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	successNum := int64(1)
	seedRehydrationRun(t, db, "acme/once", &successNum, base, "SUCCESS")

	got, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{"acme/once"}, 0)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	if len(got) != 1 || len(got[0].Records) != 1 {
		t.Fatalf("want exactly one record, got %+v", got)
	}
	if got[0].Records[0].BuildNumber != successNum {
		t.Fatalf("want build number %d, got %d", successNum, got[0].Records[0].BuildNumber)
	}
}

// TestListRehydrationRecordsNextNumberSurvivesRetention asserts the next
// build number still comes from job_numbers after every intent and run
// of a job is deleted, matching retention's own promise.
func TestListRehydrationRecordsNextNumberSurvivesRetention(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	if err := raiseWatermark(ctx, pool, testCluster, testTenant, "acme/retained", 413); err != nil {
		t.Fatalf("raise watermark: %v", err)
	}
	n := int64(412)
	runID := seedRehydrationRun(t, db, "acme/retained", &n, time.Now().UTC().Add(-time.Hour), "SUCCESS")
	if _, err := db.Exec(`DELETE FROM varroa.runs WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM varroa.run_intents WHERE job_path = $1`, "acme/retained"); err != nil {
		t.Fatalf("delete run_intents: %v", err)
	}

	got, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{"acme/retained"}, 10)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one job path answer, got %d", len(got))
	}
	if got[0].NextBuildNumber != 413 {
		t.Fatalf("want next build number 413, got %d", got[0].NextBuildNumber)
	}
	if len(got[0].Records) != 0 {
		t.Fatalf("want no record after deletion, got %d", len(got[0].Records))
	}
}

// TestListRehydrationRecordsOversizedRequestIsRefused asserts a request
// naming 33 job paths is refused and reads nothing.
func TestListRehydrationRecordsOversizedRequestIsRefused(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	ctx := context.Background()

	paths := make([]string, 33)
	for i := range paths {
		paths[i] = "acme/job-" + string(rune('a'+i))
	}

	if _, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, paths, 10); err == nil {
		t.Fatal("want an error for 33 job paths, got none")
	}
}

// TestListRehydrationRecordsCarriesParamsMaskedNamesAndChanges asserts a
// record carries its parameters, its masked parameter names, and its
// changes.
func TestListRehydrationRecordsCarriesParamsMaskedNamesAndChanges(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	seedRehydrationRunFull(t, db, "acme/full", 1, time.Now().UTC().Add(-time.Hour))

	got, err := ListRehydrationRecords(ctx, pool, testCluster, testTenant, []string{"acme/full"}, 10)
	if err != nil {
		t.Fatalf("ListRehydrationRecords: %v", err)
	}
	if len(got) != 1 || len(got[0].Records) != 1 {
		t.Fatalf("want exactly one record, got %+v", got)
	}
	rec := got[0].Records[0]

	var params map[string]string
	if err := json.Unmarshal(rec.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params["BRANCH"] != "main" {
		t.Fatalf("want BRANCH=main, got %+v", params)
	}
	if len(rec.MaskedParameterNames) != 1 || rec.MaskedParameterNames[0] != "TOKEN" {
		t.Fatalf("want masked parameter names [TOKEN], got %v", rec.MaskedParameterNames)
	}
	if rec.CauseDescription != "Started by user tester" {
		t.Fatalf("want cause description, got %q", rec.CauseDescription)
	}
	var changes []json.RawMessage
	if err := json.Unmarshal(rec.Changes, &changes); err != nil {
		t.Fatalf("decode changes: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("want 2 changes, got %d", len(changes))
	}
}
