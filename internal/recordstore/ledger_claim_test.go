package recordstore

import (
	"context"
	"database/sql"
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

// TestNewKeyGrantedWritesOneRow asserts a new key is granted and writes
// exactly one run_intents row.
func TestNewKeyGrantedWritesOneRow(t *testing.T) {
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

// TestRepeatConsumedIntentHigherGenerationUnchanged: a repeat of the
// stored run_id at a higher generation, for an intent run.started
// already consumed, answers granted with every column unchanged.
func TestRepeatConsumedIntentHigherGenerationUnchanged(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-consumed", 1)
	first := mustGrant(t, ledger, req)

	runID := uuid.MustParse(first.RunID)
	if err := consumeIntent(context.Background(), pool, runID, first.BuildNumber, time.Now().UTC()); err != nil {
		t.Fatalf("consume intent: %v", err)
	}

	repeat := req
	repeat.PlacementGeneration = 5
	repeat.CellInstance = "pod-different"

	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != first.RunID {
		t.Fatalf("want granted with unchanged run_id %q, got %+v", first.RunID, resp)
	}

	var cell, cellInstance, state string
	var generation int64
	if err := db.QueryRow(`
		SELECT cell, cell_instance, placement_generation, state FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&cell, &cellInstance, &generation, &state); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cell != req.Cell || cellInstance != req.CellInstance || generation != 1 || state != stateConsumed {
		t.Fatalf("want unchanged cell=%s cell_instance=%s generation=1 state=CONSUMED, got %s/%s/%d/%s",
			req.Cell, req.CellInstance, cell, cellInstance, generation, state)
	}
}

// TestRepeatDifferentCellInstanceMovesOwnership: a repeat of the stored
// run_id from a different cell instance, for a GRANTED intent with no
// runs row, answers granted with cell, cell_instance, and
// placement_generation moved to the request's values.
func TestRepeatDifferentCellInstanceMovesOwnership(t *testing.T) {
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
