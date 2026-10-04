package recordstore

import (
	"context"
	"database/sql"
	"testing"
)

// TestNextBuildNumberNoRow asserts a job with no job_numbers row returns 1.
func TestNextBuildNumberNoRow(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()

	got, err := NextBuildNumber(ctx, pool, testCluster, testTenant, "acme/no-history")
	if err != nil {
		t.Fatalf("NextBuildNumber: %v", err)
	}
	if got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
}

// TestGrantAdvancesWatermark asserts a grant on a job at next_number=7
// returns build_number=7 and leaves next_number at 8, and that a repeat
// with the same run_id answers the stored number again with no further
// advance.
func TestGrantAdvancesWatermark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-grant"

	if _, err := db.Exec(`
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, 7)
	`, testCluster, testTenant, jobPath); err != nil {
		t.Fatalf("seed job_numbers at 7: %v", err)
	}

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	req.JobPath = jobPath

	resp := mustGrant(t, ledger, req)
	if resp.BuildNumber != 7 {
		t.Fatalf("want build_number 7, got %d", resp.BuildNumber)
	}
	assertNextNumber(ctx, t, pool, jobPath, 8)

	repeat := ledger.Claim(ctx, req)
	if repeat.Decision != DecisionGranted || repeat.BuildNumber != 7 {
		t.Fatalf("want repeated grant with build_number 7, got %+v", repeat)
	}
	assertNextNumber(ctx, t, pool, jobPath, 8)
}

// TestTransferKeepsStoredRunIDReservesNoNumber asserts a transfer that
// keeps the stored run_id (a re-answer) reserves no new number.
func TestTransferKeepsStoredRunIDReservesNoNumber(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-reanswer"

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 1)
	req.JobPath = jobPath
	req.CellInstance = "pod-a"

	first := mustGrant(t, ledger, req)
	assertNextNumber(ctx, t, pool, jobPath, first.BuildNumber+1)

	reanswer := req
	reanswer.CellInstance = "pod-b" // an ownership move, not a run_id transfer
	resp := ledger.Claim(ctx, reanswer)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want unchanged grant, got %+v", resp)
	}
	assertNextNumber(ctx, t, pool, jobPath, first.BuildNumber+1)
}

// TestTransferWithNewRunIDReservesNewNumber asserts an ownership
// transfer that carries a new run_id writes a newly reserved number into
// the intent row.
func TestTransferWithNewRunIDReservesNewNumber(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-transfer"

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 1)
	req.JobPath = jobPath

	first := mustGrant(t, ledger, req)

	next := req
	next.RunID = newTestUUIDv7String()
	next.PlacementGeneration = 2

	resp := ledger.Claim(ctx, next)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}
	if resp.BuildNumber != first.BuildNumber+1 {
		t.Fatalf("want a newly reserved build number %d, got %d", first.BuildNumber+1, resp.BuildNumber)
	}

	var storedBuildNumber int64
	if err := db.QueryRow(`
		SELECT build_number FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&storedBuildNumber); err != nil {
		t.Fatalf("read stored build_number: %v", err)
	}
	if storedBuildNumber != resp.BuildNumber {
		t.Fatalf("want stored build_number %d, got %d", resp.BuildNumber, storedBuildNumber)
	}
	assertNextNumber(ctx, t, pool, jobPath, resp.BuildNumber+1)
}

// TestJenkinsNextBuildNumberLiftsWatermark asserts a claim reporting
// jenkins_next_build_number=41 against a watermark of 12 grants 41 and
// never grants a number below 41 for that job again.
func TestJenkinsNextBuildNumberLiftsWatermark(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-lift"

	if _, err := db.Exec(`
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, 12)
	`, testCluster, testTenant, jobPath); err != nil {
		t.Fatalf("seed job_numbers at 12: %v", err)
	}

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	req.JobPath = jobPath
	req.JenkinsNextBuildNumber = 41

	resp := mustGrant(t, ledger, req)
	if resp.BuildNumber != 41 {
		t.Fatalf("want build_number 41, got %d", resp.BuildNumber)
	}
	assertNextNumber(ctx, t, pool, jobPath, 42)

	second := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	second.JobPath = jobPath
	secondResp := mustGrant(t, ledger, second)
	if secondResp.BuildNumber < 41 {
		t.Fatalf("want a number at or above 41, got %d", secondResp.BuildNumber)
	}
}

// TestRaiseWatermarkForStartedBuild asserts the run.started raise helper
// lifts next_number to build_number+1 when that is higher, and creates
// the row when none exists.
func TestRaiseWatermarkForStartedBuild(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-started"

	if err := RaiseWatermarkForStartedBuild(ctx, pool, testCluster, testTenant, jobPath, 99); err != nil {
		t.Fatalf("raise watermark for a job with no row: %v", err)
	}
	assertNextNumber(ctx, t, pool, jobPath, 100)

	if err := RaiseWatermarkForStartedBuild(ctx, pool, testCluster, testTenant, jobPath, 5); err != nil {
		t.Fatalf("raise watermark with a lower number: %v", err)
	}
	assertNextNumber(ctx, t, pool, jobPath, 100)
}

// TestNumberingSurvivesRetention asserts NextBuildNumber reads
// job_numbers even after every intent and run of a job is gone.
func TestNumberingSurvivesRetention(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-retained"

	if _, err := db.Exec(`
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, 413)
	`, testCluster, testTenant, jobPath); err != nil {
		t.Fatalf("seed job_numbers at 413: %v", err)
	}

	got, err := NextBuildNumber(ctx, pool, testCluster, testTenant, jobPath)
	if err != nil {
		t.Fatalf("NextBuildNumber: %v", err)
	}
	if got != 413 {
		t.Fatalf("want 413, got %d", got)
	}
}

// TestNextBuildNumberAtBigintMaximumErrors asserts NextBuildNumber
// returns an error when next_number already equals the bigint maximum.
func TestNextBuildNumberAtBigintMaximumErrors(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-ceiling"

	if _, err := db.Exec(`
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, $4)
	`, testCluster, testTenant, jobPath, int64(bigintMax)); err != nil {
		t.Fatalf("seed job_numbers at the bigint maximum: %v", err)
	}

	if _, err := NextBuildNumber(ctx, pool, testCluster, testTenant, jobPath); err == nil {
		t.Fatal("want an error at the bigint maximum, got none")
	}
}

// assertNextNumber asserts the job_numbers.next_number stored for a job.
func assertNextNumber(ctx context.Context, t *testing.T, db DBTX, jobPath string, want int64) {
	t.Helper()
	var got int64
	if err := db.QueryRow(ctx, `
		SELECT next_number FROM varroa.job_numbers WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, testCluster, testTenant, jobPath).Scan(&got); err != nil {
		t.Fatalf("read next_number for %q: %v", jobPath, err)
	}
	if got != want {
		t.Fatalf("next_number for %q: want %d, got %d", jobPath, want, got)
	}
}

// seedJobNumbers inserts a job_numbers row for jobPath at next.
func seedJobNumbers(t *testing.T, db *sql.DB, jobPath string, next int64) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, $4)
	`, testCluster, testTenant, jobPath, next); err != nil {
		t.Fatalf("seed job_numbers at %d: %v", next, err)
	}
}

// TestStaleReanswerAdvancesWatermarkOnce asserts a re-answer from a new
// instance whose counter passed the stored number reserves the next
// number once, and a further re-answer with the same counter reserves
// nothing.
func TestStaleReanswerAdvancesWatermarkOnce(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-stale-once"
	seedJobNumbers(t, db, jobPath, 7)

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	req.JobPath = jobPath

	first := mustGrant(t, ledger, req)
	if first.BuildNumber != 7 {
		t.Fatalf("want build_number 7, got %d", first.BuildNumber)
	}
	assertNextNumber(ctx, t, pool, jobPath, 8)

	stale := req
	stale.CellInstance = "pod-b"
	stale.JenkinsNextBuildNumber = 8
	resp := ledger.Claim(ctx, stale)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != 8 {
		t.Fatalf("want refreshed grant with build_number 8, got %+v", resp)
	}
	assertNextNumber(ctx, t, pool, jobPath, 9)

	again := ledger.Claim(ctx, stale)
	if again.Decision != DecisionGranted || again.BuildNumber != 8 {
		t.Fatalf("want repeated grant with build_number 8, got %+v", again)
	}
	assertNextNumber(ctx, t, pool, jobPath, 9)
}

// TestStaleReanswerLiftsWatermarkToReported asserts a re-answer from a
// new instance whose counter is far past the stored number grants the
// reported counter and moves the watermark past it.
func TestStaleReanswerLiftsWatermarkToReported(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	jobPath := "acme/watermark-stale-lift"
	seedJobNumbers(t, db, jobPath, 7)

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	req.JobPath = jobPath
	first := mustGrant(t, ledger, req)
	if first.BuildNumber != 7 {
		t.Fatalf("want build_number 7, got %d", first.BuildNumber)
	}

	stale := req
	stale.CellInstance = "pod-b"
	stale.JenkinsNextBuildNumber = 20
	resp := ledger.Claim(ctx, stale)
	if resp.Decision != DecisionGranted || resp.BuildNumber != 20 {
		t.Fatalf("want refreshed grant with build_number 20, got %+v", resp)
	}
	assertNextNumber(ctx, t, pool, jobPath, 21)
}
