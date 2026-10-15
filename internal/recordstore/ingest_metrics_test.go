package recordstore

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestIngestMetricsRegisterAndRecord asserts every ingest instrument
// registers on the varroa-recorder meter and records a value.
func TestIngestMetricsRegisterAndRecord(t *testing.T) {
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

	ctx := context.Background()
	metrics.addUnknown(ctx, UnknownCauseDecode)
	metrics.addConflict(ctx)
	metrics.addOrphan(ctx)
	metrics.addScheduleTruncated(ctx)
	metrics.addScheduleStale(ctx)
	metrics.addSupersededStart(ctx)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}

	want := []string{
		"recorder_ingest_unknown_total",
		"recorder_ingest_conflict_total",
		"recorder_ingest_orphan_total",
		"recorder_schedules_truncated_total",
		"recorder_schedules_stale_total",
		"recorder_ingest_superseded_start_total",
		"recorder_ingest_backlog_msgs",
		"recorder_ingest_backlog_bytes",
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("instrument %q was not registered (found %v)", name, names)
		}
	}
}
