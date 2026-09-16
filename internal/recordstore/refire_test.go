package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// refireDeadInstance is the cell instance every re-fire test names as the
// confirmed-gone owner of its granted intent.
const refireDeadInstance = "pod-dead"

// refiredCountBy sums every recorder_intent_refired_total data point
// carrying the given outcome and reason attributes.
func refiredCountBy(t *testing.T, reader *sdkmetric.ManualReader, outcome, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "recorder_intent_refired_total" {
				continue
			}
			data, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				gotOutcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				gotReason, _ := dp.Attributes.Value(attribute.Key("reason"))
				if gotOutcome.AsString() == outcome && gotReason.AsString() == reason {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// seedRefireIntent inserts one GRANTED run_intents row with no runs row
// behind it: the shape of a build whose trigger never started.
func seedRefireIntent(t *testing.T, db *sql.DB, intentKey, cellInstance, paramsDigest string) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	var digest any
	if paramsDigest != "" {
		digest = paramsDigest
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents (
			intent_key, run_id, created_at, cluster, namespace, tenant, job_path,
			cell, cell_instance, placement_generation, state, actor, params_digest)
		VALUES ($1, $2, now(), $3, $4, $5, $6, 'cell-old', $7, 1, 'GRANTED', 'alice', $8)
	`, intentKey, runID, testCluster, testNamespace, testTenant, testJobPath, cellInstance, digest); err != nil {
		t.Fatalf("seed granted intent: %v", err)
	}
	return runID
}

// refireCurrentCell is the cell every re-fire test's placement row names
// as the tenant's current one, after the dead instance's own cell.
const refireCurrentCell = "cell-new"

// seedRefirePlacement inserts the tenant's placement row, naming
// refireCurrentCell, cellInstance, and rehydratedInstance, with
// refireDeadInstance listed dead and confirmed gone when confirmed is
// true. A rehydratedInstance equal to cellInstance is a cell that has
// applied the tenant's seed and therefore holds a Jenkins token.
func seedRefirePlacement(t *testing.T, db *sql.DB, cellInstance, rehydratedInstance string, confirmed bool) {
	t.Helper()
	now := time.Now().UTC()
	entry := TenantSyncDeadInstance{Instance: refireDeadInstance, LastSeenAt: now.Add(-time.Minute)}
	if confirmed {
		at := now.Add(-45 * time.Second)
		entry.ConfirmedGoneAt = &at
	}
	deadJSON, err := json.Marshal([]TenantSyncDeadInstance{entry})
	if err != nil {
		t.Fatalf("marshal dead instances: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance,
			 cell_alive, dead_instances)
		VALUES ($1, $2, $3, $4, 1, $5, $6, true, $7::jsonb)
	`, testCluster, testTenant, testNamespace, refireCurrentCell, cellInstance, rehydratedInstance,
		string(deadJSON)); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
}

// refiredAt reads one intent's re-fire marker.
func refiredAt(t *testing.T, db *sql.DB, intentKey string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := db.QueryRow(`SELECT refired_at FROM varroa.run_intents WHERE intent_key = $1`,
		intentKey).Scan(&at); err != nil {
		t.Fatalf("read refired_at: %v", err)
	}
	return at
}

// collectActions subscribes to the cluster's ledger-action subject and
// answers a channel of every action published on it.
func collectActions(t *testing.T, conn *bus.Conn) chan LedgerAction {
	t.Helper()
	received := make(chan LedgerAction, 8)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()
	return received
}

// runRefire runs one re-fire round for the test tenant.
func runRefire(t *testing.T, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics) int {
	t.Helper()
	published, err := RefireLostIntents(context.Background(), pool, conn, metrics, nil,
		testCluster, testTenant, testGrace, time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("RefireLostIntents: %v", err)
	}
	return published
}

// TestRefirePublishesAndMarks asserts the happy path: a granted,
// parameterless intent whose owner is confirmed gone publishes one
// trigger_run action carrying the stored identity and naming the
// tenant's CURRENT cell, and stamps refired_at.
func TestRefirePublishesAndMarks(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	if got := runRefire(t, pool, conn, metrics); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionTriggerRun {
			t.Fatalf("kind = %q, want %q", a.Kind, LedgerActionTriggerRun)
		}
		if a.IntentKey != intentKey || a.RunID != runID.String() {
			t.Fatalf("action = %+v, want the stored intent key %q and run id %s", a, intentKey, runID)
		}
		if a.Controller != refireCurrentCell || a.Namespace != testNamespace {
			t.Fatalf("action names %s/%s, want the tenant's current cell %s/%s",
				a.Namespace, a.Controller, testNamespace, refireCurrentCell)
		}
		if a.JobPath != testJobPath || a.Actor != "alice" {
			t.Fatalf("action = %+v, want job path %q and actor alice", a, testJobPath)
		}
		if len(a.Parameters) != 0 {
			t.Fatalf("parameters = %s, want none", a.Parameters)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want one trigger_run action")
	}

	if refiredAt(t, db, intentKey) == nil {
		t.Fatal("want refired_at set after the publish")
	}
	if got := refiredCountBy(t, reader, refireOutcomePublished, refireReasonNone); got != 1 {
		t.Fatalf("published outcome count = %d, want 1", got)
	}
}

// TestRefireSecondRoundPublishesNothing asserts the at-most-once rule: a
// second round over the same intent publishes no second action, because
// refired_at is set.
func TestRefireSecondRoundPublishesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	intentKey := "manual:" + newTestUUIDv7String()
	seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	if got := runRefire(t, pool, conn, nil); got != 1 {
		t.Fatalf("first round published = %d, want 1", got)
	}
	<-received

	if got := runRefire(t, pool, conn, nil); got != 0 {
		t.Fatalf("second round published = %d, want 0", got)
	}
	select {
	case a := <-received:
		t.Fatalf("want no second action, got %+v", a)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRefireClearsTheMarkerOnAPublishFailure asserts the rollback: a
// publish that fails must leave refired_at null, because the candidate
// query, the round's own tenant list, and the dead-instances trim all
// filter on that column. A marker left behind would lose the queued
// build forever. A later round with a live connection then publishes.
func TestRefireClearsTheMarkerOnAPublishFailure(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)

	intentKey := "manual:" + newTestUUIDv7String()
	seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	// A closed connection is the publish-failure seam: PublishLedgerAction
	// answers "nats: connection closed" and writes no message.
	deadConn := newTestActionConn(t)
	deadConn.Close()
	if got := runRefire(t, pool, deadConn, metrics); got != 0 {
		t.Fatalf("published = %d, want 0 on a failed publish", got)
	}
	if at := refiredAt(t, db, intentKey); at != nil {
		t.Fatalf("refired_at = %v, want null: a failed publish must roll the marker back", at)
	}
	if got := refiredCountBy(t, reader, refireOutcomeSkipped, refireReasonPublishFailed); got != 1 {
		t.Fatalf("skipped/publish-failed count = %d, want 1", got)
	}

	// The candidate query selects the intent again.
	candidates, err := selectRefireCandidates(context.Background(), pool, testCluster, testTenant)
	if err != nil {
		t.Fatalf("selectRefireCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].intentKey != intentKey {
		t.Fatalf("candidates = %+v, want the rolled-back intent %q", candidates, intentKey)
	}
	pairs, err := listRefirePairs(context.Background(), pool)
	if err != nil {
		t.Fatalf("listRefirePairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("pairs = %+v, want the tenant still listed for a later round", pairs)
	}

	// A later round with a live connection publishes and marks.
	liveConn := newTestActionConn(t)
	received := collectActions(t, liveConn)
	if got := runRefire(t, pool, liveConn, metrics); got != 1 {
		t.Fatalf("published = %d, want 1 on the retry round", got)
	}
	select {
	case a := <-received:
		if a.IntentKey != intentKey {
			t.Fatalf("action intent key = %q, want %q", a.IntentKey, intentKey)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want one trigger_run action from the retry round")
	}
	if refiredAt(t, db, intentKey) == nil {
		t.Fatal("want refired_at set after the retry round's publish")
	}
}

// TestRefireMarkerAdmitsOneWriter asserts two concurrent markers over one
// intent leave exactly one winner, which is what makes two recorder
// replicas publish one action between them.
func TestRefireMarkerAdmitsOneWriter(t *testing.T) {
	pool, db := setupLedgerPool(t)

	intentKey := "manual:" + newTestUUIDv7String()
	seedRefireIntent(t, db, intentKey, refireDeadInstance, "")

	now := time.Now().UTC()
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() {
			marked, err := markIntentRefired(context.Background(), pool, intentKey, now)
			if err != nil {
				results <- false
				return
			}
			results <- marked
		}()
	}
	winners := 0
	for i := 0; i < 2; i++ {
		if <-results {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

// TestRefireRefusalRules asserts the four refusal rules. A live cell
// re-fires nothing. A parameterized intent is skipped and counted. A
// placement with no live instance defers and leaves refired_at null. A
// started build's runs row blocks the re-fire.
func TestRefireRefusalRules(t *testing.T) {
	t.Run("a live cell re-fires nothing", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		conn := newTestActionConn(t)

		intentKey := "manual:" + newTestUUIDv7String()
		seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
		// The entry carries no confirmation, so the cell is not proved gone.
		seedRefirePlacement(t, db, "pod-new", "pod-new", false)

		if got := runRefire(t, pool, conn, nil); got != 0 {
			t.Fatalf("published = %d, want 0", got)
		}
		if refiredAt(t, db, intentKey) != nil {
			t.Fatal("want refired_at null: no confirmation, no re-fire")
		}
	})

	t.Run("a parameterized intent is skipped", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		reader := useManualMeter(t)
		conn := newTestActionConn(t)

		intentKey := "manual:" + newTestUUIDv7String()
		seedRefireIntent(t, db, intentKey, refireDeadInstance, "deadbeefdeadbeefdeadbeefdeadbeef")
		seedRefirePlacement(t, db, "pod-new", "pod-new", true)

		metrics, err := NewInterruptedMetrics()
		if err != nil {
			t.Fatalf("new interrupted metrics: %v", err)
		}
		if got := runRefire(t, pool, conn, metrics); got != 0 {
			t.Fatalf("published = %d, want 0", got)
		}
		if refiredAt(t, db, intentKey) != nil {
			t.Fatal("want refired_at null for a parameterized intent")
		}
		if got := refiredCountBy(t, reader, refireOutcomeSkipped, refireReasonParams); got != 1 {
			t.Fatalf("skipped/parameterized count = %d, want 1", got)
		}
	})

	t.Run("an empty parameter digest still re-fires", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		conn := newTestActionConn(t)

		intentKey := "manual:" + newTestUUIDv7String()
		seedRefireIntent(t, db, intentKey, refireDeadInstance, emptyParamsDigest)
		seedRefirePlacement(t, db, "pod-new", "pod-new", true)

		if got := runRefire(t, pool, conn, nil); got != 1 {
			t.Fatalf("published = %d, want 1: the empty-parameter digest proves no parameters", got)
		}
	})

	t.Run("a missing live instance defers the re-fire", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		reader := useManualMeter(t)
		conn := newTestActionConn(t)

		intentKey := "manual:" + newTestUUIDv7String()
		seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
		// The operator clears its stored instance at the confirmation.
		seedRefirePlacement(t, db, "", "", true)

		metrics, err := NewInterruptedMetrics()
		if err != nil {
			t.Fatalf("new interrupted metrics: %v", err)
		}
		if got := runRefire(t, pool, conn, metrics); got != 0 {
			t.Fatalf("published = %d, want 0", got)
		}
		if refiredAt(t, db, intentKey) != nil {
			t.Fatal("want refired_at null: rule 3 defers a re-fire, it never drops one")
		}
		if got := refiredCountBy(t, reader, refireOutcomeSkipped, refireReasonNoPlace); got != 1 {
			t.Fatalf("skipped/no-placement count = %d, want 1", got)
		}

		// A replacement pod reports a new instance, but the cell has not
		// applied the tenant's seed yet, so its mite holds no Jenkins
		// token. The re-fire still defers, now on the rehydration test.
		if _, err := db.Exec(`
			UPDATE varroa.tenant_placements SET cell_instance = 'pod-new' WHERE cluster = $1 AND tenant = $2
		`, testCluster, testTenant); err != nil {
			t.Fatalf("report the replacement instance: %v", err)
		}
		if got := runRefire(t, pool, conn, metrics); got != 0 {
			t.Fatalf("published = %d, want 0: the replacement cell is not rehydrated yet", got)
		}
		if refiredAt(t, db, intentKey) != nil {
			t.Fatal("want refired_at null while the cell is not rehydrated")
		}
		if got := refiredCountBy(t, reader, refireOutcomeSkipped, refireReasonNotRehydrated); got != 1 {
			t.Fatalf("skipped/not-rehydrated count = %d, want 1", got)
		}

		// The cell applies the seed; the next round fires.
		if _, err := db.Exec(`
			UPDATE varroa.tenant_placements SET rehydrated_instance = 'pod-new'
			WHERE cluster = $1 AND tenant = $2
		`, testCluster, testTenant); err != nil {
			t.Fatalf("report the applied seed: %v", err)
		}
		if got := runRefire(t, pool, conn, metrics); got != 1 {
			t.Fatalf("published = %d, want 1 once the cell reports the applied seed", got)
		}
	})

	t.Run("a started build is never re-fired", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		conn := newTestActionConn(t)

		intentKey := "manual:" + newTestUUIDv7String()
		runID := seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
		seedRefirePlacement(t, db, "pod-new", "pod-new", true)
		// The ingest lag keeps the intent GRANTED while the build runs.
		if _, err := db.Exec(`
			INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
			VALUES ($1, now(), $2, $3, $4, 'cell-old', $5, $6, 'RUNNING')
		`, runID, intentKey, testCluster, testNamespace, testTenant, testJobPath); err != nil {
			t.Fatalf("insert running run: %v", err)
		}

		if got := runRefire(t, pool, conn, nil); got != 0 {
			t.Fatalf("published = %d, want 0: a started build must never be re-fired", got)
		}
		if refiredAt(t, db, intentKey) != nil {
			t.Fatal("want refired_at null for a started build")
		}
	})
}

// refireAttempts reads one intent's attempt counter.
func refireAttempts(t *testing.T, db *sql.DB, intentKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT refire_attempts FROM varroa.run_intents WHERE intent_key = $1`,
		intentKey).Scan(&n); err != nil {
		t.Fatalf("read refire_attempts: %v", err)
	}
	return n
}

// ingestRejectedTrigger applies one run.trigger_failed envelope for runID
// with the given reason code and HTTP status.
func ingestRejectedTrigger(t *testing.T, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	runID uuid.UUID, intentKey, reasonCode string, status int) {
	t.Helper()
	raw, err := json.Marshal(RunTriggerFailedPayload{
		Error:      "trigger build " + testJobPath + ": rejected",
		ReasonCode: reasonCode,
		HTTPStatus: status,
		Actor:      "user:alice",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetRefireMetrics(metrics)
	if _, err := ingester.Apply(context.Background(), IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunTriggerFailed,
		RunID:         runID.String(),
		IntentKey:     intentKey,
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          refireCurrentCell,
		CellInstance:  "pod-new",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}
}

// TestRefireRejectedTriggerClearsTheMarker asserts the self-heal. A
// re-fired trigger that a cell rejects is transient: the cell answered,
// so a later round can retry. The ingest therefore clears the marker in
// the same transaction that records the failure, and the next round
// re-selects the intent.
func TestRefireRejectedTriggerClearsTheMarker(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	if got := runRefire(t, pool, conn, metrics); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}
	<-received
	if refiredAt(t, db, intentKey) == nil {
		t.Fatal("want refired_at set after the publish")
	}
	if got := refireAttempts(t, db, intentKey); got != 1 {
		t.Fatalf("refire_attempts = %d, want 1", got)
	}

	ingestRejectedTrigger(t, pool, metrics, runID, intentKey, reasonTriggerRejected, 401)

	if at := refiredAt(t, db, intentKey); at != nil {
		t.Fatalf("refired_at = %v, want null: a rejected trigger must clear the marker", at)
	}
	if got := refireAttempts(t, db, intentKey); got != 1 {
		t.Fatalf("refire_attempts = %d, want 1: the clear must never lower the counter", got)
	}
	if got := refiredCountBy(t, reader, refireOutcomeCleared, refireReasonRejectedTemp); got != 1 {
		t.Fatalf("cleared/rejected-transient count = %d, want 1", got)
	}

	// The next round re-selects the intent and publishes again.
	if got := runRefire(t, pool, conn, metrics); got != 1 {
		t.Fatalf("retry round published = %d, want 1", got)
	}
	if got := refireAttempts(t, db, intentKey); got != 2 {
		t.Fatalf("refire_attempts = %d, want 2 after the retry", got)
	}
}

// TestRefirePermanentRejectionKeepsTheMarker asserts a permanent reason
// keeps the marker. No retry fixes a job that is absent or disabled, so
// the intent stays for a person.
func TestRefirePermanentRejectionKeepsTheMarker(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	if got := runRefire(t, pool, conn, metrics); got != 1 {
		t.Fatalf("published = %d, want 1", got)
	}
	<-received

	ingestRejectedTrigger(t, pool, metrics, runID, intentKey, "JOB_NOT_FOUND", 404)

	if refiredAt(t, db, intentKey) == nil {
		t.Fatal("want refired_at kept: a permanent rejection ends the retry loop")
	}
	if got := refiredCountBy(t, reader, refireOutcomeSkipped, refireReasonRejectedPerm); got != 1 {
		t.Fatalf("skipped/rejected-permanent count = %d, want 1", got)
	}
	if got := runRefire(t, pool, conn, metrics); got != 0 {
		t.Fatalf("published = %d, want 0 after a permanent rejection", got)
	}
}

// TestRefireAttemptBoundStopsTheSixthAttempt asserts the retry loop is
// bounded. A retried run keeps its own run identifier, so the runs table
// holds one row for every attempt; refire_attempts is the only bound.
func TestRefireAttemptBoundStopsTheSixthAttempt(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := useManualMeter(t)
	conn := newTestActionConn(t)
	received := collectActions(t, conn)

	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	for i := 1; i <= maxRefireAttempts; i++ {
		if got := runRefire(t, pool, conn, metrics); got != 1 {
			t.Fatalf("attempt %d published = %d, want 1", i, got)
		}
		<-received
		if got := refireAttempts(t, db, intentKey); got != i {
			t.Fatalf("after attempt %d refire_attempts = %d, want %d", i, got, i)
		}
		ingestRejectedTrigger(t, pool, metrics, runID, intentKey, reasonTriggerRejected, 401)
	}

	// The fifth rejection finds the attempts spent, so it keeps the
	// marker instead of clearing it, and the sixth attempt never runs.
	if refiredAt(t, db, intentKey) == nil {
		t.Fatal("want refired_at kept once the attempts are spent")
	}
	if got := refiredCountBy(t, reader, refireOutcomeSkipped, refireReasonExhausted); got != 1 {
		t.Fatalf("skipped/attempts-exhausted count = %d, want 1", got)
	}
	if got := runRefire(t, pool, conn, metrics); got != 0 {
		t.Fatalf("published = %d, want 0: the bound must stop the sixth attempt", got)
	}
	if got := refireAttempts(t, db, intentKey); got != maxRefireAttempts {
		t.Fatalf("refire_attempts = %d, want %d", got, maxRefireAttempts)
	}
}

// TestRefireAndMarkAreIndependent asserts one round can mark a QUEUED run
// cell-lost and re-fire its intent. The run_intents row keeps its state
// and its intent key, so a following claim from the new instance answers
// granted with the same run identifier.
func TestRefireAndMarkAreIndependent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)
	collectActions(t, conn)

	now := time.Now().UTC()
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRefireIntent(t, db, intentKey, refireDeadInstance, "")
	// The queued run the sweep marks, sharing the intent's run identifier.
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload
		) VALUES ($1, $2, $3, $4, $5, 'cell-old', $6, $7, 'QUEUED',
			jsonb_build_object('accepted_at', $2::timestamptz, 'accepted_instance', $8::text))
	`, runID, now.Add(-40*time.Second), intentKey, testCluster, testNamespace, testTenant,
		testJobPath, refireDeadInstance); err != nil {
		t.Fatalf("insert queued run: %v", err)
	}
	seedRefirePlacement(t, db, "pod-new", "pod-new", true)

	// Mark the run, then re-fire its intent, as one round does.
	c := runInterruptCandidate{
		runID: runID, createdAt: now.Add(-40 * time.Second), cluster: testCluster, namespace: testNamespace,
		cell: "cell-old", tenant: testTenant, jobPath: testJobPath, status: StatusQueued,
		cellInstance: refireDeadInstance,
	}
	changed, err := markInterrupted(context.Background(), pool, c, ReasonCellLost, time.Now())
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if !changed {
		t.Fatal("want the queued run marked")
	}
	if got := runRefire(t, pool, conn, nil); got != 1 {
		t.Fatalf("published = %d, want 1: the mark must not block the re-fire", got)
	}

	// The intent survived the mark.
	var state, storedKey string
	if err := db.QueryRow(`SELECT state, intent_key FROM varroa.run_intents WHERE run_id = $1`,
		runID).Scan(&state, &storedKey); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if state != "GRANTED" || storedKey != intentKey {
		t.Fatalf("state=%q intent_key=%q, want GRANTED and %q", state, storedKey, intentKey)
	}

	// The re-fired build claims with the stored identity from the new
	// instance, and the ledger re-answers the same run.
	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest(intentKey, 1)
	req.RunID = runID.String()
	req.Cell = "cell-new"
	req.CellInstance = "pod-new"
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("claim = %+v, want granted", resp)
	}
	if resp.RunID != runID.String() {
		t.Fatalf("run id = %q, want the stored %s", resp.RunID, runID)
	}
}
