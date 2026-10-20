package rbac

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const memberUserTestNamespace = "varroa-system"

// memberUserTestUser returns an idp-managed User object in the operator
// namespace, with the subject sub-alice. An empty argument leaves its field unset.
func memberUserTestUser(name, namespace, preferred, email string) *v1alpha1.User {
	u := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.UserSpec{Email: email},
	}
	u.Annotations[v1alpha1.AnnotationOIDCSubject] = "sub-alice"
	if preferred != "" {
		u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername] = preferred
	}
	return u
}

// withUserDirectory wires the given User objects as the operator's user
// directory.
func withUserDirectory(r *Resolver, users ...*v1alpha1.User) {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, u := range users {
		_ = idx.Add(u)
	}
	r.SetUserDirectory(idx, memberUserTestNamespace)
}

// memberUserTestWriterBinding grants developer, cluster-wide, to one subject.
func memberUserTestWriterBinding(subject v1alpha1.SubjectRef) *v1alpha1.VarroaRoleBinding {
	return &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "writers"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{subject},
			RoleRef:  "developer",
		},
	}
}

// memberUserTestSubjects returns "Kind:Name" for each subject of the member
// role of the test team, cluster-wide or narrowed.
func memberUserTestSubjects(t *testing.T, r *Resolver, roleName string) map[string]bool {
	t.Helper()
	controller := memberRoleTestController()
	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := memberRoleTestAssignment(assignments, roleName)
	if a == nil {
		t.Fatalf("expected role %s, got %+v", roleName, assignments)
	}
	out := map[string]bool{}
	for _, s := range a.Subjects {
		// The team's managed Group is always present; the tests check User entries.
		if s.Kind == "Group" && s.Name == "team-sbsoftware" {
			continue
		}
		out[s.Kind+":"+s.Name] = true
	}
	return out
}

func TestJenkinsRoleAssignments_TenantMemberRole_UserSubjectBySubAddsPrincipal(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "alice", "alice@example.com"))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	want := map[string]bool{"User:sub-alice": true, "User:alice": true}
	if len(got) != len(want) {
		t.Fatalf("subjects = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing subject %s in %v", k, got)
		}
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_UserPrincipalFallsBackToEmail(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "", "alice@example.com"))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if !got["User:alice@example.com"] {
		t.Errorf("expected principal alice@example.com, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_UserAliasIgnoresUnconfiguredClaim(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "alice@example.com"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	// The claim list holds sub and preferred_username only, so an email value
	// never matches a User object.
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "alice", "alice@example.com"))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if got["User:alice"] {
		t.Errorf("unconfigured claim must not add an alias, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_UserAliasIgnoresOtherNamespaceAndLocalUsers(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	// The local user carries managed-by=local, so it must not be idp-managed.
	local := memberUserTestUser("local", memberUserTestNamespace, "bob", "")
	local.Labels[v1alpha1.LabelManagedBy] = v1alpha1.ManagedByLocal
	withUserDirectory(r,
		memberUserTestUser("forged", "tenant-ns", "mallory", ""),
		local,
	)

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if got["User:mallory"] || got["User:bob"] {
		t.Errorf("forged or local user must add no alias, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_NarrowedRoleAliasesUserSubject(t *testing.T) {
	controller := memberRoleTestController()
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "projgen-devs"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "sub-alice"}},
			RoleRef:  "developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "sbsoftware", Jobs: []string{"repos/projgen/*"}}},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, []*v1alpha1.Controller{controller})
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "alice", ""))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware:projgen-devs")
	if !got["User:alice"] || !got["User:sub-alice"] {
		t.Errorf("narrowed role must hold both subjects, got %v", got)
	}
}

func TestExpandMemberUserSubjects_GroupAndPrincipalNotAliased(t *testing.T) {
	aliases := map[string][]string{"sub-alice": {"alice"}}
	subjects := []v1alpha1.SubjectRef{
		{Kind: "Group", Name: "sub-alice"},
		{Kind: "User", Name: "agent:varroa-system/sbsoftware/bot"},
	}
	got := expandMemberUserSubjects(subjects, aliases)
	if len(got) != len(subjects) {
		t.Fatalf("group and agent subjects must not expand, got %v", got)
	}
}

func TestExpandMemberUserSubjects_DedupesAndKeepsOriginal(t *testing.T) {
	aliases := map[string][]string{"sub-alice": {"alice"}}
	subjects := []v1alpha1.SubjectRef{
		{Kind: "User", Name: "sub-alice"},
		{Kind: "User", Name: "alice"},
	}
	got := expandMemberUserSubjects(subjects, aliases)
	if len(got) != 2 || got[0].Name != "sub-alice" || got[1].Name != "alice" {
		t.Fatalf("expected original order with no duplicate, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_NoUserDirectoryKeepsSubjects(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if len(got) != 1 || !got["User:sub-alice"] {
		t.Errorf("nil directory must keep only the original subject, got %v", got)
	}
}

func TestGenerate_TenantMemberRoleRendersAliasedUser(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "alice", ""))

	yamlOut, err := NewGenerator(r).Generate(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yamlOut, "sub-alice") || !strings.Contains(yamlOut, "alice") {
		t.Errorf("expected both user subjects in YAML, got:\n%s", yamlOut)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_AgentPrincipalNotAliased(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "agent:varroa-system/sbsoftware/bot", ""))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if got["User:agent:varroa-system/sbsoftware/bot"] {
		t.Errorf("an agent-prefixed principal must not be aliased, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_FormerPreferredUsernameNotGranted(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	user := memberUserTestUser("alice", memberUserTestNamespace, "", "alice@example.com")
	user.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] = `["alice"]`
	withUserDirectory(r, user)

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if !got["User:alice@example.com"] {
		t.Errorf("expected principal alice@example.com, got %v", got)
	}
	if got["User:alice"] {
		t.Errorf("former preferred username granted: %v", got)
	}
}

// memberUserTestWithClaims stores custom user claims on a User object, the way
// a login records them.
func memberUserTestWithClaims(u *v1alpha1.User) *v1alpha1.User {
	u.Annotations[v1alpha1.AnnotationOIDCUserClaims] = `{"employee_id":"E123"}`
	return u
}

// memberUserTestCustomClaims sets the configured claim names, including the
// custom claim employee_id.
func memberUserTestCustomClaims(r *Resolver) {
	r.userClaimNames = []string{"sub", "preferred_username", "employee_id"}
}

func TestJenkinsRoleAssignments_TenantMemberRole_UserSubjectByCustomClaimAddsPrincipal(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "E123"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	memberUserTestCustomClaims(r)
	withUserDirectory(r, memberUserTestWithClaims(memberUserTestUser("alice", memberUserTestNamespace, "alice", "")))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if !got["User:E123"] || !got["User:alice"] {
		t.Errorf("expected User:E123 and User:alice, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_StoredClaimNotConfiguredNotAliased(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "E123"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	// employee_id is stored on the User but is not a configured claim.
	withUserDirectory(r, memberUserTestWithClaims(memberUserTestUser("alice", memberUserTestNamespace, "alice", "")))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware")
	if got["User:alice"] {
		t.Errorf("a stored claim that is not configured must not alias, got %v", got)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_NarrowedRoleAliasesCustomClaim(t *testing.T) {
	controller := memberRoleTestController()
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "projgen-devs"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "E123"}},
			RoleRef:  "developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "sbsoftware", Jobs: []string{"repos/projgen/*"}}},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, []*v1alpha1.Controller{controller})
	memberUserTestCustomClaims(r)
	withUserDirectory(r, memberUserTestWithClaims(memberUserTestUser("alice", memberUserTestNamespace, "alice", "")))

	got := memberUserTestSubjects(t, r, "varroa:tenant-member:sbsoftware:projgen-devs")
	if !got["User:alice"] {
		t.Errorf("expected narrowed role to alias User:alice, got %v", got)
	}
}

func TestTenantMemberSubjectExpander_CustomClaim(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer")
	binding := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "E123"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	memberUserTestCustomClaims(r)
	withUserDirectory(r, memberUserTestWithClaims(memberUserTestUser("alice", memberUserTestNamespace, "alice", "")))

	expand := r.TenantMemberSubjectExpander()
	out := expand([]v1alpha1.SubjectRef{{Kind: "User", Name: "E123"}})
	found := false
	for _, s := range out {
		if s.Kind == "User" && s.Name == "alice" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected expander to add User:alice, got %v", out)
	}
}
