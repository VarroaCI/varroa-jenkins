package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// claimExpiryNow is the fixed instant every claim-expiry test reads as
// the round's clock.
var claimExpiryNow = time.Now().UTC().Truncate(time.Microsecond)

// sumCounter sums every data point of metricName.
func sumCounter(t *testing.T, reader *sdkmetric.ManualReader, metricName string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricName {
				continue
			}
			if data, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// readStatusResult reads one run's status and result.
func readStatusResult(t *testing.T, db *sql.DB, runID uuid.UUID) (string, sql.NullString) {
	t.Helper()
	var status string
	var result sql.NullString
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &result); err != nil {
		t.Fatalf("read runs row: %v", err)
	}
	return status, result
}

// TestExpireUnclaimedRunsMarksAnAgedQueuedRun asserts a queued run past
// the age limit with no intent becomes TRIGGER_FAILED with reason
// CLAIM_EXPIRED, and writes one activity row, one notification, and one
// count.
func TestExpireUnclaimedRunsMarksAnAgedQueuedRun(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useTestMeterProvider(t)
	metrics, err := NewClaimExpiryMetrics()
	if err != nil {
		t.Fatalf("register metrics: %v", err)
	}
	notifier, sent, _ := newCaptureNotifier(t)

	accepted := claimExpiryNow.Add(-ClaimExpiryAge - time.Minute)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", accepted)

	n, err := ExpireUnclaimedRuns(context.Background(), pool, notifier, metrics, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}

	status, result := readStatusResult(t, db, runID)
	if status != StatusCompleted || result.String != ResultTriggerFailed {
		t.Fatalf("status = %q, result = %v, want COMPLETED and TRIGGER_FAILED", status, result)
	}
	var endedAt time.Time
	if err := db.QueryRow(`SELECT ended_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&endedAt); err != nil {
		t.Fatalf("read ended_at: %v", err)
	}
	if !endedAt.Equal(claimExpiryNow) {
		t.Fatalf("ended_at = %v, want %v", endedAt, claimExpiryNow)
	}

	payload := readRunPayload(t, db, runID.String())
	var code, msg string
	_ = json.Unmarshal(payload["trigger_reason_code"], &code)
	_ = json.Unmarshal(payload["trigger_error"], &msg)
	if code != ReasonClaimExpired {
		t.Fatalf("trigger_reason_code = %q, want %q", code, ReasonClaimExpired)
	}
	if msg == "" {
		t.Fatal("trigger_error is empty, want a message")
	}

	var activityRows int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.claim_expired' AND item_path = $1`,
		testJobPath).Scan(&activityRows); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityRows != 1 {
		t.Fatalf("build.claim_expired activity rows = %d, want 1", activityRows)
	}

	if sent.count() != 1 {
		t.Fatalf("notifications = %d, want 1", sent.count())
	}
	got := sent.payloads[0]
	if got.RunID != runID.String() || got.Status != StatusCompleted || got.Result != ResultTriggerFailed {
		t.Fatalf("notification = %+v, want the run COMPLETED with TRIGGER_FAILED", got)
	}

	if c := sumCounter(t, reader, "recorder_claim_expired_total"); c != 1 {
		t.Fatalf("recorder_claim_expired_total = %d, want 1", c)
	}

	again, err := ExpireUnclaimedRuns(context.Background(), pool, notifier, metrics, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if again != 0 {
		t.Fatalf("second round expired = %d, want 0", again)
	}
}

// TestExpireUnclaimedRunsLeavesAYoungRun asserts a queued run inside the
// age limit stays QUEUED.
func TestExpireUnclaimedRunsLeavesAYoungRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	accepted := claimExpiryNow.Add(-ClaimExpiryAge + time.Minute)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", accepted)

	n, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0", n)
	}
	if status, result := readStatusResult(t, db, runID); status != StatusQueued || result.Valid {
		t.Fatalf("status = %q, result = %v, want QUEUED and null", status, result)
	}
}

// TestExpireUnclaimedRunsLeavesARunWithAnIntent asserts the claim-expiry
// class never marks a run the ledger holds an intent for. A GRANTED
// intent with no build ends through the claim-abandoned class instead.
func TestExpireUnclaimedRunsLeavesARunWithAnIntent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	accepted := claimExpiryNow.Add(-3 * ClaimExpiryAge)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", accepted)
	req := newTestClaimRequest("manual:"+runID.String(), 0)
	req.RunID = runID.String()
	mustGrant(t, NewLedger(pool, nil), req)

	if _, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil); err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	payload := readRunPayload(t, db, runID.String())
	var code string
	_ = json.Unmarshal(payload["trigger_reason_code"], &code)
	if code != ReasonClaimAbandoned {
		t.Fatalf("trigger_reason_code = %q, want %q and never %q", code, ReasonClaimAbandoned, ReasonClaimExpired)
	}
}

// TestExpireUnclaimedRunsLeavesARunWithAResume asserts a queued run that
// holds a mesh resume record stays QUEUED. The mesh owns that run.
func TestExpireUnclaimedRunsLeavesARunWithAResume(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	accepted := claimExpiryNow.Add(-2 * ClaimExpiryAge)
	runID := seedAcceptedOnlyRunRowInstance(t, db, "pod-a", accepted)
	if _, err := db.Exec(`
		UPDATE varroa.runs
		SET payload = payload || jsonb_build_object('mesh', jsonb_build_object('resume',
			jsonb_build_object('arm_expires', $2::timestamptz)))
		WHERE run_id = $1
	`, runID, claimExpiryNow.Add(time.Minute)); err != nil {
		t.Fatalf("arm a resume: %v", err)
	}

	n, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired = %d, want 0", n)
	}
	if status, _ := readStatusResult(t, db, runID); status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", status)
	}
}

// TestExpireUnclaimedRunsBoundsOneRound asserts one round marks at most
// claimExpiryBatch runs, and the next round marks the rest.
func TestExpireUnclaimedRunsBoundsOneRound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	accepted := claimExpiryNow.Add(-2 * ClaimExpiryAge)
	total := claimExpiryBatch + 3
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload
		)
		SELECT gen_random_uuid(), $1, 'manual:batch-' || g, $2, $3, 'cell-a', $4, $5, 'QUEUED',
			jsonb_build_object('accepted_at', $1::timestamptz, 'accepted_instance', 'pod-a')
		FROM generate_series(1, $6::int) AS g
	`, accepted, testCluster, testNamespace, testTenant, testJobPath, total); err != nil {
		t.Fatalf("seed accepted runs: %v", err)
	}

	first, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if first != claimExpiryBatch {
		t.Fatalf("first round expired = %d, want %d", first, claimExpiryBatch)
	}
	second, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if second != total-claimExpiryBatch {
		t.Fatalf("second round expired = %d, want %d", second, total-claimExpiryBatch)
	}

	var activityRows int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE type = 'build.claim_expired'`).
		Scan(&activityRows); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if activityRows != total {
		t.Fatalf("build.claim_expired activity rows = %d, want one per run, %d", activityRows, total)
	}
}

// TestRunStartedSupersedesAnExpiredRun asserts a late claim and a late
// run.started replace an expired row with the real run.
func TestRunStartedSupersedesAnExpiredRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	intentKey := "manual:late-" + runID
	if _, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, runID, intentKey)); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE varroa.runs SET payload = payload || jsonb_build_object('accepted_at', $2::timestamptz)
		WHERE run_id = $1
	`, runID, claimExpiryNow.Add(-ClaimExpiryAge-time.Minute)); err != nil {
		t.Fatalf("age the accepted row: %v", err)
	}

	n, err := ExpireUnclaimedRuns(context.Background(), pool, nil, nil, claimExpiryNow, nil)
	if err != nil {
		t.Fatalf("expire unclaimed runs: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}

	req := newTestClaimRequest(intentKey, 0)
	req.RunID = runID
	mustGrant(t, NewLedger(pool, nil), req)

	parsed := uuid.MustParse(runID)
	buildNumber := int64(1)
	started := newTestEnvelope(t, parsed, intentKey, KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	status, result := readStatusResult(t, db, parsed)
	if status != StatusRunning || result.Valid {
		t.Fatalf("status = %q, result = %v, want RUNNING and null", status, result)
	}
	payload := readRunPayload(t, db, runID)
	for _, key := range []string{"trigger_reason_code", "trigger_error", "accepted_at"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload still holds %q after run.started", key)
		}
	}
}
