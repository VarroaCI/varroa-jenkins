package bundle

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeterProvider returns a meter provider of the test with a manual
// reader. Pass the provider to NewResolver or NewCloneCache, and collect
// the values from the reader.
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

// collectOperatorMetric returns the metric with name on the
// "varroa-operator" meter of reader, or false when there is no such metric.
func collectOperatorMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Metrics, bool) {
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

// counterSum returns the sum of the int64 counter name, or 0 when the
// reader has no data for it.
func counterSum(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	m, ok := collectOperatorMetric(t, reader, name)
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s data is %T, want metricdata.Sum[int64]", name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

// cloneDurationCount returns the number of clone durations recorded with
// the result attribute, or 0 when there is none.
func cloneDurationCount(t *testing.T, reader *sdkmetric.ManualReader, result string) uint64 {
	t.Helper()
	m, ok := collectOperatorMetric(t, reader, "varroa.bundle.clone.duration")
	if !ok {
		return 0
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("varroa.bundle.clone.duration data is %T, want metricdata.Histogram[float64]", m.Data)
	}
	want := attribute.String("result", result)
	for _, dp := range hist.DataPoints {
		if v, ok := dp.Attributes.Value(want.Key); ok && v == want.Value {
			return dp.Count
		}
	}
	return 0
}

func TestCloneCache_CountsMissThenHit(t *testing.T) {
	t.Parallel()
	bareURL, _ := newBareFixture(t)
	mp, reader := newTestMeterProvider(t)
	c, err := NewCloneCache(mp, t.TempDir(), 10, 100, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(t, err)
	c.AllowLocalTransportForTest()

	checkout := func(name string) {
		t.Helper()
		target := filepath.Join(t.TempDir(), name)
		must(t, os.MkdirAll(target, 0o755))
		_, _, err := c.Checkout(context.Background(), bareURL, "main", target, nil)
		must(t, err)
	}

	checkout("cold")
	if got := counterSum(t, reader, "varroa.bundle.git.cache.misses"); got != 1 {
		t.Fatalf("misses after a cold checkout = %d, want 1", got)
	}
	if got := counterSum(t, reader, "varroa.bundle.git.cache.hits"); got != 0 {
		t.Fatalf("hits after a cold checkout = %d, want 0", got)
	}

	checkout("warm")
	if got := counterSum(t, reader, "varroa.bundle.git.cache.hits"); got != 1 {
		t.Fatalf("hits after a warm checkout = %d, want 1", got)
	}
	if got := counterSum(t, reader, "varroa.bundle.git.cache.misses"); got != 1 {
		t.Fatalf("misses after a warm checkout = %d, want 1", got)
	}
}

func TestResolver_MaterializeRecordsCloneDuration(t *testing.T) {
	t.Parallel()
	bareURL, commit := newBareFixture(t)
	_ = commit("bundle.yaml", `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
`)
	_ = commit("jenkins.yaml", "jenkins: {}")

	mp, reader := newTestMeterProvider(t)
	r := NewResolver(mp, t.TempDir())
	r.cloner.AllowLocalTransportForTest()

	if _, err := r.Materialize(context.Background(), bareURL, ".", "main", t.TempDir(), nil); err != nil {
		t.Fatalf("materialize a bundle from the fixture: %v", err)
	}
	if got := cloneDurationCount(t, reader, "success"); got != 1 {
		t.Fatalf("clone durations with result=success = %d, want 1", got)
	}

	missing := "file://" + filepath.Join(t.TempDir(), "missing.git")
	if _, err := r.Materialize(context.Background(), missing, ".", "main", t.TempDir(), nil); err == nil {
		t.Fatal("materialize from a missing repository: got nil error, want a clone error")
	}
	if got := cloneDurationCount(t, reader, "error"); got != 1 {
		t.Fatalf("clone durations with result=error = %d, want 1", got)
	}
}
