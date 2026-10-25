package mcp

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// lintTestDeps wires the tenant resolver the HTTP server wires, so the tool
// reaches the same tenant refusals as the route.
func lintTestDeps(store *crdstore.Fake, verb string) *api.Dependencies {
	authorizer := pipelinesTestAuthorizer(verb)
	return &api.Dependencies{
		Store:          store,
		RecordStore:    newFakePipelineReader(),
		Authorizer:     authorizer,
		TenantResolver: api.NewTenantTenantResolver(store, nil, authorizer),
	}
}

// TestLintJenkinsfileToolRequiresTenantAndJenkinsfile asserts both
// arguments are required. A missing one is a tool error.
func TestLintJenkinsfileToolRequiresTenantAndJenkinsfile(t *testing.T) {
	deps := lintTestDeps(crdstore.NewFake(), "read")

	if tr := callPipelineTool(t, deps, "lint_jenkinsfile", map[string]interface{}{"jenkinsfile": "pipeline {}"}); !tr.IsError {
		t.Error("missing tenant: expected a tool error")
	}
	if tr := callPipelineTool(t, deps, "lint_jenkinsfile", map[string]interface{}{"tenant": "team-a"}); !tr.IsError {
		t.Error("missing jenkinsfile: expected a tool error")
	}
}

// TestLintJenkinsfileToolRefusesWithoutTenantRead asserts a caller without
// the tenant read grant gets the same refusal as the HTTP route.
func TestLintJenkinsfileToolRefusesWithoutTenantRead(t *testing.T) {
	store := crdstore.NewFake()
	seedWarningTenant(t, store, "team-b")
	deps := lintTestDeps(store, "write")

	tr := callPipelineTool(t, deps, "lint_jenkinsfile", map[string]interface{}{
		"tenant": "team-b", "jenkinsfile": "pipeline {}",
	})
	if !tr.IsError {
		t.Fatal("expected lint_jenkinsfile to deny a caller without the tenant read grant")
	}
	if got := toolText(tr); got != "tenant not found" {
		t.Errorf("error text = %q, want %q", got, "tenant not found")
	}
}
