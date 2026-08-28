package api

// PrometheusQueryConfig holds optional Prometheus query integration config.
type PrometheusQueryConfig struct {
	Enabled bool
	BaseURL string
}

// OpenTelemetryLinkConfig holds optional OpenTelemetry query/link config.
type OpenTelemetryLinkConfig struct {
	Enabled  bool
	Backend  string // e.g. "tempo", "jaeger", "elastic"
	LinkBase string // base URL for trace links
}

// BackendIntegration holds the current state of backend query integrations.
type BackendIntegration struct {
	Prometheus    PrometheusQueryConfig
	OpenTelemetry OpenTelemetryLinkConfig
}

// BackendIntegrationProvider supplies backend integration status.
// The normalizer uses this to determine integrated states for
// prometheus and opentelemetry providers.
type BackendIntegrationProvider interface {
	Integration() BackendIntegration
}

type staticBackendIntegration struct{ cfg BackendIntegration }

func (s *staticBackendIntegration) Integration() BackendIntegration { return s.cfg }

// NewStaticBackendIntegration creates a BackendIntegrationProvider from a
// static config. Use this when backend integration is fixed at startup.
func NewStaticBackendIntegration(cfg BackendIntegration) BackendIntegrationProvider {
	return &staticBackendIntegration{cfg: cfg}
}
