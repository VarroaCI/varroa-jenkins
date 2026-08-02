package api

import (
	"sort"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/observability"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// ObservatoryNormalizer builds a normalized ControllerObservability from
// bundle intent, mite reports, and backend integration evidence.
type ObservatoryNormalizer struct {
	transport transport.Transport
	backends  BackendIntegrationProvider
}

// NewObservatoryNormalizer creates a new ObservatoryNormalizer.
func NewObservatoryNormalizer(t transport.Transport, backends BackendIntegrationProvider) *ObservatoryNormalizer {
	return &ObservatoryNormalizer{transport: t, backends: backends}
}

type controllerObservabilityJSON struct {
	Sources      []sourceJSON            `json:"sources"`
	Capabilities []string                `json:"capabilities"`
	Level        int                     `json:"level"`
	LevelName    string                  `json:"levelName"`
	Warnings     []observability.Warning `json:"warnings,omitempty"`
	Freshness    freshnessJSON           `json:"freshness"`
	Summary      *summaryJSON            `json:"summary,omitempty"`
}

type sourceJSON struct {
	Provider string            `json:"provider"`
	Status   string            `json:"status"`
	Error    string            `json:"error,omitempty"`
	Hints    map[string]string `json:"hints,omitempty"`
}

type freshnessJSON struct {
	ObservedAt string `json:"observedAt,omitempty"`
	MiteTTL    int    `json:"miteTTL,omitempty"`
	Stale      bool   `json:"stale"`
}

type summaryJSON struct {
	TotalJobs     int               `json:"totalJobs,omitempty"`
	RunningBuilds int               `json:"runningBuilds,omitempty"`
	RecentBuilds  []recentBuildJSON `json:"recentBuilds,omitempty"`
}

type recentBuildJSON struct {
	JobName         string `json:"jobName"`
	BuildNumber     int    `json:"buildNumber"`
	Status          string `json:"status"`
	StartedAt       string `json:"startedAt,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	URL             string `json:"url,omitempty"`
}

func (n *ObservatoryNormalizer) normalize(ns, name string, intentAnns map[string]string) controllerObservabilityJSON {
	intentProviders, intentCapabilities, warnings := observability.UnionIntents(intentAnns)

	report := n.transport.ObservabilityReport(ns, name)
	isConnected := n.transport.Connected(ns, name)
	now := time.Now()
	integration := BackendIntegration{}
	if n.backends != nil {
		integration = n.backends.Integration()
	}

	// Build sources by merging intent declarations with runtime evidence.
	sources := n.buildSources(intentProviders, report, isConnected, now, integration)

	capabilities := n.currentCapabilities(sources, report, intentCapabilities)
	capSet := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		capSet[capability] = true
	}

	// Freshness metadata.
	freshness := freshnessJSON{}
	if report != nil {
		freshness.ObservedAt = report.ObservedAt
		freshness.MiteTTL = report.TTLSeconds
		if !isConnected && isStale(now, report.ObservedAt, report.TTLSeconds) {
			freshness.Stale = true
		}
	}

	level := observability.DeriveLevel(capSet)
	summary := normalizeSummary(report)
	sort.Strings(capabilities)

	return controllerObservabilityJSON{
		Sources:      sources,
		Capabilities: capabilities,
		Level:        int(level),
		LevelName:    observability.LevelName(level),
		Warnings:     warnings,
		Freshness:    freshness,
		Summary:      summary,
	}
}

func (n *ObservatoryNormalizer) buildSources(
	intentProviders []string,
	report *mitev1.ObservabilityReport,
	isConnected bool,
	now time.Time,
	integration BackendIntegration,
) []sourceJSON {
	result := make(map[string]sourceJSON)

	// Seed with declared intent.
	for _, p := range intentProviders {
		result[p] = sourceJSON{Provider: p, Status: observability.StatusIntended}
	}

	// Overlay report sources. Report evidence outranks intent.
	if report != nil {
		for _, src := range report.Sources {
			s := sourceJSON{
				Provider: src.Provider,
				Status:   src.Status,
				Error:    src.Error,
				Hints:    src.Hints,
			}
			// Apply precedence: never downgrade status from report to intent.
			if existing, ok := result[src.Provider]; ok {
				if precedence(s.Status) < precedence(existing.Status) {
					continue
				}
			}
			result[src.Provider] = s
		}
	}

	if integration.Prometheus.Enabled {
		if existing, ok := result[observability.ProviderPrometheus]; ok && isUsableStatus(existing.Status) {
			existing.Status = observability.StatusIntegrated
			existing.Hints = mergeHints(existing.Hints, map[string]string{"queryBaseURL": integration.Prometheus.BaseURL})
			result[observability.ProviderPrometheus] = existing
		}
	}
	if integration.OpenTelemetry.Enabled {
		if existing, ok := result[observability.ProviderOpenTelemetry]; ok && (existing.Status == observability.StatusConfigured || isUsableStatus(existing.Status)) {
			existing.Status = observability.StatusIntegrated
			existing.Hints = mergeHints(existing.Hints, map[string]string{
				"backend":  integration.OpenTelemetry.Backend,
				"linkBase": integration.OpenTelemetry.LinkBase,
			})
			result[observability.ProviderOpenTelemetry] = existing
		}
	}

	// Degrade stale sources when mite is disconnected.
	if !isConnected && report != nil {
		stale := isStale(now, report.ObservedAt, report.TTLSeconds)
		for p, s := range result {
			if stale && isUsableStatus(s.Status) {
				s.Status = observability.StatusDegraded
				result[p] = s
			}
		}
	}

	providers := make([]string, 0, len(result))
	for provider := range result {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	out := make([]sourceJSON, 0, len(result))
	for _, provider := range providers {
		out = append(out, result[provider])
	}
	return out
}

func (n *ObservatoryNormalizer) currentCapabilities(
	sources []sourceJSON,
	report *mitev1.ObservabilityReport,
	intentCapabilities []string,
) []string {
	capSet := make(map[string]bool)
	statusByProvider := make(map[string]string, len(sources))
	for _, source := range sources {
		statusByProvider[source.Provider] = source.Status
	}

	if statusProvidesCapabilities(statusByProvider[observability.ProviderJenkinsAPI]) {
		capSet[observability.CapabilityJenkinsHealth] = true
		if report != nil && report.Summary != nil {
			capSet[observability.CapabilityJenkinsJobsSummary] = true
			capSet[observability.CapabilityJenkinsBuildsRecent] = true
		}
	}
	if statusProvidesCapabilities(statusByProvider[observability.ProviderPrometheus]) {
		capSet[observability.CapabilityJenkinsMetricsEndpoint] = true
	}
	if statusByProvider[observability.ProviderPrometheus] == observability.StatusIntegrated {
		capSet[observability.CapabilityJenkinsMetricsQuery] = true
	}
	if statusProvidesCapabilities(statusByProvider[observability.ProviderOpenTelemetry]) {
		capSet[observability.CapabilityJenkinsTracesExporting] = true
	}
	if statusByProvider[observability.ProviderOpenTelemetry] == observability.StatusIntegrated {
		capSet[observability.CapabilityJenkinsTracesQuery] = true
	}

	if report != nil {
		for _, capability := range report.Capabilities {
			capSet[capability] = true
		}
	}
	for _, capability := range intentCapabilities {
		if capability == observability.CapabilityJenkinsHealth && statusProvidesCapabilities(statusByProvider[observability.ProviderJenkinsAPI]) {
			capSet[capability] = true
		}
	}

	capabilities := make([]string, 0, len(capSet))
	for capability := range capSet {
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func normalizeSummary(report *mitev1.ObservabilityReport) *summaryJSON {
	if report == nil || report.Summary == nil {
		return nil
	}
	summary := &summaryJSON{
		TotalJobs:     report.Summary.TotalJobs,
		RunningBuilds: report.Summary.RunningBuilds,
		RecentBuilds:  make([]recentBuildJSON, 0, len(report.Summary.RecentBuilds)),
	}
	for _, build := range report.Summary.RecentBuilds {
		if build == nil {
			continue
		}
		summary.RecentBuilds = append(summary.RecentBuilds, recentBuildJSON{
			JobName:         build.JobName,
			BuildNumber:     build.BuildNumber,
			Status:          build.Status,
			StartedAt:       build.StartedAt,
			DurationSeconds: build.DurationSeconds,
			URL:             build.URL,
		})
	}
	return summary
}

func mergeHints(existing, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return existing
	}
	if existing == nil {
		existing = map[string]string{}
	}
	for key, value := range extra {
		if value == "" {
			continue
		}
		existing[key] = value
	}
	return existing
}

func statusProvidesCapabilities(status string) bool {
	return status == observability.StatusConfigured ||
		status == observability.StatusExposed ||
		status == observability.StatusIntegrated ||
		status == observability.StatusDegraded
}

// precedence returns a numeric precedence for source statuses.
// Higher values outrank lower. 0 = declaration-only, 1 = configured, 2 = exposed, 3 = integrated.
func precedence(status string) int {
	switch status {
	case observability.StatusIntegrated:
		return 3
	case observability.StatusExposed:
		return 2
	case observability.StatusConfigured:
		return 1
	case observability.StatusIntended:
		return 0
	default:
		return 0
	}
}

func isUsableStatus(s string) bool {
	return s == observability.StatusExposed ||
		s == observability.StatusConfigured ||
		s == observability.StatusIntegrated
}

func isStale(now time.Time, observedAt string, ttlSeconds int) bool {
	if observedAt == "" || ttlSeconds <= 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, observedAt)
	if err != nil {
		return false
	}
	return now.After(t.Add(time.Duration(ttlSeconds) * time.Second))
}
