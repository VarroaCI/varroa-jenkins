package controller

import "testing"

func TestPipelineTemplateSummaries_InputOrderAndEmpty(t *testing.T) {
	if got := pipelineTemplateSummaries(""); len(got) != 0 || got == nil {
		t.Fatalf("empty document must list an empty, non-nil slice, got %#v", got)
	}
	doc := "templates:\n" +
		"  - apiVersion: varroa.dev/v1\n    kind: PipelineTemplate\n    metadata: {name: b, displayName: B}\n    spec: {version: 2.0.0, jenkinsfile: 'x'}\n" +
		"  - apiVersion: varroa.dev/v1\n    kind: PipelineTemplate\n    metadata: {name: a, description: first}\n    spec: {version: 1.0.0, jenkinsfile: 'y'}\n"
	got := pipelineTemplateSummaries(doc)
	if len(got) != 2 || got[0].Name != "b" || got[0].Version != "2.0.0" || got[0].DisplayName != "B" || got[1].Name != "a" || got[1].Description != "first" {
		t.Fatalf("unexpected summaries: %#v", got)
	}
}
