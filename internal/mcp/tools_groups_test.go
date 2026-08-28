package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// groupStore seeds two cluster-scoped groups: one populated (three members)
// and one empty.
func groupStore(t *testing.T) *crdstore.Fake {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: v1alpha1.GroupSpec{
			DisplayName: "Team A",
			Members:     []string{"alice", "bob", "carol"},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: "team-empty"},
		Spec:       v1alpha1.GroupSpec{DisplayName: "Empty"},
	})
	return store
}

// groupDeps returns Dependencies whose caller is a global admin. list_groups
// is admin-gated and the handler calls IsAdmin unconditionally, so a real
// Authorizer is required — built exactly like adminDeps in tools_leak_test.go.
func groupDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"*"}, Verbs: []string{"*"}},
			}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
				RoleRef:  "admin",
			},
		},
	}
	return &api.Dependencies{
		Client:     &stubClient{},
		Store:      groupStore(t),
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
	}
}

// nonAdminGroupDeps returns Dependencies whose caller holds no roles at all, so
// IsAdmin is false.
func nonAdminGroupDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	return &api.Dependencies{
		Client:     &stubClient{},
		Store:      groupStore(t),
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(nil, nil), false),
	}
}

// listGroupsResult calls list_groups and returns the decoded structuredContent.
func listGroupsResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(groupDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_groups",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_groups returned error: %v", tr.Content)
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

// findFullGroupItem selects a full-CR item by its metadata.name. A raw Group
// CR has no top-level name key (that is the summary's projection), and list
// order is nondeterministic, so the verbose test must match by name — never
// by index.
func findFullGroupItem(t *testing.T, sc map[string]any, name string) map[string]any {
	t.Helper()
	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("no items in result: %v", sc)
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if meta, _ := m["metadata"].(map[string]any); meta["name"] == name {
			return m
		}
	}
	t.Fatalf("no full item named %q in result: %v", name, sc)
	return nil
}

// The cost of a raw Group CR is spec.members; the summary drops the membership
// list and reports its length. Group is cluster-scoped, so the summary must NOT
// carry namespace.
func TestListGroups_DefaultsToSummary(t *testing.T) {
	sc := listGroupsResult(t, map[string]interface{}{})
	if sc["count"] != float64(2) {
		t.Errorf("count = %v, want 2", sc["count"])
	}
	item := findItem(t, sc, "team-a")

	allowed := map[string]bool{
		"name": true, "displayName": true, "memberCount": true,
	}
	for k := range item {
		if !allowed[k] {
			t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
		}
	}
	if item["name"] != "team-a" {
		t.Errorf("name = %v, want team-a", item["name"])
	}
	if item["displayName"] != "Team A" {
		t.Errorf("displayName = %v, want Team A", item["displayName"])
	}
	// JSON-decoded numbers come back as float64.
	if item["memberCount"] != float64(3) {
		t.Errorf("memberCount = %v (%T), want 3", item["memberCount"], item["memberCount"])
	}
	for _, k := range []string{"namespace", "members", "metadata", "spec", "status"} {
		if _, present := item[k]; present {
			t.Errorf("summary must not contain %q", k)
		}
	}
}

// A group with no members is a legitimate summary, not an omission: it must
// surface with memberCount 0 rather than vanishing or dropping the field.
func TestListGroups_EmptyGroupHasZeroMemberCount(t *testing.T) {
	item := findItem(t, listGroupsResult(t, map[string]interface{}{}), "team-empty")
	if item["memberCount"] != float64(0) {
		t.Errorf("memberCount = %v, want 0 for an empty group", item["memberCount"])
	}
	if item["displayName"] != "Empty" {
		t.Errorf("displayName = %v, want Empty", item["displayName"])
	}
}

// verbose is the escape hatch from the projection: the full CR, members
// intact. The default stays a summary. Both are pinned side by side so a
// regression in either direction fails loudly, and items are matched by name
// (metadata.name on the full CR) because list order is nondeterministic.
func TestListGroups_VerboseReturnsFullCR(t *testing.T) {
	verboseItem := findFullGroupItem(t, listGroupsResult(t, map[string]interface{}{"verbose": true}), "team-a")
	if _, ok := verboseItem["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", verboseItem)
	}
	if _, ok := verboseItem["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", verboseItem)
	}
	b, _ := json.Marshal(verboseItem)
	if !strings.Contains(string(b), "alice") {
		t.Errorf("verbose result must keep the full members array: %s", b)
	}

	// The default path must stay a summary: no CR internals.
	summaryItem := findItem(t, listGroupsResult(t, map[string]interface{}{}), "team-a")
	for _, k := range []string{"metadata", "spec", "status", "members"} {
		if _, present := summaryItem[k]; present {
			t.Errorf("default must stay a summary, got %q in %v", k, summaryItem)
		}
	}
}

// summarizeGroup must survive a nil pointer (the same guard the other
// summarizers carry) rather than panicking.
func TestSummarizeGroup_NilIsEmpty(t *testing.T) {
	if got := summarizeGroup(nil); got.MemberCount != 0 || got.Name != "" {
		t.Errorf("summarizeGroup(nil) = %+v, want zero value", got)
	}
}

// list_groups is admin-gated: a caller without the wildcard admin capability
// must be denied even though the group CRs themselves are cluster-scoped.
func TestListGroups_RequiresAdmin(t *testing.T) {
	handler := NewHandler(nonAdminGroupDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_groups",
		"arguments": map[string]interface{}{},
	}, &auth.Claims{Subject: "regular-user"})
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("expected access-denied error, got success")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "access denied") {
		t.Errorf("expected access-denied message, got: %v", tr.Content)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright.
func TestListGroups_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(groupListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_groups.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_groups.json")
	if err != nil {
		t.Fatalf("declared schema does not compile: %v", err)
	}

	sc := listGroupsResult(t, map[string]interface{}{})
	// Round-trip so the instance is plain JSON types.
	b, _ := json.Marshal(sc)
	var instance any
	if err := json.Unmarshal(b, &instance); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Errorf("list_groups output violates declared outputSchema: %v\npayload: %s", err, b)
	}
}

// oneOf would reject every default result, because a summary satisfies both the
// summary branch and the open object branch.
func TestGroupListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(groupListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(groupListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}

// Every group handler calls deps.Authorizer.IsAdmin unconditionally. On an
// incompletely-wired server Authorizer is nil, and each handler must answer a
// tool error rather than dereference nil and panic (#472).
func TestGroupTools_AuthorizerNilReturnsError(t *testing.T) {
	deps := &api.Dependencies{Client: &stubClient{}, Store: crdstore.NewFake()}
	handler := NewHandler(deps)
	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"list_groups", map[string]interface{}{}},
		{"create_group", map[string]interface{}{"name": "g1"}},
		{"delete_group", map[string]interface{}{"name": "g1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      tc.name,
				"arguments": tc.args,
			}, mcpAdminClaims)
			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatalf("%s with nil Authorizer must return a tool error, got success: %v", tc.name, tr.Content)
			}
			if got := toolText(tr); got != "authorizer not configured" {
				t.Errorf("%s error text = %q, want %q", tc.name, got, "authorizer not configured")
			}
		})
	}
}
