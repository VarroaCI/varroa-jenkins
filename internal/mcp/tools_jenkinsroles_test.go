package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// jenkinsRoleDeps seeds two roles: one carrying the full field set Task 7
// exists to cut (an 11-entry permissions array, roleType, description) and one
// with only the required permissions field — the minimal shape a role must
// have.
func jenkinsRoleDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.JenkinsRole{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-deploy"},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Item",
				Description: "Deploy permissions for CI",
				Permissions: []string{
					"hudson.model.Item.Build",
					"hudson.model.Item.Configure",
					"hudson.model.Item.Create",
					"hudson.model.Item.Delete",
					"hudson.model.Item.Read",
					"hudson.model.Item.Workspace",
					"hudson.model.Run.Delete",
					"hudson.model.Run.Update",
					"hudson.scm.SCMTag",
					"hudson.model.View.Create",
					"hudson.model.View.Read",
				},
			},
		},
		&v1alpha1.JenkinsRole{
			ObjectMeta: metav1.ObjectMeta{Name: "reader"},
			Spec: v1alpha1.JenkinsRoleSpec{
				Permissions: []string{"hudson.model.Item.Read"},
			},
		},
	)
	return &api.Dependencies{Client: &stubClient{}, Store: store}
}

// listJenkinsRolesResult calls list_jenkins_roles and returns the decoded
// structuredContent.
func listJenkinsRolesResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(jenkinsRoleDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_jenkins_roles",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_jenkins_roles returned error: %v", tr.Content)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var payload struct {
		StructuredContent map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if payload.StructuredContent == nil {
		t.Fatalf("no structuredContent in result: %s", b)
	}
	return payload.StructuredContent
}

// The projection exists to drop spec.permissions[] — up to 11 permission IDs
// per role, the entire reason a raw JenkinsRole costs 633 B/item. The count
// must survive; the payload text must not.
func TestSummarizeJenkinsRole_DropsPermissionsKeepsCount(t *testing.T) {
	perms := []string{
		"hudson.model.Item.Build",
		"hudson.model.Item.Configure",
		"hudson.model.Item.Create",
		"hudson.model.Item.Delete",
		"hudson.model.Item.Read",
		"hudson.model.Item.Workspace",
		"hudson.model.Run.Delete",
		"hudson.model.Run.Update",
		"hudson.scm.SCMTag",
		"hudson.model.View.Create",
		"hudson.model.View.Read",
	}
	got := summarizeJenkinsRole(&v1alpha1.JenkinsRole{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-deploy"},
		Spec: v1alpha1.JenkinsRoleSpec{
			RoleType:    "Item",
			Description: "Deploy permissions for CI",
			Permissions: perms,
		},
	})
	if got.PermissionCount != len(perms) {
		t.Errorf("PermissionCount = %d, want %d", got.PermissionCount, len(perms))
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "hudson.model") {
		t.Errorf("summary still carries permission text: %s", b)
	}
	for _, want := range []string{"ci-deploy", "Item", "Deploy permissions for CI"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("summary dropped %s: %s", want, b)
		}
	}
}

// summarizeJenkinsRole must tolerate a nil receiver like every other
// summarize* in the package, so a caller can never panic a listing.
func TestSummarizeJenkinsRole_NilRoleDoesNotPanic(t *testing.T) {
	got := summarizeJenkinsRole(nil)
	if got.Name != "" || got.RoleType != "" || got.Description != "" || got.PermissionCount != 0 {
		t.Errorf("nil role must project a zero summary, got %+v", got)
	}
}

// The default list result must be the summary projection: identifying fields
// plus the permission count, never the permissions array or raw CR internals —
// and no namespace, because JenkinsRole is cluster-scoped. The fake store
// returns items in map order, so locate each seed by name rather than by index.
func TestListJenkinsRoles_DefaultsToSummary(t *testing.T) {
	sc := listJenkinsRolesResult(t, map[string]interface{}{})
	items, ok := sc["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 items, got %v", sc["items"])
	}
	ciDeploy, reader := findJenkinsRoles(t, items)

	for _, item := range []map[string]any{ciDeploy, reader} {
		allowed := map[string]bool{
			"name": true, "roleType": true, "description": true, "permissionCount": true,
		}
		for k := range item {
			if !allowed[k] {
				t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
			}
		}
		if _, present := item["namespace"]; present {
			t.Errorf("JenkinsRole is cluster-scoped; summary must not carry a namespace: %v", item)
		}
		if item["name"] == nil || item["name"] == "" {
			t.Errorf("summary missing %q: %v", "name", item)
		}
		for _, k := range []string{"metadata", "spec", "status"} {
			if _, present := item[k]; present {
				t.Errorf("summary must not contain %q", k)
			}
		}
	}

	if ciDeploy["permissionCount"] != float64(11) {
		t.Errorf("permissionCount = %v, want 11 (JSON-decoded numbers are float64)", ciDeploy["permissionCount"])
	}
	if ciDeploy["roleType"] != "Item" || ciDeploy["description"] != "Deploy permissions for CI" {
		t.Errorf("roleType/description not projected in list result: %v", ciDeploy)
	}
	if reader["permissionCount"] != float64(1) {
		t.Errorf("minimal role permissionCount = %v, want 1", reader["permissionCount"])
	}

	b, _ := json.Marshal(items)
	if strings.Contains(string(b), "hudson.model") {
		t.Errorf("list summary still carries permission text: %s", b)
	}
}

// findJenkinsRoles splits the listed items back out by name, failing if either
// seeded role is missing.
func findJenkinsRoles(t *testing.T, items []any) (ciDeploy, reader map[string]any) {
	t.Helper()
	for _, raw := range items {
		item := raw.(map[string]any)
		switch item["name"] {
		case "ci-deploy":
			ciDeploy = item
		case "reader":
			reader = item
		}
	}
	if ciDeploy == nil || reader == nil {
		t.Fatalf("seeded roles not both present: %v", items)
	}
	return ciDeploy, reader
}

// verbose is an escape hatch from the projection: the full CR, permissions
// array intact.
func TestListJenkinsRoles_VerboseReturnsFullCR(t *testing.T) {
	sc := listJenkinsRolesResult(t, map[string]interface{}{"verbose": true})
	items, ok := sc["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected items, got %v", sc["items"])
	}
	var ciDeploy map[string]any
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["metadata"].(map[string]any)["name"] == "ci-deploy" {
			ciDeploy = item
			break
		}
	}
	if ciDeploy == nil {
		t.Fatalf("ci-deploy not in verbose results: %v", items)
	}
	if _, ok := ciDeploy["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", ciDeploy)
	}
	if _, ok := ciDeploy["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", ciDeploy)
	}
	b, _ := json.Marshal(ciDeploy)
	if !strings.Contains(string(b), "hudson.model.Item.Build") {
		t.Errorf("verbose result must keep the full permissions array: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListJenkinsRoles_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(jenkinsRoleListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_jenkins_roles.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_jenkins_roles.json")
	if err != nil {
		t.Fatalf("declared schema does not compile: %v", err)
	}

	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"summary", map[string]interface{}{}},
		{"verbose", map[string]interface{}{"verbose": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := listJenkinsRolesResult(t, tc.args)
			// Round-trip so the instance is plain JSON types.
			b, _ := json.Marshal(sc)
			var instance any
			if err := json.Unmarshal(b, &instance); err != nil {
				t.Fatalf("unmarshal instance: %v", err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Errorf("%s output violates declared outputSchema: %v\npayload: %s", tc.name, err, b)
			}
		})
	}
}

// oneOf would reject every default result, because a summary satisfies both the
// summary branch and the open object branch. This pins the reasoning so a later
// "tidy-up" to oneOf fails loudly here rather than in a client.
func TestJenkinsRoleListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(jenkinsRoleListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(jenkinsRoleListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}
