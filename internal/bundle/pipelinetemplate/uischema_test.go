package pipelinetemplate

import (
	"strings"
	"testing"
)

// uischemaTestSchema is the parameters schema every test below checks
// uiSchema against: one declared string property, "goVersion".
var uischemaTestSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"goVersion": map[string]any{"type": "string"},
	},
}

func findingsContain(findings []Finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f.String(), substr) {
			return true
		}
	}
	return false
}

// TestCheckUISchema_DeclaredPropertyAccepted proves a uiSchema entry
// naming a declared property, holding only allowed "ui:" keys, produces
// no finding.
func TestCheckUISchema_DeclaredPropertyAccepted(t *testing.T) {
	uiSchema := map[string]any{
		"goVersion": map[string]any{"ui:title": "Go version"},
	}
	findings := CheckUISchema(uiSchema, uischemaTestSchema)
	if len(findings) != 0 {
		t.Errorf("expected no findings, got: %v", findings)
	}
}

// TestCheckUISchema_UndeclaredPropertyRejected proves 🔴3 of review round
// 1: a non-"ui:" key that names no declared property of the template's
// parameters schema is rejected (design D4's "mirroring properties").
func TestCheckUISchema_UndeclaredPropertyRejected(t *testing.T) {
	uiSchema := map[string]any{
		"notAParameter": map[string]any{"ui:title": "Hidden"},
	}
	findings := CheckUISchema(uiSchema, uischemaTestSchema)
	if len(findings) == 0 {
		t.Fatal("expected a finding for an undeclared property key, got none")
	}
	if !findingsContain(findings, "notAParameter") || !findingsContain(findings, "no declared property") {
		t.Errorf("expected a finding naming the undeclared key, got: %v", findings)
	}
}

// TestCheckUISchema_UIOrderWildcardAccepted proves ui:order naming a
// subset of declared properties plus "*" produces no finding.
func TestCheckUISchema_UIOrderWildcardAccepted(t *testing.T) {
	uiSchema := map[string]any{
		"ui:order": []any{"goVersion", "*"},
	}
	findings := CheckUISchema(uiSchema, uischemaTestSchema)
	if len(findings) != 0 {
		t.Errorf("expected no findings, got: %v", findings)
	}
}

// TestCheckUISchema_UIOrderMissingPropertyRejected proves ui:order that
// omits a declared property and carries no "*" is rejected.
func TestCheckUISchema_UIOrderMissingPropertyRejected(t *testing.T) {
	uiSchema := map[string]any{
		"ui:order": []any{},
	}
	findings := CheckUISchema(uiSchema, uischemaTestSchema)
	if len(findings) == 0 {
		t.Fatal("expected a finding for an omitted declared property, got none")
	}
	if !findingsContain(findings, "goVersion") || !findingsContain(findings, "omits declared property") {
		t.Errorf("expected a finding naming the omitted property, got: %v", findings)
	}
}

// TestCheckUISchema_UIOrderUnknownEntryRejected proves 🔴3's second half:
// a ui:order entry naming no declared property is rejected, even when
// the list also carries "*".
func TestCheckUISchema_UIOrderUnknownEntryRejected(t *testing.T) {
	uiSchema := map[string]any{
		"ui:order": []any{"notAParameter", "*"},
	}
	findings := CheckUISchema(uiSchema, uischemaTestSchema)
	if len(findings) == 0 {
		t.Fatal("expected a finding for an unknown ui:order entry, got none")
	}
	if !findingsContain(findings, "notAParameter") || !findingsContain(findings, "no declared property") {
		t.Errorf("expected a finding naming the unknown entry, got: %v", findings)
	}
}

// TestCheckUISchema_UnknownUIKeyRejected proves a "ui:" key outside the
// fixed subset is rejected.
func TestCheckUISchema_UnknownUIKeyRejected(t *testing.T) {
	uiSchema := map[string]any{
		"ui:field": "CustomWidget",
	}
	findings := CheckUISchema(uiSchema, uischemaTestSchema)
	if len(findings) == 0 {
		t.Fatal("expected a finding for ui:field, got none")
	}
	if !findingsContain(findings, "ui:field") {
		t.Errorf("expected a finding naming ui:field, got: %v", findings)
	}
}
