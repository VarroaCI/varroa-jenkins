package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// fanoutMetricSum sums every data point of metric name whose attributes
// match every entry of labels (an empty labels map matches every point).
func fanoutMetricSum(t *testing.T, reader *sdkmetric.ManualReader, name string, labels map[string]string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			data, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				match := true
				for k, want := range labels {
					v, ok := dp.Attributes.Value(attribute.Key(k))
					if !ok || v.AsString() != want {
						match = false
						break
					}
				}
				if match {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// fanoutTestFixture wires one producer, its own tenant_placements row,
// and its bus connection for a RunUpstreamFanout test. The producer's
// job is always <tenant>/build.
type fanoutTestFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	db       *sql.DB
	conn     *bus.Conn
	producer FanoutProducer
}

// fanoutProducerTenant is the producer tenant every test in this file
// shares: "acme".
const fanoutProducerTenant = "acme"

// newFanoutFixture opens a fresh schema, seeds fanoutProducerTenant's
// placement row on testCluster, and connects an embedded NATS server
// for the fan-out's own publishes.
func newFanoutFixture(t *testing.T) *fanoutTestFixture {
	t.Helper()
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, fanoutProducerTenant, "jenkins", "cell-p", 1, "{}")
	return &fanoutTestFixture{
		t:    t,
		pool: pool,
		db:   db,
		conn: newTestActionConn(t),
		producer: FanoutProducer{
			Cluster: testCluster,
			Tenant:  fanoutProducerTenant,
			JobPath: fanoutProducerTenant + "/build",
			RunID:   "run-" + fanoutProducerTenant,
			Result:  "SUCCESS",
		},
	}
}

// setExports overwrites the producer tenant's tenant_placements policy
// with one export entry granting job "build" to callers.
func (fx *fanoutTestFixture) setExports(callers []string) {
	fx.t.Helper()
	callersJSON, err := json.Marshal(callers)
	if err != nil {
		fx.t.Fatalf("marshal callers: %v", err)
	}
	if _, err := fx.db.Exec(`
		UPDATE varroa.tenant_placements SET policy = $1::jsonb WHERE tenant = $2
	`, fmt.Sprintf(`{"exports":[{"job":"build","callers":%s}]}`, string(callersJSON)), fx.producer.Tenant); err != nil {
		fx.t.Fatalf("set exports: %v", err)
	}
}

// fanoutSubscriberJob is the subscriber job name every test in this
// file shares: "rebuild".
const fanoutSubscriberJob = "rebuild"

// seedSubscriber inserts one job_schedules row subscribing to the
// fixture's producer, on subscriberCluster, at
// subscriberTenant/fanoutSubscriberJob, with the threshold "SUCCESS"
// every test in this file needs.
func (fx *fanoutTestFixture) seedSubscriber(subscriberCluster, subscriberTenant string, disabled bool) {
	fx.t.Helper()
	jobPath := subscriberTenant + "/" + fanoutSubscriberJob
	if _, err := fx.db.Exec(`
		INSERT INTO varroa.job_schedules
			(cluster, namespace, cell, tenant, job_path, trigger_type, observed_at, payload,
			 upstream_tenant, upstream_job, upstream_result, disabled)
		VALUES ($1, 'jenkins', 'cell-a', $2, $3, 'varroa-upstream', now(), '{}', $4, 'build', 'SUCCESS', $5)
	`, subscriberCluster, subscriberTenant, jobPath, fx.producer.Tenant, disabled); err != nil {
		fx.t.Fatalf("seed subscriber %s: %v", jobPath, err)
	}
}

// deps answers a FanoutDeps over the fixture, counting into metrics.
func (fx *fanoutTestFixture) deps(metrics *UpstreamFanoutMetrics) FanoutDeps {
	return FanoutDeps{Pool: fx.pool, Evaluator: NewGrantEvaluator(fx.pool), Conn: fx.conn, Metrics: metrics}
}

// TestRunUpstreamFanout_PublishesGrantedSubscriber asserts a single
// granted subscriber gets exactly one trigger_run action, carrying the
// varroa-upstream/<producer-tenant> actor and the t<digest> intent-key
// seq.
func TestRunUpstreamFanout_PublishesGrantedSubscriber(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber(testCluster, "beta", false)

	reader := useTestMeterProvider(t)
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
		if action.Kind != LedgerActionTriggerRun {
			t.Fatalf("kind = %q, want trigger_run", action.Kind)
		}
		if action.Actor != "varroa-upstream/acme" {
			t.Fatalf("actor = %q, want varroa-upstream/acme", action.Actor)
		}
		if action.JobPath != "beta/rebuild" {
			t.Fatalf("job_path = %q, want beta/rebuild", action.JobPath)
		}
		wantSeq := upstreamFanoutSeq(testCluster, "beta", "beta/rebuild")
		wantKey := fmt.Sprintf("upstream:%s:%s", fx.producer.RunID, wantSeq)
		if action.IntentKey != wantKey {
			t.Fatalf("intent_key = %q, want %q", action.IntentKey, wantKey)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for trigger_run action")
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 1 {
		t.Fatalf("recorder_upstream_fanout_total = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_FortySubscribersCapAt32 asserts task 4.1's own
// acceptance: 40 granted subscribers publish 32 actions and count 8
// over_cap skips.
func TestRunUpstreamFanout_FortySubscribersCapAt32(t *testing.T) {
	fx := newFanoutFixture(t)

	names := make([]string, 40)
	for i := 0; i < 40; i++ {
		names[i] = fmt.Sprintf("sub%02d", i)
	}
	fx.setExports(names)
	for i, name := range names {
		seedGrantPlacement(t, fx.db, testCluster, name, "jenkins", fmt.Sprintf("cell-%02d", i), 1, "{}")
		fx.seedSubscriber(testCluster, name, false)
	}

	reader := useTestMeterProvider(t)
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
	actors := map[string]bool{}
	for published < 32 {
		select {
		case action := <-received:
			published++
			actors[action.Actor] = true
		case <-deadline:
			t.Fatalf("timed out after %d actions, want 32", published)
		}
	}
	if len(actors) != 1 || !actors["varroa-upstream/acme"] {
		t.Fatalf("actors = %v, want only varroa-upstream/acme", actors)
	}

	select {
	case extra := <-received:
		t.Fatalf("got a 33rd action %+v, want exactly 32", extra)
	case <-time.After(300 * time.Millisecond):
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 32 {
		t.Fatalf("recorder_upstream_fanout_total = %d, want 32", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", map[string]string{"reason": FanoutSkipOverCap}); got != 8 {
		t.Fatalf("over_cap skips = %d, want 8", got)
	}
}

// TestRunUpstreamFanout_RedeliveryDerivesStableKeys asserts task 4.2's
// own acceptance: a redelivered run.completed, with a changed subscriber
// set, still derives the same key for every subscriber still present.
func TestRunUpstreamFanout_RedeliveryDerivesStableKeys(t *testing.T) {
	t.Parallel()

	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta", "gamma", "delta"})
	for i, name := range []string{"beta", "gamma", "delta"} {
		seedGrantPlacement(t, fx.db, testCluster, name, "jenkins", fmt.Sprintf("cell-%d", i), 1, "{}")
		fx.seedSubscriber(testCluster, name, false)
	}

	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	received := subscribeCancelActions(t, fx.conn, 16)
	deps := fx.deps(metrics)

	// First delivery: all three subscribers fire.
	if err := RunUpstreamFanout(context.Background(), deps, fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("first RunUpstreamFanout: %v", err)
	}
	firstKeys := map[string]string{}
	for i := 0; i < 3; i++ {
		select {
		case action := <-received:
			firstKeys[action.JobPath] = action.IntentKey
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for first delivery's action %d", i)
		}
	}

	// gamma is removed from the subscriber set (its row deleted, as a
	// tenant purge or a trigger removal would do), then the same
	// run.completed is redelivered.
	if _, err := fx.db.Exec(`
		DELETE FROM varroa.job_schedules WHERE tenant = 'gamma' AND job_path = 'gamma/rebuild'
	`); err != nil {
		t.Fatalf("delete gamma subscriber: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), deps, fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("second RunUpstreamFanout: %v", err)
	}
	secondKeys := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case action := <-received:
			secondKeys[action.JobPath] = action.IntentKey
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for second delivery's action %d", i)
		}
	}

	for jobPath, key := range secondKeys {
		if firstKeys[jobPath] != key {
			t.Fatalf("subscriber %s derived key %q on redelivery, want its first key %q", jobPath, key, firstKeys[jobPath])
		}
	}
	if _, stillThere := secondKeys["gamma/rebuild"]; stillThere {
		t.Fatal("removed subscriber gamma/rebuild fired on the redelivery")
	}
}

// TestRunUpstreamFanout_DisabledSubscriberSkipped asserts a disabled
// subscriber row is counted with reason "disabled" and builds nothing.
func TestRunUpstreamFanout_DisabledSubscriberSkipped(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber(testCluster, "beta", true)

	reader := useTestMeterProvider(t)
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
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", map[string]string{"reason": FanoutSkipDisabled}); got != 1 {
		t.Fatalf("disabled skips = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_DeniedSubscriberSkipped asserts a subscriber not
// listed in the producer's exports is counted with reason "denied".
func TestRunUpstreamFanout_DeniedSubscriberSkipped(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"payments"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber(testCluster, "beta", false)

	reader := useTestMeterProvider(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", map[string]string{"reason": FanoutSkipDenied}); got != 1 {
		t.Fatalf("denied skips = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_UnplacedSubscriberSkipped asserts a subscriber
// with no tenant_placements row is counted with reason "unplaced".
func TestRunUpstreamFanout_UnplacedSubscriberSkipped(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	// beta subscribes but is never placed.
	fx.seedSubscriber(testCluster, "beta", false)

	reader := useTestMeterProvider(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", map[string]string{"reason": FanoutSkipUnplaced}); got != 1 {
		t.Fatalf("unplaced skips = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_PublishFailureCountsAndContinues asserts a
// publish failure counts reason "publish_failed", and RunUpstreamFanout
// itself answers no error: a lost publish never rolls the source run's
// record back.
func TestRunUpstreamFanout_PublishFailureCountsAndContinues(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber(testCluster, "beta", false)

	reader := useTestMeterProvider(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	deps := fx.deps(metrics)
	fx.conn.Close() // a closed connection makes every publish fail.
	if err := RunUpstreamFanout(context.Background(), deps, fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", map[string]string{"reason": FanoutSkipPublishFailed}); got != 1 {
		t.Fatalf("publish_failed skips = %d, want 1", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
}

// TestRunUpstreamFanout_ThresholdBelowResultDoesNotFire asserts a
// producer result below the subscriber's threshold selects no row.
func TestRunUpstreamFanout_ThresholdBelowResultDoesNotFire(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.producer.Result = "FAILURE"
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber(testCluster, "beta", false)

	reader := useTestMeterProvider(t)
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
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", nil); got != 0 {
		t.Fatalf("skips = %d, want 0 (never selected)", got)
	}
}

// TestRunUpstreamFanout_ClusterResolutionExcludesOtherClusterSubscriber
// asserts a subscriber whose own cluster also hosts the producer tenant
// never fires on a different cluster's completion, design.md D9's own
// rule.
func TestRunUpstreamFanout_ClusterResolutionExcludesOtherClusterSubscriber(t *testing.T) {
	fx := newFanoutFixture(t)
	// acme is also placed on "other", and the subscriber lives there too.
	seedGrantPlacement(t, fx.db, "other", "acme", "jenkins", "cell-p2", 1, "{}")
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, "other", "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber("other", "beta", false)

	reader := useTestMeterProvider(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	// The producer's own completion is on testCluster ("core"), but
	// beta's own cluster ("other") also hosts a placement for acme, so
	// beta's own resolution of acme targets "other", never "core".
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total", map[string]string{"reason": FanoutSkipUnplaced}); got != 1 {
		t.Fatalf("unplaced skips for a cluster mismatch = %d, want 1", got)
	}
}

// TestRunUpstreamFanout_MemberClusterSubscriberPublishesOnItsCluster
// asserts a subscriber on member-a, whose producer tenant is placed only on
// core, gets its trigger_run action on member-a's subject. Only the member-a
// operator subscribes there.
func TestRunUpstreamFanout_MemberClusterSubscriberPublishesOnItsCluster(t *testing.T) {
	t.Parallel()

	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, "member-a", "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber("member-a", "beta", false)

	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}

	member := make(chan LedgerAction, 1)
	sub, err := fx.conn.Subscribe(bus.LedgerActionSubject("member-a"), func(msg *nats.Msg) {
		var a LedgerAction
		if err := json.Unmarshal(msg.Data, &a); err == nil {
			member <- a
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	core := subscribeCancelActions(t, fx.conn, 4)

	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}

	select {
	case action := <-member:
		if action.Cluster != "member-a" || action.Kind != LedgerActionTriggerRun || action.JobPath != "beta/rebuild" {
			t.Fatalf("got %+v, want a member-a trigger_run for beta/rebuild", action)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action on the member-a subject")
	}
	select {
	case action := <-core:
		t.Fatalf("core subject received %+v, want nothing", action)
	case <-time.After(200 * time.Millisecond):
	}
}
