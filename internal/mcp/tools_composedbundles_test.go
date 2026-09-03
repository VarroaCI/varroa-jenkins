package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// composedBundleDeps seeds one composed bundle carrying the fields whose cost
// Task 2 exists to cut: warnings, inputSummary and the resolved content hash.
func composedBundleDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
		Spec:       v1alpha1.ComposedBundleSpec{DisplayName: "Platform bundle"},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:        v1alpha1.ComposedBundleReady,
			ItemCount:    7,
			ResolvedHash: "sha256:deadbeef",
			ContentRef:   "cb1-content",
			Message:      "composed cleanly",
			Warnings:     []string{"w1", "w2"},
			InputSummary: []v1alpha1.InputSummaryEntry{
				{Kind: "itemRef", Type: "jcasc"},
				{Kind: "gitSource", Type: "git"},
			},
		},
	})
	return &api.Dependencies{Client: &stubClient{}, Store: store}
}

// listComposedBundlesResult calls list_composed_bundles and returns the decoded
// structuredContent.
func listComposedBundlesResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(composedBundleDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_composed_bundles",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_composed_bundles returned error: %v", tr.Content)
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

// The projection exists to drop status.warnings[], status.inputSummary[] and
// status.observedRevisions[] — the arrays that cost 2.3 kB per bundle at fleet
// scale. The counts must survive; the payload text must not.
func TestSummarizeComposedBundle_DropsWarningsKeepsCount(t *testing.T) {
	got := summarizeComposedBundle(&v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "varroa"},
		Spec:       v1alpha1.ComposedBundleSpec{DisplayName: "Platform bundle"},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:     "Ready",
			ItemCount: 7,
			Warnings:  []string{"w1", "w2", "w3"},
		},
	})
	if got.WarningCount != 3 {
		t.Errorf("WarningCount = %d, want 3", got.WarningCount)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "w1") {
		t.Errorf("summary still carries warning text: %s", b)
	}
	for _, want := range []string{"cb1", "varroa", "Platform bundle", "Ready"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("summary dropped %s: %s", want, b)
		}
	}
}

// message mirrors the dropped warnings text, so a summary must not carry it
// wholesale: it is capped at messagePreviewLimit runes, truncated on a rune
// boundary so the JSON stays valid UTF-8.
func TestSummarizeComposedBundle_TruncatesMessage(t *testing.T) {
	t.Run("long message truncated with ellipsis", func(t *testing.T) {
		// Multi-byte runes: a byte-slice truncation would split them and emit
		// invalid UTF-8. Truncating on rune boundaries must stay valid.
		long := strings.Repeat("⚠️ ", 300)
		got := summarizeComposedBundle(&v1alpha1.ComposedBundle{
			Status: v1alpha1.ComposedBundleStatus{Message: long},
		})
		want := string([]rune(long)[:messagePreviewLimit]) + "…"
		if got.Message != want {
			t.Errorf("message = %q, want %q", got.Message, want)
		}
		if !strings.HasSuffix(got.Message, "…") {
			t.Errorf("message %q does not end with the ellipsis rune", got.Message)
		}
		if !utf8.ValidString(got.Message) {
			t.Errorf("message is not valid UTF-8: %q", got.Message)
		}
	})

	t.Run("short message returned unchanged", func(t *testing.T) {
		short := "composed cleanly"
		got := summarizeComposedBundle(&v1alpha1.ComposedBundle{
			Status: v1alpha1.ComposedBundleStatus{Message: short},
		})
		if got.Message != short {
			t.Errorf("message = %q, want %q", got.Message, short)
		}
	})

	t.Run("message at the limit is not truncated", func(t *testing.T) {
		// Exactly messagePreviewLimit runes: the cap is "at most 160, and only
		// append the ellipsis if it was longer", so this must pass through.
		exact := strings.Repeat("a", messagePreviewLimit)
		got := summarizeComposedBundle(&v1alpha1.ComposedBundle{
			Status: v1alpha1.ComposedBundleStatus{Message: exact},
		})
		if got.Message != exact {
			t.Errorf("message = %q, want unchanged %q", got.Message, exact)
		}
	})
}

// The default list result must be the summary projection: identifying fields
// plus counts, never the arrays themselves or raw CR internals.
func TestListComposedBundles_DefaultsToSummary(t *testing.T) {
	item := firstItem(t, listComposedBundlesResult(t, map[string]interface{}{"namespace": "ns"}))

	allowed := map[string]bool{
		"name": true, "namespace": true, "displayName": true, "phase": true,
		"itemCount": true, "inputCount": true, "warningCount": true,
		"resolvedHash": true, "contentRef": true, "message": true,
	}
	for k := range item {
		if !allowed[k] {
			t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
		}
	}
	// The projection is worthless if it drops the identifying fields.
	for _, k := range []string{"name", "namespace", "displayName", "phase"} {
		if item[k] == nil || item[k] == "" {
			t.Errorf("summary missing %q: %v", k, item)
		}
	}
	// The warning text is exactly the payload the projection drops.
	b, _ := json.Marshal(item)
	if strings.Contains(string(b), "w1") {
		t.Errorf("summary must not carry warning text: %s", b)
	}
	if item["warningCount"] != float64(2) {
		t.Errorf("warningCount = %v, want 2", item["warningCount"])
	}
	if item["inputCount"] != float64(2) {
		t.Errorf("inputCount = %v, want 2", item["inputCount"])
	}
	if item["itemCount"] != float64(7) {
		t.Errorf("itemCount = %v, want 7", item["itemCount"])
	}
	for _, k := range []string{"metadata", "spec", "status"} {
		if _, present := item[k]; present {
			t.Errorf("summary must not contain %q", k)
		}
	}
}

// verbose is an escape hatch from the projection: the full CR, arrays intact.
func TestListComposedBundles_VerboseReturnsFullCR(t *testing.T) {
	item := firstItem(t, listComposedBundlesResult(t, map[string]interface{}{
		"namespace": "ns", "verbose": true,
	}))

	if _, ok := item["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	if _, ok := item["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	b, _ := json.Marshal(item)
	if !strings.Contains(string(b), "w1") {
		t.Errorf("verbose result must keep the full warnings array: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListComposedBundles_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(composedBundleListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_composed_bundles.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_composed_bundles.json")
	if err != nil {
		t.Fatalf("declared schema does not compile: %v", err)
	}

	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"summary", map[string]interface{}{"namespace": "ns"}},
		{"verbose", map[string]interface{}{"namespace": "ns", "verbose": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := listComposedBundlesResult(t, tc.args)
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
func TestComposedBundleListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(composedBundleListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(composedBundleListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}

// Both create and update must declare displayName and inputs in their input
// schema. With WithInputSchemaValidation an undeclared argument is rejected
// before the handler runs, so a handler that sets a field from args while the
// schema omits it makes the field unreachable.
func TestComposedBundleTools_DeclareDisplayName(t *testing.T) {
	var missing []string
	for _, tool := range liveTools(t) {
		if tool.Name != "create_composed_bundle" && tool.Name != "update_composed_bundle" {
			continue
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("unmarshal inputSchema for %s: %v", tool.Name, err)
		}
		if _, ok := schema.Properties["displayName"]; !ok {
			missing = append(missing, tool.Name+".displayName")
		}
		inputs, ok := schema.Properties["inputs"].(map[string]any)
		if !ok {
			missing = append(missing, tool.Name+".inputs")
			continue
		}
		if inputs["type"] != "array" {
			t.Errorf("%s inputs type = %v, want array", tool.Name, inputs["type"])
		}
	}
	if len(missing) > 0 {
		t.Errorf("composed bundle tools missing properties in inputSchema: %v", missing)
	}
}

// update_composed_bundle declares a variables argument and must apply it to
// the stored object: a handler that acknowledges the update while dropping
// variables leaves a caller's change silently never happened.
func TestUpdateComposedBundle_AppliesVariables(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			DisplayName: "Platform bundle",
			Variables:   map[string]string{"env": "prod"},
		},
	})
	handler := NewHandler(composedBundleAdminDeps(store))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_composed_bundle",
		"arguments": map[string]interface{}{
			"namespace": "ns",
			"name":      "cb1",
			"variables": map[string]interface{}{"env": "staging", "region": "us-east-1"},
		},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_composed_bundle returned error: %v", tr.Content)
	}
	updated, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
	if err != nil {
		t.Fatalf("get updated bundle: %v", err)
	}
	want := map[string]string{"env": "staging", "region": "us-east-1"}
	if !reflect.DeepEqual(updated.Spec.Variables, want) {
		t.Errorf("spec.variables = %v, want %v", updated.Spec.Variables, want)
	}
}

// composedBundleAdminDeps wires a full-access authorizer over store, which is
// what the composed-bundle write tools need before they reach their own logic.
func composedBundleAdminDeps(store crdstore.Backend) *api.Dependencies {
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
		Store:      store,
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
	}
}

// The schema-declaration half is covered above; this is the other half.
// Declaring displayName is worthless if the handler never applies it, and a
// test that only reads the schema stays green while the assignment is deleted.
//
// The three string fields are set by key presence rather than by non-empty
// value, so each supports all three operations: omit to preserve, pass a value
// to set, pass "" to clear. Testing the value instead would make a non-empty
// field unclearable through MCP while REST PUT clears it by submitting a
// replacement resource.
func TestUpdateComposedBundle_StringFieldsSetAndClear(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want v1alpha1.ComposedBundleSpec
	}{
		{
			name: "set",
			args: map[string]interface{}{"displayName": "Renamed", "description": "new desc"},
			want: v1alpha1.ComposedBundleSpec{DisplayName: "Renamed", Description: "new desc", JcascMergeStrategy: "override"},
		},
		{
			name: "clear",
			args: map[string]interface{}{"displayName": "", "jcascMergeStrategy": ""},
			want: v1alpha1.ComposedBundleSpec{DisplayName: "", Description: "old desc", JcascMergeStrategy: ""},
		},
		{
			name: "omit preserves",
			args: map[string]interface{}{},
			want: v1alpha1.ComposedBundleSpec{DisplayName: "Platform bundle", Description: "old desc", JcascMergeStrategy: "override"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := crdstore.NewFake()
			crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
				ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
				Spec: v1alpha1.ComposedBundleSpec{
					DisplayName:        "Platform bundle",
					Description:        "old desc",
					JcascMergeStrategy: "override",
				},
			})
			args := map[string]interface{}{"namespace": "ns", "name": "cb1"}
			for k, v := range tc.args {
				args[k] = v
			}
			handler := NewHandler(composedBundleAdminDeps(store))
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      "update_composed_bundle",
				"arguments": args,
			}, mcpAdminClaims)

			tr := parseToolResult(t, resp.Result)
			if tr.IsError {
				t.Fatalf("update_composed_bundle returned error: %v", tr.Content)
			}
			updated, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
			if err != nil {
				t.Fatalf("get updated bundle: %v", err)
			}
			if updated.Spec.DisplayName != tc.want.DisplayName {
				t.Errorf("displayName = %q, want %q", updated.Spec.DisplayName, tc.want.DisplayName)
			}
			if updated.Spec.Description != tc.want.Description {
				t.Errorf("description = %q, want %q", updated.Spec.Description, tc.want.Description)
			}
			if updated.Spec.JcascMergeStrategy != tc.want.JcascMergeStrategy {
				t.Errorf("jcascMergeStrategy = %q, want %q", updated.Spec.JcascMergeStrategy, tc.want.JcascMergeStrategy)
			}
		})
	}
}

// composeRecordingConfigBrood implements ComposeBundle for the dry-run tools.
// It embeds the interface so the other methods need no implementation; any
// unexpected call nil-panics loudly.
type composeRecordingConfigBrood struct {
	api.ConfigBrood
	cluster string
	ns      string
	spec    json.RawMessage
	result  *bus.BundleComposePreview
	err     error
}

func (c *composeRecordingConfigBrood) ComposeBundle(_ context.Context, cluster, ns string, spec json.RawMessage) (*bus.BundleComposePreview, error) {
	c.cluster = cluster
	c.ns = ns
	c.spec = spec
	if c.err != nil {
		return nil, c.err
	}
	return c.result, nil
}

// composedBundleDryRunResult calls a composed-bundle dry-run tool and returns
// the decoded structuredContent.
func composedBundleDryRunResult(t *testing.T, deps *api.Dependencies, name string, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("%s returned error: %v", name, tr.Content)
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

func composeDryRunDeps(cb *composeRecordingConfigBrood) *api.Dependencies {
	deps := composedBundleAdminDeps(crdstore.NewFake())
	deps.ConfigBrood = cb
	return deps
}

func TestValidateComposedBundle_Valid(t *testing.T) {
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{
		BundleYAML:          "bundle: ok",
		Missing:             []string{},
		Drifted:             []string{},
		Warnings:            []string{"note 1", "note 2"},
		UnresolvedVariables: []string{"NOPE"},
	}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "validate_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
		"variables":          map[string]interface{}{"env": "prod"},
		"jcascMergeStrategy": "override",
	})

	if got["valid"] != true {
		t.Errorf("valid = %v, want true", got["valid"])
	}
	if !reflect.DeepEqual(got["errors"], []interface{}{}) {
		t.Errorf("errors = %v, want empty slice", got["errors"])
	}
	if !reflect.DeepEqual(got["warnings"], []interface{}{"note 1", "note 2"}) {
		t.Errorf("warnings = %v, want [note 1 note 2]", got["warnings"])
	}
	if !reflect.DeepEqual(got["unresolvedVariables"], []interface{}{"NOPE"}) {
		t.Errorf("unresolvedVariables = %v, want [NOPE]", got["unresolvedVariables"])
	}

	// The spec that reached the composer must carry the parsed inputs and
	// variables, and the call must target the local-cluster default.
	if cb.cluster != "core" {
		t.Errorf("cluster = %q, want core (no brood)", cb.cluster)
	}
	if cb.ns != "ns" {
		t.Errorf("ns = %q, want ns", cb.ns)
	}
	var spec v1alpha1.ComposedBundleSpec
	if err := json.Unmarshal(cb.spec, &spec); err != nil {
		t.Fatalf("unmarshal composed spec: %v", err)
	}
	if len(spec.Inputs) != 1 || spec.Inputs[0].ItemRef == nil || spec.Inputs[0].ItemRef.Name != "item-1" {
		t.Errorf("spec.Inputs = %+v, want one itemRef item-1", spec.Inputs)
	}
	if spec.Variables["env"] != "prod" {
		t.Errorf("spec.Variables = %v, want env=prod", spec.Variables)
	}
	if spec.JcascMergeStrategy != "override" {
		t.Errorf("spec.JcascMergeStrategy = %q, want override", spec.JcascMergeStrategy)
	}
}

func TestValidateComposedBundle_ErrorsMakeInvalid(t *testing.T) {
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{
		Errors: []string{"item-1: unknown catalog item"},
	}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "validate_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	if got["valid"] != false {
		t.Errorf("valid = %v, want false", got["valid"])
	}
	if !reflect.DeepEqual(got["errors"], []interface{}{"item-1: unknown catalog item"}) {
		t.Errorf("errors = %v, want the composer errors", got["errors"])
	}
}

func TestValidateComposedBundle_MissingMakesInvalid(t *testing.T) {
	// A missing item is not an error, but the bundle cannot resolve, so it must
	// map to valid=false exactly as the REST validate handler does.
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{
		Missing: []string{"item-1"},
	}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "validate_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	if got["valid"] != false {
		t.Errorf("valid = %v, want false when inputs are missing", got["valid"])
	}
	if !reflect.DeepEqual(got["errors"], []interface{}{}) {
		t.Errorf("errors = %v, want empty even when missing", got["errors"])
	}
}

func TestPreviewComposedBundle_ReturnsFullEnvelope(t *testing.T) {
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{
		BundleYAML:          "bundle: ok",
		JenkinsYAML:         "jenkins: ok",
		PluginsYAML:         "plugins: ok",
		ItemsYAML:           "items: ok",
		RbacYAML:            "rbac: ok",
		Missing:             []string{},
		Drifted:             []string{"item-2"},
		Warnings:            []string{"note"},
		UnresolvedVariables: []string{"NOPE"},
		Errors:              []string{"boom"},
	}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "preview_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	if got["bundleYaml"] != "bundle: ok" {
		t.Errorf("bundleYaml = %v, want rendered YAML", got["bundleYaml"])
	}
	if got["jenkinsYaml"] != "jenkins: ok" {
		t.Errorf("jenkinsYaml = %v, want rendered YAML", got["jenkinsYaml"])
	}
	if !reflect.DeepEqual(got["drifted"], []interface{}{"item-2"}) {
		t.Errorf("drifted = %v, want [item-2]", got["drifted"])
	}
	if !reflect.DeepEqual(got["warnings"], []interface{}{"note"}) {
		t.Errorf("warnings = %v, want [note]", got["warnings"])
	}
	if !reflect.DeepEqual(got["errors"], []interface{}{"boom"}) {
		t.Errorf("errors = %v, want [boom]", got["errors"])
	}
	if cb.cluster != "core" {
		t.Errorf("cluster = %q, want core (no brood)", cb.cluster)
	}
}

func TestPreviewComposedBundle_NormalizesNilSlices(t *testing.T) {
	// A composer result with nil lists must serialize as [] not null, the same
	// envelope the REST preview handler normalizes.
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{BundleYAML: "bundle: ok"}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "preview_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	b, _ := json.Marshal(got)
	for _, key := range []string{"missing", "drifted", "warnings", "unresolvedVariables"} {
		if !strings.Contains(string(b), `"`+key+`":[]`) {
			t.Errorf("%s must serialize as [] not null: %s", key, b)
		}
		if got[key] == nil {
			t.Errorf("%s must be present as an empty slice: %s", key, b)
		}
	}
}

func TestValidateComposedBundle_PinPreflightConflict(t *testing.T) {
	// The validate tool's result must carry the same pinPreflight shape and
	// content the REST validate response reports for equivalent input.
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{
		BundleYAML: "bundle: ok",
		PinPreflight: bus.PinPreflightReport{
			Conflicts: []bus.PinConflict{{ArtifactID: "git", BundleVersion: "999.999", SetVersion: "5.10.1"}},
			Missing:   []bus.MissingPlugin{},
		},
	}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "validate_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	pinPreflight, ok := got["pinPreflight"].(map[string]any)
	if !ok {
		t.Fatalf("pinPreflight = %v, want a map", got["pinPreflight"])
	}
	conflicts, ok := pinPreflight["conflicts"].([]any)
	if !ok || len(conflicts) != 1 {
		t.Fatalf("pinPreflight.conflicts = %v, want one conflict", pinPreflight["conflicts"])
	}
	conflict, ok := conflicts[0].(map[string]any)
	if !ok || conflict["artifactId"] != "git" {
		t.Errorf("conflict = %v, want artifactId git", conflicts[0])
	}
}

func TestPreviewComposedBundle_PinPreflightWithoutSeparateRequest(t *testing.T) {
	// The preview tool must surface pinPreflight with no request field needed
	// to opt in, using the same conflict data the validate call would report.
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{
		BundleYAML: "bundle: ok",
		PinPreflight: bus.PinPreflightReport{
			Conflicts: []bus.PinConflict{{ArtifactID: "git", BundleVersion: "999.999", SetVersion: "5.10.1"}},
			Missing:   []bus.MissingPlugin{},
		},
	}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "preview_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	pinPreflight, ok := got["pinPreflight"].(map[string]any)
	if !ok {
		t.Fatalf("pinPreflight = %v, want a map", got["pinPreflight"])
	}
	conflicts, ok := pinPreflight["conflicts"].([]any)
	if !ok || len(conflicts) != 1 {
		t.Fatalf("pinPreflight.conflicts = %v, want one conflict", pinPreflight["conflicts"])
	}
	conflict, ok := conflicts[0].(map[string]any)
	if !ok || conflict["artifactId"] != "git" {
		t.Errorf("conflict = %v, want artifactId git", conflicts[0])
	}
}

func TestValidateComposedBundle_PinPreflightAllClear(t *testing.T) {
	// A zero-value PinPreflight (the all-clear case) must still serialize as
	// empty, non-null arrays rather than null.
	cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{BundleYAML: "bundle: ok"}}
	got := composedBundleDryRunResult(t, composeDryRunDeps(cb), "validate_composed_bundle", map[string]interface{}{
		"namespace": "ns",
		"inputs": []interface{}{
			map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
		},
	})

	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"pinPreflight":{"conflicts":[],"missing":[]}`) {
		t.Errorf("pinPreflight must be present with empty, non-null arrays: %s", b)
	}
}

func TestComposedBundleDryRunTools_NilConfigBroodErrors(t *testing.T) {
	// Authorizer wired so the handler clears the authz gate and reaches the
	// ConfigBrood guard this test exists to exercise.
	deps := composedBundleAdminDeps(crdstore.NewFake())
	handler := NewHandler(deps)
	for _, name := range []string{"validate_composed_bundle", "preview_composed_bundle"} {
		resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
			"name": name,
			"arguments": map[string]interface{}{
				"namespace": "ns",
				"inputs": []interface{}{
					map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
				},
			},
		}, mcpAdminClaims)
		tr := parseToolResult(t, resp.Result)
		if !tr.IsError {
			t.Errorf("%s with nil ConfigBrood must return a tool error, got %v", name, tr.Content)
		}
		b, _ := json.Marshal(tr.Content)
		if !strings.Contains(string(b), "config brood not configured") {
			t.Errorf("%s error = %s, want config brood not configured", name, b)
		}
	}
}

// composedBundleScopedDeps builds deps whose caller holds composedbundles
// create only in namespace "team-a", with a recording ConfigBrood so tests can
// assert whether ComposeBundle ran. The dry-run tools resolve git/OCI
// credential Secrets from the target namespace, so a namespace-scoped caller
// must not be able to preview a foreign namespace.
func composedBundleScopedDeps(cb *composeRecordingConfigBrood) *api.Dependencies {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"composedbundles"}, Verbs: []string{"read", "create"}},
			}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-developer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	return &api.Dependencies{
		Client:      &stubClient{},
		Store:       crdstore.NewFake(),
		Authorizer:  api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		ConfigBrood: cb,
	}
}

// The REST validate/preview handlers gate on CanWriteComposedBundlesInNamespace
// before ComposeBundle, because composition resolves git/OCI credential Secrets
// from the target namespace. The MCP dry-run tools must mirror that: a caller
// without composedbundles:create in the requested namespace is denied, and
// ComposeBundle is never invoked.
func TestComposedBundleDryRunTools_EnforceNamespaceAuthz(t *testing.T) {
	for _, name := range []string{"validate_composed_bundle", "preview_composed_bundle"} {
		t.Run(name, func(t *testing.T) {
			cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{BundleYAML: "bundle: ok"}}
			handler := NewHandler(composedBundleScopedDeps(cb))

			// Bound namespace: authorized and ComposeBundle runs.
			ok := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name": name,
				"arguments": map[string]interface{}{
					"namespace": "team-a",
					"inputs": []interface{}{
						map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
					},
				},
			}, mcpScopedDevClaims)
			if tr := parseToolResult(t, ok.Result); tr.IsError {
				t.Fatalf("%s in bound namespace: unexpected error: %v", name, tr.Content)
			}
			if cb.ns != "team-a" {
				t.Errorf("%s: ComposeBundle namespace = %q, want team-a", name, cb.ns)
			}
			cb.cluster, cb.ns, cb.spec = "", "", nil

			// Foreign namespace: denied, ComposeBundle must NOT be called.
			denied := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name": name,
				"arguments": map[string]interface{}{
					"namespace": "team-b",
					"inputs": []interface{}{
						map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
					},
				},
			}, mcpScopedDevClaims)
			requireAccessDenied(t, parseToolResult(t, denied.Result))
			if cb.cluster != "" || cb.ns != "" || cb.spec != nil {
				t.Errorf("%s: ComposeBundle was called despite denial (cluster=%q ns=%q spec=%s)", name, cb.cluster, cb.ns, cb.spec)
			}
		})
	}
}

// A namespace-scoped caller must not bypass the tenant gate by passing an
// empty namespace. EffectiveAPICapabilities(claims, "", "") deliberately skips
// scope filtering and returns the full unscoped capability set, so "" would let
// a team-a-only caller compose in an unscoped context. Both dry-run tools must
// reject "" up front — before the authorizer — and never invoke ComposeBundle.
func TestComposedBundleDryRunTools_RejectEmptyNamespace(t *testing.T) {
	for _, name := range []string{"validate_composed_bundle", "preview_composed_bundle"} {
		t.Run(name, func(t *testing.T) {
			cb := &composeRecordingConfigBrood{result: &bus.BundleComposePreview{BundleYAML: "bundle: ok"}}
			handler := NewHandler(composedBundleScopedDeps(cb))

			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name": name,
				"arguments": map[string]interface{}{
					"namespace": "",
					"inputs": []interface{}{
						map[string]interface{}{"itemRef": map[string]interface{}{"name": "item-1"}},
					},
				},
			}, mcpScopedDevClaims)
			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatalf("%s with namespace \"\" must error, got success: %v", name, tr.Content)
			}
			b, _ := json.Marshal(tr.Content)
			if !strings.Contains(string(b), "namespace is required") {
				t.Errorf("%s error = %s, want namespace is required", name, b)
			}
			if cb.cluster != "" || cb.ns != "" || cb.spec != nil {
				t.Errorf("%s: ComposeBundle was called despite empty namespace (cluster=%q ns=%q spec=%s)", name, cb.cluster, cb.ns, cb.spec)
			}
		})
	}
}

// The schema is a client's only contract for what the handler parses, so a
// parameter the handler reads but the schema never advertises is unreachable
// (WithInputSchemaValidation rejects it).
// Both dry-run tools must declare jcascMergeStrategy like create does, and the
// inputs description must name the ociSource variant alongside itemRef/gitSource.
func TestComposedBundleDryRunTools_DeclareJcascMergeStrategyAndOciInputs(t *testing.T) {
	for _, tool := range liveTools(t) {
		if tool.Name != "validate_composed_bundle" && tool.Name != "preview_composed_bundle" {
			continue
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("unmarshal inputSchema for %s: %v", tool.Name, err)
		}
		if _, ok := schema.Properties["jcascMergeStrategy"]; !ok {
			t.Errorf("%s missing jcascMergeStrategy in inputSchema", tool.Name)
		}
		inputs, ok := schema.Properties["inputs"].(map[string]any)
		if !ok {
			t.Errorf("%s missing inputs in inputSchema", tool.Name)
			continue
		}
		desc, _ := inputs["description"].(string)
		if !strings.Contains(desc, "ociSource") {
			t.Errorf("%s inputs description must mention ociSource, got %q", tool.Name, desc)
		}
	}
}

// inputs is the merge order of a bundle, so a partial edit has no well-defined
// meaning: the list is replaced wholesale like create_composed_bundle and REST
// PUT do. Omitting the argument must leave the stored list alone, and every
// other field must survive an inputs-only call.
func TestUpdateComposedBundle_ReplacesInputs(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			DisplayName: "Platform bundle",
			Inputs: []v1alpha1.ComposedInput{
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "base"}},
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "broken"}},
			},
		},
	})
	handler := NewHandler(composedBundleAdminDeps(store))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_composed_bundle",
		"arguments": map[string]interface{}{
			"namespace": "ns",
			"name":      "cb1",
			"inputs": []interface{}{
				map[string]interface{}{"itemRef": map[string]interface{}{"name": "base"}},
			},
		},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_composed_bundle returned error: %v", tr.Content)
	}
	updated, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
	if err != nil {
		t.Fatalf("get updated bundle: %v", err)
	}
	if len(updated.Spec.Inputs) != 1 || updated.Spec.Inputs[0].ItemRef == nil || updated.Spec.Inputs[0].ItemRef.Name != "base" {
		t.Fatalf("inputs = %+v, want the single base itemRef", updated.Spec.Inputs)
	}
	if updated.Spec.DisplayName != "Platform bundle" {
		t.Fatalf("displayName must be preserved when omitted, got %q", updated.Spec.DisplayName)
	}
}

func TestUpdateComposedBundle_OmittedInputsPreserved(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			DisplayName: "Platform bundle",
			Inputs: []v1alpha1.ComposedInput{
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "base"}},
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "extra"}},
			},
		},
	})
	handler := NewHandler(composedBundleAdminDeps(store))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_composed_bundle",
		"arguments": map[string]interface{}{
			"namespace":   "ns",
			"name":        "cb1",
			"displayName": "Renamed bundle",
		},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_composed_bundle returned error: %v", tr.Content)
	}
	updated, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
	if err != nil {
		t.Fatalf("get updated bundle: %v", err)
	}
	if len(updated.Spec.Inputs) != 2 {
		t.Fatalf("inputs = %+v, want the two seeded entries preserved", updated.Spec.Inputs)
	}
	if updated.Spec.DisplayName != "Renamed bundle" {
		t.Errorf("displayName = %q, want %q", updated.Spec.DisplayName, "Renamed bundle")
	}
}

// An empty list is a caller mistake, not a request to empty the bundle: a
// bundle with no inputs composes to nothing and wedges every controller using
// it. Reject it before anything is written.
func TestUpdateComposedBundle_EmptyInputsRejected(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			DisplayName: "Platform bundle",
			Inputs: []v1alpha1.ComposedInput{
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "base"}},
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "extra"}},
			},
		},
	})
	handler := NewHandler(composedBundleAdminDeps(store))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_composed_bundle",
		"arguments": map[string]interface{}{
			"namespace": "ns",
			"name":      "cb1",
			"inputs":    []interface{}{},
		},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("empty inputs must be rejected")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "at least one input") {
		t.Errorf("error message = %v, want it to name the at-least-one-input rule", tr.Content)
	}
	stored, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
	if err != nil {
		t.Fatalf("get bundle: %v", err)
	}
	if len(stored.Spec.Inputs) != 2 {
		t.Errorf("inputs = %+v, want the seeded entries untouched", stored.Spec.Inputs)
	}
}

// Malformed inputs are caught at two layers, and both must leave the stored
// list untouched. A non-array never reaches the handler: the declared array
// type makes input-schema validation reject it first. An array whose entries
// are not input objects satisfies the schema (no items constraint) and is the
// handler's own decode guard to catch.
func TestUpdateComposedBundle_MalformedInputsRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		inputs  interface{}
		wantMsg string
	}{
		"non-array rejected by schema validation": {
			inputs:  "not-an-array",
			wantMsg: "/inputs",
		},
		"array of non-objects rejected by the handler": {
			inputs:  []interface{}{float64(123)},
			wantMsg: "invalid inputs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := crdstore.NewFake()
			crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
				ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
				Spec: v1alpha1.ComposedBundleSpec{
					DisplayName: "Platform bundle",
					Inputs: []v1alpha1.ComposedInput{
						{ItemRef: &v1alpha1.ComposedItemRef{Name: "base"}},
						{ItemRef: &v1alpha1.ComposedItemRef{Name: "extra"}},
					},
				},
			})
			handler := NewHandler(composedBundleAdminDeps(store))
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name": "update_composed_bundle",
				"arguments": map[string]interface{}{
					"namespace": "ns",
					"name":      "cb1",
					"inputs":    tc.inputs,
				},
			}, mcpAdminClaims)

			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatal("malformed inputs must be rejected")
			}
			if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, tc.wantMsg) {
				t.Errorf("error message = %v, want it to contain %q", tr.Content, tc.wantMsg)
			}
			stored, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
			if err != nil {
				t.Fatalf("get bundle: %v", err)
			}
			if len(stored.Spec.Inputs) != 2 {
				t.Errorf("inputs = %+v, want the seeded entries untouched", stored.Spec.Inputs)
			}
		})
	}
}

func TestCreateComposedBundle_EmptyInputsRejected(t *testing.T) {
	store := crdstore.NewFake()
	handler := NewHandler(composedBundleAdminDeps(store))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_composed_bundle",
		"arguments": map[string]interface{}{
			"namespace":   "ns",
			"name":        "cb-empty",
			"displayName": "Empty",
			"inputs":      []interface{}{},
		},
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("empty inputs must be rejected on create, matching update")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "at least one input") {
		t.Errorf("error message = %v, want it to name the at-least-one-input rule", tr.Content)
	}
	if _, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb-empty", "ns"); err == nil {
		t.Error("a rejected create must not persist the bundle")
	}
}
