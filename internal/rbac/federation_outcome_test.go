package rbac

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func outcomeBinding(name, roleRef string, subjects []v1alpha1.SubjectRef, scope *v1alpha1.VarroaRoleBindingScope) *v1alpha1.VarroaRoleBinding {
	return &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: roleRef, Subjects: subjects, Scope: scope},
	}
}

// Every source binding gets exactly one outcome, keyed by its name. A skip
// names a fixed reason code, and a projected binding names its children.
func TestDesiredFederatedCRsReportsOneOutcomePerBinding(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{
		outcomeBinding("no-subjects", "ref-role", nil, nil),
		outcomeBinding("tenant", "ref-role", alice, &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a"}}}),
		outcomeBinding("missing-role", "ghost", alice, nil),
		outcomeBinding("pipelines-only", "pipelines-role", alice, nil),
		outcomeBinding("missing-jenkins-role", "dangling-role", alice, nil),
		outcomeBinding("non-global", "item-role", alice, nil),
		outcomeBinding("reserved", "system-mite", alice, nil),
		outcomeBinding("projected", "ref-role", alice, nil),
		outcomeBinding("projected-twin", "ref-role", alice, nil),
	}
	roles := map[string]*v1alpha1.VarroaRole{
		"ref-role":       {ObjectMeta: metav1.ObjectMeta{Name: "ref-role"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "jenkins-admin"}},
		"pipelines-role": {ObjectMeta: metav1.ObjectMeta{Name: "pipelines-role"}},
		"dangling-role":  {ObjectMeta: metav1.ObjectMeta{Name: "dangling-role"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "absent"}},
		"item-role":      {ObjectMeta: metav1.ObjectMeta{Name: "item-role"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "item-jr"}},
		"system-mite":    {ObjectMeta: metav1.ObjectMeta{Name: "system-mite"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsPermissions: []string{"Overall/Read"}}},
	}
	jenkinsRoles := map[string]*v1alpha1.JenkinsRole{
		"jenkins-admin": {ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}},
		"item-jr":       {ObjectMeta: metav1.ObjectMeta{Name: "item-jr"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Item"}},
	}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs,
		func(name string) (*v1alpha1.VarroaRole, bool) { r, ok := roles[name]; return r, ok },
		func(name string) (*v1alpha1.JenkinsRole, bool) { r, ok := jenkinsRoles[name]; return r, ok },
	)

	wantSkip := map[string]string{
		"no-subjects":          SkipNoSubjects,
		"tenant":               SkipTenantScope,
		"missing-role":         SkipRoleNotFound,
		"pipelines-only":       SkipNoJenkinsProjection,
		"missing-jenkins-role": SkipJenkinsRoleNotFound,
		"non-global":           SkipNonGlobalJenkinsRole,
		"reserved":             SkipReservedRole,
	}
	if len(outcomes) != len(vrbs) {
		t.Fatalf("outcomes = %d entries, want %d: %#v", len(outcomes), len(vrbs), outcomes)
	}
	for name, reason := range wantSkip {
		got, ok := outcomes[name]
		if !ok {
			t.Fatalf("no outcome for %s", name)
		}
		if got.SkipReason != reason || got.SkipMessage == "" {
			t.Fatalf("outcome[%s] = %#v, want skip reason %s with a message", name, got, reason)
		}
		if got.RoleName != "" || got.BindingName != "" {
			t.Fatalf("outcome[%s] names children for a skip: %#v", name, got)
		}
	}
	if !strings.Contains(outcomes["missing-role"].SkipMessage, "ghost") {
		t.Fatalf("missing-role message = %q, want the role name", outcomes["missing-role"].SkipMessage)
	}

	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v, want one shared child", bindings)
	}
	for _, name := range []string{"projected", "projected-twin"} {
		got := outcomes[name]
		if got.SkipReason != "" || got.RoleName != "jenkins-admin" || got.BindingName != bindings[0].Name {
			t.Fatalf("outcome[%s] = %#v, want role jenkins-admin and binding %s", name, got, bindings[0].Name)
		}
	}
}
