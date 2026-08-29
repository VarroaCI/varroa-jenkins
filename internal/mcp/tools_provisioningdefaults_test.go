package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// stubVersionProfileBrood stands in for api.ConfigBrood on the one method
// list_jenkins_version_profile actually calls. Unlike the crdstore-backed
// domains, this listing goes through the brood, whose ListVersionProfiles
// returns raw JSON messages rather than typed CRs — that is exactly the shape
// the tool's summary loop has to unmarshal. Embedding the interface means only
// the method the tool calls has to be implemented.
type stubVersionProfileBrood struct {
	api.ConfigBrood
	items []json.RawMessage
}

func (s *stubVersionProfileBrood) ListVersionProfiles(context.Context, string) ([]json.RawMessage, error) {
	return s.items, nil
}

// versionProfileDepsWithRaw seeds the brood with raw messages as the bus would
// deliver them.
func versionProfileDepsWithRaw(t *testing.T, raw ...json.RawMessage) *api.Dependencies {
	t.Helper()
	return &api.Dependencies{
		Client:      &stubClient{},
		Store:       crdstore.NewFake(),
		ConfigBrood: &stubVersionProfileBrood{items: raw},
	}
}

// versionProfileDeps serializes typed profiles down to raw JSON, mirroring the
// operator->bus->brood path.
func versionProfileDeps(t *testing.T, profiles ...v1alpha1.JenkinsVersionProfile) *api.Dependencies {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(profiles))
	for _, p := range profiles {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal profile %q: %v", p.Name, err)
		}
		raw = append(raw, b)
	}
	return versionProfileDepsWithRaw(t, raw...)
}

// seedProfileDeps holds three profiles — one ready, one not ready, one
// metadata-only — plus one element that is not a JenkinsVersionProfile at all,
// so the summary loop's skip path is exercised.
func seedProfileDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	deps := versionProfileDeps(t, readyProfile, notReadyProfile, metadataOnlyProfile)
	brood := deps.ConfigBrood.(*stubVersionProfileBrood)
	brood.items = append(brood.items, json.RawMessage(`"not-a-profile"`))
	return deps
}

var (
	// readyProfile carries a resolved PluginSetRef and a PluginSetReady=True
	// condition.
	readyProfile = v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "2.479"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:      "2.479",
			Channel:      "lts",
			PluginSetRef: &v1alpha1.ConfigMapRef{Name: "plugins-2.479"},
		},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue, Reason: "Resolved"},
			},
		},
	}

	// notReadyProfile has a PluginSetReady=False condition carrying a reason.
	notReadyProfile = v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "2.480-weekly"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:      "2.480",
			Channel:      "weekly",
			PluginSetRef: &v1alpha1.ConfigMapRef{Name: "plugins-2.480"},
		},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionFalse, Reason: "ResolveFailed"},
			},
		},
	}

	// metadataOnlyProfile has a nil PluginSetRef and no conditions — the shape
	// of a profile that exists but whose plugin set has not been resolved.
	metadataOnlyProfile = v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "scratch"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.481",
			Channel: "weekly",
		},
	}
)

func listVersionProfilesResult(t *testing.T, deps *api.Dependencies, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_jenkins_version_profile",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_jenkins_version_profile returned error: %v", tr.Content)
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

// The ready/not-ready split is the whole point of the projection: the two full
// condition objects collapse to a boolean plus, only when not ready, the
// reason.
func TestSummarizeVersionProfile_ReadyAndNotReady(t *testing.T) {
	ready := summarizeVersionProfile(&v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "2.479"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:      "2.479",
			Channel:      "lts",
			PluginSetRef: &v1alpha1.ConfigMapRef{Name: "plugins-2.479"},
		},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue, Reason: "Resolved"},
			},
		},
	})
	if !ready.PluginSetReady {
		t.Error("PluginSetReady=True must set PluginSetReady")
	}
	if ready.PluginSetRef != "plugins-2.479" {
		t.Errorf("PluginSetRef = %q, want plugins-2.479", ready.PluginSetRef)
	}
	if ready.NotReadyReason != "" {
		t.Errorf("NotReadyReason = %q, want empty on a ready profile", ready.NotReadyReason)
	}
	b, _ := json.Marshal(ready)
	if strings.Contains(string(b), "notReadyReason") {
		t.Errorf("ready summary must not carry notReadyReason: %s", b)
	}

	notReady := summarizeVersionProfile(&v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "2.480-weekly"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:      "2.480",
			Channel:      "weekly",
			PluginSetRef: &v1alpha1.ConfigMapRef{Name: "plugins-2.480"},
		},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionFalse, Reason: "ResolveFailed"},
			},
		},
	})
	if notReady.PluginSetReady {
		t.Error("PluginSetReady != True must leave PluginSetReady false")
	}
	if notReady.NotReadyReason != "ResolveFailed" {
		t.Errorf("NotReadyReason = %q, want ResolveFailed", notReady.NotReadyReason)
	}
}

// PluginSetRef is a *ConfigMapRef, nil on a metadata-only profile. The guard
// matters: a nil deref here takes down the whole listing because of one
// unresolved profile.
func TestSummarizeVersionProfile_MetadataOnlyNilPluginSetRef(t *testing.T) {
	got := summarizeVersionProfile(&v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "scratch"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.481", Channel: "weekly"},
	})
	if got.PluginSetReady {
		t.Error("metadata-only profile with no conditions must not be ready")
	}
	if got.PluginSetRef != "" {
		t.Errorf("PluginSetRef = %q, want empty for a nil ref", got.PluginSetRef)
	}
	if got.NotReadyReason != "" {
		t.Errorf("NotReadyReason = %q, want empty when no PluginSetReady condition exists", got.NotReadyReason)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "pluginSetRef") {
		t.Errorf("nil PluginSetRef must be omitted (omitempty), got %s", b)
	}
}

// JenkinsVersionProfile is cluster-scoped (types.go:
// +kubebuilder:resource:scope=Cluster), so the summary and its schema must not
// project a namespace — there is no namespace to report.
func TestSummarizeVersionProfile_OmitsNamespace(t *testing.T) {
	b, err := json.Marshal(summarizeVersionProfile(&v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "2.479"},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "namespace") {
		t.Errorf("cluster-scoped summary must not project a namespace: %s", b)
	}
	if strings.Contains(string(versionProfileListOutputSchema), "namespace") {
		t.Error("output schema must not declare a namespace for a cluster-scoped profile")
	}
}

func TestSummarizeVersionProfile_NilProfile(t *testing.T) {
	if got := summarizeVersionProfile(nil); got != (versionProfileSummary{}) {
		t.Errorf("summarizeVersionProfile(nil) = %+v, want zero value", got)
	}
}

// The default list result must be the summary projection, and the malformed
// raw element — which is not a JenkinsVersionProfile — must be skipped rather
// than surfacing as a broken summary.
func TestListVersionProfiles_DefaultsToSummary(t *testing.T) {
	sc := listVersionProfilesResult(t, seedProfileDeps(t), map[string]interface{}{})

	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("items not an array: %v", sc)
	}
	// 3 valid profiles; the "not-a-profile" element must be skipped.
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3 (malformed element skipped)", len(items))
	}
	// JSON-decoded numbers are float64, never int.
	if sc["count"] != float64(3) {
		t.Errorf("count = %v (%T), want float64(3)", sc["count"], sc["count"])
	}

	byName := map[string]map[string]any{}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object: %T", it)
		}
		name, _ := m["name"].(string)
		byName[name] = m
	}

	ready := byName["2.479"]
	if ready == nil {
		t.Fatalf("missing ready profile summary; got names %v", keys(byName))
	}
	if ready["pluginSetReady"] != true {
		t.Errorf("2.479 pluginSetReady = %v, want true", ready["pluginSetReady"])
	}
	if ready["pluginSetRef"] != "plugins-2.479" {
		t.Errorf("2.479 pluginSetRef = %v, want plugins-2.479", ready["pluginSetRef"])
	}
	if _, present := ready["notReadyReason"]; present {
		t.Errorf("ready profile must not carry notReadyReason: %v", ready)
	}

	notReady := byName["2.480-weekly"]
	if notReady == nil {
		t.Fatalf("missing not-ready profile summary; got names %v", keys(byName))
	}
	if notReady["pluginSetReady"] != false {
		t.Errorf("2.480-weekly pluginSetReady = %v, want false", notReady["pluginSetReady"])
	}
	if notReady["notReadyReason"] != "ResolveFailed" {
		t.Errorf("2.480-weekly notReadyReason = %v, want ResolveFailed", notReady["notReadyReason"])
	}

	meta := byName["scratch"]
	if meta == nil {
		t.Fatalf("missing metadata-only profile summary; got names %v", keys(byName))
	}
	if _, present := meta["pluginSetRef"]; present {
		t.Errorf("metadata-only profile must omit pluginSetRef: %v", meta)
	}
	if meta["pluginSetReady"] != false {
		t.Errorf("scratch pluginSetReady = %v, want false", meta["pluginSetReady"])
	}

	// The summary must never carry CR internals.
	for name, m := range byName {
		for _, k := range []string{"metadata", "spec", "status", "namespace"} {
			if _, present := m[k]; present {
				t.Errorf("%s summary must not contain %q: %v", name, k, m)
			}
		}
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A null element unmarshals without error into the zero struct, so the empty
// name is the guard that keeps it from surfacing as a nameless summary. The
// verbose path is the raw passthrough, so null survives there.
func TestListVersionProfiles_SkipsNullElement(t *testing.T) {
	deps := versionProfileDepsWithRaw(t, mustMarshalProfile(t, readyProfile), json.RawMessage(`null`))

	sc := listVersionProfilesResult(t, deps, map[string]interface{}{})
	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("items not an array: %v", sc)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 (null element skipped)", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item not an object: %T", items[0])
	}
	if item["name"] != "2.479" {
		t.Errorf("name = %v, want 2.479", item["name"])
	}

	verbose := listVersionProfilesResult(t, deps, map[string]interface{}{"verbose": true})
	vitems, ok := verbose["items"].([]any)
	if !ok {
		t.Fatalf("verbose items not an array: %v", verbose)
	}
	if len(vitems) != 2 {
		t.Fatalf("verbose len(items) = %d, want 2 (raw passthrough keeps null)", len(vitems))
	}
}

func mustMarshalProfile(t *testing.T, p v1alpha1.JenkinsVersionProfile) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal profile %q: %v", p.Name, err)
	}
	return b
}

// verbose is the raw-message passthrough: full resources, arrays and conditions
// intact — and, because it does not project, even the malformed element
// survives.
func TestListVersionProfiles_VerboseReturnsRawMessages(t *testing.T) {
	sc := listVersionProfilesResult(t, seedProfileDeps(t), map[string]interface{}{"verbose": true})

	items, ok := sc["items"].([]any)
	if !ok {
		t.Fatalf("items not an array: %v", sc)
	}
	if len(items) != 4 {
		t.Fatalf("len(items) = %d, want 4 (verbose keeps every raw message)", len(items))
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("verbose item is not an object: %T", items[0])
	}
	for _, k := range []string{"metadata", "spec", "status"} {
		if _, present := first[k]; !present {
			t.Errorf("verbose must return full resources, missing %q: %v", k, first)
		}
	}
	b, _ := json.Marshal(first)
	if !strings.Contains(string(b), "PluginSetReady") {
		t.Errorf("verbose result must keep the status conditions: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListVersionProfiles_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(versionProfileListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_jenkins_version_profile.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_jenkins_version_profile.json")
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
			sc := listVersionProfilesResult(t, versionProfileDeps(t, readyProfile, notReadyProfile, metadataOnlyProfile), tc.args)
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
func TestVersionProfileListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(versionProfileListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(versionProfileListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}

// get/create/update/delete_jenkins_version_profile dereference
// deps.ConfigBrood. On an incompletely-wired server it is nil, and each
// handler must answer a tool error rather than panic. create/update
// share the writeProfile closure, whose guard covers both registrations.
func TestVersionProfileTools_ConfigBroodNilReturnsError(t *testing.T) {
	deps := &api.Dependencies{Client: &stubClient{}, Store: crdstore.NewFake()}
	handler := NewHandler(deps)
	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"get_jenkins_version_profile", map[string]interface{}{"name": "2.479"}},
		{"create_jenkins_version_profile", map[string]interface{}{"name": "2.479"}},
		{"update_jenkins_version_profile", map[string]interface{}{"name": "2.479"}},
		{"delete_jenkins_version_profile", map[string]interface{}{"name": "2.479"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      tc.name,
				"arguments": tc.args,
			}, mcpAdminClaims)
			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatalf("%s with nil ConfigBrood must return a tool error, got success: %v", tc.name, tr.Content)
			}
			if got := toolText(tr); got != "config brood not configured" {
				t.Errorf("%s error text = %q, want %q", tc.name, got, "config brood not configured")
			}
		})
	}
}

// writeProfile derives the permission-verb from the tool name by trimming the
// "_jenkins_version_profile" SUFFIX. A TrimPrefix mistake leaves the verb as
// the full tool name, and the permission-denial text would then read
// "version-profiles:create_jenkins_version_profile". The verb is only
// observable through that text, so assert on it.
func TestCreateJenkinsVersionProfile_PermissionTextUsesVerb(t *testing.T) {
	// ConfigBrood is present so the handler reaches the authorization check;
	// Authorizer is nil, which the writeProfile closure treats as a denial.
	deps := versionProfileDepsWithRaw(t)
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "create_jenkins_version_profile",
		"arguments": map[string]interface{}{"name": "2.479"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("expected access-denied error, got success: %v", tr.Content)
	}
	text := toolText(tr)
	if !strings.Contains(text, "create") {
		t.Errorf("permission text %q does not contain the verb %q", text, "create")
	}
	if strings.Contains(text, "create_jenkins_version_profile") {
		t.Errorf("permission text %q carries the full tool name instead of the verb; TrimSuffix not applied", text)
	}
}

// TestMCPUpdateProvisioningDefaults_StoreFallbackDoesNotStampCluster covers the
// branch a helper-level test cannot reach: with no ConfigBrood the write goes to
// the local store, so a caller-supplied cluster must not be stamped onto the
// audit event.
func TestMCPUpdateProvisioningDefaults_StoreFallbackDoesNotStampCluster(t *testing.T) {
	sink := &recordingSink{}
	deps := adminDeps()
	deps.ActivityPublisher = sink
	deps.ConfigBrood = nil // force the local-store fallback

	if err := deps.Store.(*crdstore.Fake).Seed(&v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_provisioning_defaults",
		"arguments": map[string]interface{}{
			"name": "varroa-defaults", "cluster": "prod", "defaultCPU": "2",
		},
	}, mcpAdminClaims)
	if b, _ := json.Marshal(resp.Result); strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("tool errored: %s", b)
	}

	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	if got := sink.events[0].Cluster; got != "" {
		t.Errorf("Cluster = %q, want empty: the write went to the local store, not %q", got, "prod")
	}
}
