package api

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/observability"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

func TestBuildSourcesIntentOnly(t *testing.T) {
	n := &ObservatoryNormalizer{}
	sources := n.buildSources([]string{"prometheus"}, nil, false, time.Now(), BackendIntegration{})

	if len(sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(sources))
	}
	if sources[0].Status != observability.StatusIntended {
		t.Errorf("expected intended, got %s", sources[0].Status)
	}
}

func TestBuildSourcesReportOutranksIntent(t *testing.T) {
	n := &ObservatoryNormalizer{}
	report := &mitev1.ObservabilityReport{
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		TTLSeconds: 180,
		Sources: []*mitev1.ObservableSource{
			{Provider: "prometheus", Status: "exposed"},
		},
	}
	sources := n.buildSources([]string{"prometheus"}, report, true, time.Now(), BackendIntegration{})

	if sources[0].Status != "exposed" {
		t.Errorf("expected exposed, got %s", sources[0].Status)
	}
}

func TestBuildSourcesDisconnectedFreshKeepsStatus(t *testing.T) {
	n := &ObservatoryNormalizer{}
	now := time.Now()
	report := &mitev1.ObservabilityReport{
		ObservedAt: now.Format(time.RFC3339),
		TTLSeconds: 180,
		Sources: []*mitev1.ObservableSource{
			{Provider: "prometheus", Status: "exposed"},
		},
	}
	sources := n.buildSources([]string{"prometheus"}, report, false, now, BackendIntegration{})

	// Fresh (within TTL) + disconnected should keep exposed.
	if sources[0].Status != "exposed" {
		t.Errorf("expected exposed (fresh), got %s", sources[0].Status)
	}
}

func TestBuildSourcesDisconnectedStaleDegrades(t *testing.T) {
	n := &ObservatoryNormalizer{}
	now := time.Now()
	report := &mitev1.ObservabilityReport{
		ObservedAt: now.Add(-200 * time.Second).Format(time.RFC3339),
		TTLSeconds: 180,
		Sources: []*mitev1.ObservableSource{
			{Provider: "prometheus", Status: "exposed"},
		},
	}
	sources := n.buildSources([]string{"prometheus"}, report, false, now, BackendIntegration{})

	if sources[0].Status != "degraded" {
		t.Errorf("expected degraded (stale), got %s", sources[0].Status)
	}
}

func TestBuildSourcesDisconnectedFallbackToIntended(t *testing.T) {
	n := &ObservatoryNormalizer{}
	sources := n.buildSources([]string{"prometheus"}, nil, false, time.Now(), BackendIntegration{})

	if sources[0].Status != observability.StatusIntended {
		t.Errorf("expected intended, got %s", sources[0].Status)
	}
}

func TestBuildSourcesBackendIntegrationUpgradesProvider(t *testing.T) {
	n := &ObservatoryNormalizer{}
	report := &mitev1.ObservabilityReport{
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		TTLSeconds: 180,
		Sources: []*mitev1.ObservableSource{
			{Provider: observability.ProviderPrometheus, Status: observability.StatusExposed},
		},
	}
	sources := n.buildSources([]string{observability.ProviderPrometheus}, report, true, time.Now(), BackendIntegration{
		Prometheus: PrometheusQueryConfig{Enabled: true, BaseURL: "http://prometheus:9090"},
	})

	if got := sources[0].Status; got != observability.StatusIntegrated {
		t.Fatalf("expected integrated status, got %s", got)
	}
	if got := sources[0].Hints["queryBaseURL"]; got != "http://prometheus:9090" {
		t.Fatalf("expected queryBaseURL hint, got %q", got)
	}
}

func TestNormalizeUsesCurrentCapabilitiesNotIntentOnly(t *testing.T) {
	report := &mitev1.ObservabilityReport{
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		TTLSeconds: 180,
		Sources:    []*mitev1.ObservableSource{{Provider: observability.ProviderJenkinsAPI, Status: observability.StatusExposed}},
	}
	n := &ObservatoryNormalizer{transport: &stubTransport{report: report, connected: true}}
	obs := n.normalize("ns", "name", map[string]string{
		observability.AnnotationCapabilities: strings.Join([]string{
			observability.CapabilityJenkinsJobsSummary,
			observability.CapabilityJenkinsBuildsRecent,
		}, ","),
	})

	sort.Strings(obs.Capabilities)
	if len(obs.Capabilities) != 1 || obs.Capabilities[0] != observability.CapabilityJenkinsHealth {
		t.Fatalf("expected only live health capability, got %#v", obs.Capabilities)
	}
}

func TestNormalizeIncludesSummaryCapabilitiesAndPayload(t *testing.T) {
	report := &mitev1.ObservabilityReport{
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		TTLSeconds: 180,
		Sources:    []*mitev1.ObservableSource{{Provider: observability.ProviderJenkinsAPI, Status: observability.StatusExposed}},
		Summary: &mitev1.ObservabilitySummary{
			TotalJobs:     7,
			RunningBuilds: 2,
			RecentBuilds:  []*mitev1.ObservabilityBuild{{JobName: "deploy", BuildNumber: 42, Status: "SUCCESS"}},
		},
	}
	n := &ObservatoryNormalizer{transport: &stubTransport{report: report, connected: true}}
	obs := n.normalize("ns", "name", nil)

	if obs.Summary == nil || obs.Summary.TotalJobs != 7 {
		t.Fatalf("expected normalized summary payload, got %#v", obs.Summary)
	}
	if !containsString(obs.Capabilities, observability.CapabilityJenkinsJobsSummary) || !containsString(obs.Capabilities, observability.CapabilityJenkinsBuildsRecent) {
		t.Fatalf("expected summary capabilities, got %#v", obs.Capabilities)
	}
}

type stubTransport struct {
	report    *mitev1.ObservabilityReport
	connected bool
}

func (s *stubTransport) Send(_ context.Context, _, _ string, _ *mitev1.OperatorMessage) error {
	return nil
}
func (s *stubTransport) SendImperative(_ context.Context, _, _ string, _ *mitev1.ImperativeCommand) error {
	return nil
}
func (s *stubTransport) DrainResults(_, _ string) []*mitev1.CommandResult { return nil }
func (s *stubTransport) Snapshot(_, _ string) *mitev1.StateSnapshot       { return nil }
func (s *stubTransport) Health(_, _ string) string                        { return "" }
func (s *stubTransport) Connected(_, _ string) bool                       { return s.connected }
func (s *stubTransport) Info(_, _ string) (string, time.Time, time.Time, bool) {
	return "", time.Time{}, time.Time{}, false
}
func (s *stubTransport) ConnEpoch(_, _ string) (int64, bool)               { return 0, false }
func (s *stubTransport) ClearDesired(_ context.Context, _, _ string) error { return nil }
func (s *stubTransport) IdleGauges(_, _ string) (*mitev1.IdleGauges, time.Time, bool) {
	return nil, time.Time{}, false
}
func (s *stubTransport) ObservabilityReport(_, _ string) *mitev1.ObservabilityReport { return s.report }
func (s *stubTransport) FetchLastApplied(_ context.Context, _, _ string) (*mitev1.ContentResponse, error) {
	return nil, transport.ErrContentUnavailable
}

func (s *stubTransport) PluginInventory(_, _ string) *mitev1.PluginInventory { return nil }

func (s *stubTransport) InstalledPluginsHash(_, _ string) (string, bool) { return "", false }

func (s *stubTransport) PluginClassification(_, _ string) (*transport.ClassifiedInventory, bool) {
	return nil, false
}

func (s *stubTransport) PutPluginClassification(_ context.Context, _, _ string, _ *transport.ClassifiedInventory) error {
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPrecedence(t *testing.T) {
	if precedence("integrated") <= precedence("exposed") {
		t.Error("integrated should outrank exposed")
	}
	if precedence("exposed") <= precedence("configured") {
		t.Error("exposed should outrank configured")
	}
	if precedence("configured") <= precedence("intended") {
		t.Error("configured should outrank intended")
	}
}

func TestDeriveLevel0Thru3(t *testing.T) {
	tests := []struct {
		name string
		caps map[string]bool
		want int
	}{
		{"level 0", map[string]bool{observability.CapabilityJenkinsHealth: true}, 0},
		{"level 1", map[string]bool{
			observability.CapabilityJenkinsJobsSummary:  true,
			observability.CapabilityJenkinsBuildsRecent: true,
		}, 1},
		{"level 2", map[string]bool{
			observability.CapabilityJenkinsMetricsEndpoint: true,
			observability.CapabilityJenkinsMetricsQuery:    true,
			observability.CapabilityJenkinsBuildsTrends:    true,
		}, 2},
		{"level 3", map[string]bool{
			observability.CapabilityJenkinsTracesExporting: true,
		}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := int(observability.DeriveLevel(tt.caps)); got != tt.want {
				t.Errorf("DeriveLevel() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsStale(t *testing.T) {
	now := time.Now()
	if isStale(now, now.Add(-200*time.Second).Format(time.RFC3339), 180) {
		t.Log("correctly stale")
	} else {
		t.Error("expected stale")
	}
	if isStale(now, now.Format(time.RFC3339), 180) {
		t.Error("expected not stale for current time")
	}
	if isStale(now, "", 180) {
		t.Error("expected not stale for empty observedAt")
	}
}

func TestBackendIntegrationProvider(t *testing.T) {
	cfg := BackendIntegration{
		Prometheus:    PrometheusQueryConfig{Enabled: true, BaseURL: "http://prom:9090"},
		OpenTelemetry: OpenTelemetryLinkConfig{Enabled: false},
	}
	prov := NewStaticBackendIntegration(cfg)
	intg := prov.Integration()
	if !intg.Prometheus.Enabled {
		t.Error("expected Prometheus enabled")
	}
	if intg.OpenTelemetry.Enabled {
		t.Error("expected OpenTelemetry disabled")
	}
}

func TestPrometheusStates(t *testing.T) {
	_ = PrometheusQueryConfig{Enabled: false}
	_ = PrometheusQueryConfig{Enabled: true, BaseURL: "http://prom:9090"}
}

func TestOpenTelemetryStates(t *testing.T) {
	_ = OpenTelemetryLinkConfig{Enabled: false}
	_ = OpenTelemetryLinkConfig{Enabled: true, Backend: "tempo", LinkBase: "http://tempo:3200"}
}
