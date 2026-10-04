package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestActivityRecorderConsumerConfig asserts D3's consumer shape:
// DeliverAll, AckExplicit, a 30s ack wait, unlimited delivery, the
// shared durable name, and the 256-message fetch batch this change
// chooses.
func TestActivityRecorderConsumerConfig(t *testing.T) {
	cfg := activityRecorderConsumerConfig()
	if cfg.Durable != "activity-recorder" {
		t.Fatalf("durable = %q, want activity-recorder", cfg.Durable)
	}
	if cfg.DeliverPolicy != nats.DeliverAllPolicy {
		t.Fatalf("deliver policy = %v, want DeliverAllPolicy", cfg.DeliverPolicy)
	}
	if cfg.AckPolicy != nats.AckExplicitPolicy {
		t.Fatalf("ack policy = %v, want AckExplicitPolicy", cfg.AckPolicy)
	}
	if cfg.AckWait != 30*time.Second {
		t.Fatalf("ack wait = %v, want 30s", cfg.AckWait)
	}
	if cfg.MaxDeliver != -1 {
		t.Fatalf("max deliver = %d, want -1 (unlimited)", cfg.MaxDeliver)
	}
	if activityFetchBatch != 256 {
		t.Fatalf("fetch batch = %d, want 256", activityFetchBatch)
	}
}

// metricValue sums every data point of the named counter across a
// fresh collection.
func metricValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
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
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// TestActivityConsumerInsertsRow asserts a well-formed event is stored
// and acknowledged.
func TestActivityConsumerInsertsRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	mp := noop.NewMeterProvider()

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	metrics, err := NewActivityMetrics(mp, conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunActivityIngestConsumer(ctx, conn, ingester, metrics, 30*24*time.Hour, nil) }()

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "foo", Source: "operator",
		Type: "controller.created", Message: "created", Timestamp: time.Now().UTC(),
	}
	publishActivityEvent(t, conn, "foo", e)

	waitForRow(t, func() bool {
		var count int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE type = 'controller.created'").Scan(&count)
		return count == 1
	})
}

// TestActivityConsumerDuplicateRedelivery asserts a redelivered event
// is acknowledged and counted as a duplicate, without a second row.
func TestActivityConsumerDuplicateRedelivery(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	mp, reader := newTestMeterProvider(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	metrics, err := NewActivityMetrics(mp, conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunActivityIngestConsumer(ctx, conn, ingester, metrics, 30*24*time.Hour, nil) }()

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "dup", Source: "operator",
		Type: "controller.created", Message: "created", Timestamp: time.Now().UTC(),
	}
	publishActivityEvent(t, conn, "dup", e)
	publishActivityEvent(t, conn, "dup", e)

	waitForRow(t, func() bool {
		var count int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'dup'").Scan(&count)
		return count == 1
	})
	waitForRow(t, func() bool { return metricValue(t, reader, "recorder_activity_duplicate_total") >= 1 })
}

// TestActivityConsumerExpiredEventDropped asserts an event older than
// the configured retention is acknowledged, counted expired, and writes
// no row.
func TestActivityConsumerExpiredEventDropped(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	mp, reader := newTestMeterProvider(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	metrics, err := NewActivityMetrics(mp, conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunActivityIngestConsumer(ctx, conn, ingester, metrics, 30*24*time.Hour, nil) }()

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "old", Source: "operator",
		Type: "controller.created", Message: "created",
		Timestamp: time.Now().UTC().Add(-40 * 24 * time.Hour),
	}
	publishActivityEvent(t, conn, "old", e)

	waitForRow(t, func() bool { return metricValue(t, reader, "recorder_activity_expired_total") >= 1 })

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'old'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("want no row for an expired event, got %d", count)
	}
}

// TestActivityConsumerUndecodablePayload asserts a message whose payload
// does not decode as an activity event is acknowledged, counted, and
// never stops the consumer from processing the rest of the batch.
func TestActivityConsumerUndecodablePayload(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	mp, reader := newTestMeterProvider(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	metrics, err := NewActivityMetrics(mp, conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunActivityIngestConsumer(ctx, conn, ingester, metrics, 30*24*time.Hour, nil) }()

	if err := conn.Publish("activity.core.jenkins.bad", []byte("not json")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "after-bad", Source: "operator",
		Type: "controller.created", Message: "created", Timestamp: time.Now().UTC(),
	}
	publishActivityEvent(t, conn, "after-bad", e)

	waitForRow(t, func() bool { return metricValue(t, reader, "recorder_activity_unknown_total") >= 1 })
	waitForRow(t, func() bool {
		var count int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'after-bad'").Scan(&count)
		return count == 1
	})
}

// TestActivityConsumerCreatesMissingPartition asserts an event whose
// month has no partition yet still gets stored: the consumer creates
// that month's partition and retries the insert once.
func TestActivityConsumerCreatesMissingPartition(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	mp := noop.NewMeterProvider()

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	metrics, err := NewActivityMetrics(mp, conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunActivityIngestConsumer(ctx, conn, ingester, metrics, 365*24*time.Hour, nil) }()

	// migration 00002 creates only the previous, current, and next two
	// months. Five months out has no partition yet.
	ts := time.Now().UTC().AddDate(0, 5, 0)
	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "future", Source: "operator",
		Type: "controller.created", Message: "created", Timestamp: ts,
	}
	publishActivityEvent(t, conn, "future", e)

	waitForRow(t, func() bool {
		var count int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'future'").Scan(&count)
		return count == 1
	})
}

// TestActivityConsumerLegacySubjectUnattributed asserts a message on a
// pre-cutover three-token subject is stored with whatever cluster its
// payload carries, and is never re-attributed.
func TestActivityConsumerLegacySubjectUnattributed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	metrics, err := NewActivityMetrics(noop.NewMeterProvider(), conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = RunActivityIngestConsumer(ctx, conn, ingester, metrics, 30*24*time.Hour, nil) }()

	// Legacy payload: no Cluster field at all, and a three-token subject
	// (activity.<namespace>.<controller>) rather than the current
	// cluster-qualified four-token form.
	legacy := activity.Event{
		Namespace: "jenkins", Controller: "legacy-foo", Source: "operator",
		Type: "controller.created", Message: "created", Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.Publish("activity.jenkins.legacy-foo", data); err != nil {
		t.Fatalf("publish legacy: %v", err)
	}

	waitForRow(t, func() bool {
		var cluster string
		err := db.QueryRowContext(ctx, "SELECT cluster FROM varroa.activity WHERE controller = 'legacy-foo'").Scan(&cluster)
		return err == nil && cluster == ""
	})
}

// TestActivityConsumerSubstitutesMessageTimestamp asserts a payload with
// no timestamp gets the JetStream message timestamp, and that the
// substitution is stable across a redelivery: the JetStream message
// timestamp is fixed to when that one stored message was appended, so
// two deliveries of the SAME stored message compute the same EventKey
// and produce one row. This drives handleActivityMessage directly on
// one fetched *nats.Msg, twice, rather than publishing twice — two
// separate publishes would each get their own message timestamp, which
// is not what a redelivery is.
func TestActivityConsumerSubstitutesMessageTimestamp(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	if err := conn.EnsureConsumer(bus.ActivityStreamName, ActivityRecorderConsumerName, activityRecorderConsumerConfig()); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	metrics, err := NewActivityMetrics(noop.NewMeterProvider(), conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	ingester := NewActivityIngester(pool)

	// No Timestamp field: decodes to the zero value.
	untimed := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "untimed", Source: "operator",
		Type: "controller.created", Message: "created",
	}
	data, err := json.Marshal(untimed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.Publish("activity.core.jenkins.untimed", data); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sub, err := conn.PullSubscribe(bus.ActivityWildcard, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	var msg *nats.Msg
	waitForRow(t, func() bool {
		msgs, ferr := sub.Fetch(1, nats.MaxWait(time.Second))
		if ferr != nil || len(msgs) != 1 {
			return false
		}
		msg = msgs[0]
		return true
	})

	ctx := context.Background()
	// Deliver the exact same fetched message twice: its JetStream
	// metadata (and therefore its substituted timestamp) is fixed to
	// when it was stored, regardless of how many times it is handled.
	handleActivityMessage(ctx, ingester, metrics, 30*24*time.Hour, nil, msg)
	handleActivityMessage(ctx, ingester, metrics, 30*24*time.Hour, nil, msg)

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'untimed'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("want one row for two deliveries of the same untimed message, got %d", count)
	}
}

// TestActivityConsumerInsertFailureNotAcked asserts D3's disposition
// table: a message whose insert fails is neither acknowledged nor
// dropped. It handles one fetched message twice — first through an
// ingester whose pool is already closed, so Insert fails and the
// message goes back to the stream unacknowledged (Nak, D3's default
// case), then, after the backoff redelivers it, through a healthy
// ingester that inserts the row and acknowledges.
func TestActivityConsumerInsertFailureNotAcked(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	dsn := requireTestDSN(t)

	brokenPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open broken pool: %v", err)
	}
	brokenPool.Close() // Closed before use: every Exec on it fails.

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	if err := conn.EnsureConsumer(bus.ActivityStreamName, ActivityRecorderConsumerName, activityRecorderConsumerConfig()); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	metrics, err := NewActivityMetrics(noop.NewMeterProvider(), conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "insert-fail", Source: "operator",
		Type: "controller.created", Message: "created", Timestamp: time.Now().UTC(),
	}
	publishActivityEvent(t, conn, "insert-fail", e)

	sub, err := conn.PullSubscribe(bus.ActivityWildcard, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	var msg *nats.Msg
	waitForRow(t, func() bool {
		msgs, ferr := sub.Fetch(1, nats.MaxWait(time.Second))
		if ferr != nil || len(msgs) != 1 {
			return false
		}
		msg = msgs[0]
		return true
	})

	ctx := context.Background()
	logger := slog.Default()
	brokenIngester := NewActivityIngester(brokenPool)
	handleActivityMessage(ctx, brokenIngester, metrics, 30*24*time.Hour, logger, msg)

	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'insert-fail'").Scan(&count); err != nil {
		t.Fatalf("count after failed insert: %v", err)
	}
	if count != 0 {
		t.Fatalf("count after failed insert = %d, want 0 (a failed insert must write no row)", count)
	}

	// A Nak'd message redelivers after the backoff floor; fetch it again
	// and hand it to a healthy ingester.
	var redelivered *nats.Msg
	waitForRow(t, func() bool {
		msgs, ferr := sub.Fetch(1, nats.MaxWait(time.Second))
		if ferr != nil || len(msgs) != 1 {
			return false
		}
		redelivered = msgs[0]
		return true
	})

	goodIngester := NewActivityIngester(pool)
	handleActivityMessage(ctx, goodIngester, metrics, 30*24*time.Hour, logger, redelivered)

	waitForRow(t, func() bool {
		var c int
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM varroa.activity WHERE controller = 'insert-fail'").Scan(&c)
		return c == 1
	})
}

// publishActivityEvent marshals e and publishes it as a core,
// fire-and-forget publish on the activity subject for
// core/jenkins/controller, matching how a publisher emits an activity
// event.
func publishActivityEvent(t *testing.T, conn *bus.Conn, controller string, e activity.Event) {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	subject := bus.ActivitySubject("core", "jenkins", controller)
	if err := conn.Publish(subject, data); err != nil {
		t.Fatalf("publish activity event: %v", err)
	}
}
