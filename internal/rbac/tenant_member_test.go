package rbac

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// memberRoleTestTeam returns a Team whose managed Group is ready and whose
// spec subjects hold the given roleRef.
func memberRoleTestTeam(roleRef string, subjects ...v1alpha1.SubjectRef) *v1alpha1.Team {
	const name = "sbsoftware"
	return &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TeamSpec{RoleRef: roleRef, Subjects: subjects},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-" + name,
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
}

// memberRoleTestAssignment finds one assignment by role name.
func memberRoleTestAssignment(assignments []RoleAssignment, name string) *RoleAssignment {
	for i := range assignments {
		if assignments[i].RoleName == name {
			return &assignments[i]
		}
	}
	return nil
}

func memberRoleTestController() *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "varroa-system"},
		Status:     v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{Tenants: []string{"sbsoftware"}}},
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_TeamSubjects(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer", v1alpha1.SubjectRef{Kind: "Group", Name: "sbsoftware:Owners"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})

	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := memberRoleTestAssignment(assignments, "varroa:tenant-member:sbsoftware")
	if a == nil {
		t.Fatalf("expected varroa:tenant-member:sbsoftware, got %+v", assignments)
	}
	if a.RoleType != "Item" {
		t.Errorf("expected RoleType Item, got %s", a.RoleType)
	}
	if want := "^sbsoftware($|/.*)$"; a.Pattern != want {
		t.Errorf("expected pattern %q, got %q", want, a.Pattern)
	}
	if !equalStringSets(a.Permissions, tenantMemberPermissions) {
		t.Errorf("expected permissions %v, got %v", tenantMemberPermissions, a.Permissions)
	}
	hasOwners := false
	for _, s := range a.Subjects {
		if s.Kind == "Group" && s.Name == "sbsoftware:Owners" {
			hasOwners = true
		}
	}
	if !hasOwners {
		t.Errorf("expected subject Group:sbsoftware:Owners, got %+v", a.Subjects)
	}

	re := regexp.MustCompile(a.Pattern)
	for path, want := range map[string]bool{
		"sbsoftware":                    true,
		"sbsoftware/repos":              true,
		"sbsoftware/repos/projgen/main": true,
		"sbsoftware2/x":                 false,
	} {
		if got := re.MatchString(path); got != want {
			t.Errorf("pattern %q on %q = %v, want %v", a.Pattern, path, got, want)
		}
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_ReadOnlyGrantExcluded(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("viewer", v1alpha1.SubjectRef{Kind: "Group", Name: "sbsoftware:Readers"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, nil, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})

	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range assignments {
		if strings.HasPrefix(a.RoleName, "varroa:tenant-member:") {
			t.Errorf("read-only grant must not yield a member role, got %+v", a)
		}
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_TenantScopedNarrowed(t *testing.T) {
	controller := memberRoleTestController()
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "projgen-devs"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "sbsoftware:Devs"}},
			RoleRef:  "developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{{Name: "sbsoftware", Jobs: []string{"repos/projgen/*"}}},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, []*v1alpha1.Controller{controller})

	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := memberRoleTestAssignment(assignments, "varroa:tenant-member:sbsoftware:projgen-devs")
	if a == nil {
		t.Fatalf("expected narrowed member role, got %+v", assignments)
	}
	re := regexp.MustCompile(a.Pattern)
	if !re.MatchString("sbsoftware/repos/projgen/main") {
		t.Errorf("pattern %q must match a job under the glob", a.Pattern)
	}
	if re.MatchString("sbsoftware/repos") {
		t.Errorf("pattern %q must not match the folder outside the glob", a.Pattern)
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_RepeatedTenantEntryMerges(t *testing.T) {
	controller := memberRoleTestController()
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "two-scopes"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "sbsoftware:Devs"}},
			RoleRef:  "developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{
					{Name: "sbsoftware", Jobs: []string{"release/*"}},
					{Name: "sbsoftware", Jobs: []string{"deploy/*"}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, []*v1alpha1.VarroaRoleBinding{binding}, nil, []*v1alpha1.Controller{controller})

	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var names []string
	var a *RoleAssignment
	for i := range assignments {
		if strings.HasPrefix(assignments[i].RoleName, "varroa:tenant-member:") {
			names = append(names, assignments[i].RoleName)
			a = &assignments[i]
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected one narrowed member role for the binding, got %v", names)
	}
	re := regexp.MustCompile(a.Pattern)
	for path, want := range map[string]bool{
		"sbsoftware/release/eu": true,
		"sbsoftware/deploy/eu":  true,
		"sbsoftware/other/eu":   false,
	} {
		if got := re.MatchString(path); got != want {
			t.Errorf("pattern %q on %q = %v, want %v", a.Pattern, path, got, want)
		}
	}
}

func TestJenkinsRoleAssignments_TenantMemberRole_NotCapped(t *testing.T) {
	controller := memberRoleTestController()
	var bindings []*v1alpha1.VarroaRoleBinding
	for i := 0; i < 60; i++ {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "wide-" + strings.Repeat("x", i+1)},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "g-" + strings.Repeat("y", i+1)}},
				RoleRef:  "developer",
			},
		})
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, nil, []*v1alpha1.Controller{controller})

	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := memberRoleTestAssignment(assignments, "varroa:tenant-member:sbsoftware")
	if a == nil {
		t.Fatalf("expected member role, got %+v", assignments)
	}
	if len(a.Subjects) != 60 {
		t.Errorf("expected 60 subjects, got %d", len(a.Subjects))
	}
}

func TestGenerate_TenantMemberRoleRendered(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer", v1alpha1.SubjectRef{Kind: "Group", Name: "sbsoftware:Owners"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})

	yamlOut, err := NewGenerator(r).Generate(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yamlOut, "varroa:tenant-member:sbsoftware") {
		t.Fatalf("expected member role in YAML, got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "sbsoftware:Owners") {
		t.Errorf("expected subject sbsoftware:Owners in YAML, got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "Job/Build") {
		t.Errorf("expected Job/Build in YAML, got:\n%s", yamlOut)
	}
}

// memberRoleSortedKeys returns the Kind:Name keys of subjects, sorted.
func memberRoleSortedKeys(subjects []v1alpha1.SubjectRef) []string {
	keys := make([]string, 0, len(subjects))
	for _, s := range subjects {
		keys = append(keys, s.Kind+":"+s.Name)
	}
	sort.Strings(keys)
	return keys
}

// memberRoleAssignmentKeys returns the Kind:Name keys of a generated role.
func memberRoleAssignmentKeys(subjects []SubjectAssignment) []string {
	keys := make([]string, 0, len(subjects))
	for _, s := range subjects {
		keys = append(keys, s.Kind+":"+s.Name)
	}
	sort.Strings(keys)
	return keys
}

// memberRoleMixedFixture grants developer to a Team group, cluster-wide to one
// User subject, and narrowed to two job globs of the same tenant.
func memberRoleMixedFixture(t *testing.T) (*Resolver, *v1alpha1.Controller) {
	t.Helper()
	controller := memberRoleTestController()
	team := memberRoleTestTeam("developer", v1alpha1.SubjectRef{Kind: "Group", Name: "sbsoftware:Owners"})
	narrowed := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "projgen-devs"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "sbsoftware:Devs"}},
			RoleRef:  "developer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Tenants: []v1alpha1.TenantScope{
					{Name: "sbsoftware", Jobs: []string{"repos/projgen/*"}},
					{Name: "sbsoftware", Jobs: []string{"repos/other/*"}},
				},
			},
		},
	}
	writer := memberUserTestWriterBinding(v1alpha1.SubjectRef{Kind: "User", Name: "sub-alice"})
	r := testTenantResolver(
		[]*v1alpha1.VarroaRole{developerRole()},
		[]*v1alpha1.VarroaRoleBinding{writer, narrowed},
		[]*v1alpha1.Team{team},
		[]*v1alpha1.Controller{controller},
	)
	withUserDirectory(r, memberUserTestUser("alice", memberUserTestNamespace, "alice", ""))
	return r, controller
}

func TestTenantMemberRoles_MatchesJenkinsRoleAssignments(t *testing.T) {
	r, controller := memberRoleMixedFixture(t)
	assignments, err := r.JenkinsRoleAssignments(controller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roles := r.TenantMemberRoles(ParsedTenant{Kind: TenantTeam, Team: "sbsoftware", Folder: "sbsoftware"})
	if len(roles) != 2 {
		t.Fatalf("expected 2 member roles, got %d: %+v", len(roles), roles)
	}
	for _, role := range roles {
		a := memberRoleTestAssignment(assignments, role.Name)
		if a == nil {
			t.Fatalf("role %q missing from JenkinsRoleAssignments", role.Name)
		}
		if a.Pattern != role.Pattern {
			t.Errorf("role %q pattern = %q, want %q", role.Name, a.Pattern, role.Pattern)
		}
		if !equalStringSets(a.Permissions, role.Permissions) {
			t.Errorf("role %q permissions = %v, want %v", role.Name, a.Permissions, role.Permissions)
		}
		got := memberRoleAssignmentKeys(a.Subjects)
		want := memberRoleSortedKeys(role.Subjects)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("role %q subjects = %v, want %v", role.Name, got, want)
		}
	}
}

func TestTenantMemberRoles_SubtreeRoleFirstThenNarrowed(t *testing.T) {
	r, _ := memberRoleMixedFixture(t)
	roles := r.TenantMemberRoles(ParsedTenant{Kind: TenantTeam, Team: "sbsoftware", Folder: "sbsoftware"})
	if len(roles) == 0 {
		t.Fatal("expected member roles")
	}
	if roles[0].Name != TenantMemberRoleName("sbsoftware") || roles[0].Binding != "" {
		t.Errorf("first role = %+v, want the subtree role with no binding", roles[0])
	}
	for _, role := range roles[1:] {
		if role.Binding == "" {
			t.Errorf("narrowed role %q must name its binding", role.Name)
		}
	}
	if roles[1].Name != TenantMemberNarrowedRoleName("sbsoftware", "projgen-devs") {
		t.Errorf("second role = %q", roles[1].Name)
	}
	if want := TenantMemberNarrowedPattern("sbsoftware", []string{"repos/projgen/*", "repos/other/*"}); roles[1].Pattern != want {
		t.Errorf("narrowed pattern = %q, want %q", roles[1].Pattern, want)
	}
}

func TestTenantMemberRoles_ReadOnlyGrantYieldsNone(t *testing.T) {
	controller := memberRoleTestController()
	team := memberRoleTestTeam("viewer", v1alpha1.SubjectRef{Kind: "Group", Name: "sbsoftware:Readers"})
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, nil, []*v1alpha1.Team{team}, []*v1alpha1.Controller{controller})
	if roles := r.TenantMemberRoles(ParsedTenant{Kind: TenantTeam, Team: "sbsoftware", Folder: "sbsoftware"}); len(roles) != 0 {
		t.Errorf("read-only grant must yield no member role, got %+v", roles)
	}
}

func TestTenantMemberRoles_NonTeamTenantYieldsNil(t *testing.T) {
	r, _ := memberRoleMixedFixture(t)
	if roles := r.TenantMemberRoles(ParsedTenant{Kind: TenantController, Namespace: "varroa-system", Controller: "cell-a"}); roles != nil {
		t.Errorf("controller tenant must yield nil, got %+v", roles)
	}
	var nilResolver *Resolver
	if roles := nilResolver.TenantMemberRoles(ParsedTenant{Kind: TenantTeam, Team: "sbsoftware"}); roles != nil {
		t.Errorf("nil resolver must yield nil, got %+v", roles)
	}
}
