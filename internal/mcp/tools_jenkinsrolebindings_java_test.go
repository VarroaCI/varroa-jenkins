package mcp

import "testing"

// A Pattern scope that Java compiles but RE2 refuses is stored unchanged. The
// lookahead is a Java-only construct, so the check must not use Go regexp.
func TestCreateJenkinsRoleBinding_JavaOnlyPatternAccepted(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-java-only", map[string]interface{}{
		"jenkinsScope": map[string]interface{}{"type": "Pattern", "pattern": "(?=payments/).*"},
	}))
	if tr.IsError {
		t.Fatalf("create refused a pattern Java compiles: %s", toolText(tr))
	}
	got := storedJRB(t, deps, "jrb-java-only")
	if got.Spec.JenkinsScope == nil || got.Spec.JenkinsScope.Pattern != "(?=payments/).*" {
		t.Errorf("stored jenkinsScope = %+v, want the pattern unchanged", got.Spec.JenkinsScope)
	}
}
