package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

// TestMetricsEndpointExposesOTelCounters asserts a counter the recorder
// registers on the "varroa-recorder" OTel meter reaches the same
// /metrics handler run() wires through newMux. It builds the provider
// through telemetry.NewMeterProvider, which InitTelemetry calls, with the
// env the chart renders. The test does not install the provider as the
// global provider. It reads /metrics through newMux and looks for the
// counter name.
func TestMetricsEndpointExposesOTelCounters(t *testing.T) {
	t.Setenv("VARROA_SERVICE_NAME", "varroa-recorder")
	t.Setenv("VARROA_TELEMETRY_DISABLED", "false")
	t.Setenv("OTEL_METRICS_ENABLED", "true")
	t.Setenv("OTEL_TRACES_ENABLED", "false")
	t.Setenv("OTEL_LOGS_ENABLED", "false")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("METRICS_TOKEN", "")

	res, err := resource.New(context.Background(), resource.WithAttributes(telemetry.ResourceAttributes()...))
	if err != nil {
		t.Fatalf("build telemetry resource: %v", err)
	}
	mp, err := telemetry.NewMeterProvider(context.Background(), res)
	if err != nil {
		t.Fatalf("build meter provider: %v", err)
	}
	if mp == nil {
		t.Fatal("build meter provider: got nil, want the Prometheus provider")
	}
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
	})

	meter := mp.Meter("varroa-recorder")
	counter, err := meter.Int64Counter("recorder_fence_veto_total",
		metric.WithDescription("Claims the placement fence vetoed, labelled by reason"))
	if err != nil {
		t.Fatalf("register recorder_fence_veto_total: %v", err)
	}
	counter.Add(context.Background(), 1)

	var migrated atomic.Bool
	migrated.Store(true)
	mux := newMux(&migrated, nil, func() bool { return true })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "recorder_fence_veto_total") {
		t.Fatalf("/metrics body does not carry recorder_fence_veto_total:\n%s", body)
	}
}
