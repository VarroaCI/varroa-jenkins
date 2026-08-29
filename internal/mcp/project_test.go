package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// listControllersResult calls list_controllers and returns the decoded
// structuredContent.
func listControllersResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(adminDeps())
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_controllers",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_controllers returned error: %v", tr.Content)
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

func firstItem(t *testing.T, sc map[string]any) map[string]any {
	t.Helper()
	items, ok := sc["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty items, got %v", sc)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("expected object items, got %T", items[0])
	}
	return item
}

func TestListControllers_DefaultsToSummary(t *testing.T) {
	item := firstItem(t, listControllersResult(t, map[string]interface{}{"namespace": "ns"}))

	allowed := map[string]bool{
		"name": true, "namespace": true, "cluster": true, "phase": true,
		"version": true, "powerState": true,
		"createdAt": true, "lastReconcileError": true,
	}
	for k := range item {
		if !allowed[k] {
			t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
		}
	}
	// The projection is worthless if it drops the identifying fields.
	for _, k := range []string{"name", "namespace", "phase", "version"} {
		if item[k] == nil || item[k] == "" {
			t.Errorf("summary missing %q: %v", k, item)
		}
	}
	// The summary must never carry the heavyweight resource fields.
	for _, k := range []string{"metadata", "spec", "status"} {
		if _, present := item[k]; present {
			t.Errorf("summary must not contain %q", k)
		}
	}
}

// The cluster field is an assertion about where a controller lives, and callers
// act on it. Echoing the caller's own filter back would let `cluster: "prod"`
// return local controllers stamped "prod", which is worse than reporting
// nothing — the summary would be confidently wrong.
func TestListControllers_DoesNotEchoCallerSuppliedCluster(t *testing.T) {
	item := firstItem(t, listControllersResult(t, map[string]interface{}{
		"namespace": "ns",
		"cluster":   "totally-made-up-cluster",
	}))
	if got := item["cluster"]; got != nil && got != "" {
		t.Errorf("cluster = %v; without a brood there is no authoritative cluster "+
			"name, so it must be omitted rather than reflected from the request", got)
	}
}

func TestListControllers_VerboseReturnsFullButSanitizedCRs(t *testing.T) {
	item := firstItem(t, listControllersResult(t, map[string]interface{}{
		"namespace": "ns", "verbose": true,
	}))

	if _, ok := item["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	if _, ok := item["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}

	// Verbose is an escape hatch from the projection, never from sanitization.
	b, _ := json.Marshal(item)
	for _, forbidden := range []string{"wakeToken", testWakeToken, "managedFields"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("verbose result must not contain %q: %s", forbidden, b)
		}
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. The schema is
// validated against real structuredContent output in both modes.
func TestListControllers_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(controllerListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_controllers.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_controllers.json")
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
			sc := listControllersResult(t, tc.args)
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
func TestControllerListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(controllerListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(controllerListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}

// endpoint was projected, declared in the schema, and named in the tool
// description, but no operator code ever assigns Controller.Status.Endpoint —
// `grep -rn "Status\.Endpoint\s*=" --include=*.go .` returns nothing, and the
// field is absent from a Connected controller's status. Projecting a field
// nobody populates advertises a promise the server cannot keep.
func TestControllerSummary_OmitsUnpopulatedEndpoint(t *testing.T) {
	b, err := json.Marshal(summarizeController("core", &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "varroa"},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "endpoint") {
		t.Errorf("summary still carries endpoint: %s", b)
	}
	if strings.Contains(string(controllerListOutputSchema), "endpoint") {
		t.Error("output schema still declares endpoint")
	}
}

func TestListOutputSchema_UsesAnyOfAndDeclaresFields(t *testing.T) {
	raw := listOutputSchema("Default summary projection.", []schemaField{
		{Name: "name", Type: "string", Desc: "Object name."},
		{Name: "itemCount", Type: "integer", Desc: "Number of items."},
	})
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if strings.Contains(string(raw), `"oneOf"`) {
		t.Error("schema uses oneOf; a summary matches both branches so oneOf rejects every result")
	}
	if !strings.Contains(string(raw), `"anyOf"`) {
		t.Error("schema must use anyOf for the summary/verbose dual shape")
	}
	for _, want := range []string{`"name"`, `"itemCount"`, `"integer"`, `"count"`, `"items"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("schema missing %s: %s", want, raw)
		}
	}
}

// Go quoting and JSON quoting are different languages: fmt's %q emits \a, \v
// and \xNN, none of which JSON accepts. A description carrying a control
// character must still produce a parseable schema.
func TestListOutputSchema_EscapesJSONCorrectly(t *testing.T) {
	cases := map[string]string{
		"bell":    "ring\a it",
		"vtab":    "down\v under",
		"control": "ctrl\x01char",
		"quote":   `say "hello"`,
		"newline": "two\nlines",
		"unicode": "em—dash and é",
		"solidus": `a\b path`,
	}
	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := listOutputSchema(desc, []schemaField{{Name: "f", Type: "string", Desc: desc}})
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("schema is not valid JSON for %q: %v\n%s", desc, err, raw)
			}
		})
	}
}

// A projection with no declared fields must still emit a parseable schema
// rather than a dangling "properties" block.
func TestListOutputSchema_EmptyFieldsStillValid(t *testing.T) {
	raw := listOutputSchema("Empty projection.", nil)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("empty-field schema is not valid JSON: %v\n%s", err, raw)
	}
}

// itemNamed returns the boxed item whose "name" matches, failing if absent.
//
// List results come back in nondeterministic order, so a test that cares about
// a specific object must select it by name. Indexing into items passes in
// isolation and fails intermittently in a full package run.
func itemNamed(t *testing.T, sc map[string]any, name string) map[string]any {
	t.Helper()
	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("expected items array, got %v", sc)
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("expected object items, got %T", raw)
		}
		if item["name"] == name {
			return item
		}
	}
	t.Fatalf("no item named %q in %v", name, items)
	return nil
}
