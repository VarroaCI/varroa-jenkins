package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// occurrenceRFC3339 formats occ as job.schedules' wire format.
func occurrenceRFC3339(occ time.Time) string {
	return occ.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// ingestCronSchedule applies one job.schedules envelope carrying a
// single cron row for tenant/jobPath, so the round reads job_schedules
// the way a real cell's snapshot would populate it.
func ingestCronSchedule(t *testing.T, g *Ingester, tenant, jobPath string, placementGeneration int64, occurrences ...time.Time) {
	t.Helper()
	fireTimes := make([]string, len(occurrences))
	for i, occ := range occurrences {
		fireTimes[i] = occurrenceRFC3339(occ)
	}
	env := newJobScheduleEnvelope(t, "cell-a", time.Now().UTC(), JobSchedulesPayload{
		Hash: jobPath,
		Rows: []JobScheduleRowInput{{
			Tenant: tenant, PlacementGeneration: placementGeneration, JobPath: jobPath,
			TriggerType: "cron", Spec: "H/10 * * * *", NextFireTimes: fireTimes,
		}},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply job.schedules for %s: %v", jobPath, err)
	}
}

// countBackstopActivity counts build.scheduled.backstop activity rows
// for jobPath.
func countBackstopActivity(t *testing.T, db *sql.DB, jobPath string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity WHERE type = 'build.scheduled.backstop' AND item_path = $1
	`, jobPath).Scan(&n); err != nil {
		t.Fatalf("count backstop activity for %s: %v", jobPath, err)
	}
	return n
}

// TestRunBackstopRoundOnlyOneReplicaRuns asserts a second call finds the
// backstop advisory lock already held and skips its round.
func TestRunBackstopRoundOnlyOneReplicaRuns(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()

	holder, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire holder connection: %v", err)
	}
	defer holder.Release()
	var got bool
	if err := holder.QueryRow(context.Background(), "SELECT pg_try_advisory_lock($1)", backstopLockKey).Scan(&got); err != nil {
		t.Fatalf("hold backstop lock: %v", err)
	}
	if !got {
		t.Fatal("want the test's own holder connection to take the backstop lock first")
	}
	defer func() { _, _ = holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", backstopLockKey) }()

	ran, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{})
	if err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}
	if ran {
		t.Fatal("want the round to skip while another replica holds the lock")
	}
}

// TestRunBackstopRoundDropsPairWithBackstopOff asserts a tenant whose
// policy sets cronBackstop off produces no fire, even for an otherwise
// clear candidate occurrence.
func TestRunBackstopRoundDropsPairWithBackstopOff(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedTenantPlacementPolicy(t, db, `{"cronBackstop":false}`)

	g := NewIngester(pool, nil, nil)
	jobPath := "acme/backstop-off"
	ingestCronSchedule(t, g, testTenant, jobPath, 0, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeCancelActions(t, conn, 4)

	if _, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	select {
	case a := <-received:
		t.Fatalf("want no action for a cronBackstop-off tenant, got %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
	if n := countBackstopActivity(t, db, jobPath); n != 0 {
		t.Fatalf("backstop activity rows = %d, want 0", n)
	}
}

// TestRunBackstopRoundSingleTenantUsesDefaults asserts a tenant with no
// tenant_placements row fires a missed occurrence, using the
// backstop-scheduler capability's defaults (backstop on, catch-up one,
// grace 5m).
func TestRunBackstopRoundSingleTenantUsesDefaults(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	g := NewIngester(pool, nil, nil)
	jobPath := "acme/single-tenant-default"
	ingestCronSchedule(t, g, testTenant, jobPath, 0, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := make(chan LedgerAction, 4)
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

	if _, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	var kinds []string
	for i := 0; i < 2; i++ {
		select {
		case a := <-received:
			kinds = append(kinds, a.Kind)
		case <-time.After(3 * time.Second):
			t.Fatalf("want two actions, got %v", kinds)
		}
	}
	if len(kinds) != 2 || kinds[0] != LedgerActionWakeCell || kinds[1] != LedgerActionRetryRun {
		t.Fatalf("published kinds = %v, want [wake_cell retry_run]", kinds)
	}
	if n := countBackstopActivity(t, db, jobPath); n != 1 {
		t.Fatalf("backstop activity rows = %d, want 1", n)
	}
}

// TestRunBackstopRoundSkipsRowOffItsHome asserts a job_schedules row
// whose cell is not the job's home produces no fire: that row came from
// a cell the job has left.
func TestRunBackstopRoundSkipsRowOffItsHome(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")

	g := NewIngester(pool, nil, nil)
	jobPath := "acme/off-home"
	ingestCronSchedule(t, g, testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))
	// The ingest refuses a non-home export, so the test moves the row.
	if _, err := db.Exec(`
		UPDATE varroa.job_schedules SET cell = 'cell-old'
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, testCluster, testTenant, jobPath); err != nil {
		t.Fatalf("move the row off its home: %v", err)
	}

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := subscribeCancelActions(t, conn, 4)

	// The full round's home refresh would move the row back to its home
	// first, so the test calls the schedule pass alone.
	if err := runBackstopSchedule(context.Background(), pool, conn, BackstopConfig{}, time.Now()); err != nil {
		t.Fatalf("runBackstopSchedule: %v", err)
	}

	select {
	case a := <-received:
		t.Fatalf("want no action for a row off its home, got %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
	if n := countBackstopActivity(t, db, jobPath); n != 0 {
		t.Fatalf("backstop activity rows = %d, want 0", n)
	}
}

// TestRunBackstopRoundFiresHomeRowBelowGeneration asserts a row on its
// job's home cell fires although its placement_generation is below the
// tenant's stored generation. A generation raise does not move a row
// whose home keeps its cell.
func TestRunBackstopRoundFiresHomeRowBelowGeneration(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 3, "{}")

	g := NewIngester(pool, nil, nil)
	jobPath := "acme/home-below-generation"
	ingestCronSchedule(t, g, testTenant, jobPath, 3, time.Now().UTC().Add(-10*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET generation = 5 WHERE cluster = $1 AND tenant = $2`,
		testCluster, testTenant); err != nil {
		t.Fatalf("raise tenant generation: %v", err)
	}

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	if _, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	actions := wait(2)
	if actions[0].Kind != LedgerActionWakeCell || actions[1].Kind != LedgerActionRetryRun {
		t.Fatalf("kinds = [%s %s], want [wake_cell retry_run]", actions[0].Kind, actions[1].Kind)
	}
	for _, a := range actions {
		if a.Namespace != jsTestNS || a.Controller != "cell-a" {
			t.Fatalf("%s address = %s/%s, want %s/cell-a", a.Kind, a.Namespace, a.Controller, jsTestNS)
		}
	}
	if n := countBackstopActivity(t, db, jobPath); n != 1 {
		t.Fatalf("backstop activity rows = %d, want 1", n)
	}
}

// TestRunBackstopRoundRunsInterruptedSweepInSameRound asserts D9's
// interrupted sweep runs inside the same round as the backstop schedule,
// under the same advisory lock: a silent run on a confirmed-dead cell is
// marked interrupted by a bare RunBackstopRound call, with no separate
// sweep call.
func TestRunBackstopRoundRunsInterruptedSweepInSameRound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	past := time.Now().Add(-17 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", false, &past, &past, nil)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	sweepMetrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	cfg := BackstopConfig{MaxSweepPending: DefaultSweepMaxPending, SweepMetrics: sweepMetrics}

	if _, err := RunBackstopRound(context.Background(), pool, conn, cfg); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	var status, result string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &result); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "COMPLETED" || result != "INTERRUPTED" {
		t.Fatalf("status=%q result=%q, want COMPLETED/INTERRUPTED", status, result)
	}
}

// TestRunBackstopRoundFiresMissAcrossALaterExport asserts an occurrence
// that passes while a cell rolls stays a candidate after the new pod's
// first export, which names only future occurrences.
func TestRunBackstopRoundFiresMissAcrossALaterExport(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	g := NewIngester(pool, nil, nil)
	jobPath := "acme/missed-during-roll"
	now := time.Now().UTC()
	missed := now.Add(-10 * time.Minute).Truncate(time.Minute)

	exportAt := func(occurredAt time.Time, occurrences ...time.Time) {
		fireTimes := make([]string, len(occurrences))
		for i, occ := range occurrences {
			fireTimes[i] = occurrenceRFC3339(occ)
		}
		env := newJobScheduleEnvelope(t, "cell-a", occurredAt, JobSchedulesPayload{
			Hash: jobPath,
			Rows: []JobScheduleRowInput{{
				Tenant: testTenant, JobPath: jobPath,
				TriggerType: "cron", Spec: "H/10 * * * *", NextFireTimes: fireTimes,
			}},
		})
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply job.schedules: %v", err)
		}
	}
	// The old pod exports the occurrence before it passes. The new pod
	// exports only the next occurrence after the roll.
	exportAt(missed.Add(-2*time.Minute), missed)
	exportAt(now.Add(-time.Minute), missed.Add(10*time.Minute), missed.Add(20*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	received := make(chan LedgerAction, 4)
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

	if _, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	var retry *LedgerAction
	for i := 0; i < 2; i++ {
		select {
		case a := <-received:
			if a.Kind == LedgerActionRetryRun {
				retry = &a
			}
		case <-time.After(3 * time.Second):
			t.Fatal("want two actions for the missed occurrence")
		}
	}
	if retry == nil || retry.IntentKey != cronIntentKey(jobPath, missed) {
		t.Fatalf("retry_run = %+v, want intent key %q", retry, cronIntentKey(jobPath, missed))
	}
	if n := countBackstopActivity(t, db, jobPath); n != 1 {
		t.Fatalf("backstop activity rows = %d, want 1", n)
	}
}

// TestRunBackstopRoundCatchUpOneFiresOnce asserts cronCatchUp "one"
// fires one build for a run of kept misses: a later round does not fire
// an older miss that the earlier fire superseded.
func TestRunBackstopRoundCatchUpOneFiresOnce(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	g := NewIngester(pool, nil, nil)
	jobPath := "globex/catch-up-one"
	now := time.Now().UTC().Truncate(time.Minute)
	ingestCronSchedule(t, g, "globex", jobPath, 0,
		now.Add(-14*time.Minute), now.Add(-12*time.Minute), now.Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()

	for round := 1; round <= 3; round++ {
		if _, err := RunBackstopRound(context.Background(), pool, conn, BackstopConfig{}); err != nil {
			t.Fatalf("RunBackstopRound %d: %v", round, err)
		}
	}
	if n := countBackstopActivity(t, db, jobPath); n != 1 {
		t.Fatalf("backstop activity rows = %d, want 1", n)
	}
	var reason string
	if err := db.QueryRow(`
		SELECT reason FROM varroa.activity WHERE type = 'build.scheduled.backstop' AND item_path = $1
	`, jobPath).Scan(&reason); err != nil {
		t.Fatalf("read backstop activity: %v", err)
	}
	if want := occurrenceMinuteKey(now.Add(-10 * time.Minute)); reason != want {
		t.Fatalf("fired occurrence = %q, want the newest %q", reason, want)
	}
}

// lockNotifications answers the lock-kind payloads sent captured, in
// publish order.
func lockNotifications(sent *captureSend) []RunNotification {
	var out []RunNotification
	for i := 0; i < sent.count(); i++ {
		if p := sent.payload(i); p.Kind == "lock" {
			out = append(out, p)
		}
	}
	return out
}

// seedHeldLockOfRun inserts a held fleet_locks row of runID under
// fleetLockStateMachineTestName, owned by testTenant and stamped with
// the owning cell instance pod-a.
func seedHeldLockOfRun(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	now := time.Now()
	insertLockRowForTenant(t, db, fleetLockStateMachineTestName, runID, testTenant, testJobPath,
		FleetLockStateHeld, now.Add(-time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a' WHERE name = $1 AND run_id = $2`,
		fleetLockStateMachineTestName, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}
}

// TestRunBackstopRoundMovesInterruptedHolderToReleasing asserts the
// round moves the held lock of a run it marks to releasing in the same
// round, and publishes one releasing notification. A second round moves
// nothing and publishes no lock notification.
func TestRunBackstopRoundMovesInterruptedHolderToReleasing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedHeldLockOfRun(t, db, runID.String())

	// The owner stays alive, so the mark is heartbeat-lost and no release
	// confirmation deletes the row in the same round.
	now := time.Now()
	silent := now.Add(-liveOwnerSilentWindow - 5*time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, silent, runID); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", true, &now, &now, nil)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	sweepMetrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	cfg := BackstopConfig{MaxSweepPending: DefaultSweepMaxPending, SweepMetrics: sweepMetrics, RunNotifier: notifier}

	if _, err := RunBackstopRound(context.Background(), pool, conn, cfg); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	var reason string
	if err := db.QueryRow(`SELECT payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if reason != ReasonHeartbeatLost {
		t.Fatalf("interrupted_reason = %q, want %q", reason, ReasonHeartbeatLost)
	}
	state, releasingSince, found := fleetLockRowState(t, db, runID.String())
	if !found || state != FleetLockStateReleasing {
		t.Fatalf("state = %q, found = %v, want releasing", state, found)
	}
	if !releasingSince.Valid {
		t.Fatal("releasing_since was not stamped")
	}
	locks := lockNotifications(sent)
	if len(locks) != 1 {
		t.Fatalf("lock notifications = %+v, want one", locks)
	}
	if got := locks[0]; got.State != FleetLockStateReleasing || got.LockName != fleetLockStateMachineTestName ||
		got.RunID != runID.String() || got.Tenant != testTenant {
		t.Fatalf("lock notification = %+v, want the moved row's own releasing notification", got)
	}

	if _, err := RunBackstopRound(context.Background(), pool, conn, cfg); err != nil {
		t.Fatalf("second RunBackstopRound: %v", err)
	}
	notifier.Close()
	if got := len(lockNotifications(sent)); got != 1 {
		t.Fatalf("lock notifications after a second round = %d, want 1: a repeat round moves nothing", got)
	}
}

// TestRunBackstopRoundReleasesConfirmedGoneHolderInOneRound asserts a
// cell-lost mark of a holder whose pod is already confirmed gone deletes
// the lock row in the same round: the move runs before the release
// check.
func TestRunBackstopRoundReleasesConfirmedGoneHolderInOneRound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))
	seedHeldLockOfRun(t, db, runID.String())

	now := time.Now()
	lastSeen := now.Add(-2 * time.Minute)
	confirmed := now.Add(-time.Minute)
	seedTenantPlacementLiveness(t, db, "pod-b", true, &now, &now, []TenantSyncDeadInstance{{
		Instance: "pod-a", LastSeenAt: lastSeen, ConfirmedGoneAt: &confirmed,
	}})

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	sweepMetrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	cfg := BackstopConfig{MaxSweepPending: DefaultSweepMaxPending, SweepMetrics: sweepMetrics, RunNotifier: notifier}

	if _, err := RunBackstopRound(context.Background(), pool, conn, cfg); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	var result string
	if err := db.QueryRow(`SELECT result FROM varroa.runs WHERE run_id = $1`, runID).Scan(&result); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if result != "INTERRUPTED" {
		t.Fatalf("result = %q, want INTERRUPTED", result)
	}
	if state, _, found := fleetLockRowState(t, db, runID.String()); found {
		t.Fatalf("lock row state = %q, want the row deleted in the same round", state)
	}
	// The notifier holds the second notification of one run until the
	// interval ends; Close publishes it.
	notifier.Close()
	locks := lockNotifications(sent)
	if len(locks) != 2 || locks[0].State != FleetLockStateReleasing || locks[1].State != FleetLockStateReleased {
		t.Fatalf("lock notifications = %+v, want releasing then released", locks)
	}
}

// TestRunBackstopRoundEndsAFailedResumeInOneRound asserts a resume that
// failed on the only home target ends its run in one backstop round: the
// takeover ends the arm, and the sweep that follows marks the run
// resume-failed. The mark writes one webhook row and one release_agents
// action, and no cancel. A confirmed dead instance adds one
// release_instance_agents action.
func TestRunBackstopRoundEndsAFailedResumeInOneRound(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedMeshRunningRun(t, pool, db)
	seedMeshPlacement(t, db, "pod-live", "pod-live")
	seedFailedArm(t, db, runID, time.Now().Add(resumeArmWindow), "pod-live")
	seedRunNodeFiles(t, db, runID, "nodes/a/config.xml", meshBuildRecordPath, meshProgramPath)
	seedTenantAgentNamespace(t, db)
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET notification_webhook = $3::jsonb
		 WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant, `{"url":"https://hooks.example.test/r","key":"whsec-test"}`); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	received := subscribeClusterActions(t, conn)

	sweepMetrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}
	notifier, _, _ := newCaptureNotifier(t)
	defer notifier.Close()
	cfg := BackstopConfig{MaxSweepPending: DefaultSweepMaxPending, SweepMetrics: sweepMetrics, RunNotifier: notifier}

	if _, err := RunBackstopRound(context.Background(), pool, conn, cfg); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}

	var status, result, reason string
	if err := db.QueryRow(`
		SELECT status, COALESCE(result, ''), COALESCE(payload->>'interrupted_reason', '')
		  FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &result, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" || reason != ReasonResumeFailed {
		t.Fatalf("status=%q result=%q reason=%q, want COMPLETED/INTERRUPTED/%s", status, result, reason, ReasonResumeFailed)
	}

	var deliveries int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.webhook_deliveries WHERE run_id = $1`, runID).
		Scan(&deliveries); err != nil {
		t.Fatalf("count webhook deliveries: %v", err)
	}
	if deliveries != 1 {
		t.Fatalf("webhook deliveries = %d, want 1", deliveries)
	}

	select {
	case a := <-received:
		if a.Kind != LedgerActionReleaseAgents || a.RunID != runID.String() {
			t.Fatalf("action = %+v, want release_agents for %s", a, runID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("want a release_agents action published")
	}
	// The same round releases the idle pods of the confirmed dead instance.
	// That action has its own tests, so only another kind fails here.
	select {
	case a := <-received:
		if a.Kind != LedgerActionReleaseInstanceAgents {
			t.Fatalf("want no second action, got %+v", a)
		}
	case <-time.After(300 * time.Millisecond):
	}
}
