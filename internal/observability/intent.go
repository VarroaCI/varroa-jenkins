package observability

import (
	"strings"
)

const (
	// AnnotationProviders declares intended observability providers.
	AnnotationProviders = "observability.varroa.dev/providers"
	// AnnotationCapabilities declares intended observability capabilities.
	AnnotationCapabilities = "observability.varroa.dev/capabilities"
)

// ParseIntent parses observability intent annotations from a single
// resource (CatalogItem or ComposedBundle). Returns valid provider
// and capability tokens plus any warnings for unrecognized tokens.
func ParseIntent(annotations map[string]string) (providers []string, capabilities []string, warnings []Warning) {
	for key, value := range annotations {
		switch key {
		case AnnotationProviders:
			tokens := splitTokens(value)
			for _, tok := range tokens {
				if ValidProviders[tok] {
					providers = append(providers, tok)
				} else {
					warnings = append(warnings, Warning{
						Message: "unknown observability provider \"" + tok + "\" in annotation " + AnnotationProviders,
					})
				}
			}
		case AnnotationCapabilities:
			tokens := splitTokens(value)
			for _, tok := range tokens {
				if ValidCapabilities[tok] {
					capabilities = append(capabilities, tok)
				} else {
					warnings = append(warnings, Warning{
						Message: "unknown observability capability \"" + tok + "\" in annotation " + AnnotationCapabilities,
					})
				}
			}
		}
	}
	return providers, capabilities, warnings
}

// UnionIntents merges intent from multiple annotation sources into a
// single set of providers and capabilities, deduplicating and collecting
// any warnings from invalid tokens.
func UnionIntents(annotationSets ...map[string]string) (providers []string, capabilities []string, warnings []Warning) {
	providerSet := make(map[string]bool)
	capabilitySet := make(map[string]bool)

	for _, anns := range annotationSets {
		ps, cs, ws := ParseIntent(anns)
		for _, p := range ps {
			providerSet[p] = true
		}
		for _, c := range cs {
			capabilitySet[c] = true
		}
		warnings = append(warnings, ws...)
	}

	for p := range providerSet {
		providers = append(providers, p)
	}
	for c := range capabilitySet {
		capabilities = append(capabilities, c)
	}
	return providers, capabilities, warnings
}

// UnionResult holds the deduplicated result of unioning intent from
// multiple annotation sources.
type UnionResult struct {
	Providers    []string
	Capabilities []string
	Warnings     []Warning
}

// UnionIntentsResult is UnionIntents with a struct return.
func UnionIntentsResult(annotationSets ...map[string]string) UnionResult {
	providers, capabilities, warnings := UnionIntents(annotationSets...)
	return UnionResult{
		Providers:    providers,
		Capabilities: capabilities,
		Warnings:     warnings,
	}
}

func splitTokens(s string) []string {
	var tokens []string
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokens
}
