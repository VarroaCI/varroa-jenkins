package controller

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// newTestMeterProvider returns a meter provider of the test with a manual
// reader. Pass the provider to the constructor that creates the
// instruments, and collect the values from the reader.
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
