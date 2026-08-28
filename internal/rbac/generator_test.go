package rbac

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestGenerate_WithAssignments(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Administer"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-developer"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read", "Job.Build", "Job.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "jenkins-admins"},
				},
				RoleRef: "jenkins-admin",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "developers"},
				},
				RoleRef: "jenkins-developer",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "ns1"}},
	}

	resolver := testResolver(roles, bindings, controllers, false)
	gen := NewGenerator(resolver)

	yamlOut, err := gen.Generate(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify YAML structure contains expected role names (prefixed with varroa:)
	if !strings.Contains(yamlOut, "varroa:jenkins-admin") {
		t.Errorf("expected generated YAML to contain 'varroa:jenkins-admin', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "varroa:jenkins-developer") {
		t.Errorf("expected generated YAML to contain 'varroa:jenkins-developer', got:\n%s", yamlOut)
	}

	// Verify permissions are present
	if !strings.Contains(yamlOut, "Overall.Administer") {
		t.Errorf("expected YAML to contain 'Overall.Administer', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "Job.Build") {
		t.Errorf("expected YAML to contain 'Job.Build', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "Job.Read") {
		t.Errorf("expected YAML to contain 'Job.Read', got:\n%s", yamlOut)
	}

	// Verify subject entries (new bucketed format uses entries with user/group keys)
	if !strings.Contains(yamlOut, "jenkins-admins") {
		t.Errorf("expected YAML to contain 'jenkins-admins', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "developers") {
		t.Errorf("expected YAML to contain 'developers', got:\n%s", yamlOut)
	}

	// Verify YAML structure markers
	if !strings.Contains(yamlOut, "authorizationStrategy") {
		t.Errorf("expected YAML to contain 'authorizationStrategy', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "roleBased") {
		t.Errorf("expected YAML to contain 'roleBased', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "permissions") {
		t.Errorf("expected YAML to contain 'permissions', got:\n%s", yamlOut)
	}
	// Bucketed format uses "entries" (map of user/group) instead of "assignments" (list of strings)
	if !strings.Contains(yamlOut, "entries") {
		t.Errorf("expected YAML to contain 'entries', got:\n%s", yamlOut)
	}
	// Verify bucketed structure
	if !strings.Contains(yamlOut, "global:") {
		t.Errorf("expected YAML to contain 'global:' bucket, got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "items:") {
		t.Errorf("expected YAML to contain 'items:' bucket, got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "agents:") {
		t.Errorf("expected YAML to contain 'agents:' bucket, got:\n%s", yamlOut)
	}
}

func TestToRoleStrategyPermission(t *testing.T) {
	cases := map[string]string{
		"hudson.model.Hudson.Administer":                             "Overall/Administer",
		"hudson.model.Hudson.Read":                                   "Overall/Read",
		"hudson.model.Item.Read":                                     "Job/Read",
		"hudson.model.Item.Build":                                    "Job/Build",
		"hudson.model.Run.Delete":                                    "Run/Delete",
		"hudson.model.View.Configure":                                "View/Configure",
		"hudson.model.Computer.Configure":                            "Agent/Configure",
		"com.cloudbees.plugins.credentials.CredentialsProvider.View": "Credentials/View",
		"Overall/Administer":                                         "Overall/Administer", // already UI format
		"some.unknown.Plugin.Permission":                             "some.unknown.Plugin.Permission",
	}
	for in, want := range cases {
		if got := toRoleStrategyPermission(in); got != want {
			t.Errorf("toRoleStrategyPermission(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerate_NoMatch(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "some-role"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "team-a"}},
				RoleRef:  "some-role",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "team-b"}},
	}

	resolver := testResolver(roles, bindings, controllers, false)
	gen := NewGenerator(resolver)

	yamlOut, err := gen.Generate(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should still produce valid YAML with empty buckets
	if !strings.Contains(yamlOut, "authorizationStrategy") {
		t.Errorf("expected YAML to contain 'authorizationStrategy', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "roleBased") {
		t.Errorf("expected YAML to contain 'roleBased', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "roles") {
		t.Errorf("expected YAML to contain 'roles', got:\n%s", yamlOut)
	}

	// Should NOT contain the varroa:-prefixed role name or permissions
	if strings.Contains(yamlOut, "varroa:some-role") {
		t.Errorf("expected YAML to NOT contain unbound role 'varroa:some-role', got:\n%s", yamlOut)
	}
	if strings.Contains(yamlOut, "Overall.Read") {
		t.Errorf("expected YAML to NOT contain permissions from unbound role, got:\n%s", yamlOut)
	}
}

func TestGenerate_ValidYAML(t *testing.T) {
	// Verify the output is parseable YAML with the correct bucketed structure.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "viewers"}},
				RoleRef:  "viewer",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-1", Namespace: "ns1"}},
	}

	resolver := testResolver(roles, bindings, controllers, false)
	gen := NewGenerator(resolver)

	yamlOut, err := gen.Generate(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it's valid YAML by parsing it back
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlOut), &parsed); err != nil {
		t.Fatalf("generated output is not valid YAML: %v\nOutput:\n%s", err, yamlOut)
	}

	// Check the top-level structure: authorizationStrategy nests under jenkins
	// so the document is a valid JCasC configuration.
	jenkinsRoot, ok := parsed["jenkins"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected top-level jenkins to be a map, got %T\nOutput:\n%s", parsed["jenkins"], yamlOut)
	}
	authStrategy, ok := jenkinsRoot["authorizationStrategy"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected authorizationStrategy to be a map, got %T\nOutput:\n%s", jenkinsRoot["authorizationStrategy"], yamlOut)
	}
	roleBased, ok := authStrategy["roleBased"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected roleBased to be a map, got %T\nOutput:\n%s", authStrategy["roleBased"], yamlOut)
	}
	rolesMap, ok := roleBased["roles"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected roles to be a map (bucketed), got %T\nOutput:\n%s", roleBased["roles"], yamlOut)
	}

	// Verify the three buckets exist
	for _, bucket := range []string{"global", "items", "agents"} {
		if _, ok := rolesMap[bucket]; !ok {
			t.Fatalf("expected roles map to contain '%s' bucket, got keys: %v\nOutput:\n%s", bucket, getKeys(rolesMap), yamlOut)
		}
	}

	// The global bucket should contain the viewer role (plus the always-present
	// synthesized varroa:system-mite role — find the viewer one specifically).
	globalRoles, ok := rolesMap["global"].([]interface{})
	if !ok {
		t.Fatalf("expected global roles to be a slice, got %T", rolesMap["global"])
	}
	var role map[string]interface{}
	for _, gr := range globalRoles {
		m, ok := gr.(map[string]interface{})
		if ok && m["name"] == "varroa:viewer" {
			role = m
			break
		}
	}
	if role == nil {
		t.Fatalf("expected a 'varroa:viewer' global role, got:\n%s", yamlOut)
	}
	if role["permissions"] == nil {
		t.Error("expected role to have permissions")
	}
	// Bucketed format uses "entries" not "assignments"
	if role["entries"] == nil {
		t.Error("expected role to have entries")
	}

	// Verify entries contain a group entry for "viewers"
	entries, ok := role["entries"].([]interface{})
	if !ok || len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", role["entries"])
	}
	entry, ok := entries[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected entry to be a map, got %T", entries[0])
	}
	if entry["group"] != "viewers" {
		t.Errorf("expected entry group 'viewers', got %v", entry["group"])
	}
}

func TestGenerate_SkipsAPIRules(t *testing.T) {
	// A role that only has apiRules (no JenkinsPermissions) should generate
	// YAML with empty buckets — the role should be skipped.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-only"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
				// No JenkinsPermissions
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-only-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}},
				RoleRef:  "api-only",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "ns1"}},
	}

	resolver := testResolver(roles, bindings, controllers, false)
	gen := NewGenerator(resolver)

	yamlOut, err := gen.Generate(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(yamlOut, "varroa:api-only") {
		t.Errorf("expected YAML to NOT contain API-only role 'varroa:api-only', got:\n%s", yamlOut)
	}
}

func TestGenerate_UserAndGroupEntries(t *testing.T) {
	// Verify both User and Group subjects appear as correct entry types.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mixed-role"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mixed-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "developers"},
					{Kind: "User", Name: "alice"},
				},
				RoleRef: "mixed-role",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-1", Namespace: "ns1"}},
	}

	resolver := testResolver(roles, bindings, controllers, false)
	gen := NewGenerator(resolver)

	yamlOut, err := gen.Generate(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both "group:" and "user:" keys should appear in entries
	if !strings.Contains(yamlOut, "group:") {
		t.Errorf("expected YAML to contain 'group:' entry, got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "user:") {
		t.Errorf("expected YAML to contain 'user:' entry, got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "developers") {
		t.Errorf("expected YAML to contain group name 'developers', got:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "alice") {
		t.Errorf("expected YAML to contain user name 'alice', got:\n%s", yamlOut)
	}
}

func getKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestHasHumanAdmin_OnlyMiteAdmin(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:system-mite",
			RoleType:    "Global",
			Permissions: []string{"hudson.model.Hudson.Administer"},
			Subjects:    []SubjectAssignment{{Kind: "User", Name: "ROLE:varroa:system-mite"}},
		},
	}
	if HasHumanAdmin(assignments) {
		t.Error("expected false for only-mite admin")
	}
}

func TestHasHumanAdmin_OnlyOperatorAdmin(t *testing.T) {
	// The synthesized varroa:system-operator assignment carries Administer but is
	// a machine identity (executeGroovy), not a human admin — it must NOT satisfy
	// the lockout guard, exactly like the mite above.
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:system-operator",
			RoleType:    "Global",
			Permissions: []string{"hudson.model.Hudson.Administer"},
			Subjects:    []SubjectAssignment{{Kind: "Group", Name: "ROLE:varroa:system-operator"}},
		},
	}
	if HasHumanAdmin(assignments) {
		t.Error("expected false for only-operator admin")
	}
}

func TestHasHumanAdmin_HumanUserAdmin(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:admin",
			RoleType:    "Global",
			Permissions: []string{"hudson.model.Hudson.Administer"},
			Subjects:    []SubjectAssignment{{Kind: "User", Name: "alice"}},
		},
	}
	if !HasHumanAdmin(assignments) {
		t.Error("expected true for human User admin")
	}
}

func TestHasHumanAdmin_NonMiteGroupAdmin(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:admin",
			RoleType:    "Global",
			Permissions: []string{"hudson.model.Hudson.Administer"},
			Subjects:    []SubjectAssignment{{Kind: "Group", Name: "varroa-admins"}},
		},
	}
	if !HasHumanAdmin(assignments) {
		t.Error("expected true for non-mite Group admin")
	}
}

func TestHasHumanAdmin_GlobalWithoutAdminister(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:developer",
			RoleType:    "Global",
			Permissions: []string{"hudson.model.Item.Read"},
			Subjects:    []SubjectAssignment{{Kind: "User", Name: "alice"}},
		},
	}
	if HasHumanAdmin(assignments) {
		t.Error("expected false for Global without Administer")
	}
}

func TestHasHumanAdmin_NonGlobalWithAdminister(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:item-admin",
			RoleType:    "Item",
			Pattern:     ".*",
			Permissions: []string{"hudson.model.Hudson.Administer"},
			Subjects:    []SubjectAssignment{{Kind: "User", Name: "alice"}},
		},
	}
	if HasHumanAdmin(assignments) {
		t.Error("expected false for non-Global (Item) with Administer")
	}
}

func TestHasHumanAdmin_EmptySlice(t *testing.T) {
	if HasHumanAdmin(nil) {
		t.Error("expected false for empty/nil slice")
	}
}

func TestHasHumanAdmin_LegacyOverallDotFormat(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:admin",
			RoleType:    "Global",
			Permissions: []string{"Overall.Administer"},
			Subjects:    []SubjectAssignment{{Kind: "Group", Name: "varroa-admins"}},
		},
	}
	if !HasHumanAdmin(assignments) {
		t.Error("expected true for Overall.Administer format (legacy VarroaRole)")
	}
}

func TestHasHumanAdmin_RoleStrategySlashFormat(t *testing.T) {
	assignments := []RoleAssignment{
		{
			RoleName:    "varroa:admin",
			RoleType:    "Global",
			Permissions: []string{"Overall/Administer"},
			Subjects:    []SubjectAssignment{{Kind: "User", Name: "alice"}},
		},
	}
	if !HasHumanAdmin(assignments) {
		t.Error("expected true for Overall/Administer format (role-strategy)")
	}
}
