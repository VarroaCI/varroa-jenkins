package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// setTelemetryEnv sets the env that the chart renders for a component that
// serves /metrics, with metrics on or off.
func setTelemetryEnv(t *testing.T, metricsEnabled string) {
	t.Helper()
	t.Setenv("VARROA_SERVICE_NAME", "varroa-recorder")
	t.Setenv("VARROA_TELEMETRY_DISABLED", "false")
	t.Setenv("OTEL_METRICS_ENABLED", metricsEnabled)
	t.Setenv("OTEL_TRACES_ENABLED", "false")
	t.Setenv("OTEL_LOGS_ENABLED", "false")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
}

// runInitTelemetry runs initTelemetry with a setter that records each
// provider it receives, and returns the providers.
func runInitTelemetry(t *testing.T) []metric.MeterProvider {
	t.Helper()
	var installed []metric.MeterProvider
	shutdown := initTelemetry(context.Background(), func(mp metric.MeterProvider) {
		installed = append(installed, mp)
	})
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("telemetry shutdown: %v", err)
		}
	})
	return installed
}

func TestInitTelemetryInstallsTheMeterProvider(t *testing.T) {
	setTelemetryEnv(t, "true")

	installed := runInitTelemetry(t)
	if len(installed) != 1 {
		t.Fatalf("initTelemetry installed %d meter providers, want 1", len(installed))
	}
	if _, ok := installed[0].(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("initTelemetry installed a %T, want *sdkmetric.MeterProvider", installed[0])
	}
}

func TestInitTelemetryInstallsNoMeterProviderWithMetricsOff(t *testing.T) {
	setTelemetryEnv(t, "false")

	if installed := runInitTelemetry(t); len(installed) != 0 {
		t.Fatalf("initTelemetry installed %d meter providers with metrics off, want 0", len(installed))
	}
}
