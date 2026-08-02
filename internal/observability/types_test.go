package observability

import (
	"testing"
)

func TestValidProviders(t *testing.T) {
	for _, p := range []string{ProviderJenkinsAPI, ProviderPrometheus, ProviderOpenTelemetry} {
		if !ValidProviders[p] {
			t.Errorf("expected %q to be a valid provider", p)
		}
	}
	if ValidProviders["unknown-provider"] {
		t.Error("unknown-provider should not be valid")
	}
}

func TestValidCapabilities(t *testing.T) {
	want := []string{
		CapabilityJenkinsHealth,
		CapabilityJenkinsJobsSummary,
		CapabilityJenkinsBuildsRecent,
		CapabilityJenkinsBuildsTrends,
		CapabilityJenkinsQueueMetrics,
		CapabilityJenkinsExecutorsMetrics,
		CapabilityJenkinsMetricsEndpoint,
		CapabilityJenkinsMetricsQuery,
		CapabilityJenkinsTracesExporting,
		CapabilityJenkinsTracesQuery,
	}
	for _, c := range want {
		if !ValidCapabilities[c] {
			t.Errorf("expected %q to be a valid capability", c)
		}
	}
	if ValidCapabilities["unknown-capability"] {
		t.Error("unknown-capability should not be valid")
	}
}

func TestDeriveLevel(t *testing.T) {
	tests := []struct {
		name string
		caps map[string]bool
		want ObservabilityLevel
	}{
		{
			name: "level 0 control-plane only",
			caps: map[string]bool{CapabilityJenkinsHealth: true},
			want: LevelControlPlane,
		},
		{
			name: "level 0 empty capabilities",
			caps: map[string]bool{},
			want: LevelControlPlane,
		},
		{
			name: "level 1 live Jenkins summary",
			caps: map[string]bool{
				CapabilityJenkinsJobsSummary:  true,
				CapabilityJenkinsBuildsRecent: true,
			},
			want: LevelLiveSummary,
		},
		{
			name: "level 2 Prometheus metrics",
			caps: map[string]bool{
				CapabilityJenkinsMetricsEndpoint: true,
				CapabilityJenkinsMetricsQuery:    true,
				CapabilityJenkinsBuildsTrends:    true,
			},
			want: LevelPrometheusMetrics,
		},
		{
			name: "level 2 beats level 1 when both satisfied",
			caps: map[string]bool{
				CapabilityJenkinsJobsSummary:     true,
				CapabilityJenkinsBuildsRecent:    true,
				CapabilityJenkinsMetricsEndpoint: true,
				CapabilityJenkinsMetricsQuery:    true,
				CapabilityJenkinsBuildsTrends:    true,
			},
			want: LevelPrometheusMetrics,
		},
		{
			name: "level 3 OpenTelemetry insights",
			caps: map[string]bool{
				CapabilityJenkinsTracesExporting: true,
			},
			want: LevelOpenTelemetry,
		},
		{
			name: "level 3 with trace query",
			caps: map[string]bool{
				CapabilityJenkinsTracesExporting: true,
				CapabilityJenkinsTracesQuery:     true,
			},
			want: LevelOpenTelemetry,
		},
		{
			name: "level 3 beats level 2",
			caps: map[string]bool{
				CapabilityJenkinsMetricsEndpoint: true,
				CapabilityJenkinsMetricsQuery:    true,
				CapabilityJenkinsBuildsTrends:    true,
				CapabilityJenkinsTracesExporting: true,
			},
			want: LevelOpenTelemetry,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveLevel(tt.caps)
			if got != tt.want {
				t.Errorf("DeriveLevel() = %d (%s), want %d (%s)", got, LevelName(got), tt.want, LevelName(tt.want))
			}
		})
	}
}

func TestLevelName(t *testing.T) {
	if LevelName(LevelControlPlane) != "Control-plane only" {
		t.Errorf("level 0 name mismatch: %q", LevelName(LevelControlPlane))
	}
	if LevelName(LevelLiveSummary) != "Live Jenkins summary" {
		t.Errorf("level 1 name mismatch: %q", LevelName(LevelLiveSummary))
	}
	if LevelName(LevelPrometheusMetrics) != "Prometheus metrics" {
		t.Errorf("level 2 name mismatch: %q", LevelName(LevelPrometheusMetrics))
	}
	if LevelName(LevelOpenTelemetry) != "OpenTelemetry insights" {
		t.Errorf("level 3 name mismatch: %q", LevelName(LevelOpenTelemetry))
	}
	if LevelName(999) != "Unknown" {
		t.Errorf("unknown level name mismatch: %q", LevelName(999))
	}
}

func TestControllerObservabilityWarning(t *testing.T) {
	w := Warning{Message: "test warning"}
	if w.Message != "test warning" {
		t.Error("warning should preserve message")
	}
}

func TestObservableSource(t *testing.T) {
	src := ObservableSource{
		Provider: ProviderPrometheus,
		Status:   StatusIntended,
		Error:    "oops",
		Hints:    map[string]string{"endpoint": "/prometheus/"},
	}
	if src.Provider != ProviderPrometheus {
		t.Error("provider mismatch")
	}
	if src.Status != StatusIntended {
		t.Error("status mismatch")
	}
	if src.Error != "oops" {
		t.Error("error mismatch")
	}
	if src.Hints["endpoint"] != "/prometheus/" {
		t.Error("hints mismatch")
	}
}

func TestJenkinsSummary(t *testing.T) {
	s := JenkinsSummary{
		TotalJobs:     5,
		RunningBuilds: 1,
		RecentBuilds: []JenkinsBuildSummary{
			{JobName: "deploy", BuildNumber: 42, Status: "SUCCESS", DurationSeconds: 120},
		},
	}
	if s.TotalJobs != 5 {
		t.Error("TotalJobs mismatch")
	}
	if s.RunningBuilds != 1 {
		t.Error("RunningBuilds mismatch")
	}
	if len(s.RecentBuilds) != 1 {
		t.Error("RecentBuilds length mismatch")
	}
	if s.RecentBuilds[0].JobName != "deploy" {
		t.Error("build JobName mismatch")
	}
}

func TestFreshness(t *testing.T) {
	f := Freshness{
		ObservedAt: "2026-01-01T00:00:00Z",
		MiteTTL:    180,
		Stale:      false,
	}
	if f.ObservedAt != "2026-01-01T00:00:00Z" {
		t.Error("ObservedAt mismatch")
	}
	if f.MiteTTL != 180 {
		t.Error("MiteTTL mismatch")
	}
	if f.Stale {
		t.Error("expected stale=false")
	}
}
