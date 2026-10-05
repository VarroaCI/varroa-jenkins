package sharding

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeterProvider returns a meter provider of the test with a manual
// reader. Pass the provider to NewShardManager, and collect the values
// from the reader.
func newTestMeterProvider(t *testing.T) (metric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down test meter provider: %v", err)
		}
	})
	return mp, reader
}

// collectMetric returns the metric with name from reader, or false when
// the reader has no such metric.
func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "varroa-operator" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// handoffSum returns the varroa.operator.shard.handoffs sum for direction,
// or 0 when no data point for that direction exists.
func handoffSum(t *testing.T, reader *sdkmetric.ManualReader, direction string) int64 {
	t.Helper()
	m, ok := collectMetric(t, reader, "varroa.operator.shard.handoffs")
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("varroa.operator.shard.handoffs data is %T, want metricdata.Sum[int64]", m.Data)
	}
	want := attribute.String("direction", direction)
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(want.Key); ok && v == want.Value {
			return dp.Value
		}
	}
	return 0
}

func TestShardManager_HandoffsCounterRecordsDirection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	mp0, reader0 := newTestMeterProvider(t)
	w0 := h.addManagerWithMeterProvider("op-0", ring, mp0)

	h.runRebalances(1)
	if got := handoffSum(t, reader0, "acquired"); got != int64(ring.Shards()) {
		t.Fatalf("acquired handoffs = %d, want %d", got, ring.Shards())
	}
	if got := handoffSum(t, reader0, "released"); got != 0 {
		t.Fatalf("released handoffs before a newcomer = %d, want 0", got)
	}

	h.addManager("op-1", ring)
	h.runRebalances(2)

	released := handoffSum(t, reader0, "released")
	if released == 0 {
		t.Fatal("released handoffs = 0 after a newcomer took shards, want more than 0")
	}
	if want := int64(ring.Shards() - w0.m.held.Count()); released != want {
		t.Fatalf("released handoffs = %d, want %d (shards op-0 no longer holds)", released, want)
	}
}

func TestShardManager_ShardsHeldGaugeObservesHeldCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	ring := NewRing(8)
	mp, reader := newTestMeterProvider(t)
	w := h.addManagerWithMeterProvider("op-0", ring, mp)

	h.runRebalances(1)

	m, ok := collectMetric(t, reader, "varroa.operator.shards.held")
	if !ok {
		t.Fatal("varroa.operator.shards.held is not collected")
	}
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("varroa.operator.shards.held data is %T, want metricdata.Gauge[int64]", m.Data)
	}
	if len(gauge.DataPoints) != 1 {
		t.Fatalf("varroa.operator.shards.held has %d data points, want 1", len(gauge.DataPoints))
	}
	if got, want := gauge.DataPoints[0].Value, int64(w.m.held.Count()); got != want || want != int64(ring.Shards()) {
		t.Fatalf("varroa.operator.shards.held = %d, want %d", got, ring.Shards())
	}
}
