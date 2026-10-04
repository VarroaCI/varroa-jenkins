package recordstore

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestActivityMetricsRegisterAndRecord asserts all four
// recorder_activity_* instruments register on the varroa-recorder meter
// and record a value.
func TestActivityMetricsRegisterAndRecord(t *testing.T) {
	reader := useTestMeterProvider(t)

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

	metrics, err := NewActivityMetrics(conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}

	ctx := context.Background()
	metrics.addDuplicate(ctx)
	metrics.addExpired(ctx)
	metrics.addUnknown(ctx)

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
		"recorder_activity_backlog_msgs",
		"recorder_activity_duplicate_total",
		"recorder_activity_expired_total",
		"recorder_activity_unknown_total",
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("instrument %q was not registered (found %v)", name, names)
		}
	}
}
