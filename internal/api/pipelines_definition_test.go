package api

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// TestBuildPipelineDefinition_SourceNamesWhereTheScriptLives asserts the
// Defined by card's data for the ways an item declares its script: a
// repository, an inline script, a catalog template, and none of them.
func TestBuildPipelineDefinition_SourceNamesWhereTheScriptLives(t *testing.T) {
	scm := &items.Item{Kind: "pipeline", Name: "build", Definition: &items.PipelineDefinition{
		CpsScmFlowDefinition: &items.CpsScmFlowDefinition{SCM: items.SCM{GitSCM: &items.GitSCM{
			UserRemoteConfigs: []items.UserRemoteConfig{{UserRemoteConfig: items.RemoteConfig{URL: "https://git.example.com/payments/api.git"}}},
		}}},
	}}
	got := buildPipelineDefinition(scm)
	if got == nil || got.Source != "jenkinsfile" || got.JenkinsfileRepo != "https://git.example.com/payments/api.git" {
		t.Fatalf("SCM item: got %+v, want source jenkinsfile with the remote URL", got)
	}

	multibranch := &items.Item{Kind: "multibranch", Name: "api", SourcesList: []items.BranchSourceEntry{
		{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{Git: &items.GitSCMSource{Remote: "https://git.example.com/payments/api.git"}}}},
	}}
	got = buildPipelineDefinition(multibranch)
	if got == nil || got.Source != "jenkinsfile" || got.JenkinsfileRepo != "https://git.example.com/payments/api.git" {
		t.Fatalf("multibranch item: got %+v, want source jenkinsfile with the branch source remote", got)
	}

	inline := &items.Item{Kind: "pipeline", Name: "build", Definition: &items.PipelineDefinition{Script: "pipeline { agent any }"}}
	got = buildPipelineDefinition(inline)
	if got == nil || got.Source != "inline" || got.JenkinsfileRepo != "" {
		t.Fatalf("inline item: got %+v, want source inline and no repository", got)
	}

	templated := &items.Item{Kind: "pipeline", Name: "build", Definition: &items.PipelineDefinition{
		VarroaTemplate: &items.VarroaTemplateDefinition{Template: "go-service", TemplateVersion: "2.1.4"},
	}}
	got = buildPipelineDefinition(templated)
	if got == nil || got.Source != "template" || got.JenkinsfileRepo != "" {
		t.Fatalf("template item: got %+v, want source template and no repository", got)
	}

	if got := buildPipelineDefinition(&items.Item{Kind: "pipeline", Name: "bare"}); got != nil {
		t.Fatalf("an item with no definition must produce no row, got %+v", got)
	}
	if got := buildPipelineDefinition(nil); got != nil {
		t.Fatalf("a nil item must produce no row, got %+v", got)
	}
}
