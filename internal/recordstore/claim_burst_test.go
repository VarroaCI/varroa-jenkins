package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// burstGrant claims one fresh intent on the shared test job and answers
// the run identifier and the granted number.
func burstGrant(t *testing.T, ledger *Ledger, intentKey string) (uuid.UUID, int64) {
	t.Helper()
	req := newTestClaimRequest(intentKey, 1)
	req.JenkinsNextBuildNumber = 1
	resp := mustGrant(t, ledger, req)
	return uuid.MustParse(resp.RunID), resp.BuildNumber
}

// startWith applies one run.started envelope that reports number.
func startWith(t *testing.T, g *Ingester, runID uuid.UUID, intentKey string, number int64) ApplyResult {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{BuildNumber: &number})
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.started for %s at %d: %v", runID, number, err)
	}
	return res
}

// readRunNumber answers runs.build_number of one run, nil for NULL.
func readRunNumber(t *testing.T, db *sql.DB, runID uuid.UUID) *int64 {
	t.Helper()
	var n *int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("read runs.build_number: %v", err)
	}
	return n
}

// TestClaimBurstStartsInAnotherOrderKeepEveryNumber pins the burst shape:
// three runs of one job hold grants in claim order, and Jenkins starts
// them with the same numbers in another order. The number Jenkins used
// is the truth, so every run keeps its reported number, and a run whose
// grant another run took gets no number until it starts.
func TestClaimBurstStartsInAnotherOrderKeepEveryNumber(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	runA, nA := burstGrant(t, ledger, "manual:burst-a")
	runB, nB := burstGrant(t, ledger, "manual:burst-b")
	runC, nC := burstGrant(t, ledger, "manual:burst-c")

	// A starts with B's number while B is still GRANTED and unstarted.
	if res := startWith(t, g, runA, "manual:burst-a", nB); res.Conflict {
		t.Fatalf("A took an unstarted grant, want no conflict, got %+v", res)
	}
	if got := readRunNumber(t, db, runA); got == nil || *got != nB {
		t.Fatalf("A runs.build_number = %v, want %d", got, nB)
	}
	if state, number := readIntentStateAndNumber(t, db, runB); state != stateGranted || number != nil {
		t.Fatalf("B intent = %s/%v, want GRANTED with a null number", state, number)
	}

	// B starts with A's old number, which is free now.
	if res := startWith(t, g, runB, "manual:burst-b", nA); res.Conflict {
		t.Fatalf("B at a free number, want no conflict, got %+v", res)
	}
	if got := readRunNumber(t, db, runB); got == nil || *got != nA {
		t.Fatalf("B runs.build_number = %v, want %d", got, nA)
	}

	// C starts with its own grant.
	if res := startWith(t, g, runC, "manual:burst-c", nC); res.Conflict {
		t.Fatalf("C at its own grant, want no conflict, got %+v", res)
	}
	for _, id := range []uuid.UUID{runA, runB, runC} {
		if got := readRunNumber(t, db, id); got == nil {
			t.Fatalf("run %s holds a null build number", id)
		}
	}
}

// TestClaimBurstStartOverAConsumedNumberStoresNull pins the one case
// that keeps a null number: another run already started with the
// reported number, so two builds claim one number and the recorder cannot
// tell which one Jenkins numbered correctly.
func TestClaimBurstStartOverAConsumedNumberStoresNull(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	runA, nA := burstGrant(t, ledger, "manual:burst-consumed-a")
	runB, _ := burstGrant(t, ledger, "manual:burst-consumed-b")

	startWith(t, g, runA, "manual:burst-consumed-a", nA)
	res := startWith(t, g, runB, "manual:burst-consumed-b", nA)
	if !res.Conflict {
		t.Fatalf("want the conflict disposition, got %+v", res)
	}
	if res.ConflictRunID != runA.String() || res.ConflictNumber != nA {
		t.Fatalf("want the conflict to name run %s at %d, got %s at %d", runA, nA, res.ConflictRunID, res.ConflictNumber)
	}
	if got := readRunNumber(t, db, runB); got != nil {
		t.Fatalf("B runs.build_number = %d, want null", *got)
	}
}

// TestReanswerOfATakenGrantReservesAFreshNumber pins the re-answer of a
// run whose grant another run took at run.started: the intent holds no
// number, so a repeat claim reserves a fresh one.
func TestReanswerOfATakenGrantReservesAFreshNumber(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	reqA := newTestClaimRequest("manual:taken-a", 1)
	reqA.JenkinsNextBuildNumber = 1
	a := mustGrant(t, ledger, reqA)
	reqB := newTestClaimRequest("manual:taken-b", 1)
	reqB.JenkinsNextBuildNumber = 1
	b := mustGrant(t, ledger, reqB)

	startWith(t, g, uuid.MustParse(a.RunID), reqA.IntentKey, b.BuildNumber)

	repeat := reqB
	repeat.JenkinsNextBuildNumber = b.BuildNumber + 1
	resp := ledger.Claim(context.Background(), repeat)
	if resp.Decision != DecisionGranted || resp.RunID != b.RunID {
		t.Fatalf("want granted for run %s, got %+v", b.RunID, resp)
	}
	if resp.BuildNumber <= b.BuildNumber {
		t.Fatalf("want a fresh number above %d, got %d", b.BuildNumber, resp.BuildNumber)
	}
	if _, number := readIntentStateAndNumber(t, db, uuid.MustParse(b.RunID)); number == nil || *number != resp.BuildNumber {
		t.Fatalf("B intent number = %v, want %d", number, resp.BuildNumber)
	}
}

// TestNumberConflictReclaimRefreshesOnTheSameInstance pins the re-claim a
// NUMBER_CONFLICT sends. The cell cannot apply the stored number, because
// its counter already passed it. A plain repeat from the same instance
// answers the stale number again; a repeat that names the number as a
// conflict gets a fresh one, and the old number stays burned.
func TestNumberConflictReclaimRefreshesOnTheSameInstance(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:number-conflict", 1)
	req.JenkinsNextBuildNumber = 1
	first := mustGrant(t, ledger, req)
	// Later claims move the watermark past the stored number.
	burstGrant(t, ledger, "manual:number-conflict-later-1")
	burstGrant(t, ledger, "manual:number-conflict-later-2")

	plain := req
	plain.JenkinsNextBuildNumber = first.BuildNumber + 2
	if resp := mustGrant(t, ledger, plain); resp.BuildNumber != first.BuildNumber {
		t.Fatalf("a plain same-instance repeat: want the stored %d, got %d", first.BuildNumber, resp.BuildNumber)
	}

	conflict := plain
	conflict.NumberConflict = first.BuildNumber
	resp := mustGrant(t, ledger, conflict)
	if resp.RunID != first.RunID || resp.BuildNumber <= first.BuildNumber+2 {
		t.Fatalf("want run %s refreshed above %d, got %+v", first.RunID, first.BuildNumber+2, resp)
	}
	if _, number := readIntentStateAndNumber(t, db, uuid.MustParse(first.RunID)); number == nil || *number != resp.BuildNumber {
		t.Fatalf("intent number = %v, want %d", number, resp.BuildNumber)
	}

	// A second re-claim that names the old number again changes nothing.
	again := mustGrant(t, ledger, conflict)
	if again.BuildNumber != resp.BuildNumber {
		t.Fatalf("a repeat naming the burned number: want %d, got %d", resp.BuildNumber, again.BuildNumber)
	}
}

// TestRunStartedCountsAGrantMismatch pins the granted-number field: a
// run.started that reports a number other than its grant is flagged, and
// the field changes no stored value.
func TestRunStartedCountsAGrantMismatch(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	runA, nA := burstGrant(t, ledger, "manual:mismatch-a")
	reported := nA + 5
	env := newTestEnvelope(t, runA, "manual:mismatch-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &reported, GrantedBuildNumber: &nA,
	})
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.NumberMismatch || res.MismatchGranted != nA || res.MismatchReported != reported {
		t.Fatalf("want a mismatch of %d against %d, got %+v", reported, nA, res)
	}
	if got := readRunNumber(t, db, runA); got == nil || *got != reported {
		t.Fatalf("runs.build_number = %v, want %d", got, reported)
	}

	runB, nB := burstGrant(t, ledger, "manual:mismatch-b")
	match := startWith(t, g, runB, "manual:mismatch-b", nB)
	if match.NumberMismatch {
		t.Fatalf("a start with no granted field flags no mismatch, got %+v", match)
	}
}

// TestExpireUnclaimedRunsEndsAnAbandonedGrant pins the claim-abandoned
// class: an aged queued run with a GRANTED intent and no build ends as
// TRIGGER_FAILED with CLAIM_ABANDONED, and a recent one stays queued.
func TestExpireUnclaimedRunsEndsAnAbandonedGrant(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	mp, reader := newTestMeterProvider(t)
	metrics, err := NewClaimExpiryMetrics(mp)
	if err != nil {
		t.Fatalf("register metrics: %v", err)
	}
	ledger := NewLedger(pool, nil)

	seed := func(age time.Duration) uuid.UUID {
		runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", claimExpiryNow.Add(-age))
		req := newTestClaimRequest("manual:"+runID.String(), 0)
		req.RunID = runID.String()
		mustGrant(t, ledger, req)
		backdateGrant(t, db, runID, claimExpiryNow.Add(-age))
		return runID
	}
	aged := seed(ClaimExpiryAge + time.Minute)
	recent := seed(ClaimExpiryAge - time.Minute)

	n, err := ExpireUnclaimedRuns(context.Background(), pool, nil, metrics, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	if status, result := readStatusResult(t, db, aged); status != StatusCompleted || result.String != ResultTriggerFailed {
		t.Fatalf("aged run = %s/%v, want COMPLETED/TRIGGER_FAILED", status, result)
	}
	payload := readRunPayload(t, db, aged.String())
	var code string
	_ = json.Unmarshal(payload["trigger_reason_code"], &code)
	if code != ReasonClaimAbandoned {
		t.Fatalf("trigger_reason_code = %q, want %q", code, ReasonClaimAbandoned)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.claim_abandoned'`).Scan(&rows); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("build.claim_abandoned rows = %d, want 1", rows)
	}
	if status, _ := readStatusResult(t, db, recent); status != StatusQueued {
		t.Fatalf("recent run = %s, want QUEUED", status)
	}
	if c := sumCounter(t, reader, "recorder_claim_expired_total"); c != 1 {
		t.Fatalf("recorder_claim_expired_total = %d, want 1", c)
	}
}

// TestExpireUnclaimedRunsSharesOneBatch pins the shared batch: one round
// marks at most one batch over both classes, and the abandoned class comes
// first, so a full backlog of unclaimed runs never starves it.
func TestExpireUnclaimedRunsSharesOneBatch(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	accepted := claimExpiryNow.Add(-2 * ClaimExpiryAge)
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload
		)
		SELECT gen_random_uuid(), $1, 'manual:shared-' || g, $2, $3, 'cell-a', $4, $5, 'QUEUED',
			jsonb_build_object('accepted_at', $1::timestamptz, 'accepted_instance', 'pod-a')
		FROM generate_series(1, $6::int) AS g
	`, accepted, testCluster, testNamespace, testTenant, testJobPath, claimExpiryBatch); err != nil {
		t.Fatalf("seed accepted runs: %v", err)
	}
	abandoned := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", accepted)
	req := newTestClaimRequest("manual:"+abandoned.String(), 0)
	req.RunID = abandoned.String()
	mustGrant(t, NewLedger(pool, nil), req)
	backdateGrant(t, db, abandoned, accepted)

	first, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if first != claimExpiryBatch {
		t.Fatalf("first round marked %d, want %d", first, claimExpiryBatch)
	}
	if status, _ := readStatusResult(t, db, abandoned); status != StatusCompleted {
		t.Fatalf("abandoned run after the first round = %s, want COMPLETED", status)
	}
	second, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if second != 1 {
		t.Fatalf("second round marked %d, want 1", second)
	}
}

// TestTheLedgerKeepsNoGrantAfterTheStart pins how to read the ledger
// after a burst. A run.started overwrites the intent's granted number with
// the reported one, and raises job_numbers to the highest started number
// plus one. So neither the intents nor the watermark count the grants:
// three grants from 1 that start as 16, 17, and 18 leave the intents at
// 16..18 and the watermark at 19.
func TestTheLedgerKeepsNoGrantAfterTheStart(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)

	runs := make([]uuid.UUID, 0, 3)
	for i, key := range []string{"manual:read-a", "manual:read-b", "manual:read-c"} {
		run, n := burstGrant(t, ledger, key)
		if n != int64(i+1) {
			t.Fatalf("grant %d = %d, want %d", i, n, i+1)
		}
		runs = append(runs, run)
	}
	for i, key := range []string{"manual:read-a", "manual:read-b", "manual:read-c"} {
		startWith(t, g, runs[i], key, int64(16+i))
	}
	for i, run := range runs {
		if _, n := readIntentStateAndNumber(t, db, run); n == nil || *n != int64(16+i) {
			t.Fatalf("intent %d holds %v, want %d", i, n, 16+i)
		}
	}
	if next := readNextNumber(t, db); next != 19 {
		t.Fatalf("next_number = %d, want 19", next)
	}
}
