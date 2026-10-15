package recordstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestIngestConsumerAcknowledgesMalformedEnvelope asserts a malformed
// run_id and an undecodable typed payload are each acknowledged once,
// never redelivered, and counted toward recorder_ingest_unknown_total.
// A permanent input error must not retry forever.
func TestIngestConsumerAcknowledgesMalformedEnvelope(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	payloadRunID := seedGrantedIntent(t, pool, "manual:malformed-payload")

	mp, reader := newTestMeterProvider(t)

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
	metrics, err := NewIngestMetrics(mp, conn, bus.BuildsStreamName, RecorderConsumerName)
	if err != nil {
		t.Fatalf("new ingest metrics: %v", err)
	}

	ingester := NewIngester(pool, conn, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunIngestConsumer(ctx, conn, ingester, metrics, time.Hour, nil)
	}()

	badRunID := IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunStarted,
		RunID:         "not-a-uuid",
		IntentKey:     "manual:malformed-runid",
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC(),
		Payload:       json.RawMessage(`{}`),
	}
	publishBuildEvent(t, conn, badRunID)

	badPayload := IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunStarted,
		RunID:         payloadRunID.String(),
		IntentKey:     "manual:malformed-payload",
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC(),
		Payload:       json.RawMessage(`"oops"`),
	}
	publishBuildEvent(t, conn, badPayload)

	waitForRow(t, func() bool {
		info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
		return err == nil && info.State.Msgs == 0
	})

	// A permanent error must never redeliver. Wait past one ack cycle and
	// confirm the stream stays drained.
	time.Sleep(200 * time.Millisecond)
	info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("State.Msgs = %d, want 0 (a malformed envelope must never redeliver)", info.State.Msgs)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var unknownSum int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "recorder_ingest_unknown_total" {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					unknownSum += dp.Value
				}
			}
		}
	}
	if unknownSum < 2 {
		t.Fatalf("recorder_ingest_unknown_total sum = %d, want at least 2", unknownSum)
	}
}
