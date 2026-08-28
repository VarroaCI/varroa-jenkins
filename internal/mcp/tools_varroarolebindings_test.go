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

// varroaRoleBindingDeps seeds two bindings: one carrying every field whose
// cost Task 9 exists to cut (subjects, scope namespaces, controllerSelector)
// and one whose Scope is nil — the cluster-wide shape, which an unguarded
// len(r.Spec.Scope.Namespaces) would panic on.
func varroaRoleBindingDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				RoleRef:  "ci-deploy",
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}, {Kind: "Group", Name: "devs"}},
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces:         []string{"team-a", "team-b", "team-c"},
					ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "jenkins"}},
				},
			},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-cluster-wide"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				RoleRef:  "ci-deploy",
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ci-bot"}},
				// Scope intentionally left nil: this binding applies
				// cluster-wide, so an unguarded deref would panic here.
			},
		},
	)
	return &api.Dependencies{Client: &stubClient{}, Store: store}
}

// listVarroaRoleBindingsResult calls list_varroa_role_bindings and returns
// the decoded structuredContent.
func listVarroaRoleBindingsResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(varroaRoleBindingDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_varroa_role_bindings",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_varroa_role_bindings returned error: %v", tr.Content)
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

// The nil Scope case is the point of the guard: Spec.Scope is optional, nil
// means a cluster-wide binding, and len(r.Spec.Scope.Namespaces) panics on a
// nil receiver. A summary that dereferences unguarded panics instead of
// returning a row, taking the whole listing down with it.
func TestSummarizeVarroaRoleBinding_NilScopeDoesNotPanic(t *testing.T) {
	got := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-cluster-wide"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "ci-deploy",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ci-bot"}},
		},
	})
	if got.ScopeNamespaces != 0 {
		t.Errorf("nil Scope must project scopeNamespaces 0, got %d", got.ScopeNamespaces)
	}
	if got.SubjectCount != 1 {
		t.Errorf("SubjectCount = %d, want 1", got.SubjectCount)
	}
}

// The projection exists to drop spec.subjects[] and the scope namespaces /
// controllerSelector internals. The counts must survive; the payload text
// must not.
func TestSummarizeVarroaRoleBinding_DropsArraysKeepsCounts(t *testing.T) {
	got := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-scoped"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "ci-deploy",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}, {Kind: "Group", Name: "devs"}},
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Namespaces:         []string{"team-a", "team-b", "team-c"},
				ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "jenkins"}},
			},
		},
	})
	if got.SubjectCount != 2 {
		t.Errorf("SubjectCount = %d, want 2", got.SubjectCount)
	}
	if got.ScopeNamespaces != 3 {
		t.Errorf("ScopeNamespaces = %d, want 3", got.ScopeNamespaces)
	}
	b, _ := json.Marshal(got)
	for _, dropped := range []string{"alice", "devs", "team-a", "jenkins"} {
		if strings.Contains(string(b), dropped) {
			t.Errorf("summary still carries dropped %q: %s", dropped, b)
		}
	}
}

// The default list result must be the summary projection: identifying fields
// plus the two counts, never the subjects, scope internals, or a namespace
// (VarroaRoleBinding is cluster-scoped). The fake store returns items in map
// order, so locate each seed by name rather than by index.
func TestListVarroaRoleBindings_DefaultsToSummary(t *testing.T) {
	sc := listVarroaRoleBindingsResult(t, map[string]interface{}{})
	items, ok := sc["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 items, got %v", sc["items"])
	}
	scoped, clusterWide := findVarroaRoleBindings(t, items)

	for _, item := range []map[string]any{scoped, clusterWide} {
		allowed := map[string]bool{
			"name": true, "roleRef": true, "subjectCount": true, "scopeNamespaces": true,
		}
		for k := range item {
			if !allowed[k] {
				t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
			}
		}
		if _, present := item["namespace"]; present {
			t.Errorf("VarroaRoleBinding is cluster-scoped; summary must not carry a namespace: %v", item)
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

	if scoped["subjectCount"] != float64(2) {
		t.Errorf("subjectCount = %v, want 2 (JSON-decoded numbers are float64)", scoped["subjectCount"])
	}
	if scoped["scopeNamespaces"] != float64(3) {
		t.Errorf("scopeNamespaces = %v, want 3 (JSON-decoded numbers are float64)", scoped["scopeNamespaces"])
	}

	// The nil-scope binding proves the whole list survives the Scope guard end
	// to end, not just the unit function.
	if clusterWide["subjectCount"] != float64(1) {
		t.Errorf("cluster-wide binding subjectCount = %v, want 1", clusterWide["subjectCount"])
	}
	if clusterWide["scopeNamespaces"] != float64(0) {
		t.Errorf("cluster-wide binding scopeNamespaces = %v, want 0", clusterWide["scopeNamespaces"])
	}

	b, _ := json.Marshal(items)
	for _, dropped := range []string{"alice", "devs", "team-a", "jenkins"} {
		if strings.Contains(string(b), dropped) {
			t.Errorf("list summary still carries dropped %q: %s", dropped, b)
		}
	}
}

// findVarroaRoleBindings splits the listed items back out by name, failing if
// either seeded binding is missing.
func findVarroaRoleBindings(t *testing.T, items []any) (scoped, clusterWide map[string]any) {
	t.Helper()
	for _, raw := range items {
		item := raw.(map[string]any)
		switch item["name"] {
		case "vrb-scoped":
			scoped = item
		case "vrb-cluster-wide":
			clusterWide = item
		}
	}
	if scoped == nil || clusterWide == nil {
		t.Fatalf("seeded bindings not both present: %v", items)
	}
	return scoped, clusterWide
}

// verbose is an escape hatch from the projection: the full CR, subjects and
// scope internals intact.
func TestListVarroaRoleBindings_VerboseReturnsFullCR(t *testing.T) {
	sc := listVarroaRoleBindingsResult(t, map[string]interface{}{"verbose": true})
	items, ok := sc["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected items, got %v", sc["items"])
	}
	var scoped map[string]any
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["metadata"].(map[string]any)["name"] == "vrb-scoped" {
			scoped = item
			break
		}
	}
	if scoped == nil {
		t.Fatalf("vrb-scoped not in verbose results: %v", items)
	}
	if _, ok := scoped["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", scoped)
	}
	if _, ok := scoped["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", scoped)
	}
	b, _ := json.Marshal(scoped)
	if !strings.Contains(string(b), "alice") {
		t.Errorf("verbose result must keep the full subjects array: %s", b)
	}
	if !strings.Contains(string(b), "team-a") {
		t.Errorf("verbose result must keep the scope namespaces list: %s", b)
	}
	if !strings.Contains(string(b), "jenkins") {
		t.Errorf("verbose result must keep the scope controllerSelector: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListVarroaRoleBindings_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(varroaRoleBindingListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_varroa_role_bindings.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_varroa_role_bindings.json")
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
			sc := listVarroaRoleBindingsResult(t, tc.args)
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
func TestVarroaRoleBindingListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(varroaRoleBindingListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(varroaRoleBindingListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}
