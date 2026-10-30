package api

import (
	"regexp"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// pipelineJobGrantAuthorizer grants verbs on "pipelines" for the Tenant pay,
// limited to the folder-relative job globs in jobs. It builds the binding the
// way pipelineGrantAuthorizer builds its Tenants-scoped binding.
func pipelineJobGrantAuthorizer(verbs []string, jobs ...string) *Authorizer {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pl"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: verbs}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "pl",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "pay", Jobs: jobs}},
			},
		},
	}
	return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)
}

func TestPipelineReadJobPatternsMatchGrantsAllow(t *testing.T) {
	globs := []string{"release/v1.2+*", "eu/**", "a?c", "*"}
	paths := []string{"pay", "pay/release/v1.2+x", "pay/release/v1x2+x", "pay/release/v1.2+x/y",
		"pay/eu/a/b", "pay/abc", "pay/a/c", "pay/top", "other/top", "payx/top"}
	a := pipelineJobGrantAuthorizer([]string{"read"}, globs...)
	ref := TenantRef{Tenant: "pay", TenantName: "pay", Folder: "pay"}
	patterns, ok := a.PipelineReadJobPatterns(resolverClaims, ref)
	if !ok || patterns == nil {
		t.Fatalf("patterns = %v, ok = %v; want a non-nil list", patterns, ok)
	}
	grants, _ := a.tenantGrants(resolverClaims, "pay", "pay")
	for _, p := range paths {
		want := grantsAllow(grants, "pay", p, "read")
		got := false
		for _, re := range patterns {
			if regexp.MustCompile(re).MatchString(p) {
				got = true
			}
		}
		if got != want {
			t.Errorf("path %q: regexp %v, grantsAllow %v", p, got, want)
		}
	}
}

func TestPipelineReadJobPatternsUnscopedGrantIsNil(t *testing.T) {
	a := pipelineGrantAuthorizer([]string{"read"}, "pay")
	ref := TenantRef{Tenant: "pay", TenantName: "pay", Folder: "pay"}
	patterns, ok := a.PipelineReadJobPatterns(resolverClaims, ref)
	if patterns != nil || !ok {
		t.Fatalf("patterns = %v, ok = %v; want nil and true", patterns, ok)
	}
}

func TestPipelineReadJobPatternsWriteOnlyIsNotOK(t *testing.T) {
	a := pipelineGrantAuthorizer([]string{"write"}, "pay")
	ref := TenantRef{Tenant: "pay", TenantName: "pay", Folder: "pay"}
	if _, ok := a.PipelineReadJobPatterns(resolverClaims, ref); ok {
		t.Fatal("a write-only grant answered ok for read")
	}
}

func TestJobPatternRegexpsControllerTenant(t *testing.T) {
	got := jobPatternRegexps("", []string{"svc-*"})
	want := []string{"^(?:svc-[^/]*)$"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("jobPatternRegexps = %v; want %v", got, want)
	}
}
