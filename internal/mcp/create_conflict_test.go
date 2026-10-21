package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// assertCreateRefused seeds one object, runs a create tool over the same name,
// and checks that the tool answers a conflict error and keeps the stored object.
func assertCreateRefused[T any](t *testing.T, store *crdstore.Fake, seed *T, ns, name, tool string, args map[string]any, want string) {
	t.Helper()
	ctx := context.Background()
	crdstore.MustSeed(store, seed)
	before, err := crdstore.Get[T](ctx, store, name, ns)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	deps := deleteUserDeps(store)
	deps.IdentityConfig.Mode = "local"
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      tool,
		"arguments": args,
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("%s over an existing name returned success: %v", tool, tr.Content)
	}
	if !strings.Contains(toolText(tr), want) {
		t.Errorf("%s error = %q, want it to contain %q", tool, toolText(tr), want)
	}

	after, err := crdstore.Get[T](ctx, store, name, ns)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("%s changed the stored object: before %+v, after %+v", tool, before, after)
	}
}

func TestCreateGroup_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.Group{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.GroupSpec{Members: []string{"alice"}}},
		"", "team-a", "create_group",
		map[string]interface{}{"name": "team-a", "members": []interface{}{"bob"}},
		"group team-a already exists")
}

// TestCreateGroup_RefusedWhenLocalAccountsOff pins that create_group refuses
// the call and writes no Group when local accounts are off.
func TestCreateGroup_RefusedWhenLocalAccountsOff(t *testing.T) {
	store := crdstore.NewFake()
	deps := deleteUserDeps(store)
	deps.IdentityConfig = api.IdentityConfig{Mode: "oidc"}
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "create_group",
		"arguments": map[string]interface{}{"name": "g-new", "members": []interface{}{"bob"}},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "group management is only available with local accounts" {
		t.Errorf("create_group with local accounts off = %q, want the local-accounts tool error", toolText(tr))
	}
	_, err := crdstore.Get[v1alpha1.Group](context.Background(), store, "g-new", "")
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get g-new after a refused create_group: err = %v, want not found", err)
	}
}

// TestUpdateGroup_EditsMembers pins that update_group replaces the member list
// of an existing group and answers a tool error for a missing one.
func TestUpdateGroup_EditsMembers(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Group{ObjectMeta: metav1.ObjectMeta{Name: "g1"}, Spec: v1alpha1.GroupSpec{Members: []string{"alice"}}})
	localDeps := deleteUserDeps(store)
	localDeps.IdentityConfig = api.IdentityConfig{Mode: "local"}
	handler := NewHandler(localDeps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_group",
		"arguments": map[string]interface{}{"name": "g1", "displayName": "Devs", "members": []interface{}{"alice", "bob"}},
	}, mcpAdminClaims)
	if tr := parseToolResult(t, resp.Result); tr.IsError {
		t.Fatalf("update_group returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.Group](context.Background(), store, "g1", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Members) != 2 || got.Spec.DisplayName != "Devs" {
		t.Errorf("group = %+v, want members [alice bob] and displayName Devs", got.Spec)
	}

	resp = mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "update_group",
		"arguments": map[string]interface{}{"name": "nope", "members": []interface{}{}},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "group nope not found" {
		t.Errorf("update_group on a missing group = %q, want the not-found tool error", toolText(tr))
	}

	oidcDeps := deleteUserDeps(store)
	oidcDeps.IdentityConfig = api.IdentityConfig{Mode: "oidc"}
	resp = mcpRequest(t, NewHandler(oidcDeps), "tools/call", map[string]interface{}{
		"name":      "update_group",
		"arguments": map[string]interface{}{"name": "g1", "members": []interface{}{}},
	}, mcpAdminClaims)
	tr = parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "group management is only available with local accounts" {
		t.Errorf("update_group with local accounts off = %q, want the local-accounts tool error", toolText(tr))
	}
}

func TestCreateVarroaRole_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: "role-a"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "keep"}},
		"", "role-a", "create_varroa_role",
		map[string]interface{}{"name": "role-a", "jenkinsRoleRef": "other"},
		"role role-a already exists; use update_varroa_role to change it")
}

func TestCreateVarroaRoleBinding_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding-a"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "keep"}},
		"", "binding-a", "create_varroa_role_binding",
		map[string]interface{}{"name": "binding-a", "roleRef": "other", "subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "bob"}}},
		"role binding binding-a already exists; use update_varroa_role_binding to change it")
}

func TestCreateJenkinsRole_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jr-a"}, Spec: v1alpha1.JenkinsRoleSpec{RoleType: "keep"}},
		"", "jr-a", "create_jenkins_role",
		map[string]interface{}{"name": "jr-a", "roleType": "other", "permissions": []interface{}{"Overall/Read"}},
		"Jenkins role jr-a already exists; use update_jenkins_role to change it")
}

func TestCreateJenkinsRoleBinding_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "jrb-a"}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "keep"}},
		"", "jrb-a", "create_jenkins_role_binding",
		map[string]interface{}{"name": "jrb-a", "roleRef": "other", "subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "bob"}}},
		"Jenkins role binding jrb-a already exists; use update_jenkins_role_binding to change it")
}

func TestCreateCatalogSource_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns"}},
		"ns", "src", "create_catalog_source",
		map[string]interface{}{"namespace": "ns", "name": "src", "repoURL": "https://example.com/other.git"},
		"catalog source ns/src already exists; use update_catalog_source to change it")
}

func TestCreateComposedBundle_ExistingNameIsToolError(t *testing.T) {
	assertCreateRefused(t, crdstore.NewFake(),
		&v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "cb-a", Namespace: "ns"}},
		"ns", "cb-a", "create_composed_bundle",
		map[string]interface{}{"namespace": "ns", "name": "cb-a", "inputs": []interface{}{map[string]interface{}{"itemRef": map[string]interface{}{"name": "cat-item"}}}},
		"composed bundle ns/cb-a already exists; use update_composed_bundle to change it")
}

// createUserArgs returns valid create_user arguments for the operator namespace.
func createUserArgs(name string) map[string]interface{} {
	return map[string]interface{}{
		"namespace": "varroa-system",
		"name":      name,
		"password":  "pw-12345678",
	}
}

// localUserDeps returns admin deps with local accounts on and the operator
// namespace set to varroa-system.
func localUserDeps(store crdstore.Backend) *api.Dependencies {
	deps := deleteUserDeps(store)
	deps.IdentityConfig.Mode = "local"
	deps.OperatorNamespace = "varroa-system"
	return deps
}

func TestCreateUser_SetsPasswordAndLocalLabel(t *testing.T) {
	store := crdstore.NewFake()
	resp := mcpRequest(t, NewHandler(localUserDeps(store)), "tools/call", map[string]interface{}{
		"name":      "create_user",
		"arguments": createUserArgs("bob"),
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("create_user returned error: %v", tr.Content)
	}
	if strings.Contains(toolText(tr), "pw-12345678") {
		t.Errorf("create_user result leaks the password: %s", toolText(tr))
	}
	got, err := crdstore.Get[v1alpha1.User](context.Background(), store, "bob", "varroa-system")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Password != "pw-12345678" {
		t.Errorf("stored password = %q, want the supplied password", got.Spec.Password)
	}
	if got.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByLocal {
		t.Errorf("managed-by label = %q, want %q", got.Labels[v1alpha1.LabelManagedBy], v1alpha1.ManagedByLocal)
	}
}

func TestCreateUser_RequiresPassword(t *testing.T) {
	store := crdstore.NewFake()
	args := createUserArgs("bob")
	args["password"] = ""
	resp := mcpRequest(t, NewHandler(localUserDeps(store)), "tools/call", map[string]interface{}{
		"name":      "create_user",
		"arguments": args,
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "password is required" {
		t.Errorf("create_user without a password = %q, want the password-required tool error", toolText(tr))
	}
	if _, err := crdstore.Get[v1alpha1.User](context.Background(), store, "bob", "varroa-system"); err == nil {
		t.Errorf("a User was written without a password")
	}
}

func TestCreateUser_RefusedWhenLocalAccountsOff(t *testing.T) {
	store := crdstore.NewFake()
	deps := localUserDeps(store)
	deps.IdentityConfig.Mode = "oidc"
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "create_user",
		"arguments": createUserArgs("bob"),
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "user creation is only available with local accounts" {
		t.Errorf("create_user with local accounts off = %q, want the local-accounts tool error", toolText(tr))
	}
}

// TestUpdateUser_RefusedWhenLocalAccountsOff pins that update_user refuses
// the call and keeps the stored email when local accounts are off.
func TestUpdateUser_RefusedWhenLocalAccountsOff(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "varroa-system"},
		Spec:       v1alpha1.UserSpec{Email: "old@example.com"},
	})
	deps := localUserDeps(store)
	deps.IdentityConfig.Mode = "oidc"
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "update_user",
		"arguments": map[string]interface{}{"namespace": "varroa-system", "name": "bob", "email": "new@example.com"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "user editing is only available with local accounts" {
		t.Errorf("update_user with local accounts off = %q, want the local-accounts tool error", toolText(tr))
	}
	got, err := crdstore.Get[v1alpha1.User](context.Background(), store, "bob", "varroa-system")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Email != "old@example.com" {
		t.Errorf("email = %q, want the stored old@example.com", got.Spec.Email)
	}
}

func TestCreateUser_InvalidUsernameIsToolError(t *testing.T) {
	store := crdstore.NewFake()
	resp := mcpRequest(t, NewHandler(localUserDeps(store)), "tools/call", map[string]interface{}{
		"name":      "create_user",
		"arguments": createUserArgs("Bad_Name"),
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("create_user with an invalid username returned success: %v", tr.Content)
	}
	if _, err := crdstore.Get[v1alpha1.User](context.Background(), store, "Bad_Name", "varroa-system"); err == nil {
		t.Errorf("a User with an invalid name was written")
	}
}

func TestCreateUser_OtherNamespaceIsToolError(t *testing.T) {
	store := crdstore.NewFake()
	args := createUserArgs("bob")
	args["namespace"] = "team-a"
	resp := mcpRequest(t, NewHandler(localUserDeps(store)), "tools/call", map[string]interface{}{
		"name":      "create_user",
		"arguments": args,
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || toolText(tr) != "local users live in the operator namespace varroa-system" {
		t.Errorf("create_user in team-a = %q, want the operator-namespace tool error", toolText(tr))
	}
}

// TestUserTools_SchemaHasNoGroups pins that neither user tool declares a
// groups argument, and that create_user requires password.
func TestUserTools_SchemaHasNoGroups(t *testing.T) {
	resp := mcpRequest(t, NewHandler(localUserDeps(crdstore.NewFake())), "tools/list", map[string]interface{}{}, mcpAdminClaims)
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	found := map[string]bool{}
	for _, tool := range list.Tools {
		if tool.Name != "create_user" && tool.Name != "update_user" {
			continue
		}
		found[tool.Name] = true
		if _, ok := tool.InputSchema.Properties["groups"]; ok {
			t.Errorf("%s still declares a groups argument", tool.Name)
		}
		if tool.Name == "create_user" && !containsString(tool.InputSchema.Required, "password") {
			t.Errorf("create_user required = %v, want password", tool.InputSchema.Required)
		}
	}
	if !found["create_user"] || !found["update_user"] {
		t.Fatalf("tools/list missing create_user or update_user: %v", found)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
