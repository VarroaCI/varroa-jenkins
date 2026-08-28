package telemetry

import (
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// Disabled returns whether telemetry is disabled via VARROA_TELEMETRY_DISABLED.
func Disabled() bool {
	return os.Getenv("VARROA_TELEMETRY_DISABLED") == "true"
}

// TracesEnabled returns whether traces are enabled via OTEL_TRACES_ENABLED.
func TracesEnabled() bool {
	return strings.ToLower(os.Getenv("OTEL_TRACES_ENABLED")) == "true"
}

// MetricsEnabled returns whether metrics are enabled via OTEL_METRICS_ENABLED.
func MetricsEnabled() bool {
	return strings.ToLower(os.Getenv("OTEL_METRICS_ENABLED")) == "true"
}

// LogsEnabled returns whether logs are enabled via OTEL_LOGS_ENABLED.
func LogsEnabled() bool {
	return strings.ToLower(os.Getenv("OTEL_LOGS_ENABLED")) == "true"
}

// SamplerRatio returns the trace sampler ratio from OTEL_TRACES_SAMPLER_RATIO.
func SamplerRatio() float64 {
	v := os.Getenv("OTEL_TRACES_SAMPLER_RATIO")
	if v == "" {
		return 1.0
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 1.0
	}
	return r
}

// OTLPEndpoint returns the OTLP exporter endpoint.
func OTLPEndpoint() string {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
}

// LogLevel returns the log level from OTEL_LOG_LEVEL, defaulting to "info".
func LogLevel() string {
	if v := os.Getenv("OTEL_LOG_LEVEL"); v != "" {
		return v
	}
	return "info"
}

// ResourceAttributes returns parsed OTEL_RESOURCE_ATTRIBUTES plus Varroa metadata.
func ResourceAttributes() []attribute.KeyValue {
	var attrs []attribute.KeyValue

	if v := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				attrs = append(attrs, attribute.String(strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])))
			}
		}
	}

	if v := os.Getenv("VARROA_SERVICE_NAME"); v != "" {
		attrs = append(attrs, attribute.String("service.name", v))
	}
	if v := os.Getenv("VARROA_VERSION"); v != "" {
		attrs = append(attrs, attribute.String("service.version", v))
	}
	if v := os.Getenv("NAMESPACE"); v != "" {
		attrs = append(attrs, attribute.String("k8s.namespace.name", v))
	}

	return attrs
}

// ServesPrometheusMetrics returns true if this service exports a /metrics endpoint.
func ServesPrometheusMetrics() bool {
	name := os.Getenv("VARROA_SERVICE_NAME")
	switch name {
	case "varroa-bff", "varroa-operator", "varroa-gateway", "varroa-updatecenter":
		return true
	}
	return false
}
