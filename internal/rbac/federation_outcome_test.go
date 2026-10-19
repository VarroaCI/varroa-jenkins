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
		func(name string) (*v1alpha1.JenkinsRole, bool) { r, ok := jenkinsRoles[name]; return r, ok }, nil,
	)

	wantSkip := map[string]string{
		"no-subjects":          SkipNoSubjects,
		"tenant":               SkipNoTenantWrite,
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
		if got.RoleName != "" || len(got.BindingNames) != 0 {
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
		if got.SkipReason != "" || got.RoleName != "jenkins-admin" || len(got.BindingNames) != 1 || got.BindingNames[0] != bindings[0].Name {
			t.Fatalf("outcome[%s] = %#v, want role jenkins-admin and binding %s", name, got, bindings[0].Name)
		}
	}
}

// A tenant-scoped binding whose role grants pipelines write gets one copy for
// each tenant. A narrowed tenant gets the job globs, and a whole-folder tenant
// gets no annotation.
func TestDesiredFederatedCRsTenantWriteGetsOneChildPerTenant(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	scope := &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{
		{Name: "team-a", Jobs: []string{"deploy/*"}},
		{Name: "team-b"},
	}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("tenant-write", "pipelines-writer", alice, scope)}
	roles := map[string]*v1alpha1.VarroaRole{
		"pipelines-writer": {
			ObjectMeta: metav1.ObjectMeta{Name: "pipelines-writer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"pipelines"}, Verbs: []string{"write"}},
			}},
		},
	}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs,
		func(name string) (*v1alpha1.VarroaRole, bool) { r, ok := roles[name]; return r, ok },
		func(string) (*v1alpha1.JenkinsRole, bool) { return nil, false }, nil,
	)
	got := outcomes["tenant-write"]
	if got.SkipReason != "" || len(got.BindingNames) != 2 {
		t.Fatalf("outcome = %#v, want two child names", got)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %d, want 2", len(bindings))
	}
	byTeam := map[string]*v1alpha1.JenkinsRoleBinding{}
	for _, b := range bindings {
		if b.Labels[LabelFederatedTenantMember] != "true" || b.Labels[LabelFederatedFrom] != "pipelines-writer" {
			t.Fatalf("labels = %#v, want tenant-member and federated-from", b.Labels)
		}
		if b.Spec.ControllerScope != nil || b.Spec.JenkinsScope != nil {
			t.Fatalf("spec scopes = %#v / %#v, want none", b.Spec.ControllerScope, b.Spec.JenkinsScope)
		}
		byTeam[b.Annotations[AnnotationFederatedTenant]] = b
	}
	narrowed := byTeam["team-a"]
	if narrowed == nil || narrowed.Spec.RoleRef != "varroa:tenant-member:team-a" {
		t.Fatalf("team-a copy = %#v", narrowed)
	}
	if narrowed.Annotations[AnnotationFederatedTenantJobs] != `["deploy/*"]` {
		t.Fatalf("team-a jobs annotation = %q", narrowed.Annotations[AnnotationFederatedTenantJobs])
	}
	whole := byTeam["team-b"]
	if whole == nil || whole.Spec.RoleRef != "varroa:tenant-member:team-b" {
		t.Fatalf("team-b copy = %#v", whole)
	}
	if _, has := whole.Annotations[AnnotationFederatedTenantJobs]; has {
		t.Fatalf("team-b carries a jobs annotation: %#v", whole.Annotations)
	}
}

// A tenant entry with an empty job list grants nothing, so the binding skips.
func TestDesiredFederatedCRsTenantEmptyJobsSkips(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	scope := &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "team-a", Jobs: []string{}}}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("empty", "pipelines-writer", alice, scope)}
	roles := map[string]*v1alpha1.VarroaRole{
		"pipelines-writer": {
			ObjectMeta: metav1.ObjectMeta{Name: "pipelines-writer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"pipelines"}, Verbs: []string{"write"}},
			}},
		},
	}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs,
		func(name string) (*v1alpha1.VarroaRole, bool) { r, ok := roles[name]; return r, ok },
		func(string) (*v1alpha1.JenkinsRole, bool) { return nil, false }, nil,
	)
	if got := outcomes["empty"]; got.SkipReason != SkipNoTenantJobs {
		t.Fatalf("outcome = %#v, want %s", got, SkipNoTenantJobs)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %d, want 0", len(bindings))
	}
}
