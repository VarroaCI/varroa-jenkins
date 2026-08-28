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

// varroaRoleDeps seeds one VarroaRole carrying the fields whose cost Task 3
// exists to cut: spec.jenkinsPermissions[] and spec.apiRules[].resources/verbs.
// VarroaRole is cluster-scoped (types.go: +kubebuilder:resource:scope=Cluster),
// so the object carries no namespace.
func varroaRoleDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: v1alpha1.VarroaRoleSpec{
			JenkinsRoleRef:     "developer-jenkins-role",
			JenkinsPermissions: []string{"hudson.model.Item.Build", "hudson.model.Item.Read"},
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"get", "list"}},
				{Resources: []string{"users"}, Verbs: []string{"get"}},
			},
		},
	})
	return &api.Dependencies{Client: &stubClient{}, Store: store}
}

// listVarroaRolesResult calls list_varroa_roles and returns the decoded
// structuredContent.
func listVarroaRolesResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(varroaRoleDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_varroa_roles",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_varroa_roles returned error: %v", tr.Content)
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

// The projection exists to drop spec.jenkinsPermissions[] and
// spec.apiRules[].resources/verbs — the arrays that cost 923 B per role at
// fleet scale. The counts must survive; the permission and apiRule text must
// not.
func TestSummarizeVarroaRole_DropsPermissionsKeepsCount(t *testing.T) {
	got := summarizeVarroaRole(&v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: v1alpha1.VarroaRoleSpec{
			JenkinsRoleRef:     "developer-jenkins-role",
			JenkinsPermissions: []string{"hudson.model.Item.Build"},
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"get"}},
				{Resources: []string{"users"}, Verbs: []string{"list"}},
			},
		},
	})
	if got.JenkinsPermissionCount != 1 {
		t.Errorf("JenkinsPermissionCount = %d, want 1", got.JenkinsPermissionCount)
	}
	if got.APIRuleCount != 2 {
		t.Errorf("APIRuleCount = %d, want 2", got.APIRuleCount)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "hudson.model.Item.Build") {
		t.Errorf("summary still carries permission text: %s", b)
	}
	if strings.Contains(string(b), "controllers") {
		t.Errorf("summary still carries apiRule text: %s", b)
	}
	for _, want := range []string{"developer", "developer-jenkins-role"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("summary dropped %s: %s", want, b)
		}
	}
}

// VarroaRole is cluster-scoped, so unlike the namespaced domains the summary
// must not project a namespace — there is no namespace to report.
func TestSummarizeVarroaRole_OmitsNamespace(t *testing.T) {
	b, err := json.Marshal(summarizeVarroaRole(&v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "namespace") {
		t.Errorf("cluster-scoped summary must not project a namespace: %s", b)
	}
	if strings.Contains(string(varroaRoleListOutputSchema), "namespace") {
		t.Error("output schema must not declare a namespace for a cluster-scoped role")
	}
}

// The summarize function must tolerate a nil role (the store can return an
// empty result) without panicking.
func TestSummarizeVarroaRole_NilRole(t *testing.T) {
	if got := summarizeVarroaRole(nil); got != (varroaRoleSummary{}) {
		t.Errorf("summarizeVarroaRole(nil) = %+v, want zero value", got)
	}
}

// The default list result must be the summary projection: identifying fields
// plus counts, never the arrays themselves or raw CR internals.
func TestListVarroaRoles_DefaultsToSummary(t *testing.T) {
	item := firstItem(t, listVarroaRolesResult(t, map[string]interface{}{}))

	allowed := map[string]bool{
		"name": true, "jenkinsRoleRef": true,
		"apiRuleCount": true, "jenkinsPermissionCount": true,
	}
	for k := range item {
		if !allowed[k] {
			t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
		}
	}
	// The projection is worthless if it drops the identifying fields.
	if item["name"] == nil || item["name"] == "" {
		t.Errorf("summary missing %q: %v", "name", item)
	}
	// The permission and apiRule text is exactly the payload the projection drops.
	b, _ := json.Marshal(item)
	if strings.Contains(string(b), "hudson.model.Item.Build") {
		t.Errorf("summary must not carry permission text: %s", b)
	}
	if strings.Contains(string(b), "controllers") {
		t.Errorf("summary must not carry apiRule text: %s", b)
	}
	// JSON-decoded numbers are float64, never int.
	if item["apiRuleCount"] != float64(2) {
		t.Errorf("apiRuleCount = %v, want 2", item["apiRuleCount"])
	}
	if item["jenkinsPermissionCount"] != float64(2) {
		t.Errorf("jenkinsPermissionCount = %v, want 2", item["jenkinsPermissionCount"])
	}
	// Cluster-scoped: no namespace anywhere in the summary.
	for _, k := range []string{"metadata", "spec", "status", "namespace"} {
		if _, present := item[k]; present {
			t.Errorf("summary must not contain %q", k)
		}
	}
}

// verbose is an escape hatch from the projection: the full CR, arrays intact.
func TestListVarroaRoles_VerboseReturnsFullCR(t *testing.T) {
	item := firstItem(t, listVarroaRolesResult(t, map[string]interface{}{"verbose": true}))

	if _, ok := item["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	if _, ok := item["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	b, _ := json.Marshal(item)
	if !strings.Contains(string(b), "hudson.model.Item.Build") {
		t.Errorf("verbose result must keep the full permissions array: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListVarroaRoles_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(varroaRoleListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_varroa_roles.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_varroa_roles.json")
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
			sc := listVarroaRolesResult(t, tc.args)
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
func TestVarroaRoleListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(varroaRoleListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(varroaRoleListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}
