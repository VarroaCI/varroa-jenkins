package recordstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestApplyUnknownKindIsRejected asserts Apply returns ErrUnknownKind for
// a kind outside the eight-kind set, with no database write.
func TestApplyUnknownKindIsRejected(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:unknown-kind-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:unknown-kind-1", "run.frobnicated", map[string]any{})
	if _, err := g.Apply(context.Background(), env); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v, want ErrUnknownKind", err)
	}
}

// TestApplyIntentMissingIsRejected asserts Apply returns ErrIntentMissing
// for a run_id with no run_intents row.
func TestApplyIntentMissingIsRejected(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	env := IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunHeartbeat,
		RunID:         runID,
		IntentKey:     "manual:missing",
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
	}
	if _, err := g.Apply(context.Background(), env); !errors.Is(err, ErrIntentMissing) {
		t.Fatalf("err = %v, want ErrIntentMissing", err)
	}
}

// TestApplyBuildNumberConflictStoresNullAndFlagsConflict asserts a
// run.started write that collides with another run's already-started
// build number commits with a null build_number, moves the intent to
// CONSUMED with a null build number, and reports ApplyResult.Conflict.
func TestApplyBuildNumberConflictStoresNullAndFlagsConflict(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runA := seedGrantedIntent(t, pool, "manual:conflict-a") // reserves build_number 1
	runB := seedGrantedIntent(t, pool, "manual:conflict-b") // reserves build_number 2
	g := NewIngester(pool, nil, nil)

	colliding := int64(2)
	startB := newTestEnvelope(t, runB, "manual:conflict-b", KindRunStarted, RunStartedPayload{BuildNumber: &colliding})
	if _, err := g.Apply(context.Background(), startB); err != nil {
		t.Fatalf("apply run.started for B: %v", err)
	}
	env := newTestEnvelope(t, runA, "manual:conflict-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &colliding,
	})

	result, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.started with colliding build number: %v", err)
	}
	if !result.Conflict {
		t.Fatal("ApplyResult.Conflict = false, want true")
	}
	if result.ConflictRunID != runB.String() || result.ConflictNumber != colliding {
		t.Fatalf("conflict names %s at %d, want %s at %d", result.ConflictRunID, result.ConflictNumber, runB, colliding)
	}

	var buildNumber *int64
	var state string
	if err := db.QueryRow(`SELECT build_number, state FROM varroa.run_intents WHERE run_id = $1`, runA).
		Scan(&buildNumber, &state); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if buildNumber != nil {
		t.Fatalf("run_intents.build_number = %v, want nil", *buildNumber)
	}
	if state != stateConsumed {
		t.Fatalf("state = %q, want CONSUMED", state)
	}

	var runsBuildNumber *int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.runs WHERE run_id = $1`, runA).Scan(&runsBuildNumber); err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if runsBuildNumber != nil {
		t.Fatalf("runs.build_number = %v, want nil", *runsBuildNumber)
	}
}

// TestApplyFailedWritePublishesNothing asserts a write that returns an
// error publishes no run notification.
func TestApplyFailedWritePublishesNothing(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:notify-rollback")
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runID, "manual:notify-rollback", KindRunStage, "oops")
	if _, err := g.Apply(context.Background(), env); err == nil {
		t.Fatal("Apply committed a run.stage whose payload does not decode")
	}
	if got := sent.count(); got != 0 {
		t.Fatalf("published %d notifications for a failed write, want 0", got)
	}
}

// TestApplySupersededStartPublishesNothing asserts a run.started that
// dispatch reports superseded publishes no notification.
func TestApplySupersededStartPublishesNothing(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	const key = "manual:notify-superseded"
	runID, old, _ := refreshedIntent(t, ledger, key)
	env := newTestEnvelope(t, runID, key, KindRunStarted, RunStartedPayload{BuildNumber: &old})

	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply superseded run.started: %v", err)
	}
	if !res.SupersededStart {
		t.Fatalf("want a superseded start, got %+v", res)
	}
	if got := sent.count(); got != 0 {
		t.Fatalf("published %d notifications for a superseded start, want 0", got)
	}
}

// TestApplyRunStagePublishesOneNotification asserts a committed run.stage
// publishes exactly one stage notification.
func TestApplyRunStagePublishesOneNotification(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:notify-stage")
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runID, "manual:notify-stage", KindRunStage, RunStagePayload{
		StageID: "node-1", Name: "Build", NodeType: "stage", Status: "IN_PROGRESS",
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.stage: %v", err)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications, want 1", got)
	}
	got := sent.payload(0)
	if got.Kind != "stage" || got.RunID != env.RunID || got.Status != "" || got.Result != "" {
		t.Fatalf("published %+v, want one stage notification for the run", got)
	}
	if got.Cluster != testCluster || got.Tenant != testTenant || got.JobPath != testJobPath {
		t.Fatalf("published coordinates %s/%s/%s, want %s/%s/%s",
			got.Cluster, got.Tenant, got.JobPath, testCluster, testTenant, testJobPath)
	}
}

// TestApplyRunSourcePublishesOneNotification asserts a committed
// run.source write publishes exactly one run notification.
func TestApplyRunSourcePublishesOneNotification(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:notify-source")
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runID, "manual:notify-source", KindRunSource, RunSourcePayload{
		Seq:            0,
		DefinitionPath: "Jenkinsfile",
	})
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.source: %v", err)
	}
	if res.RefusedSource {
		t.Fatal("the run.source write was refused, want applied")
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications, want 1", got)
	}
	got := sent.payload(0)
	if got.Kind != "run" || got.RunID != env.RunID || got.Status != "" || got.Result != "" {
		t.Fatalf("published %+v, want one run notification for the run", got)
	}
}

// TestApplyRefusedRunSourcePublishesNothing asserts a run.source write
// the guard refused publishes no notification.
func TestApplyRefusedRunSourcePublishesNothing(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:notify-source-refused")
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	first := newTestEnvelope(t, runID, "manual:notify-source-refused", KindRunSource, RunSourcePayload{
		Seq:            5,
		DefinitionPath: "Jenkinsfile",
	})
	if _, err := g.Apply(context.Background(), first); err != nil {
		t.Fatalf("apply first run.source: %v", err)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the first write, want 1", got)
	}

	lower := newTestEnvelope(t, runID, "manual:notify-source-refused", KindRunSource, RunSourcePayload{
		Seq:            4,
		DefinitionPath: "Other.groovy",
	})
	res, err := g.Apply(context.Background(), lower)
	if err != nil {
		t.Fatalf("apply lower run.source: %v", err)
	}
	if !res.RefusedSource {
		t.Fatal("the lower run.source write was applied, want refused")
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications after the refused write, want 1", got)
	}
}

// TestApplyBuildNumberConflictPublishesOnce asserts the retried
// run.started publishes one notification, after the retry commits.
func TestApplyBuildNumberConflictPublishesOnce(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runA := seedGrantedIntent(t, pool, "manual:notify-conflict-a") // reserves build_number 1
	runB := seedGrantedIntent(t, pool, "manual:notify-conflict-b") // reserves build_number 2
	g := NewIngester(pool, nil, nil)
	colliding := int64(2)
	startB := newTestEnvelope(t, runB, "manual:notify-conflict-b", KindRunStarted, RunStartedPayload{BuildNumber: &colliding})
	if _, err := g.Apply(context.Background(), startB); err != nil {
		t.Fatalf("apply run.started for B: %v", err)
	}
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runA, "manual:notify-conflict-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &colliding,
	})
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.started with colliding build number: %v", err)
	}
	if !res.Conflict {
		t.Fatal("ApplyResult.Conflict = false, want true")
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the retried write, want 1", got)
	}
	// The notification reports the envelope's own build number. The
	// record may hold a null number after the conflict.
	got := sent.payload(0)
	if got.Status != "RUNNING" || got.BuildNumber != colliding {
		t.Fatalf("published %+v, want RUNNING at build number %d", got, colliding)
	}
}

// TestIngestConsumerAcknowledgesBuildEventWhenPublishFails asserts a
// notifier whose send fails still leaves the record in place, and still
// lets the durable consumer acknowledge the build event.
func TestIngestConsumerAcknowledgesBuildEventWhenPublishFails(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:notify-send-error")

	buildNumber := int64(1)
	env := newTestEnvelope(t, runID, "manual:notify-send-error", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber,
	})

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if err := EnsureIngestConsumer(conn, bus.BuildsStreamName); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}

	ingester := NewIngester(pool, conn, nil)
	notifier := NewRunNotifier(nil, nil)
	notifier.send = func(string, []byte) error { return errors.New("bus down") }
	ingester.SetRunNotifier(notifier)
	t.Cleanup(notifier.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunIngestConsumer(ctx, conn, ingester, nil, time.Hour, nil)
	}()

	publishBuildEvent(t, conn, env)

	// The publish fails, and the write stands: a lost notification never
	// fails an ingest.
	waitForRow(t, func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
			return false
		}
		return n == 1
	})
	waitForRow(t, func() bool {
		info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
		return err == nil && info.State.Msgs == 0
	})
}

// countNotifications answers how many published notifications of each
// kind the notifier recorded, after the run's own interval ends.
func countNotifications(t *testing.T, notifier *RunNotifier, sent *captureSend, runID string) (runCount, lockCount int) {
	t.Helper()
	// The run's terminal notification ends its own interval, so the lock
	// notifications that follow it are the ones a flush reaches.
	notifier.now = func() time.Time { return notifyClockStart.Add(2 * notifier.interval) }
	notifier.flush(runID)
	for i := 0; i < sent.count(); i++ {
		switch sent.payload(i).Kind {
		case "run":
			runCount++
		case "lock":
			lockCount++
		}
	}
	return runCount, lockCount
}

// TestIngestNotifyPublishesOneRunAndOneLockNotification asserts a
// terminal run.completed that holds one lock publishes one run
// notification and one lock notification, the second naming the deleted
// row's own lock name and job path.
func TestIngestNotifyPublishesOneRunAndOneLockNotification(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:lock-notify-one")

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runID, "manual:lock-notify-one", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	runCount, lockCount := countNotifications(t, notifier, sent, env.RunID)
	if runCount != 1 || lockCount != 1 {
		t.Fatalf("published %d run and %d lock notifications, want 1 and 1", runCount, lockCount)
	}
	for i := 0; i < sent.count(); i++ {
		got := sent.payload(i)
		if got.Kind != "lock" {
			continue
		}
		if got.State != FleetLockStateReleased || got.LockName != "alpha/deploy-prod" {
			t.Fatalf("lock notification = state %q name %q, want released/alpha/deploy-prod", got.State, got.LockName)
		}
		if got.RunID != env.RunID || got.JobPath != "alpha/release" {
			t.Fatalf("lock notification names run %s job %s, want the deleted row's own values", got.RunID, got.JobPath)
		}
	}
}

// TestIngestNotifyPublishesTwoLockNotificationsForNestedLocks asserts a
// terminal run.completed that holds two locks publishes one run
// notification and one lock notification per released row.
func TestIngestNotifyPublishesTwoLockNotificationsForNestedLocks(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:lock-notify-two")

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/deploy-prod", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/deploy-gate", runID.String(), FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runID, "manual:lock-notify-two", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	runCount, lockCount := countNotifications(t, notifier, sent, env.RunID)
	if runCount != 1 || lockCount != 2 {
		t.Fatalf("published %d run and %d lock notifications, want 1 and 2", runCount, lockCount)
	}
	names := map[string]bool{}
	for i := 0; i < sent.count(); i++ {
		if got := sent.payload(i); got.Kind == "lock" {
			names[got.LockName] = true
		}
	}
	if !names["alpha/deploy-prod"] || !names["alpha/deploy-gate"] {
		t.Fatalf("published lock names = %v, want one notification per released row", names)
	}
}

// TestIngestNotifyPublishesOnlyTheRunNotificationWithNoLock asserts a
// terminal run.completed that holds no lock publishes the run
// notification alone.
func TestIngestNotifyPublishesOnlyTheRunNotificationWithNoLock(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:lock-notify-none")

	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	env := newTestEnvelope(t, runID, "manual:lock-notify-none", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("SUCCESS")})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	runCount, lockCount := countNotifications(t, notifier, sent, env.RunID)
	if runCount != 1 || lockCount != 0 {
		t.Fatalf("published %d run and %d lock notifications, want 1 and 0", runCount, lockCount)
	}
}
