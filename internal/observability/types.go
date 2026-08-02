package observability

import "time"

// Provider constants.
const (
	ProviderJenkinsAPI    = "jenkins-api"
	ProviderPrometheus    = "prometheus"
	ProviderOpenTelemetry = "opentelemetry"
)

// ValidProviders is the complete set of valid observability providers.
var ValidProviders = map[string]bool{
	ProviderJenkinsAPI:    true,
	ProviderPrometheus:    true,
	ProviderOpenTelemetry: true,
}

// Capability constants.
const (
	CapabilityJenkinsHealth           = "jenkins.health"
	CapabilityJenkinsJobsSummary      = "jenkins.jobs.summary"
	CapabilityJenkinsBuildsRecent     = "jenkins.builds.recent"
	CapabilityJenkinsBuildsTrends     = "jenkins.builds.trends"
	CapabilityJenkinsQueueMetrics     = "jenkins.queue.metrics"
	CapabilityJenkinsExecutorsMetrics = "jenkins.executors.metrics"
	CapabilityJenkinsMetricsEndpoint  = "jenkins.metrics.endpoint"
	CapabilityJenkinsMetricsQuery     = "jenkins.metrics.query"
	CapabilityJenkinsTracesExporting  = "jenkins.traces.exporting"
	CapabilityJenkinsTracesQuery      = "jenkins.traces.query"
)

// ValidCapabilities is the complete set of valid observability capability labels.
var ValidCapabilities = map[string]bool{
	CapabilityJenkinsHealth:           true,
	CapabilityJenkinsJobsSummary:      true,
	CapabilityJenkinsBuildsRecent:     true,
	CapabilityJenkinsBuildsTrends:     true,
	CapabilityJenkinsQueueMetrics:     true,
	CapabilityJenkinsExecutorsMetrics: true,
	CapabilityJenkinsMetricsEndpoint:  true,
	CapabilityJenkinsMetricsQuery:     true,
	CapabilityJenkinsTracesExporting:  true,
	CapabilityJenkinsTracesQuery:      true,
}

// Source status constants.
const (
	StatusNotConfigured = "not-configured"
	StatusIntended      = "intended"
	StatusConfigured    = "configured"
	StatusExposed       = "exposed"
	StatusIntegrated    = "integrated"
	StatusDegraded      = "degraded"
	StatusUnavailable   = "unavailable"
	StatusUnknown       = "unknown"
)

// ObservabilityLevel is a numeric level indicating the richest available observability.
//
//nolint:revive // stutter is intentional for API clarity in this internal API model
type ObservabilityLevel int

const (
	// LevelControlPlane indicates only controller-plane health is available.
	LevelControlPlane ObservabilityLevel = 0
	// LevelLiveSummary indicates cached Jenkins summary data is available.
	LevelLiveSummary ObservabilityLevel = 1
	// LevelPrometheusMetrics indicates Prometheus query-backed metrics are available.
	LevelPrometheusMetrics ObservabilityLevel = 2
	// LevelOpenTelemetry indicates OpenTelemetry exporting is available.
	LevelOpenTelemetry ObservabilityLevel = 3
)

// LevelName returns the display name for an observability level.
func LevelName(l ObservabilityLevel) string {
	switch l {
	case LevelControlPlane:
		return "Control-plane only"
	case LevelLiveSummary:
		return "Live Jenkins summary"
	case LevelPrometheusMetrics:
		return "Prometheus metrics"
	case LevelOpenTelemetry:
		return "OpenTelemetry insights"
	default:
		return "Unknown"
	}
}

// DeriveLevel derives an ObservabilityLevel from a set of available capabilities.
func DeriveLevel(caps map[string]bool) ObservabilityLevel {
	hasOTelExporting := caps[CapabilityJenkinsTracesExporting]
	hasMetricsQuery := caps[CapabilityJenkinsMetricsQuery]
	hasMetricsEndpoint := caps[CapabilityJenkinsMetricsEndpoint]
	hasBuildTrends := caps[CapabilityJenkinsBuildsTrends]
	hasJobsSummary := caps[CapabilityJenkinsJobsSummary]
	hasBuildsRecent := caps[CapabilityJenkinsBuildsRecent]

	if hasOTelExporting {
		return LevelOpenTelemetry
	}
	if hasMetricsEndpoint && hasMetricsQuery && hasBuildTrends {
		return LevelPrometheusMetrics
	}
	if hasJobsSummary && hasBuildsRecent {
		return LevelLiveSummary
	}
	return LevelControlPlane
}

// ObservableSource describes a single observability provider and its current
// runtime or backend evidence.
type ObservableSource struct {
	Provider string            `json:"provider"`
	Status   string            `json:"status"`
	Error    string            `json:"error,omitempty"`
	Hints    map[string]string `json:"hints,omitempty"`
}

// JenkinsBuildSummary is a bounded summary of a single recent build.
type JenkinsBuildSummary struct {
	JobName         string `json:"jobName"`
	BuildNumber     int    `json:"buildNumber"`
	Status          string `json:"status"`
	StartedAt       string `json:"startedAt,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	URL             string `json:"url,omitempty"`
}

// JenkinsSummary holds a bounded Jenkins API summary.
type JenkinsSummary struct {
	TotalJobs     int                   `json:"totalJobs,omitempty"`
	RunningBuilds int                   `json:"runningBuilds,omitempty"`
	RecentBuilds  []JenkinsBuildSummary `json:"recentBuilds,omitempty"`
}

// ObservabilityReport is a mite-to-operator message that carries runtime
// Jenkins observability detections. It is separate from heartbeat liveness
// and desired-state convergence.
//
//nolint:revive // stutter is intentional for API clarity in this internal API model
type ObservabilityReport struct {
	ObservedAt   time.Time          `json:"observedAt"`
	Sources      []ObservableSource `json:"sources"`
	Capabilities []string           `json:"capabilities"`
	Summary      *JenkinsSummary    `json:"summary,omitempty"`
	TTLSeconds   int                `json:"ttlSeconds"`
}

// Warning holds a non-firing warning surfaced during observability normalization.
type Warning struct {
	Message string `json:"message"`
}

// ControllerObservability is the normalized per-controller observability model
// exposed by the API.
type ControllerObservability struct {
	Sources      []ObservableSource `json:"sources"`
	Capabilities []string           `json:"capabilities"`
	Level        ObservabilityLevel `json:"level"`
	LevelName    string             `json:"levelName"`
	Warnings     []Warning          `json:"warnings,omitempty"`
	Freshness    Freshness          `json:"freshness"`
}

// Freshness carries metadata about the age and staleness of observability data.
type Freshness struct {
	ObservedAt string `json:"observedAt,omitempty"`
	MiteTTL    int    `json:"miteTTL,omitempty"`
	Stale      bool   `json:"stale"`
}
