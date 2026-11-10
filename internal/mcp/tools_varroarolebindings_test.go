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
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
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

// The summary tallies the per-hive federation entries by state.
func TestSummarizeVarroaRoleBinding_CountsHivesByState(t *testing.T) {
	now := metav1.Now()
	got := summarizeVarroaRoleBinding(&v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb"},
		Status: v1alpha1.VarroaRoleBindingStatus{Federation: []v1alpha1.BindingFederationStatus{
			{Cluster: "a", State: v1alpha1.BindingFederated, LastTransitionTime: now},
			{Cluster: "b", State: v1alpha1.BindingFederated, LastTransitionTime: now},
			{Cluster: "c", State: v1alpha1.BindingFailed, Reason: "HiveUnavailable", LastTransitionTime: now},
			{Cluster: "d", State: v1alpha1.BindingSkipped, Reason: "NoTenantWrite", LastTransitionTime: now},
		}},
	})
	if got.HivesFederated != 2 || got.HivesFailed != 1 || got.HivesSkipped != 1 {
		t.Fatalf("counts = %d federated, %d skipped, %d failed; want 2, 1, 1", got.HivesFederated, got.HivesSkipped, got.HivesFailed)
	}
}

// get_varroa_role_binding returns the stored status with the spec.
func TestGetVarroaRoleBinding_ReturnsFederationStatus(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{RoleRef: "ci-deploy", Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ci-bot"}}},
		Status: v1alpha1.VarroaRoleBindingStatus{Federation: []v1alpha1.BindingFederationStatus{
			{Cluster: "edge", State: v1alpha1.BindingFailed, Reason: "HiveUnavailable", Message: "offline", LastTransitionTime: metav1.Now()},
		}},
	})
	handler := NewHandler(&api.Dependencies{Client: &stubClient{}, Store: store})
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_varroa_role_binding",
		"arguments": map[string]interface{}{"name": "vrb"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("get_varroa_role_binding returned error: %v", tr.Content)
	}
	b, _ := json.Marshal(resp.Result)
	for _, want := range []string{`"federation"`, `"HiveUnavailable"`, `"failed"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("result lacks %s: %s", want, b)
		}
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
			"hivesFederated": true, "hivesSkipped": true, "hivesFailed": true,
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

// varroaRoleBindingWriterDeps grants "writer-user" a cluster-wide
// rolebindings create/update/delete binding, so create_varroa_role_binding
// tests exercise the tool without a scope check getting in the way.
func varroaRoleBindingWriterDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "rb-writer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"rolebindings"}, Verbs: []string{"read", "create", "update", "delete"}},
			}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-writer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "writer-user"}},
				RoleRef:  "rb-writer",
			},
		},
	}
	return &api.Dependencies{
		Client:     &stubClient{},
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		Store:      crdstore.NewFake(),
	}
}

var mcpRBWriterClaims = &auth.Claims{Subject: "writer-user"}

// A tenants scope round-trips through create_varroa_role_binding: the args
// hold a list of {name, jobs} objects, and the stored spec carries the same
// TenantScope entries.
func TestCreateVarroaRoleBinding_TenantsScopeRoundTrips(t *testing.T) {
	deps := varroaRoleBindingWriterDeps(t)
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_varroa_role_binding",
		"arguments": map[string]interface{}{
			"name":     "vrb-tenants",
			"roleRef":  "developer",
			"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
			"scope": map[string]interface{}{
				"tenants": []interface{}{
					map[string]interface{}{"name": "team-a", "jobs": []interface{}{"release/*"}},
					map[string]interface{}{"name": "team-b"},
				},
			},
		},
	}, mcpRBWriterClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("create_varroa_role_binding returned error: %v", tr.Content)
	}

	stored, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), deps.Store, "vrb-tenants", "")
	if err != nil {
		t.Fatalf("get created binding: %v", err)
	}
	if stored.Spec.Scope == nil {
		t.Fatalf("stored binding has no scope: %+v", stored.Spec)
	}
	want := []v1alpha1.TenantScope{
		{Name: "team-a", Jobs: []string{"release/*"}},
		{Name: "team-b"},
	}
	got := stored.Spec.Scope.Tenants
	if len(got) != len(want) {
		t.Fatalf("stored tenants = %+v, want %+v", got, want)
	}
	if got[0].Name != want[0].Name || len(got[0].Jobs) != 1 || got[0].Jobs[0] != "release/*" {
		t.Errorf("stored tenants[0] = %+v, want %+v", got[0], want[0])
	}
	if got[1].Name != want[1].Name || len(got[1].Jobs) != 0 {
		t.Errorf("stored tenants[1] = %+v, want %+v", got[1], want[1])
	}
}

// TestCreateVarroaRoleBinding_InvalidTenantScopeRejected proves that
// create_varroa_role_binding rejects, locally, every tenant scope shape
// that the crdstore's omitempty round trip would otherwise turn into a
// broader-than-written grant: a tenant entry with an empty name, a
// present-and-empty tenants list, a present-and-empty jobs list, a
// non-object tenant entry, and a "tenants" value that is not a list at
// all. The CEL rules on
// api/v1alpha1.VarroaRoleBindingScope remain the server-side guard for the
// direct Kubernetes path; this test proves the MCP path never needs them,
// because the request never reaches the apiserver.
func TestCreateVarroaRoleBinding_InvalidTenantScopeRejected(t *testing.T) {
	cases := []struct {
		name  string
		scope map[string]interface{}
	}{
		{
			name: "empty tenant name",
			scope: map[string]interface{}{
				"tenants": []interface{}{
					map[string]interface{}{"name": ""},
				},
			},
		},
		{
			name: "empty tenants list",
			scope: map[string]interface{}{
				"tenants": []interface{}{},
			},
		},
		{
			name: "empty jobs list",
			scope: map[string]interface{}{
				"tenants": []interface{}{
					map[string]interface{}{"name": "team-a", "jobs": []interface{}{}},
				},
			},
		},
		{
			name: "non-object tenant entry",
			scope: map[string]interface{}{
				"tenants": []interface{}{7},
			},
		},
		{
			name: "non-array tenants value",
			scope: map[string]interface{}{
				"tenants": "team-a",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := varroaRoleBindingWriterDeps(t)
			handler := NewHandler(deps)
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name": "create_varroa_role_binding",
				"arguments": map[string]interface{}{
					"name":     "vrb-invalid-tenant-scope",
					"roleRef":  "developer",
					"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
					"scope":    tc.scope,
				},
			}, mcpRBWriterClaims)

			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatalf("create_varroa_role_binding did not reject scope %+v", tc.scope)
			}

			if _, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), deps.Store, "vrb-invalid-tenant-scope", ""); err == nil {
				t.Fatalf("rejected scope must not create a binding")
			}
		})
	}
}

// TestUpdateVarroaRoleBinding_ExpressionsOnlySelectorRoundTrips proves that a
// get, edit, update round trip keeps a stored matchExpressions term. Before
// the fix, the update dropped the term and left an empty selector, which
// matches every controller.
func TestUpdateVarroaRoleBinding_ExpressionsOnlySelectorRoundTrips(t *testing.T) {
	deps := adminDeps()
	store := deps.Store.(*crdstore.Fake)
	want := []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}
	crdstore.MustSeed(store, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-expr"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "developer",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			Scope: &v1alpha1.VarroaRoleBindingScope{
				ControllerSelector: &metav1.LabelSelector{MatchExpressions: want},
			},
		},
	})

	read := callToolAs(t, deps, mcpAdminClaims, "get_varroa_role_binding", map[string]interface{}{"name": "vrb-expr"})
	if read.IsError {
		t.Fatalf("get_varroa_role_binding returned error: %v", read.Content)
	}
	var got struct {
		Spec struct {
			Scope map[string]interface{} `json:"scope"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(read.StructuredContent, &got); err != nil {
		t.Fatalf("decode get result: %v", err)
	}
	if got.Spec.Scope == nil {
		t.Fatalf("get result has no scope")
	}

	upd := callToolAs(t, deps, mcpAdminClaims, "update_varroa_role_binding", map[string]interface{}{
		"name": "vrb-expr",
		"subjects": []interface{}{
			map[string]interface{}{"kind": "User", "name": "alice"},
			map[string]interface{}{"kind": "User", "name": "bob"},
		},
		"scope": got.Spec.Scope,
	})
	if upd.IsError {
		t.Fatalf("update_varroa_role_binding returned error: %v", upd.Content)
	}

	stored, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), store, "vrb-expr", "")
	if err != nil {
		t.Fatalf("get updated binding: %v", err)
	}
	if len(stored.Spec.Subjects) != 2 {
		t.Errorf("stored subjects = %+v, want 2 subjects", stored.Spec.Subjects)
	}
	if stored.Spec.Scope == nil || stored.Spec.Scope.ControllerSelector == nil {
		t.Fatalf("stored scope lost its controllerSelector: %+v", stored.Spec.Scope)
	}
	sel := stored.Spec.Scope.ControllerSelector
	if !apiequality.Semantic.DeepEqual(sel.MatchExpressions, want) {
		t.Errorf("stored matchExpressions = %+v, want %+v", sel.MatchExpressions, want)
	}
	if len(sel.MatchLabels) != 0 {
		t.Errorf("stored matchLabels = %+v, want none", sel.MatchLabels)
	}
	parsed, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatalf("stored selector does not parse: %v", err)
	}
	if parsed.Matches(labels.Set{"env": "dev"}) {
		t.Errorf("stored selector matches env=dev; the binding widened")
	}
}

// TestCreateVarroaRoleBinding_ControllerSelectorKeepsExpressions proves that
// create_varroa_role_binding stores both matchLabels and matchExpressions.
func TestCreateVarroaRoleBinding_ControllerSelectorKeepsExpressions(t *testing.T) {
	deps := varroaRoleBindingWriterDeps(t)
	tr := callToolAs(t, deps, mcpRBWriterClaims, "create_varroa_role_binding", map[string]interface{}{
		"name":     "vrb-selector",
		"roleRef":  "developer",
		"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
		"scope": map[string]interface{}{
			"namespaces": []interface{}{"team-a"},
			"controllerSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": "jenkins"},
				"matchExpressions": []interface{}{
					map[string]interface{}{"key": "env", "operator": "In", "values": []interface{}{"prod", "staging"}},
					map[string]interface{}{"key": "tier", "operator": "Exists"},
				},
			},
		},
	})
	if tr.IsError {
		t.Fatalf("create_varroa_role_binding returned error: %v", tr.Content)
	}

	stored, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), deps.Store, "vrb-selector", "")
	if err != nil {
		t.Fatalf("get created binding: %v", err)
	}
	if stored.Spec.Scope == nil {
		t.Fatalf("stored binding has no scope")
	}
	if len(stored.Spec.Scope.Namespaces) != 1 || stored.Spec.Scope.Namespaces[0] != "team-a" {
		t.Errorf("stored namespaces = %v, want [team-a]", stored.Spec.Scope.Namespaces)
	}
	want := &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "jenkins"},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod", "staging"}},
			{Key: "tier", Operator: metav1.LabelSelectorOpExists},
		},
	}
	if !apiequality.Semantic.DeepEqual(stored.Spec.Scope.ControllerSelector, want) {
		t.Errorf("stored controllerSelector = %+v, want %+v", stored.Spec.Scope.ControllerSelector, want)
	}
}

// TestCreateVarroaRoleBinding_EmptyControllerSelectorAccepted proves that an
// empty selector is stored as an empty selector, so it survives a round trip.
func TestCreateVarroaRoleBinding_EmptyControllerSelectorAccepted(t *testing.T) {
	deps := varroaRoleBindingWriterDeps(t)
	tr := callToolAs(t, deps, mcpRBWriterClaims, "create_varroa_role_binding", map[string]interface{}{
		"name":     "vrb-empty-selector",
		"roleRef":  "developer",
		"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
		"scope":    map[string]interface{}{"controllerSelector": map[string]interface{}{}},
	})
	if tr.IsError {
		t.Fatalf("create_varroa_role_binding returned error: %v", tr.Content)
	}
	stored, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), deps.Store, "vrb-empty-selector", "")
	if err != nil {
		t.Fatalf("get created binding: %v", err)
	}
	if stored.Spec.Scope == nil || stored.Spec.Scope.ControllerSelector == nil {
		t.Fatalf("stored binding lost its empty controllerSelector: %+v", stored.Spec.Scope)
	}
}

// TestCreateVarroaRoleBinding_InvalidScopeRejected proves that a scope shape
// the parser cannot keep whole is refused before any object is written.
func TestCreateVarroaRoleBinding_InvalidScopeRejected(t *testing.T) {
	cases := []struct {
		name  string
		scope map[string]interface{}
	}{
		{
			name:  "controllerSelector is a string",
			scope: map[string]interface{}{"controllerSelector": "app=jenkins"},
		},
		{
			name: "unknown selector key",
			scope: map[string]interface{}{"controllerSelector": map[string]interface{}{
				"matchExpression": []interface{}{map[string]interface{}{"key": "env", "operator": "In", "values": []interface{}{"prod"}}},
			}},
		},
		{
			name: "non-string label value",
			scope: map[string]interface{}{"controllerSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": 5},
			}},
		},
		{
			name: "unknown operator",
			scope: map[string]interface{}{"controllerSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{"key": "env", "operator": "Equals", "values": []interface{}{"prod"}}},
			}},
		},
		{
			name: "In without values",
			scope: map[string]interface{}{"controllerSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{"key": "env", "operator": "In"}},
			}},
		},
		{
			name: "Exists with values",
			scope: map[string]interface{}{"controllerSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{"key": "env", "operator": "Exists", "values": []interface{}{"prod"}}},
			}},
		},
		{
			name: "unknown requirement key",
			scope: map[string]interface{}{"controllerSelector": map[string]interface{}{
				"matchExpressions": []interface{}{map[string]interface{}{"key": "env", "operator": "In", "value": []interface{}{"prod"}}},
			}},
		},
		{
			name:  "namespaces is not a list",
			scope: map[string]interface{}{"namespaces": "team-a"},
		},
		{
			name:  "scope names no dimension",
			scope: map[string]interface{}{},
		},
		{
			name:  "namespaces is null",
			scope: map[string]interface{}{"namespaces": nil},
		},
		{
			name:  "controllerSelector is null",
			scope: map[string]interface{}{"controllerSelector": nil},
		},
		{
			name:  "namespaces is an empty list",
			scope: map[string]interface{}{"namespaces": []interface{}{}},
		},
		{
			name:  "namespaces holds a non-string",
			scope: map[string]interface{}{"namespaces": []interface{}{7}},
		},
		{
			name:  "unknown top-level scope key",
			scope: map[string]interface{}{"controllerScope": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "jenkins"}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := varroaRoleBindingWriterDeps(t)
			tr := callToolAs(t, deps, mcpRBWriterClaims, "create_varroa_role_binding", map[string]interface{}{
				"name":     "vrb-invalid-scope",
				"roleRef":  "developer",
				"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
				"scope":    tc.scope,
			})
			if !tr.IsError {
				t.Fatalf("create_varroa_role_binding did not reject scope %+v", tc.scope)
			}
			if !strings.Contains(toolText(tr), "invalid scope") {
				t.Errorf("error %q does not name an invalid scope", toolText(tr))
			}
			if _, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), deps.Store, "vrb-invalid-scope", ""); err == nil {
				t.Fatalf("rejected scope must not create a binding")
			}
		})
	}
}

// TestUpdateVarroaRoleBinding_InvalidControllerSelectorKeepsStoredScope proves
// that a refused selector leaves the stored binding unchanged.
func TestUpdateVarroaRoleBinding_InvalidControllerSelectorKeepsStoredScope(t *testing.T) {
	deps := adminDeps()
	store := deps.Store.(*crdstore.Fake)
	seed := []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}
	crdstore.MustSeed(store, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-keep"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			RoleRef:  "developer",
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
			Scope: &v1alpha1.VarroaRoleBindingScope{
				ControllerSelector: &metav1.LabelSelector{MatchExpressions: seed},
			},
		},
	})

	tr := callToolAs(t, deps, mcpAdminClaims, "update_varroa_role_binding", map[string]interface{}{
		"name":    "vrb-keep",
		"roleRef": "admin",
		"scope": map[string]interface{}{"controllerSelector": map[string]interface{}{
			"matchExpressions": []interface{}{map[string]interface{}{"key": "env", "operator": "Equals", "values": []interface{}{"prod"}}},
		}},
	})
	if !tr.IsError {
		t.Fatalf("update_varroa_role_binding did not reject the selector")
	}
	if !strings.Contains(toolText(tr), "invalid scope") {
		t.Errorf("error %q does not name an invalid scope", toolText(tr))
	}

	stored, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), store, "vrb-keep", "")
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if stored.Spec.RoleRef != "developer" {
		t.Errorf("stored roleRef = %q, want developer; a refused update wrote", stored.Spec.RoleRef)
	}
	if stored.Spec.Scope == nil || stored.Spec.Scope.ControllerSelector == nil ||
		!apiequality.Semantic.DeepEqual(stored.Spec.Scope.ControllerSelector.MatchExpressions, seed) {
		t.Errorf("stored scope = %+v, want the seeded selector", stored.Spec.Scope)
	}
}

// TestCreateVarroaRoleBinding_EmptyScopeMessages pins the refusal text for the
// two empty shapes. Both keep the binding cluster-wide if accepted, so each
// message must name the fix and must not claim the scope grants nothing.
func TestCreateVarroaRoleBinding_EmptyScopeMessages(t *testing.T) {
	cases := []struct {
		name  string
		scope map[string]interface{}
		want  string
	}{
		{
			name:  "empty scope",
			scope: map[string]interface{}{},
			want:  "invalid scope: an empty scope sets no limit, so the binding becomes cluster-wide; name namespaces, controllerSelector, or tenants, or omit scope",
		},
		{
			name:  "empty namespaces list",
			scope: map[string]interface{}{"namespaces": []interface{}{}},
			want:  "invalid scope: scope.namespaces is empty and sets no namespace limit; name one namespace or omit the key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := varroaRoleBindingWriterDeps(t)
			tr := callToolAs(t, deps, mcpRBWriterClaims, "create_varroa_role_binding", map[string]interface{}{
				"name":     "vrb-empty-message",
				"roleRef":  "developer",
				"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}},
				"scope":    tc.scope,
			})
			if !tr.IsError {
				t.Fatalf("create_varroa_role_binding did not reject scope %+v", tc.scope)
			}
			if got := toolText(tr); got != tc.want {
				t.Errorf("error text = %q, want %q", got, tc.want)
			}
		})
	}
}
