package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var logProvider *sdklog.LoggerProvider

// InitTelemetry initializes OpenTelemetry tracing, metrics, and logging providers.
// It returns a shutdown function that should be deferred.
func InitTelemetry(ctx context.Context) func(context.Context) error {
	if Disabled() {
		return func(context.Context) error { return nil }
	}

	res, err := resource.New(ctx, resource.WithAttributes(ResourceAttributes()...))
	if err != nil {
		return func(context.Context) error { return nil }
	}

	var (
		tp *sdktrace.TracerProvider
		mp *sdkmetric.MeterProvider
		lp *sdklog.LoggerProvider
	)

	endpoint := OTLPEndpoint()

	if TracesEnabled() && endpoint != "" {
		exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
		if err == nil {
			tp = sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(exp,
					sdktrace.WithMaxExportBatchSize(512),
					sdktrace.WithBatchTimeout(5*time.Second),
					sdktrace.WithMaxQueueSize(2048),
				),
				sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(SamplerRatio()))),
				sdktrace.WithResource(res),
			)
			otel.SetTracerProvider(tp)
		}
	}

	if MetricsEnabled() {
		if ServesPrometheusMetrics() {
			reg := promclient.DefaultRegisterer
			exp, err := prometheus.New(prometheus.WithRegisterer(reg))
			if err == nil {
				mp = sdkmetric.NewMeterProvider(
					sdkmetric.WithReader(exp),
					sdkmetric.WithResource(res),
				)
				otel.SetMeterProvider(mp)
			}
		} else if endpoint != "" {
			exp, err := otlpmetricgrpc.New(ctx,
				otlpmetricgrpc.WithEndpoint(endpoint),
				otlpmetricgrpc.WithInsecure(),
			)
			if err == nil {
				reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(30*time.Second))
				mp = sdkmetric.NewMeterProvider(
					sdkmetric.WithReader(reader),
					sdkmetric.WithResource(res),
				)
				otel.SetMeterProvider(mp)
			}
		}
	}

	if LogsEnabled() && endpoint != "" {
		exp, err := otlploggrpc.New(ctx,
			otlploggrpc.WithEndpoint(endpoint),
			otlploggrpc.WithInsecure(),
		)
		if err == nil {
			lp = sdklog.NewLoggerProvider(
				sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
				sdklog.WithResource(res),
			)
			logProvider = lp
		}
	}

	return func(ctx context.Context) error {
		var errs []error
		if tp != nil {
			if err := tp.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if mp != nil {
			if err := mp.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if lp != nil {
			if err := lp.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// LogHandler wraps a slog.Logger with an OpenTelemetry bridge, teeing output to both.
func LogHandler(logger *slog.Logger) *slog.Logger {
	if logProvider == nil {
		return logger
	}
	bridge := otelslog.NewHandler("varroa-telemetry",
		otelslog.WithLoggerProvider(logProvider),
	)
	return slog.New(&teeHandler{logger.Handler(), bridge})
}

type teeHandler struct {
	a, b slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.a.Enabled(ctx, level) || t.b.Enabled(ctx, level)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	err1 := t.a.Handle(ctx, r)
	err2 := t.b.Handle(ctx, r)
	if err1 != nil {
		return err1
	}
	return err2
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{t.a.WithAttrs(attrs), t.b.WithAttrs(attrs)}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{t.a.WithGroup(name), t.b.WithGroup(name)}
}

// MetricsAuthMiddleware protects the /metrics endpoint with a bearer token from METRICS_TOKEN.
func MetricsAuthMiddleware(next http.Handler) http.Handler {
	token := os.Getenv("METRICS_TOKEN")
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" || parts[1] != token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HealthzHandler returns an HTTP handler for the /healthz endpoint.
func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}

// HealthzHandlerWithBus is HealthzHandler plus a "bus" field. It stays 200
// while the bus is down on purpose: the BFF still serves the dashboard and the
// gateway's replicas share one credential, so evicting either from its Service
// would help nothing. The field and the varroa.bus.connected gauge are the
// visibility.
func HealthzHandlerWithBus(connected func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		bus := "connected"
		if !connected() {
			bus = "disconnected"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "bus": bus})
	})
}
