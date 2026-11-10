package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// jenkinsRoleBindingDeps seeds two bindings: one carrying every field whose
// cost Task 6 exists to cut (subjects, controllerScope selector, pattern,
// propagate) and one whose JenkinsScope is nil, the shape that a create
// without jenkinsScope produces.
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
					Pattern:   "tenant-.*",
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

// The nil JenkinsScope case is the point of the guard: a create without
// jenkinsScope leaves spec.jenkinsScope nil. A summary that dereferences
// unguarded panics instead of returning a row, taking the whole listing down
// with it.
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
				Pattern:   "tenant-.*",
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
	for _, dropped := range []string{"alice", "devs", "jenkins", "Subtree", "tenant-.*"} {
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
	for _, dropped := range []string{"alice", "devs", "jenkins", "Subtree", "tenant-.*"} {
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

// writeJRBArgs builds the argument map of a create or update call. Extra keys
// are added on top of the required ones.
func writeJRBArgs(name string, extra map[string]interface{}) map[string]interface{} {
	args := map[string]interface{}{
		"name":     name,
		"roleRef":  "ci-deploy",
		"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// seededJRBStore returns admin deps whose store holds the given binding.
func seededJRBStore(t *testing.T, binding *v1alpha1.JenkinsRoleBinding) *api.Dependencies {
	t.Helper()
	deps := adminDeps()
	crdstore.MustSeed(deps.Store.(*crdstore.Fake), binding)
	return deps
}

// storedJRB reads a binding back from the store that the deps hold.
func storedJRB(t *testing.T, deps *api.Dependencies, name string) *v1alpha1.JenkinsRoleBinding {
	t.Helper()
	b, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), deps.Store, name, "")
	if err != nil {
		t.Fatalf("get binding %s: %v", name, err)
	}
	return b
}

// assertJRBAbsent fails when the named binding exists.
func assertJRBAbsent(t *testing.T, deps *api.Dependencies, name string) {
	t.Helper()
	if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), deps.Store, name, ""); err == nil {
		t.Errorf("binding %s was written; a refused call must write nothing", name)
	}
}

// jrbScopeSeed is a controllerScope with a namespace and a label selector. An
// update must keep it or replace it.
func jrbScopeSeed() *v1alpha1.VarroaRoleBindingScope {
	return &v1alpha1.VarroaRoleBindingScope{
		Namespaces: []string{"old"},
		ControllerSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"env": "dev"},
		},
	}
}

// jrbExpressions is the matchExpressions term of a selector, as JSON args.
var jrbExpressions = []interface{}{
	map[string]interface{}{"key": "env", "operator": "In", "values": []interface{}{"prod"}},
}

func TestCreateJenkinsRoleBinding_StoresBothScopes(t *testing.T) {
	deps := adminDeps()
	tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-both", map[string]interface{}{
		"controllerScope": map[string]interface{}{
			"namespaces": []interface{}{"team-a"},
			"controllerSelector": map[string]interface{}{
				"matchLabels":      map[string]interface{}{"app": "jenkins"},
				"matchExpressions": jrbExpressions,
			},
		},
		"jenkinsScope": map[string]interface{}{"type": "Folder", "folder": "payments", "propagate": "Subtree"},
	}))
	if tr.IsError {
		t.Fatalf("create returned error: %s", toolText(tr))
	}
	got := storedJRB(t, deps, "jrb-both")
	wantCS := &v1alpha1.VarroaRoleBindingScope{
		Namespaces: []string{"team-a"},
		ControllerSelector: &metav1.LabelSelector{
			MatchLabels:      map[string]string{"app": "jenkins"},
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}},
		},
	}
	if !apiequality.Semantic.DeepEqual(got.Spec.ControllerScope, wantCS) {
		t.Errorf("controllerScope = %+v, want %+v", got.Spec.ControllerScope, wantCS)
	}
	wantJS := &v1alpha1.JenkinsScope{Type: "Folder", Folder: "payments", Propagate: "Subtree"}
	if !apiequality.Semantic.DeepEqual(got.Spec.JenkinsScope, wantJS) {
		t.Errorf("jenkinsScope = %+v, want %+v", got.Spec.JenkinsScope, wantJS)
	}
}

func TestCreateJenkinsRoleBinding_JenkinsScopeShapesAccepted(t *testing.T) {
	cases := []struct {
		name string
		js   map[string]interface{}
		want v1alpha1.JenkinsScope
	}{
		{"global", map[string]interface{}{"type": "Global"}, v1alpha1.JenkinsScope{Type: "Global"}},
		{"folder", map[string]interface{}{"type": "Folder", "folder": "a"}, v1alpha1.JenkinsScope{Type: "Folder", Folder: "a"}},
		{"folder none", map[string]interface{}{"type": "Folder", "folder": "a", "propagate": "None"}, v1alpha1.JenkinsScope{Type: "Folder", Folder: "a", Propagate: "None"}},
		{"folder children", map[string]interface{}{"type": "Folder", "folder": "a", "propagate": "Children"}, v1alpha1.JenkinsScope{Type: "Folder", Folder: "a", Propagate: "Children"}},
		{"pattern", map[string]interface{}{"type": "Pattern", "pattern": "payments/.*"}, v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "payments/.*"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := adminDeps()
			tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-shape", map[string]interface{}{"jenkinsScope": c.js}))
			if tr.IsError {
				t.Fatalf("create returned error: %s", toolText(tr))
			}
			got := storedJRB(t, deps, "jrb-shape")
			if got.Spec.JenkinsScope == nil || *got.Spec.JenkinsScope != c.want {
				t.Errorf("jenkinsScope = %+v, want %+v", got.Spec.JenkinsScope, c.want)
			}
		})
	}
}

func TestCreateJenkinsRoleBinding_OmittedScopesStayNil(t *testing.T) {
	deps := adminDeps()
	tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-bare", nil))
	if tr.IsError {
		t.Fatalf("create returned error: %s", toolText(tr))
	}
	got := storedJRB(t, deps, "jrb-bare")
	if got.Spec.ControllerScope != nil || got.Spec.JenkinsScope != nil {
		t.Errorf("scopes = %+v / %+v, want both nil", got.Spec.ControllerScope, got.Spec.JenkinsScope)
	}
}

func TestCreateJenkinsRoleBinding_InvalidControllerScopeRejected(t *testing.T) {
	cases := []struct {
		name  string
		scope interface{}
		want  string // a substring of the refusal text
	}{
		{"tenants", map[string]interface{}{"tenants": []interface{}{map[string]interface{}{"name": "a"}}}, "has no tenant meaning"},
		{"empty object", map[string]interface{}{}, "invalid controllerScope: an empty controllerScope"},
		{"unknown key", map[string]interface{}{"scope": "x"}, `unknown controllerScope key "scope"`},
		{"namespaces string", map[string]interface{}{"namespaces": "team-a"}, "controllerScope.namespaces must be a list of strings"},
		{"namespaces empty", map[string]interface{}{"namespaces": []interface{}{}}, "controllerScope.namespaces is empty"},
		{"namespaces number", map[string]interface{}{"namespaces": []interface{}{7}}, "controllerScope.namespaces[0] must be a string"},
		{"selector null", map[string]interface{}{"controllerSelector": nil}, "controllerScope.controllerSelector must be an object"},
		{"selector string", map[string]interface{}{"controllerSelector": "env=prod"}, "controllerScope.controllerSelector must be an object"},
		{"selector unknown key", map[string]interface{}{"controllerSelector": map[string]interface{}{"matchExpression": []interface{}{}}}, "controllerScope.controllerSelector"},
		{"selector bad operator", map[string]interface{}{"controllerSelector": map[string]interface{}{
			"matchExpressions": []interface{}{map[string]interface{}{"key": "env", "operator": "Equals", "values": []interface{}{"prod"}}},
		}}, "controllerScope.controllerSelector"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := adminDeps()
			tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-bad-cs", map[string]interface{}{"controllerScope": c.scope}))
			if !tr.IsError {
				t.Fatalf("create accepted controllerScope %v", c.scope)
			}
			if !strings.Contains(toolText(tr), c.want) {
				t.Errorf("error %q does not contain %q", toolText(tr), c.want)
			}
			assertJRBAbsent(t, deps, "jrb-bad-cs")
		})
	}
}

func TestCreateJenkinsRoleBinding_InvalidJenkinsScopeRejected(t *testing.T) {
	cases := []struct {
		name string
		js   map[string]interface{}
		want string
	}{
		{"empty object", map[string]interface{}{}, "an empty jenkinsScope"},
		{"unknown key", map[string]interface{}{"type": "Global", "path": "x"}, `unknown jenkinsScope key "path"`},
		{"missing type", map[string]interface{}{"folder": "a"}, "jenkinsScope.type is required"},
		{"lowercase type", map[string]interface{}{"type": "folder", "folder": "a"}, `unknown jenkinsScope.type "folder"`},
		{"unknown type", map[string]interface{}{"type": "Cluster"}, `unknown jenkinsScope.type "Cluster"`},
		{"numeric type", map[string]interface{}{"type": 5}, "jenkinsScope.type must be a string"},
		{"null folder", map[string]interface{}{"type": "Folder", "folder": nil}, "jenkinsScope.folder must be a string"},
		{"folder without folder", map[string]interface{}{"type": "Folder"}, "needs a non-empty folder"},
		{"folder empty folder", map[string]interface{}{"type": "Folder", "folder": ""}, "needs a non-empty folder"},
		{"folder with pattern", map[string]interface{}{"type": "Folder", "folder": "a", "pattern": "x"}, "takes no pattern"},
		{"folder bad propagate", map[string]interface{}{"type": "Folder", "folder": "a", "propagate": "Deep"}, `unknown jenkinsScope.propagate "Deep"`},
		{"folder empty propagate", map[string]interface{}{"type": "Folder", "folder": "a", "propagate": ""}, `unknown jenkinsScope.propagate ""`},
		{"pattern without pattern", map[string]interface{}{"type": "Pattern"}, "needs a non-empty pattern"},
		{"pattern empty", map[string]interface{}{"type": "Pattern", "pattern": ""}, "needs a non-empty pattern"},
		{"pattern with folder", map[string]interface{}{"type": "Pattern", "pattern": "x", "folder": "a"}, "takes no folder or propagate"},
		{"pattern with propagate", map[string]interface{}{"type": "Pattern", "pattern": "x", "propagate": "None"}, "takes no folder or propagate"},
		{"global with folder", map[string]interface{}{"type": "Global", "folder": "a"}, "a Global jenkinsScope takes no folder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := adminDeps()
			tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-bad-js", map[string]interface{}{"jenkinsScope": c.js}))
			if !tr.IsError {
				t.Fatalf("create accepted jenkinsScope %v", c.js)
			}
			if !strings.Contains(toolText(tr), c.want) {
				t.Errorf("error %q does not contain %q", toolText(tr), c.want)
			}
			assertJRBAbsent(t, deps, "jrb-bad-js")
		})
	}
}

// A scope argument that is a JSON string or null is refused by the input
// validator before the handler runs. The binding is not written.
func TestCreateJenkinsRoleBinding_NonObjectScopeArgsRejected(t *testing.T) {
	for _, arg := range []string{"controllerScope", "jenkinsScope"} {
		for _, v := range []interface{}{`{"type":"Global"}`, nil} {
			deps := adminDeps()
			tr := callToolAs(t, deps, mcpAdminClaims, "create_jenkins_role_binding", writeJRBArgs("jrb-str", map[string]interface{}{arg: v}))
			if !tr.IsError {
				t.Errorf("%s = %v was accepted", arg, v)
			}
			assertJRBAbsent(t, deps, "jrb-str")
		}
	}
}

func TestUpdateJenkinsRoleBinding_ScopesReplaceStored(t *testing.T) {
	deps := seededJRBStore(t, &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-replace"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:         "ci-deploy",
			Subjects:        []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			ControllerScope: jrbScopeSeed(),
			JenkinsScope:    &v1alpha1.JenkinsScope{Type: "Folder", Folder: "old", Propagate: "Subtree"},
		},
	})
	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{
		"name": "jrb-replace",
		"controllerScope": map[string]interface{}{
			"controllerSelector": map[string]interface{}{"matchExpressions": jrbExpressions},
		},
		"jenkinsScope": map[string]interface{}{"type": "Pattern", "pattern": "payments/.*"},
	})
	if tr.IsError {
		t.Fatalf("update returned error: %s", toolText(tr))
	}
	got := storedJRB(t, deps, "jrb-replace")
	if got.Spec.ControllerScope == nil || len(got.Spec.ControllerScope.Namespaces) != 0 ||
		got.Spec.ControllerScope.ControllerSelector == nil || len(got.Spec.ControllerScope.ControllerSelector.MatchLabels) != 0 {
		t.Errorf("controllerScope = %+v, want the new selector only", got.Spec.ControllerScope)
	}
	if got.Spec.JenkinsScope == nil || *got.Spec.JenkinsScope != (v1alpha1.JenkinsScope{Type: "Pattern", Pattern: "payments/.*"}) {
		t.Errorf("jenkinsScope = %+v, want the pattern scope only", got.Spec.JenkinsScope)
	}
}

func TestUpdateJenkinsRoleBinding_OmittedScopesKeepStored(t *testing.T) {
	seeded := &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-keep"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:         "ci-deploy",
			Subjects:        []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			ControllerScope: jrbScopeSeed(),
			JenkinsScope:    &v1alpha1.JenkinsScope{Type: "Folder", Folder: "keep", Propagate: "Children"},
		},
	}
	deps := seededJRBStore(t, seeded)
	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{
		"name":    "jrb-keep",
		"roleRef": "ci-admin",
	})
	if tr.IsError {
		t.Fatalf("update returned error: %s", toolText(tr))
	}
	got := storedJRB(t, deps, "jrb-keep")
	if got.Spec.RoleRef != "ci-admin" {
		t.Errorf("roleRef = %q, want ci-admin", got.Spec.RoleRef)
	}
	if !apiequality.Semantic.DeepEqual(got.Spec.ControllerScope, seeded.Spec.ControllerScope) ||
		!apiequality.Semantic.DeepEqual(got.Spec.JenkinsScope, seeded.Spec.JenkinsScope) {
		t.Errorf("scopes changed: controllerScope=%+v jenkinsScope=%+v", got.Spec.ControllerScope, got.Spec.JenkinsScope)
	}
}

// The update tool reads the object that get_jenkins_role_binding returns,
// sends its scopes back, and keeps the expressions-only selector.
func TestUpdateJenkinsRoleBinding_RoundTripKeepsExpressions(t *testing.T) {
	deps := seededJRBStore(t, &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-rt"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:  "ci-deploy",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			ControllerScope: &v1alpha1.VarroaRoleBindingScope{
				ControllerSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}},
				}},
			},
			JenkinsScope: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "team-a", Propagate: "None"},
		},
	})
	seed := storedJRB(t, deps, "jrb-rt")

	got := callToolAs(t, deps, mcpAdminClaims, "get_jenkins_role_binding", map[string]interface{}{"name": "jrb-rt"})
	if got.IsError {
		t.Fatalf("get returned error: %s", toolText(got))
	}
	var read struct {
		Spec struct {
			ControllerScope map[string]interface{} `json:"controllerScope"`
			JenkinsScope    map[string]interface{} `json:"jenkinsScope"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(got.StructuredContent, &read); err != nil {
		t.Fatalf("decode get result: %v", err)
	}

	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{
		"name": "jrb-rt",
		"subjects": []interface{}{
			map[string]interface{}{"kind": "User", "name": "alice"},
			map[string]interface{}{"kind": "User", "name": "bob"},
		},
		"controllerScope": read.Spec.ControllerScope,
		"jenkinsScope":    read.Spec.JenkinsScope,
	})
	if tr.IsError {
		t.Fatalf("update returned error: %s", toolText(tr))
	}
	after := storedJRB(t, deps, "jrb-rt")
	if !apiequality.Semantic.DeepEqual(after.Spec.ControllerScope, seed.Spec.ControllerScope) {
		t.Errorf("controllerScope = %+v, want %+v", after.Spec.ControllerScope, seed.Spec.ControllerScope)
	}
	if !apiequality.Semantic.DeepEqual(after.Spec.JenkinsScope, seed.Spec.JenkinsScope) {
		t.Errorf("jenkinsScope = %+v, want %+v", after.Spec.JenkinsScope, seed.Spec.JenkinsScope)
	}
	sel, err := metav1.LabelSelectorAsSelector(after.Spec.ControllerScope.ControllerSelector)
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if sel.Matches(labels.Set{"env": "dev"}) {
		t.Error("stored selector matches env=dev; the expressions term was dropped")
	}
}

func TestUpdateJenkinsRoleBinding_InvalidScopeWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"folder without folder", map[string]interface{}{"jenkinsScope": map[string]interface{}{"type": "Folder"}}},
		{"tenants", map[string]interface{}{"controllerScope": map[string]interface{}{"tenants": []interface{}{map[string]interface{}{"name": "a"}}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seeded := &v1alpha1.JenkinsRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "jrb-refuse"},
				Spec: v1alpha1.JenkinsRoleBindingSpec{
					RoleRef:         "ci-deploy",
					Subjects:        []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
					ControllerScope: jrbScopeSeed(),
					JenkinsScope:    &v1alpha1.JenkinsScope{Type: "Global"},
				},
			}
			deps := seededJRBStore(t, seeded)
			args := map[string]interface{}{"name": "jrb-refuse", "roleRef": "other"}
			for k, v := range c.args {
				args[k] = v
			}
			tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", args)
			if !tr.IsError {
				t.Fatal("update accepted an invalid scope")
			}
			got := storedJRB(t, deps, "jrb-refuse")
			if got.Spec.RoleRef != "ci-deploy" {
				t.Errorf("roleRef = %q, want ci-deploy; a refused update wrote", got.Spec.RoleRef)
			}
			if !apiequality.Semantic.DeepEqual(got.Spec.ControllerScope, seeded.Spec.ControllerScope) ||
				!apiequality.Semantic.DeepEqual(got.Spec.JenkinsScope, seeded.Spec.JenkinsScope) {
				t.Errorf("stored scopes changed after a refused update")
			}
		})
	}
}

// {"type":"Global"} is an explicit value. It removes the item limit.
func TestUpdateJenkinsRoleBinding_GlobalTypeRemovesItemLimit(t *testing.T) {
	deps := seededJRBStore(t, &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-global"},
		Spec: v1alpha1.JenkinsRoleBindingSpec{
			RoleRef:      "ci-deploy",
			Subjects:     []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			JenkinsScope: &v1alpha1.JenkinsScope{Type: "Folder", Folder: "a"},
		},
	})
	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{
		"name":         "jrb-global",
		"jenkinsScope": map[string]interface{}{"type": "Global"},
	})
	if tr.IsError {
		t.Fatalf("update returned error: %s", toolText(tr))
	}
	got := storedJRB(t, deps, "jrb-global")
	if got.Spec.JenkinsScope == nil || *got.Spec.JenkinsScope != (v1alpha1.JenkinsScope{Type: "Global"}) {
		t.Errorf("jenkinsScope = %+v, want Global only", got.Spec.JenkinsScope)
	}
}

// The refusal texts name the argument, so the caller sees which field to fix.
func TestBuildJenkinsScope_Messages(t *testing.T) {
	cases := []struct {
		raw  interface{}
		want string
	}{
		{map[string]interface{}{"type": "Cluster"}, `invalid jenkinsScope: unknown jenkinsScope.type "Cluster"; allowed types are Global, Folder, Pattern`},
		{map[string]interface{}{"type": "Folder"}, "invalid jenkinsScope: a Folder jenkinsScope needs a non-empty folder"},
	}
	for _, c := range cases {
		_, err := buildJenkinsScope(c.raw)
		if err == nil || err.Error() != c.want {
			t.Errorf("buildJenkinsScope(%v) error = %v, want %q", c.raw, err, c.want)
		}
	}
}

func TestBuildControllerScope_Messages(t *testing.T) {
	cases := []struct {
		raw  interface{}
		want string
	}{
		{map[string]interface{}{}, "invalid controllerScope: an empty controllerScope sets no limit, so the binding becomes cluster-wide; name namespaces or controllerSelector, or omit controllerScope"},
		{map[string]interface{}{"tenants": []interface{}{}}, "invalid controllerScope: a Jenkins role binding has no tenant meaning; name namespaces or controllerSelector"},
		{map[string]interface{}{"foo": 1}, `invalid controllerScope: unknown controllerScope key "foo"; allowed keys are namespaces, controllerSelector`},
	}
	for _, c := range cases {
		_, err := buildControllerScope(c.raw)
		if err == nil || err.Error() != c.want {
			t.Errorf("buildControllerScope(%v) error = %v, want %q", c.raw, err, c.want)
		}
	}
}

// Both tools declare the two scope arguments as objects, so a client sends
// them as objects.
func TestJenkinsRoleBindingTools_DeclareScopeArgs(t *testing.T) {
	want := map[string]bool{"create_jenkins_role_binding": true, "update_jenkins_role_binding": true}
	for _, tool := range liveTools(t) {
		if !want[tool.Name] {
			continue
		}
		delete(want, tool.Name)
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		for _, arg := range []string{"controllerScope", "jenkinsScope"} {
			if schema.Properties[arg].Type != "object" {
				t.Errorf("%s argument %s type = %q, want object", tool.Name, arg, schema.Properties[arg].Type)
			}
		}
	}
	for name := range want {
		t.Errorf("tool %s is not listed", name)
	}
}
