package recordstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedNativeSubscriber inserts one job_schedules row whose trigger type
// is the native "upstream" kind. spec holds the resolved upstream job
// paths, comma separated, exactly as the plugin exports them.
func (fx *fanoutTestFixture) seedNativeSubscriber(jobName, spec, threshold string, disabled bool) {
	fx.t.Helper()
	jobPath := fanoutProducerTenant + "/" + jobName
	if _, err := fx.db.Exec(`
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload, spec,
			 upstream_tenant, upstream_job, upstream_result, disabled)
		VALUES ($1, 'jenkins', 'cell-a', $2, $3, 'upstream', now(), '{}', $4, '', '', $5, $6)
	`, testCluster, fanoutProducerTenant, jobPath, spec, threshold, disabled); err != nil {
		fx.t.Fatalf("seed native subscriber %s: %v", jobPath, err)
	}
}

// TestRunUpstreamFanout_NativeEdgeMatchesTheQualifiedPath asserts a
// native upstream row fires when its specification list holds the
// producer's tenant-qualified job path, and that the same-tenant grant
// order allows it with no export.
func TestRunUpstreamFanout_NativeEdgeMatchesTheQualifiedPath(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.seedNativeSubscriber("deploy",
		fanoutProducerTenant+"/build,"+fanoutProducerTenant+"/test", "SUCCESS", false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}

	received := subscribeCancelActions(t, fx.conn, 4)
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	select {
	case action := <-received:
		if action.JobPath != fanoutProducerTenant+"/deploy" {
			t.Fatalf("job_path = %q, want %s/deploy", action.JobPath, fanoutProducerTenant)
		}
		if action.Actor != "varroa-upstream/"+fanoutProducerTenant {
			t.Fatalf("actor = %q", action.Actor)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the native trigger_run action")
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 1 {
		t.Fatalf("recorder_upstream_fanout_total = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_NativeEdgeIgnoresTheRelativePath asserts the
// match uses the qualified path on both sides. A list holding the
// relative form alone never matches.
func TestRunUpstreamFanout_NativeEdgeIgnoresTheRelativePath(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.seedNativeSubscriber("deploy", "build", "SUCCESS", false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
}

// TestRunUpstreamFanout_NativeThresholdBelowResultDoesNotFire asserts
// the threshold test runs inside the native selection.
func TestRunUpstreamFanout_NativeThresholdBelowResultDoesNotFire(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.producer.Result = "FAILURE"
	fx.seedNativeSubscriber("deploy", fanoutProducerTenant+"/build", "SUCCESS", false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
}

// TestRunUpstreamFanout_NativeThresholdMetFires asserts the native
// selection reads each row's own threshold, not one fixed value.
func TestRunUpstreamFanout_NativeThresholdMetFires(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.producer.Result = "FAILURE"
	fx.seedNativeSubscriber("deploy", fanoutProducerTenant+"/build", "FAILURE", false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 1 {
		t.Fatalf("recorder_upstream_fanout_total = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_ForeignListedPathIsDroppedAndCountedOnce asserts
// a native row whose list names another tenant's job publishes nothing
// and counts exactly one cross_tenant skip, whatever the number of
// foreign paths.
func TestRunUpstreamFanout_ForeignListedPathIsDroppedAndCountedOnce(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.seedNativeSubscriber("deploy", "other/build,other/test", "SUCCESS", false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total",
		map[string]string{"reason": FanoutSkipCrossTenant}); got != 1 {
		t.Fatalf("cross_tenant skips = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_BothKindsBuildOnce asserts a job that declares
// both trigger kinds contributes one candidate for each, and that the
// de-duplication leaves exactly one action.
func TestRunUpstreamFanout_BothKindsBuildOnce(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.seedNativeSubscriber(fanoutSubscriberJob,
		fanoutProducerTenant+"/build", "SUCCESS", false)
	fx.seedSubscriber(testCluster, fanoutProducerTenant, false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}

	received := subscribeCancelActions(t, fx.conn, 8)
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	select {
	case action := <-received:
		if action.JobPath != fanoutProducerTenant+"/"+fanoutSubscriberJob {
			t.Fatalf("job_path = %q", action.JobPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the action")
	}
	select {
	case extra := <-received:
		t.Fatalf("got a second action %+v, want exactly one", extra)
	case <-time.After(300 * time.Millisecond):
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 1 {
		t.Fatalf("recorder_upstream_fanout_total = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_OneDisabledDeclarationDoesNotHideTheOther
// asserts the de-duplication runs after the disabled test, so a disabled
// native row leaves the subscription's own candidate standing.
func TestRunUpstreamFanout_OneDisabledDeclarationDoesNotHideTheOther(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.seedNativeSubscriber(fanoutSubscriberJob,
		fanoutProducerTenant+"/build", "SUCCESS", true)
	fx.seedSubscriber(testCluster, fanoutProducerTenant, false)

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}

	received := subscribeCancelActions(t, fx.conn, 8)
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	select {
	case action := <-received:
		if action.JobPath != fanoutProducerTenant+"/"+fanoutSubscriberJob {
			t.Fatalf("job_path = %q", action.JobPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the surviving declaration must still publish")
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 1 {
		t.Fatalf("recorder_upstream_fanout_total = %d, want 1", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total",
		map[string]string{"reason": FanoutSkipDisabled}); got != 1 {
		t.Fatalf("disabled skips = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_CapCoversBothSetsTogether asserts the 32-row cap
// applies to the de-duplicated joined list, never to each set.
func TestRunUpstreamFanout_CapCoversBothSetsTogether(t *testing.T) {
	fx := newFanoutFixture(t)

	callers := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("sub%02d", i)
		callers = append(callers, name)
		seedGrantPlacement(t, fx.db, testCluster, name, "jenkins", fmt.Sprintf("cell-%02d", i), 1, "{}")
		fx.seedSubscriber(testCluster, name, false)
	}
	fx.setExports(callers)
	for i := 0; i < 20; i++ {
		fx.seedNativeSubscriber(fmt.Sprintf("native%02d", i),
			fanoutProducerTenant+"/build", "SUCCESS", false)
	}

	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}

	received := subscribeCancelActions(t, fx.conn, 64)
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	deadline := time.After(5 * time.Second)
	published := 0
	for published < upstreamFanoutCap {
		select {
		case <-received:
			published++
		case <-deadline:
			t.Fatalf("timed out after %d actions, want %d", published, upstreamFanoutCap)
		}
	}
	select {
	case extra := <-received:
		t.Fatalf("got action %+v past the cap", extra)
	case <-time.After(300 * time.Millisecond):
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total",
		map[string]string{"reason": FanoutSkipOverCap}); got != 8 {
		t.Fatalf("over_cap skips = %d, want 8", got)
	}
}

// TestRunUpstreamFanout_ARedeliveredCompletionPublishesOnce asserts one
// producer completion publishes one action for each native subscriber,
// whatever the number of deliveries.
//
// The build stream is at least once. Every other write of the terminal
// path is idempotent, and the fan-out is not: it mints a fresh run
// identifier for each subscriber, so a second round published a second
// trigger_run action under the same intent key. The target's claim then
// answered duplicate, and the second run's accepted-trigger row recorded
// a build that never ran.
func TestRunUpstreamFanout_ARedeliveredCompletionPublishesOnce(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, testNamespace, "cell-a", 1, "{}")

	conn := newTestActionConn(t)
	fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	ingester := NewIngester(pool, conn, nil)
	ingester.SetUpstreamFanout(NewGrantEvaluator(pool), metrics)

	// One native subscriber of the producer job, inside the producer's
	// own tenant, exactly as the plugin exports it.
	subscriber := testTenant + "/consumer"
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload, spec,
			 upstream_tenant, upstream_job, upstream_result, disabled)
		VALUES ($1, $2, 'cell-a', $3, $4, 'upstream', now(), '{}', $5, '', '', 'SUCCESS', false)
	`, testCluster, testNamespace, testTenant, subscriber, testJobPath); err != nil {
		t.Fatalf("seed native subscriber: %v", err)
	}

	runID := seedGrantedIntent(t, pool, "manual:redelivery-1")
	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, "manual:redelivery-1", KindRunCompleted,
		RunCompletedPayload{Result: &result})

	received := subscribeCancelActions(t, conn, 8)
	for i := 0; i < 2; i++ {
		if _, err := ingester.Apply(context.Background(), completed); err != nil {
			t.Fatalf("apply run.completed %d: %v", i+1, err)
		}
	}

	select {
	case action := <-received:
		if action.JobPath != subscriber {
			t.Fatalf("job_path = %q, want %q", action.JobPath, subscriber)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the one trigger_run action")
	}
	select {
	case extra := <-received:
		t.Fatalf("a redelivered completion published a second action %+v", extra)
	case <-time.After(500 * time.Millisecond):
	}
}
