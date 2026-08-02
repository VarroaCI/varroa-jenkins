package observability

import (
	"testing"
)

func TestParseIntentCatalogItem(t *testing.T) {
	anns := map[string]string{
		AnnotationProviders:    "prometheus",
		AnnotationCapabilities: "jenkins.metrics.endpoint",
	}
	providers, capabilities, warnings := ParseIntent(anns)

	if len(providers) != 1 || providers[0] != "prometheus" {
		t.Errorf("providers = %v, want [prometheus]", providers)
	}
	found := false
	for _, c := range capabilities {
		if c == CapabilityJenkinsMetricsEndpoint {
			found = true
		}
	}
	if !found {
		t.Errorf("capabilities = %v, want to contain %s", capabilities, CapabilityJenkinsMetricsEndpoint)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestParseIntentComposedBundle(t *testing.T) {
	anns := map[string]string{
		AnnotationProviders:    "opentelemetry",
		AnnotationCapabilities: "jenkins.traces.exporting",
	}
	providers, capabilities, _ := ParseIntent(anns)

	if len(providers) != 1 || providers[0] != "opentelemetry" {
		t.Errorf("providers = %v, want [opentelemetry]", providers)
	}
	found := false
	for _, c := range capabilities {
		if c == CapabilityJenkinsTracesExporting {
			found = true
		}
	}
	if !found {
		t.Errorf("capabilities = %v, want to contain %s", capabilities, CapabilityJenkinsTracesExporting)
	}
}

func TestParseIntentUnion(t *testing.T) {
	bundleAnns := map[string]string{
		AnnotationProviders: "opentelemetry",
	}
	itemAnns := map[string]string{
		AnnotationProviders: "prometheus",
	}
	providers, _, _ := UnionIntents(bundleAnns, itemAnns)

	has := func(p string) bool {
		for _, x := range providers {
			if x == p {
				return true
			}
		}
		return false
	}
	if !has("opentelemetry") || !has("prometheus") {
		t.Errorf("providers = %v, want both opentelemetry and prometheus", providers)
	}
}

func TestParseIntentInvalidTokenIgnored(t *testing.T) {
	anns := map[string]string{
		AnnotationProviders: "prometheus,unknown-provider",
	}
	providers, _, warnings := ParseIntent(anns)

	if len(providers) != 1 || providers[0] != "prometheus" {
		t.Errorf("providers = %v, want [prometheus]", providers)
	}
	if len(warnings) == 0 {
		t.Error("expected warning for invalid provider token")
	}
}

func TestParseIntentInvalidCapabilityIgnored(t *testing.T) {
	anns := map[string]string{
		AnnotationCapabilities: "jenkins.health,unknown-capability",
	}
	_, capabilities, warnings := ParseIntent(anns)

	found := false
	for _, c := range capabilities {
		if c == CapabilityJenkinsHealth {
			found = true
		}
	}
	if !found {
		t.Errorf("capabilities = %v, want to contain jenkins.health", capabilities)
	}
	if len(warnings) == 0 {
		t.Error("expected warning for invalid capability token")
	}
}

func TestParseIntentEmptyAnnotations(t *testing.T) {
	providers, capabilities, warnings := ParseIntent(map[string]string{})
	if len(providers) != 0 {
		t.Errorf("providers = %v, want none", providers)
	}
	if len(capabilities) != 0 {
		t.Errorf("capabilities = %v, want none", capabilities)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestParseIntentWhitespaceHandling(t *testing.T) {
	anns := map[string]string{
		AnnotationProviders: "  prometheus ,  jenkins-api  ",
	}
	providers, _, _ := ParseIntent(anns)

	if len(providers) != 2 {
		t.Errorf("providers = %v, want 2 providers", providers)
	}
}

func TestUnionIntentsResult(t *testing.T) {
	r := UnionIntentsResult(
		map[string]string{AnnotationProviders: "prometheus"},
		map[string]string{AnnotationProviders: "prometheus,jenkins-api"},
	)
	if len(r.Providers) != 2 {
		t.Errorf("providers = %v, want 2 deduplicated", r.Providers)
	}
}
