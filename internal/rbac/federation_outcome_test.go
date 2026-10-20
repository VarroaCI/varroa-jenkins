package rbac

import (
	"slices"
	"sort"
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

// pipelinesRole returns a VarroaRole that grants one pipelines verb and
// optionally references a Global JenkinsRole.
func pipelinesRole(name, verb, jenkinsRoleRef string) *v1alpha1.VarroaRole {
	return &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules:       []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{verb}}},
			JenkinsRoleRef: jenkinsRoleRef,
		},
	}
}

func fedRoleLookup(m map[string]*v1alpha1.VarroaRole) func(string) (*v1alpha1.VarroaRole, bool) {
	return func(name string) (*v1alpha1.VarroaRole, bool) { r, ok := m[name]; return r, ok }
}

func fedJenkinsRoleLookup(m map[string]*v1alpha1.JenkinsRole) func(string) (*v1alpha1.JenkinsRole, bool) {
	return func(name string) (*v1alpha1.JenkinsRole, bool) { r, ok := m[name]; return r, ok }
}

// A cluster-wide pipelines write binding projects its Global child and one
// all-tenants copy. Both names are in the outcome, sorted.
func TestDesiredFederatedCRsClusterWideWriteGetsAllTenantsCopy(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("bind-writer", "writer", alice, nil)}
	roles := map[string]*v1alpha1.VarroaRole{"writer": pipelinesRole("writer", "write", "jenkins-dev")}
	jenkinsRoles := map[string]*v1alpha1.JenkinsRole{
		"jenkins-dev": {ObjectMeta: metav1.ObjectMeta{Name: "jenkins-dev"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}},
	}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(jenkinsRoles), nil)

	if len(bindings) != 2 {
		t.Fatalf("bindings = %d, want 2 (Global child and all-tenants copy): %#v", len(bindings), bindings)
	}
	var global, all *v1alpha1.JenkinsRoleBinding
	for _, b := range bindings {
		if b.Labels[LabelFederatedTenantMember] == "true" {
			all = b
		} else {
			global = b
		}
	}
	if global == nil || all == nil {
		t.Fatalf("missing a binding: %#v", bindings)
	}
	if global.Spec.ControllerScope != nil || global.Spec.RoleRef != "jenkins-dev" {
		t.Errorf("global child = %+v, want role jenkins-dev and no scope", global.Spec)
	}
	if all.Annotations[AnnotationFederatedTenant] != FederatedAllTenants {
		t.Errorf("all-tenants copy annotation = %q, want %q", all.Annotations[AnnotationFederatedTenant], FederatedAllTenants)
	}
	if _, narrowed := all.Annotations[AnnotationFederatedTenantJobs]; narrowed {
		t.Errorf("all-tenants copy carries a jobs annotation")
	}
	if all.Spec.RoleRef != TenantMemberRoleName(FederatedAllTenants) {
		t.Errorf("all-tenants copy roleRef = %q", all.Spec.RoleRef)
	}
	if all.Labels[LabelFederatedFrom] != "writer" {
		t.Errorf("all-tenants copy federated-from = %q", all.Labels[LabelFederatedFrom])
	}
	out := outcomes["bind-writer"]
	want := []string{global.Name, all.Name}
	sort.Strings(want)
	if out.SkipReason != "" || out.RoleName != "jenkins-dev" || !slices.Equal(out.BindingNames, want) {
		t.Errorf("outcome = %+v, want role jenkins-dev and names %v", out, want)
	}
}

// A cluster-wide pipelines write binding whose role has no Jenkins
// projection federates only the all-tenants copy.
func TestDesiredFederatedCRsClusterWideWriteWithoutProjectionFederatesCopyOnly(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("bind-writer", "writer", alice, nil)}
	roles := map[string]*v1alpha1.VarroaRole{"writer": pipelinesRole("writer", "write", "")}
	roleList, bindings, _, outcomes := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(nil), nil)

	if len(roleList) != 0 {
		t.Errorf("roles = %#v, want none", roleList)
	}
	if len(bindings) != 1 || bindings[0].Annotations[AnnotationFederatedTenant] != FederatedAllTenants {
		t.Fatalf("bindings = %#v, want one all-tenants copy", bindings)
	}
	out := outcomes["bind-writer"]
	if out.SkipReason != "" || out.RoleName != "" || len(out.BindingNames) != 1 || out.BindingNames[0] != bindings[0].Name {
		t.Errorf("outcome = %+v, want no role and the copy name", out)
	}
}

// A namespace-scoped write binding gets no all-tenants copy.
func TestDesiredFederatedCRsNamespaceScopedWriteGetsNoAllTenantsCopy(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	scope := &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"x"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("bind-ns", "writer", alice, scope)}
	roles := map[string]*v1alpha1.VarroaRole{"writer": pipelinesRole("writer", "write", "")}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(nil), nil)

	if len(bindings) != 0 {
		t.Errorf("bindings = %#v, want none", bindings)
	}
	if out := outcomes["bind-ns"]; out.SkipReason != SkipNoJenkinsProjection {
		t.Errorf("outcome = %+v, want %s", out, SkipNoJenkinsProjection)
	}
}

// A cluster-wide read binding gets no member copy.
func TestDesiredFederatedCRsClusterWideReadGetsNoMemberCopy(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("bind-reader", "reader", alice, nil)}
	roles := map[string]*v1alpha1.VarroaRole{"reader": pipelinesRole("reader", "read", "jenkins-dev")}
	jenkinsRoles := map[string]*v1alpha1.JenkinsRole{
		"jenkins-dev": {ObjectMeta: metav1.ObjectMeta{Name: "jenkins-dev"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}},
	}
	_, bindings, _, _ := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(jenkinsRoles), nil)

	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v, want only the Global child", bindings)
	}
	if bindings[0].Labels[LabelFederatedTenantMember] == "true" {
		t.Errorf("a read grant got a member copy: %#v", bindings[0])
	}
}

// A cluster-wide write binding with a dangling JenkinsRole reference is a
// full skip. It gets no member copy either.
func TestDesiredFederatedCRsClusterWideWriteMissingJenkinsRoleSkipsAll(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("bind-writer", "writer", alice, nil)}
	roles := map[string]*v1alpha1.VarroaRole{"writer": pipelinesRole("writer", "write", "absent")}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(nil), nil)

	if len(bindings) != 0 {
		t.Errorf("bindings = %#v, want none", bindings)
	}
	if out := outcomes["bind-writer"]; out.SkipReason != SkipJenkinsRoleNotFound || len(out.BindingNames) != 0 {
		t.Errorf("outcome = %+v, want %s with no names", out, SkipJenkinsRoleNotFound)
	}
}

// Two cluster-wide bindings with one role and the same subjects share one
// all-tenants copy. Its owner annotation names both bindings.
func TestDesiredFederatedCRsAllTenantsCopiesShareName(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{
		outcomeBinding("bind-a", "writer", alice, nil),
		outcomeBinding("bind-b", "writer", alice, nil),
	}
	roles := map[string]*v1alpha1.VarroaRole{"writer": pipelinesRole("writer", "write", "")}
	_, bindings, _, outcomes := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(nil), nil)

	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want one shared copy", len(bindings))
	}
	if got := bindings[0].Annotations[AnnotationFederatedBindings]; got != "bind-a,bind-b" {
		t.Errorf("owners annotation = %q, want bind-a,bind-b", got)
	}
	if !slices.Equal(outcomes["bind-a"].BindingNames, outcomes["bind-b"].BindingNames) {
		t.Errorf("outcomes disagree: %+v vs %+v", outcomes["bind-a"], outcomes["bind-b"])
	}
}

// The subject expander applies to the all-tenants copy and not to the
// Global child.
func TestDesiredFederatedCRsExpandsAllTenantsSubjects(t *testing.T) {
	alice := []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}
	vrbs := []*v1alpha1.VarroaRoleBinding{outcomeBinding("bind-writer", "writer", alice, nil)}
	roles := map[string]*v1alpha1.VarroaRole{"writer": pipelinesRole("writer", "write", "jenkins-dev")}
	jenkinsRoles := map[string]*v1alpha1.JenkinsRole{
		"jenkins-dev": {ObjectMeta: metav1.ObjectMeta{Name: "jenkins-dev"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "Global"}},
	}
	expand := func(in []v1alpha1.SubjectRef) []v1alpha1.SubjectRef {
		out := append([]v1alpha1.SubjectRef(nil), in...)
		return append(out, v1alpha1.SubjectRef{Kind: "Group", Name: "team-owners"})
	}
	_, bindings, _, _ := DesiredFederatedCRs(vrbs, fedRoleLookup(roles), fedJenkinsRoleLookup(jenkinsRoles), expand)

	for _, b := range bindings {
		isCopy := b.Labels[LabelFederatedTenantMember] == "true"
		if isCopy && len(b.Spec.Subjects) != 2 {
			t.Errorf("all-tenants copy subjects = %+v, want the expansion", b.Spec.Subjects)
		}
		if !isCopy && len(b.Spec.Subjects) != 1 {
			t.Errorf("global child subjects = %+v, want no expansion", b.Spec.Subjects)
		}
	}
}
