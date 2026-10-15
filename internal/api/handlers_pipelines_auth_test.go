package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// pipelinesAuthClaims is the caller identity every test in this file uses.
var pipelinesAuthClaims = &auth.Claims{Subject: "u"}

// readOnlyPipelinesServer returns a Server whose caller holds
// pipelines:read on tenant "team-a" and nothing else.
func readOnlyPipelinesServer() *Server {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "reader"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
			RoleRef:  "reader",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}},
		}},
	}
	return &Server{deps: &Dependencies{Authorizer: NewAuthorizer(testResolver(roles, bindings), false)}}
}

// TestCanWritePipeline_ReadGrantAloneRefusesEveryWritePath asserts that a
// caller holding only the tenant read grant is refused every write-path
// helper, per task 1.5's acceptance.
func TestCanWritePipeline_ReadGrantAloneRefusesEveryWritePath(t *testing.T) {
	s := readOnlyPipelinesServer()
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	// Sanity check: the read grant itself must work, or the refusals
	// below would prove nothing.
	if !s.canReadPipeline(pipelinesAuthClaims, ref, "") {
		t.Fatal("expected the read grant to admit canReadPipeline")
	}

	if s.canWritePipeline(pipelinesAuthClaims, ref, "") {
		t.Error("expected a read-only grant to refuse canWritePipeline")
	}
	if s.canWritePipeline(pipelinesAuthClaims, ref, "svc") {
		t.Error("expected a read-only grant to refuse canWritePipeline on a job path")
	}
}

// TestCanReadRun_OneTenantOfASharedCellCannotReadAnother asserts that a
// caller permitted on one tenant of a shared cell reads no run of a
// second tenant on that cell. The check is tenant-scoped, so two tenants
// sharing one physical cell stay separated by their run rows' own tenant
// column.
func TestCanReadRun_OneTenantOfASharedCellCannotReadAnother(t *testing.T) {
	s := readOnlyPipelinesServer() // reads "team-a" only

	if !s.canReadRun(pipelinesAuthClaims, "team-a", "team-a/svc") {
		t.Error("expected the permitted tenant's run to read")
	}
	if s.canReadRun(pipelinesAuthClaims, "team-b", "team-b/svc") {
		t.Error("expected a second tenant's run, on the same cell, to be refused")
	}
}

// TestNilAuthorizer_DeniesEveryPipelinesHelper asserts that a nil
// Authorizer returns no record and publishes no command: every helper in
// this file denies.
func TestNilAuthorizer_DeniesEveryPipelinesHelper(t *testing.T) {
	s := &Server{deps: &Dependencies{}}
	ref := TenantRef{Tenant: "team-a", Folder: "team-a"}

	if s.canReadPipeline(pipelinesAuthClaims, ref, "") {
		t.Error("expected a nil Authorizer to deny canReadPipeline")
	}
	if s.canWritePipeline(pipelinesAuthClaims, ref, "") {
		t.Error("expected a nil Authorizer to deny canWritePipeline")
	}
	if s.canReadRun(pipelinesAuthClaims, "team-a", "team-a/svc") {
		t.Error("expected a nil Authorizer to deny canReadRun")
	}
}
