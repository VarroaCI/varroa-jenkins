package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// startIngestTestServer starts an embedded NATS server with JetStream
// enabled for this package's consumer and stream tests.
func startIngestTestServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// publishBuildEvent marshals env and publishes it on
// bus.BuildSubject(env.Cluster, env.Namespace, env.Cell) with a JetStream
// ack, mirroring how the gateway publishes a build event.
func publishBuildEvent(t *testing.T, conn *bus.Conn, env IngestEnvelope) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	subject := bus.BuildSubject(env.Cluster, env.Namespace, env.Cell)
	if _, err := conn.PublishJetStream(subject, data); err != nil {
		t.Fatalf("publish build event: %v", err)
	}
}

// waitForRowTimeout bounds how long waitForRow polls before failing.
const waitForRowTimeout = 5 * time.Second

// waitForRow polls check until it returns true or waitForRowTimeout elapses.
func waitForRow(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitForRowTimeout)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the expected row")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestIngestConsumerConsumesPublishedEnvelope asserts the durable
// "recorder" consumer picks up one published build event and writes the
// row.
func TestIngestConsumerConsumesPublishedEnvelope(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:consumer-1")

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ingester := NewIngester(pool, conn, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		// metrics is nil: this test asserts the write path, not the
		// instruments (TestIngestMetricsRegisterAndRecord owns that,
		// and every metrics.addX method is nil-safe).
		_ = RunIngestConsumer(ctx, conn, ingester, nil, 24*time.Hour, nil)
	}()

	buildNumber := int64(1)
	env := newTestEnvelope(t, runID, "manual:consumer-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	publishBuildEvent(t, conn, env)

	waitForRow(t, func() bool {
		var status string
		err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status)
		return err == nil && status == StatusRunning
	})
}

// TestIngestConsumerDispositionTable asserts each disposition-table row:
// a young event with no intent still NAKs, and an expired one is
// acknowledged once and never redelivered.
func TestIngestConsumerDispositionTable(t *testing.T) {
	pool, db := setupLedgerPool(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ingester := NewIngester(pool, conn, nil)

	retention := time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunIngestConsumer(ctx, conn, ingester, nil, retention, nil)
	}()

	// An expired event (occurred_at older than retention, no intent):
	// acknowledged once, never redelivered.
	expiredRunID := newTestUUIDv7String()
	expired := IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunHeartbeat,
		RunID:         expiredRunID,
		IntentKey:     "manual:expired",
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC().Add(-2 * retention),
		Payload:       json.RawMessage(`{}`),
	}
	publishBuildEvent(t, conn, expired)

	waitForRow(t, func() bool {
		info, err := conn.JetStream().ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})

	// Confirm no row was ever written for it (it was dropped, not stored).
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, expiredRunID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0 (an orphaned event writes no row)", count)
	}

	// A young event with no intent still NAKs: work-queue retention keeps
	// an unacknowledged message in the stream regardless of redelivery
	// timing, and once its intent shows up, it is stored.
	youngRunID := seedGrantedIntent(t, pool, "manual:young-1")
	buildNumber := int64(1)
	young := newTestEnvelope(t, youngRunID, "manual:young-1", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	// Publish for a run_id whose intent does not exist yet, to force a
	// NAK, then re-publish once the intent exists so the row lands.
	unseeded := young
	unseeded.RunID = newTestUUIDv7String()
	unseeded.IntentKey = "manual:young-unseeded"
	unseeded.OccurredAt = time.Now().UTC()
	publishBuildEvent(t, conn, unseeded)

	// Give it a couple of redelivery cycles, then assert it never wrote a
	// row (still retrying, not orphaned, because it is well inside the
	// one-hour retention window).
	time.Sleep(2 * time.Second)
	var stillMissing int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, unseeded.RunID).Scan(&stillMissing); err != nil {
		t.Fatalf("count runs for young event: %v", err)
	}
	if stillMissing != 0 {
		t.Fatalf("count = %d, want 0 (a young orphan must not be stored, only retried)", stillMissing)
	}
	info, err := conn.JetStream().ConsumerInfo(bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.NumRedelivered == 0 && info.NumAckPending == 0 && info.NumPending == 0 {
		t.Fatal("expected the young orphan to still be pending redelivery")
	}

	publishBuildEvent(t, conn, young)
	waitForRow(t, func() bool {
		var status string
		err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, youngRunID).Scan(&status)
		return err == nil && status == StatusRunning
	})
}

// TestIngestConsumerUnknownKindAndSchemaVersion asserts an unknown kind
// and a below-minimum schema version are acknowledged, and an
// above-maximum schema version is retried.
func TestIngestConsumerUnknownKindAndSchemaVersion(t *testing.T) {
	pool, _ := setupLedgerPool(t)

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ingester := NewIngester(pool, conn, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunIngestConsumer(ctx, conn, ingester, nil, time.Hour, nil)
	}()

	base := IngestEnvelope{
		SchemaVersion: 1,
		RunID:         newTestUUIDv7String(),
		IntentKey:     "manual:schema",
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC(),
		Payload:       json.RawMessage(`{}`),
	}

	unknownKind := base
	unknownKind.Kind = "run.frobnicated"
	publishBuildEvent(t, conn, unknownKind)

	belowMin := base
	belowMin.Kind = KindRunHeartbeat
	belowMin.SchemaVersion = 0
	publishBuildEvent(t, conn, belowMin)

	aboveMax := base
	aboveMax.Kind = KindRunHeartbeat
	aboveMax.SchemaVersion = maxSchemaVersion + 1
	publishBuildEvent(t, conn, aboveMax)

	// The unknown-kind and below-minimum events are acknowledged, which
	// removes them from the WorkQueuePolicy stream. The above-maximum
	// event is only NAK'd, so it stays in the stream regardless of
	// redelivery timing, leaving exactly one message behind.
	waitForRow(t, func() bool {
		info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
		return err == nil && info.State.Msgs == 1
	})
}
