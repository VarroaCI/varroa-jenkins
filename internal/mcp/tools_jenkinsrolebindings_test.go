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

// jenkinsRoleBindingDeps seeds two bindings: one carrying every field whose
// cost Task 6 exists to cut (subjects, controllerScope selector, pattern,
// propagate) and one whose JenkinsScope is nil — the exact shape the MCP
// create path produces, since create_jenkins_role_binding never sets it.
func jenkinsRoleBindingDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "jrb-scoped"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:  "ci-deploy",
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}, {Kind: "Group", Name: "devs"}},
				ControllerScope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "jenkins"}},
				},
				JenkinsScope: &v1alpha1.JenkinsScope{
					Type:      "Folder",
					Folder:    "team-a/project-x",
					Propagate: "Subtree",
					Pattern:   "team-.*",
				},
			},
		},
		&v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "jrb-nil-scope"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:  "ci-deploy",
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ci-bot"}},
				// JenkinsScope intentionally left nil: bindings created over
				// MCP never set it, so an unguarded deref would panic here.
			},
		},
	)
	return &api.Dependencies{Client: &stubClient{}, Store: store}
}

// listJenkinsRoleBindingsResult calls list_jenkins_role_bindings and returns
// the decoded structuredContent.
func listJenkinsRoleBindingsResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(jenkinsRoleBindingDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_jenkins_role_bindings",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_jenkins_role_bindings returned error: %v", tr.Content)
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

// The nil JenkinsScope case is the point of the guard: create_jenkins_role_
// binding never sets spec.jenkinsScope, so every binding made over MCP has it
// nil. A summary that dereferences unguarded panics instead of returning a
// row, taking the whole listing down with it.
func TestSummarizeJenkinsRoleBinding_NilJenkinsScopeDoesNotPanic(t *testing.T) {
	got := summarizeJenkinsRoleBinding(&v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-nil-scope"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:  "ci-deploy",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ci-bot"}},
		},
	})
	if got.ScopeType != "" || got.ScopeFolder != "" {
		t.Errorf("nil JenkinsScope must project empty scope, got type=%q folder=%q",
			got.ScopeType, got.ScopeFolder)
	}
	if got.SubjectCount != 1 {
		t.Errorf("SubjectCount = %d, want 1", got.SubjectCount)
	}
}

// The projection exists to drop spec.subjects[], jenkinsScope.pattern,
// jenkinsScope.propagate and the whole controllerScope selector. The count
// must survive; the payload text must not.
func TestSummarizeJenkinsRoleBinding_DropsSubjectsKeepsCount(t *testing.T) {
	got := summarizeJenkinsRoleBinding(&v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-scoped"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:  "ci-deploy",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}, {Kind: "Group", Name: "devs"}},
			ControllerScope: &v1alpha1.VarroaRoleBindingScope{
				ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "jenkins"}},
			},
			JenkinsScope: &v1alpha1.JenkinsScope{
				Type:      "Folder",
				Folder:    "team-a/project-x",
				Propagate: "Subtree",
				Pattern:   "team-.*",
			},
		},
	})
	if got.SubjectCount != 2 {
		t.Errorf("SubjectCount = %d, want 2", got.SubjectCount)
	}
	if got.ScopeType != "Folder" || got.ScopeFolder != "team-a/project-x" {
		t.Errorf("scope not projected: type=%q folder=%q", got.ScopeType, got.ScopeFolder)
	}
	b, _ := json.Marshal(got)
	for _, dropped := range []string{"alice", "devs", "jenkins", "Subtree", "team-.*"} {
		if strings.Contains(string(b), dropped) {
			t.Errorf("summary still carries dropped %q: %s", dropped, b)
		}
	}
}

// The default list result must be the summary projection: identifying fields
// plus the subject count, never the subjects, scope internals, or a namespace
// (JenkinsRoleBinding is cluster-scoped). The fake store returns items in map
// order, so locate each seed by name rather than by index.
func TestListJenkinsRoleBindings_DefaultsToSummary(t *testing.T) {
	sc := listJenkinsRoleBindingsResult(t, map[string]interface{}{})
	items, ok := sc["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 items, got %v", sc["items"])
	}
	scoped, nilScope := findJenkinsRoleBindings(t, items)

	for _, item := range []map[string]any{scoped, nilScope} {
		allowed := map[string]bool{
			"name": true, "roleRef": true, "subjectCount": true,
			"scopeType": true, "scopeFolder": true,
		}
		for k := range item {
			if !allowed[k] {
				t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
			}
		}
		if _, present := item["namespace"]; present {
			t.Errorf("JenkinsRoleBinding is cluster-scoped; summary must not carry a namespace: %v", item)
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
	if scoped["scopeType"] != "Folder" || scoped["scopeFolder"] != "team-a/project-x" {
		t.Errorf("scope not projected in list result: %v", scoped)
	}

	// The nil-scope binding proves the whole list survives the JenkinsScope
	// guard end to end, not just the unit function.
	if nilScope["subjectCount"] != float64(1) {
		t.Errorf("nil-scope binding subjectCount = %v, want 1", nilScope["subjectCount"])
	}
	if v, ok := nilScope["scopeType"]; ok && v != "" {
		t.Errorf("nil-scope binding should not carry a scopeType, got %v", v)
	}

	b, _ := json.Marshal(items)
	for _, dropped := range []string{"alice", "devs", "jenkins", "Subtree", "team-.*"} {
		if strings.Contains(string(b), dropped) {
			t.Errorf("list summary still carries dropped %q: %s", dropped, b)
		}
	}
}

// findJenkinsRoleBindings splits the listed items back out by name, failing if
// either seeded binding is missing.
func findJenkinsRoleBindings(t *testing.T, items []any) (scoped, nilScope map[string]any) {
	t.Helper()
	for _, raw := range items {
		item := raw.(map[string]any)
		switch item["name"] {
		case "jrb-scoped":
			scoped = item
		case "jrb-nil-scope":
			nilScope = item
		}
	}
	if scoped == nil || nilScope == nil {
		t.Fatalf("seeded bindings not both present: %v", items)
	}
	return scoped, nilScope
}

// verbose is an escape hatch from the projection: the full CR, subjects and
// controllerScope intact.
func TestListJenkinsRoleBindings_VerboseReturnsFullCR(t *testing.T) {
	sc := listJenkinsRoleBindingsResult(t, map[string]interface{}{"verbose": true})
	items, ok := sc["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected items, got %v", sc["items"])
	}
	var scoped map[string]any
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["metadata"].(map[string]any)["name"] == "jrb-scoped" {
			scoped = item
			break
		}
	}
	if scoped == nil {
		t.Fatalf("jrb-scoped not in verbose results: %v", items)
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
	if !strings.Contains(string(b), "jenkins") {
		t.Errorf("verbose result must keep controllerScope selector: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListJenkinsRoleBindings_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(jenkinsRoleBindingListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_jenkins_role_bindings.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_jenkins_role_bindings.json")
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
			sc := listJenkinsRoleBindingsResult(t, tc.args)
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
func TestJenkinsRoleBindingListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(jenkinsRoleBindingListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(jenkinsRoleBindingListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}
