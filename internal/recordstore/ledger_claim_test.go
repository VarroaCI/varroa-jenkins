package recordstore

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Fixed job coordinates every claim test shares, unless a test overrides
// them, so job_numbers tests see one predictable watermark per job path.
const (
	testCluster   = "core"
	testNamespace = "jenkins"
	testTenant    = "acme"
	testJobPath   = "acme/app"
)

// newTestUUIDv7String returns a fresh UUIDv7 string.
func newTestUUIDv7String() string {
	return uuid.Must(uuid.NewV7()).String()
}

// newTestClaimRequest returns a ClaimRequest with fixed job coordinates,
// a fresh UUIDv7 run_id, and the given intent key and generation.
func newTestClaimRequest(intentKey string, generation int64) ClaimRequest {
	return ClaimRequest{
		IntentKey:           intentKey,
		RunID:               newTestUUIDv7String(),
		Cluster:             testCluster,
		Namespace:           testNamespace,
		Cell:                "cell-a",
		CellInstance:        "pod-a",
		Tenant:              testTenant,
		JobPath:             testJobPath,
		Actor:               "tester",
		PlacementGeneration: generation,
		RequestedAt:         time.Now().UTC(),
	}
}

// mustGrant runs req against ledger and fails the test unless the answer
// is granted.
func mustGrant(t *testing.T, ledger *Ledger, req ClaimRequest) ClaimResponse {
	t.Helper()
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("seed claim %q: want granted, got %+v", req.IntentKey, resp)
	}
	return resp
}

// assertIntentRowCount asserts how many run_intents rows exist for
// intentKey.
func assertIntentRowCount(t *testing.T, db *sql.DB, intentKey string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE intent_key = $1`, intentKey).Scan(&count); err != nil {
		t.Fatalf("count run_intents rows for %q: %v", intentKey, err)
	}
	if count != want {
		t.Fatalf("run_intents row count for %q: want %d, got %d", intentKey, want, count)
	}
}

// assertStoredRunID asserts the run_id a single run_intents row holds.
func assertStoredRunID(t *testing.T, db *sql.DB, intentKey, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT run_id FROM varroa.run_intents WHERE intent_key = $1`, intentKey).Scan(&got); err != nil {
		t.Fatalf("read run_id for %q: %v", intentKey, err)
	}
	if got != want {
		t.Fatalf("stored run_id for %q: want %q, got %q", intentKey, want, got)
	}
}

// insertStubRunsRow inserts a minimal varroa.runs row for runID, so a
// test can prove a transfer or ownership move checks for it.
func insertStubRunsRow(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), 'stub', $2, $3, 'cell-a', $4, $5, 'RUNNING')
	`, runID, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert stub runs row: %v", err)
	}
}

// startedRunSnapshot is every value a duplicate answer for a started run
// must leave unchanged: the intent row, the job's watermark, and the
// run's runs rows.
type startedRunSnapshot struct {
	intent   string
	next     int64
	runsRows string
}

// snapshotStartedRun reads a startedRunSnapshot for intentKey and runID.
func snapshotStartedRun(t *testing.T, db *sql.DB, intentKey, runID string) startedRunSnapshot {
	t.Helper()
	var snap startedRunSnapshot
	if err := db.QueryRow(`
		SELECT row_to_json(ri)::text FROM varroa.run_intents ri WHERE intent_key = $1
	`, intentKey).Scan(&snap.intent); err != nil {
		t.Fatalf("read intent row: %v", err)
	}
	snap.next = readNextNumber(t, db)
	if err := db.QueryRow(`
		SELECT COALESCE(json_agg(r ORDER BY r.created_at)::text, '[]') FROM varroa.runs r WHERE run_id = $1
	`, runID).Scan(&snap.runsRows); err != nil {
		t.Fatalf("read runs rows: %v", err)
	}
	return snap
}

// assertStartedRunUnchanged fails the test when the intent row, the
// watermark, or the run's runs rows differ from before.
func assertStartedRunUnchanged(t *testing.T, db *sql.DB, intentKey, runID string, before startedRunSnapshot) {
	t.Helper()
	after := snapshotStartedRun(t, db, intentKey, runID)
	if after.intent != before.intent {
		t.Fatalf("intent row changed:\nbefore %s\nafter  %s", before.intent, after.intent)
	}
	if after.next != before.next {
		t.Fatalf("next_number moved from %d to %d", before.next, after.next)
	}
	if after.runsRows != before.runsRows {
		t.Fatalf("runs rows changed:\nbefore %s\nafter  %s", before.runsRows, after.runsRows)
	}
}

// TestNewKeyGrantedWritesOneRow asserts a new key is granted and writes
// exactly one run_intents row.
func TestNewKeyGrantedWritesOneRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionGranted || resp.RunID != req.RunID {
		t.Fatalf("want granted with run_id %q, got %+v", req.RunID, resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// TestFailedWatermarkAdvanceRollsBackIntent asserts a watermark advance
// that fails rolls the intent insert back with it, in the same
// transaction.
func TestFailedWatermarkAdvanceRollsBackIntent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	if _, err := db.Exec(`
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, $4)
	`, testCluster, testTenant, testJobPath, int64(bigintMax)); err != nil {
		t.Fatalf("seed job_numbers at the bigint maximum: %v", err)
	}

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != "" {
		t.Fatalf("want an empty decision on a failed watermark advance, got %+v", resp)
	}
	if resp.Code == "" {
		t.Fatalf("want a failure code on a failed watermark advance, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// TestStoredCreatedAtEqualsIdentifierTimestamp asserts run_intents.created_at
// equals the timestamp embedded in the run_id.
func TestStoredCreatedAtEqualsIdentifierTimestamp(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	id := uuid.Must(uuid.NewV7())
	req := newTestClaimRequest("manual:"+id.String(), 0)
	req.RunID = id.String()

	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}

	sec, nsec := id.Time().UnixTime()
	want := time.Unix(sec, nsec).UTC()

	var got time.Time
	if err := db.QueryRow(`SELECT created_at FROM varroa.run_intents WHERE intent_key = $1`, req.IntentKey).Scan(&got); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("want created_at %s, got %s", want, got)
	}
}

// TestNonUUIDv7RunIDIsVetoedInvalidKey asserts a UUIDv4 run_id is vetoed
// with INVALID_KEY and writes no row.
func TestNonUUIDv7RunIDIsVetoedInvalidKey(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:not-a-v7", 0)
	req.RunID = uuid.New().String() // UUIDv4

	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want vetoed/INVALID_KEY, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// TestUnallocatedSchemeIsVetoedInvalidKey asserts an unparsable intent
// key is vetoed with INVALID_KEY and writes no row.
func TestUnallocatedSchemeIsVetoedInvalidKey(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("webhook:github:abc", 0)

	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want vetoed/INVALID_KEY, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// TestRepeatSameRunIDGrantedOneRow: row 1 of the decision table — the
// repeat of a key with the same run_id answers granted and leaves one
// row.
func TestRepeatSameRunIDGrantedOneRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-repeat", 1)
	first := mustGrant(t, ledger, req)

	second := ledger.Claim(context.Background(), req)
	if second.Decision != DecisionGranted || second.RunID != first.RunID {
		t.Fatalf("want granted with run_id %q, got %+v", first.RunID, second)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// TestDifferentRunIDSameGenerationDuplicate: row 4 of the decision
// table — a different run_id at the stored generation answers
// duplicate.
func TestDifferentRunIDSameGenerationDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-dup", 2)
	first := mustGrant(t, ledger, req)

	other := req
	other.RunID = newTestUUIDv7String()

	resp := ledger.Claim(context.Background(), other)
	if resp.Decision != DecisionDuplicate || resp.RunID != first.RunID {
		t.Fatalf("want duplicate with run_id %q, got %+v", first.RunID, resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
	assertStoredRunID(t, db, req.IntentKey, first.RunID)
}

// TestHigherGenerationTransfersOwnership: row 3 of the decision table —
// a different run_id at a higher generation on a GRANTED intent with no
// runs row answers granted and moves the ownership columns and the
// build number.
func TestHigherGenerationTransfersOwnership(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-transfer", 1)
	req.Cell = "cell-a"
	req.CellInstance = "pod-a"
	first := mustGrant(t, ledger, req)

	next := req
	next.RunID = newTestUUIDv7String()
	next.PlacementGeneration = 2
	next.Cell = "cell-b"
	next.CellInstance = "pod-b"

	resp := ledger.Claim(context.Background(), next)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}
	if resp.RunID != next.RunID {
		t.Fatalf("want run_id %q, got %q", next.RunID, resp.RunID)
	}
	if resp.BuildNumber == first.BuildNumber {
		t.Fatalf("want a newly reserved build number, got the same %d", resp.BuildNumber)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)

	var cell, cellInstance string
	var generation int64
	if err := db.QueryRow(`
		SELECT cell, cell_instance, placement_generation FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&cell, &cellInstance, &generation); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cell != "cell-b" || cellInstance != "pod-b" || generation != 2 {
		t.Fatalf("want moved ownership cell-b/pod-b/2, got %s/%s/%d", cell, cellInstance, generation)
	}
}

// insertAcceptedOnlyRunsRow inserts one QUEUED runs row holding the
// accepted keys, for runID, so a transfer test can assert the ledger
// treats it the same way it treats an absent runs row.
func insertAcceptedOnlyRunsRow(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload)
		VALUES ($1, now(), 'stub', $2, $3, 'cell-a', $4, $5, 'QUEUED',
			jsonb_build_object('accepted_at', now(), 'accepted_instance', 'pod-a'))
	`, runID, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert accepted-only runs row: %v", err)
	}
}

// TestHigherGenerationTransfersAcceptedOnlyRun asserts a higher
// generation still transfers an intent whose only runs row is
// accepted-only: such a row records no build, so it must not block the
// transfer the way a real runs row does.
func TestHigherGenerationTransfersAcceptedOnlyRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-transfer-accepted", 1)
	req.Cell = "cell-a"
	req.CellInstance = "pod-a"
	first := mustGrant(t, ledger, req)
	insertAcceptedOnlyRunsRow(t, db, first.RunID)

	next := req
	next.RunID = newTestUUIDv7String()
	next.PlacementGeneration = 2
	next.Cell = "cell-b"
	next.CellInstance = "pod-b"

	resp := ledger.Claim(context.Background(), next)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}
	if resp.RunID != next.RunID {
		t.Fatalf("want run_id %q, got %q", next.RunID, resp.RunID)
	}

	var cell, cellInstance string
	if err := db.QueryRow(`
		SELECT cell, cell_instance FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&cell, &cellInstance); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cell != "cell-b" || cellInstance != "pod-b" {
		t.Fatalf("want moved ownership cell-b/pod-b, got %s/%s", cell, cellInstance)
	}
}

// TestLowerGenerationFenced: row 1 of the decision table — a lower
// generation answers vetoed with FENCED and leaves the row unchanged.
func TestLowerGenerationFenced(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-fenced", 5)
	first := mustGrant(t, ledger, req)

	stale := req
	stale.RunID = newTestUUIDv7String()
	stale.PlacementGeneration = 4

	resp := ledger.Claim(context.Background(), stale)
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonFenced {
		t.Fatalf("want vetoed/FENCED, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
	assertStoredRunID(t, db, req.IntentKey, first.RunID)
}

// TestHigherGenerationBlockedByExistingRunsRow: a higher generation does
// not transfer an intent whose runs row exists.
func TestHigherGenerationBlockedByExistingRunsRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-blocked", 1)
	first := mustGrant(t, ledger, req)

	insertStubRunsRow(t, db, first.RunID)

	next := req
	next.RunID = newTestUUIDv7String()
	next.PlacementGeneration = 2

	resp := ledger.Claim(context.Background(), next)
	if resp.Decision != DecisionDuplicate {
		t.Fatalf("want duplicate (transfer blocked), got %+v", resp)
	}
	if resp.RunID != first.RunID {
		t.Fatalf("want stored run_id %q, got %q", first.RunID, resp.RunID)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
	assertStoredRunID(t, db, req.IntentKey, first.RunID)
}

// TestRepeatConsumedIntentHigherGenerationDuplicate: a repeat of the
// stored run_id at a higher generation from another instance, for an
// intent run.started already consumed, answers duplicate and writes
// nothing.
func TestRepeatConsumedIntentHigherGenerationDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-consumed", 1)
	first := mustGrant(t, ledger, req)

	runID := uuid.MustParse(first.RunID)
	if err := consumeIntent(context.Background(), pool, runID, first.BuildNumber, time.Now().UTC()); err != nil {
		t.Fatalf("consume intent: %v", err)
	}
	before := snapshotStartedRun(t, db, req.IntentKey, first.RunID)

	repeat := req
	repeat.PlacementGeneration = 5
	repeat.CellInstance = "pod-different"

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionDuplicate || resp.RunID != first.RunID || resp.BuildNumber != 0 {
		t.Fatalf("want duplicate for run_id %q with no build number, got %+v", first.RunID, resp)
	}
	assertStartedRunUnchanged(t, db, req.IntentKey, first.RunID, before)
}

// TestRepeatDifferentCellInstanceMovesOwnership: a repeat of the stored
// run_id from a different cell instance, for a GRANTED intent with no
// runs row, answers granted with cell, cell_instance, and
// placement_generation moved to the request's values.
func TestRepeatDifferentCellInstanceMovesOwnership(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-move", 1)
	req.Cell = "cell-a"
	req.CellInstance = "pod-a"
	first := mustGrant(t, ledger, req)

	repeat := req
	repeat.Cell = "cell-b"
	repeat.CellInstance = "pod-b"

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted with unchanged run_id %q, got %+v", first.RunID, resp)
	}

	var cell, cellInstance string
	var generation int64
	if err := db.QueryRow(`
		SELECT cell, cell_instance, placement_generation FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&cell, &cellInstance, &generation); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cell != "cell-b" || cellInstance != "pod-b" || generation != 1 {
		t.Fatalf("want moved ownership cell-b/pod-b/1, got %s/%s/%d", cell, cellInstance, generation)
	}
}

// TestRepeatSameCellInstanceUnchanged: a repeat of the same key from the
// same cell instance answers granted with every column unchanged.
func TestRepeatSameCellInstanceUnchanged(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-same", 1)
	req.Cell = "cell-a"
	req.CellInstance = "pod-a"
	first := mustGrant(t, ledger, req)

	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want unchanged granted answer, got %+v (first %+v)", resp, first)
	}

	var cell, cellInstance string
	var generation int64
	if err := db.QueryRow(`
		SELECT cell, cell_instance, placement_generation FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&cell, &cellInstance, &generation); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cell != "cell-a" || cellInstance != "pod-a" || generation != 1 {
		t.Fatalf("want unchanged cell-a/pod-a/1, got %s/%s/%d", cell, cellInstance, generation)
	}
}

// readIntentOwnerAndNumber reads the ownership columns and the build
// number of the intent row for intentKey.
func readIntentOwnerAndNumber(t *testing.T, db *sql.DB, intentKey string) (cell, cellInstance string, generation, buildNumber int64) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT cell, cell_instance, placement_generation, build_number
		FROM varroa.run_intents WHERE intent_key = $1
	`, intentKey).Scan(&cell, &cellInstance, &generation, &buildNumber); err != nil {
		t.Fatalf("read intent row: %v", err)
	}
	return cell, cellInstance, generation, buildNumber
}

// readNextNumber reads job_numbers.next_number for the shared test job.
func readNextNumber(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var next int64
	if err := db.QueryRow(`
		SELECT next_number FROM varroa.job_numbers
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, testCluster, testTenant, testJobPath).Scan(&next); err != nil {
		t.Fatalf("read next_number: %v", err)
	}
	return next
}

// TestRepeatStaleNumberDifferentInstanceRefreshesAndMoves: a repeat of
// the same run from a new cell instance whose counter already passed the
// stored number moves ownership and receives a fresh number.
func TestRepeatStaleNumberDifferentInstanceRefreshesAndMoves(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-stale-move", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)

	repeat := req
	repeat.Cell = "cell-b"
	repeat.CellInstance = "pod-b"
	repeat.PlacementGeneration = 2
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != first.BuildNumber+1 {
		t.Fatalf("want granted run %q with build_number %d, got %+v", first.RunID, first.BuildNumber+1, resp)
	}
	cell, cellInstance, generation, number := readIntentOwnerAndNumber(t, db, req.IntentKey)
	if cell != "cell-b" || cellInstance != "pod-b" || generation != 2 || number != first.BuildNumber+1 {
		t.Fatalf("want cell-b/pod-b/2/%d, got %s/%s/%d/%d", first.BuildNumber+1, cell, cellInstance, generation, number)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// TestRepeatStaleNumberSameInstanceKeepsNumber: a repeat from the same
// cell instance with a passed counter answers the stored number and
// changes no column, because that counter passed the number by running
// the build.
func TestRepeatStaleNumberSameInstanceKeepsNumber(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-stale-same", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	before := readNextNumber(t, db)

	repeat := req
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want granted with stored build_number %d, got %+v", first.BuildNumber, resp)
	}
	cell, cellInstance, generation, number := readIntentOwnerAndNumber(t, db, req.IntentKey)
	if cell != "cell-a" || cellInstance != "pod-a" || generation != 1 || number != first.BuildNumber {
		t.Fatalf("want unchanged cell-a/pod-a/1/%d, got %s/%s/%d/%d", first.BuildNumber, cell, cellInstance, generation, number)
	}
	if after := readNextNumber(t, db); after != before {
		t.Fatalf("next_number moved from %d to %d", before, after)
	}
}

// TestRepeatStaleNumberMarkedAcceptedRowRefreshes: a runs row that holds
// only the accepted keys and the sweep's INTERRUPTED mark records no
// build, so it does not block the refresh.
func TestRepeatStaleNumberMarkedAcceptedRowRefreshes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-stale-marked", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	insertAcceptedOnlyRunsRow(t, db, first.RunID)
	if _, err := db.Exec(`
		UPDATE varroa.runs SET status = 'COMPLETED', result = 'INTERRUPTED',
		    payload = payload || '{"interrupted_reason":"cell-rolled"}'::jsonb
		WHERE run_id = $1
	`, first.RunID); err != nil {
		t.Fatalf("mark the accepted row interrupted: %v", err)
	}

	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != first.BuildNumber+1 {
		t.Fatalf("want granted with build_number %d, got %+v", first.BuildNumber+1, resp)
	}
	_, cellInstance, _, number := readIntentOwnerAndNumber(t, db, req.IntentKey)
	if cellInstance != "pod-b" || number != first.BuildNumber+1 {
		t.Fatalf("want pod-b/%d, got %s/%d", first.BuildNumber+1, cellInstance, number)
	}
}

// TestRepeatCurrentNumberReservesNothing: a repeat whose counter equals
// the stored number answers the stored number and reserves nothing.
func TestRepeatCurrentNumberReservesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-current", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	before := readNextNumber(t, db)

	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.JenkinsNextBuildNumber = first.BuildNumber

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want unchanged build_number %d, got %+v", first.BuildNumber, resp)
	}
	if after := readNextNumber(t, db); after != before {
		t.Fatalf("next_number moved from %d to %d", before, after)
	}
}

// TestRepeatZeroNextNumberNeverRefreshes: a request that carries no
// counter never refreshes, even when the stored number is stale.
func TestRepeatZeroNextNumberNeverRefreshes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-zero", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	// A second key on the same job moves the watermark past the first
	// number, so the stored number is stale by the watermark's account.
	mustGrant(t, ledger, newTestClaimRequest("manual:x-zero-other", 1))
	before := readNextNumber(t, db)

	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.JenkinsNextBuildNumber = 0

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want unchanged build_number %d, got %+v", first.BuildNumber, resp)
	}
	if after := readNextNumber(t, db); after != before {
		t.Fatalf("next_number moved from %d to %d", before, after)
	}
}

// TestRepeatWithoutInstanceNeverRefreshes: a repeat that names no cell
// instance keeps the stored number even with a passed counter, so every
// refreshed intent names an owner.
func TestRepeatWithoutInstanceNeverRefreshes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-no-instance", 1)
	req.CellInstance = ""
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	before := readNextNumber(t, db)

	repeat := req
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.BuildNumber != first.BuildNumber {
		t.Fatalf("want stored build_number %d, got %+v", first.BuildNumber, resp)
	}
	if after := readNextNumber(t, db); after != before {
		t.Fatalf("next_number moved from %d to %d", before, after)
	}
}

// TestRepeatConsumedIntentDuplicate: a repeat claim for a consumed
// intent from a new pod answers duplicate whatever counter it carries,
// and writes nothing to run_intents, job_numbers, or runs. A second build
// of a started run is what the answer prevents.
func TestRepeatConsumedIntentDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-consumed", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	if _, err := db.Exec(`UPDATE varroa.run_intents SET state = 'CONSUMED' WHERE intent_key = $1`, req.IntentKey); err != nil {
		t.Fatalf("consume intent: %v", err)
	}
	insertStubRunsRow(t, db, first.RunID)
	before := snapshotStartedRun(t, db, req.IntentKey, first.RunID)

	for _, instance := range []string{"pod-b", "pod-a"} {
		repeat := req
		repeat.CellInstance = instance
		repeat.JenkinsNextBuildNumber = first.BuildNumber + 5

		resp := ledger.Claim(context.Background(), repeat)
		if resp.Decision != DecisionDuplicate || resp.RunID != first.RunID || resp.BuildNumber != 0 {
			t.Fatalf("instance %s: want duplicate for run_id %q, got %+v", instance, first.RunID, resp)
		}
		assertStartedRunUnchanged(t, db, req.IntentKey, first.RunID, before)
	}
}

// TestRepeatRecordedBuildDuplicate: a GRANTED intent whose runs row
// records a build answers duplicate to a repeat from a new pod, and the
// answer leaves the real run's row, the intent, and the watermark alone.
func TestRepeatRecordedBuildDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-recorded", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	insertStubRunsRow(t, db, first.RunID)
	if _, err := db.Exec(`UPDATE varroa.runs SET build_number = $1 WHERE run_id = $2`, first.BuildNumber, first.RunID); err != nil {
		t.Fatalf("record the build: %v", err)
	}
	before := snapshotStartedRun(t, db, req.IntentKey, first.RunID)

	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 5

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionDuplicate || resp.RunID != first.RunID || resp.BuildNumber != 0 {
		t.Fatalf("want duplicate for run_id %q, got %+v", first.RunID, resp)
	}
	assertStartedRunUnchanged(t, db, req.IntentKey, first.RunID, before)
}

// TestRepeatQueuedRunRowIsNotStarted: a runs row that holds a QUEUED
// accepted trigger records no build, so a repeat from a new pod still
// answers granted and moves the owner. The duplicate answer must never
// mark that row DUPLICATE either.
func TestRepeatQueuedRunRowIsNotStarted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-accepted", 1)
	first := mustGrant(t, ledger, req)
	_, createdAt, err := parseRunUUIDv7(first.RunID)
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload)
		VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'QUEUED',
		        jsonb_build_object('accepted_at', now(), 'accepted_instance', 'pod-a'))
	`, first.RunID, createdAt, req.IntentKey, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert accepted trigger row: %v", err)
	}

	repeat := req
	repeat.Cell = "cell-b"
	repeat.CellInstance = "pod-b"

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted for run_id %q, got %+v", first.RunID, resp)
	}
	_, cellInstance, _, _ := readIntentOwnerAndNumber(t, db, req.IntentKey)
	if cellInstance != "pod-b" {
		t.Fatalf("want the owner moved to pod-b, got %s", cellInstance)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, first.RunID).Scan(&status); err != nil {
		t.Fatalf("read runs status: %v", err)
	}
	if status != "QUEUED" {
		t.Fatalf("want the accepted row still QUEUED, got %s", status)
	}
}

// TestRepeatUnstartedIntentFromNewPodStillGranted: the re-fire path sends
// the same run_id from a replacement pod for a GRANTED intent that no
// runs row records. The claim answers granted, moves the owner, and
// refreshes a stale number, so a re-fired build still runs.
func TestRepeatUnstartedIntentFromNewPodStillGranted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-refire", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)

	repeat := req
	repeat.Cell = "cell-b"
	repeat.CellInstance = "pod-new"
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID || resp.BuildNumber != first.BuildNumber+1 {
		t.Fatalf("want granted run %q with build_number %d, got %+v", first.RunID, first.BuildNumber+1, resp)
	}
	cell, cellInstance, _, number := readIntentOwnerAndNumber(t, db, req.IntentKey)
	if cell != "cell-b" || cellInstance != "pod-new" || number != first.BuildNumber+1 {
		t.Fatalf("want cell-b/pod-new/%d, got %s/%s/%d", first.BuildNumber+1, cell, cellInstance, number)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// TestConcurrentRepeatClaimsRefreshOnce: two repeats with the same
// passed counter serialize on the row lock, so one reservation serves
// both answers.
func TestConcurrentRepeatClaimsRefreshOnce(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-concurrent", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)

	repeat := req
	repeat.CellInstance = "pod-b"
	repeat.JenkinsNextBuildNumber = first.BuildNumber + 1

	var wg sync.WaitGroup
	answers := make([]ClaimResponse, 2)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i] = ledger.Claim(context.Background(), repeat)
		}(i)
	}
	wg.Wait()

	for i, resp := range answers {
		if resp.Decision != DecisionGranted || resp.BuildNumber != first.BuildNumber+1 {
			t.Fatalf("answer %d: want granted with build_number %d, got %+v", i, first.BuildNumber+1, resp)
		}
	}
	if next := readNextNumber(t, db); next != first.BuildNumber+2 {
		t.Fatalf("want next_number %d, got %d", first.BuildNumber+2, next)
	}
}
