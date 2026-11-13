package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
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

// seedLocalPD stores varroa-defaults with rootDomain local.example.com in store.
func seedLocalPD(t *testing.T, store *crdstore.Fake) {
	t.Helper()
	crdstore.MustSeed(store, &v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{RootDomain: "local.example.com"},
	})
}

// readPD reads varroa-defaults from store.
func readPD(t *testing.T, store *crdstore.Fake) *v1alpha1.ProvisioningDefaults {
	t.Helper()
	pd, err := crdstore.Get[v1alpha1.ProvisioningDefaults](context.Background(), store, "varroa-defaults", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return pd
}

// Without a config brood, the tool refuses the write on every cluster. The
// local store does not change, and no event is emitted.
func TestMCPUpdateProvisioningDefaults_NoConfigBroodRefusesTheWrite(t *testing.T) {
	for _, cluster := range []string{"", "member-b"} {
		t.Run("cluster="+cluster, func(t *testing.T) {
			deps := adminDeps()
			sink := &recordingSink{}
			deps.ActivityPublisher = sink
			local := deps.Store.(*crdstore.Fake)
			seedLocalPD(t, local)
			args := map[string]interface{}{"name": "varroa-defaults", "storageClass": "fast"}
			if cluster != "" {
				args["cluster"] = cluster
			}
			tr := updatePDCall(t, deps, args)
			if !tr.IsError || toolText(tr) != "config brood not configured" {
				t.Fatalf("result = %q, want the config brood refusal", toolText(tr))
			}
			if got := readPD(t, local).Spec.StorageClass; got != "" {
				t.Errorf("storageClass = %q, want empty: the refused write changed the local store", got)
			}
			if len(sink.events) != 0 {
				t.Errorf("events = %+v, want none", sink.events)
			}
		})
	}
}

// Without a config brood, the get tool refuses the read on every cluster.
func TestMCPGetProvisioningDefaults_NoConfigBroodRefusesTheRead(t *testing.T) {
	for _, cluster := range []string{"", "member-b"} {
		t.Run("cluster="+cluster, func(t *testing.T) {
			deps := adminDeps()
			seedLocalPD(t, deps.Store.(*crdstore.Fake))
			args := map[string]interface{}{"name": "varroa-defaults"}
			if cluster != "" {
				args["cluster"] = cluster
			}
			resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
				"name":      "get_provisioning_defaults",
				"arguments": args,
			}, mcpAdminClaims)
			tr := parseToolResult(t, resp.Result)
			if !tr.IsError || toolText(tr) != "config brood not configured" {
				t.Fatalf("result = %q, want the config brood refusal", toolText(tr))
			}
		})
	}
}

// A write without cluster goes through the local operator. It reaches no
// member, changes the local store, and emits one event that names core.
func TestMCPUpdateProvisioningDefaults_LocalWriteGoesThroughTheLocalOperator(t *testing.T) {
	deps, local, member, sink := memberDeps(t)
	seedLocalPD(t, local)

	tr := updatePDCall(t, deps, map[string]interface{}{"name": "varroa-defaults", "storageClass": "fast"})
	if tr.IsError {
		t.Fatalf("tool errored: %s", toolText(tr))
	}
	if len(member.localWrites) != 1 || len(member.updates) != 0 {
		t.Errorf("local writes = %v, member writes = %v, want one local write only", member.localWrites, member.updates)
	}
	got := readPD(t, local)
	if got.Spec.StorageClass != "fast" || got.Spec.RootDomain != "local.example.com" {
		t.Errorf("local spec = %+v, want storageClass fast and the stored rootDomain", got.Spec)
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "core" {
		t.Errorf("events = %+v, want one event on core", sink.events)
	}
}

// A write with a member cluster goes through that member's operator only. The
// local store does not change, and the event names the member.
func TestMCPUpdateProvisioningDefaults_MemberWriteLeavesTheLocalStore(t *testing.T) {
	deps, local, member, sink := memberDeps(t)
	seedLocalPD(t, local)

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %s", toolText(tr))
	}
	if len(member.updates) != 1 || len(member.localWrites) != 0 {
		t.Errorf("member writes = %v, local writes = %v, want one member write only", member.updates, member.localWrites)
	}
	if got := readPD(t, local).Spec.StorageClass; got != "" {
		t.Errorf("local storageClass = %q, want empty: a member write changed the local store", got)
	}
	if got := readPD(t, member.store).Spec.StorageClass; got != "fast" {
		t.Errorf("member storageClass = %q, want fast", got)
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
		t.Errorf("events = %+v, want one event on member-b", sink.events)
	}
}

// A write without cluster fails when the local operator does not answer. The
// local store does not change, and no event is emitted.
func TestMCPUpdateProvisioningDefaults_LocalOperatorDownChangesNothing(t *testing.T) {
	deps, local, member, sink := memberDeps(t)
	seedLocalPD(t, local)
	member.localDown = true

	tr := updatePDCall(t, deps, map[string]interface{}{"name": "varroa-defaults", "storageClass": "fast"})
	if !tr.IsError || !strings.Contains(toolText(tr), "cluster core unreachable") {
		t.Fatalf("result = %q, want a failure that names core", toolText(tr))
	}
	if got := readPD(t, local).Spec.StorageClass; got != "" {
		t.Errorf("storageClass = %q, want empty", got)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}

// pdRoutedBrood serves ProvisioningDefaults per cluster and records every
// read and write, so a test can see which cluster the base was read from.
type pdRoutedBrood struct {
	api.ConfigBrood
	stored  map[string]json.RawMessage
	readErr error
	reads   []string
	updates []pdUpdate
	// conflicts is how many writes answer conflict. Each conflict first stores
	// afterConflict on the cluster, as a second writer would have.
	conflicts     int
	afterConflict json.RawMessage
}

type pdUpdate struct {
	cluster string
	body    v1alpha1.ProvisioningDefaults
}

func (b *pdRoutedBrood) GetProvisioningDefaults(_ context.Context, cluster, _ string) (json.RawMessage, error) {
	b.reads = append(b.reads, cluster)
	if b.readErr != nil {
		return nil, b.readErr
	}
	raw, ok := b.stored[cluster]
	if !ok {
		return nil, &api.BroodError{Code: bus.CodeNotFound, Msg: "not found"}
	}
	return raw, nil
}

func (b *pdRoutedBrood) UpdateProvisioningDefaults(_ context.Context, cluster, _ string, obj json.RawMessage) (json.RawMessage, error) {
	var body v1alpha1.ProvisioningDefaults
	if err := json.Unmarshal(obj, &body); err != nil {
		return nil, err
	}
	b.updates = append(b.updates, pdUpdate{cluster: cluster, body: body})
	if b.conflicts > 0 {
		b.conflicts--
		b.stored[cluster] = b.afterConflict
		return nil, &api.BroodError{Code: bus.CodeConflict, Msg: "changed after it was read"}
	}
	return obj, nil
}

func pdSpecRaw(t *testing.T, name, rootDomain string) json.RawMessage {
	t.Helper()
	return pdSpecRawAt(t, name, "", rootDomain)
}

func pdSpecRawAt(t *testing.T, name, resourceVersion, rootDomain string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: resourceVersion},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{RootDomain: rootDomain},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func updatePDCall(t *testing.T, deps *api.Dependencies, args map[string]interface{}) toolResult {
	t.Helper()
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "update_provisioning_defaults",
		"arguments": args,
	}, mcpAdminClaims)
	return parseToolResult(t, resp.Result)
}

// The base must come from the target cluster. Each cluster stores a different
// root domain, so a merge that copied the wrong base would show up in the body.
func TestMCPUpdateProvisioningDefaults_BaseReadFromTargetCluster(t *testing.T) {
	deps := adminDeps()
	brood := &pdRoutedBrood{stored: map[string]json.RawMessage{
		"core":     pdSpecRaw(t, "varroa-defaults", "core.example.com"),
		"member-b": pdSpecRaw(t, "varroa-defaults", "member-b.example.com"),
	}}
	deps.ConfigBrood = brood

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if len(brood.reads) != 1 || brood.reads[0] != "member-b" {
		t.Fatalf("base read clusters = %v, want [member-b]", brood.reads)
	}
	if len(brood.updates) != 1 {
		t.Fatalf("got %d updates, want 1", len(brood.updates))
	}
	u := brood.updates[0]
	if u.cluster != "member-b" {
		t.Errorf("update cluster = %q, want member-b", u.cluster)
	}
	if u.body.Spec.RootDomain != "member-b.example.com" {
		t.Errorf("rootDomain = %q, want member-b.example.com (the target's own base)", u.body.Spec.RootDomain)
	}
	if u.body.Spec.StorageClass != "fast" {
		t.Errorf("storageClass = %q, want fast", u.body.Spec.StorageClass)
	}
}

// A not-found on the target starts from an empty spec. The local store holds a
// spec too, and none of it may reach the target.
func TestMCPUpdateProvisioningDefaults_NotFoundOnTargetStartsEmpty(t *testing.T) {
	deps := adminDeps()
	if err := deps.Store.(*crdstore.Fake).Seed(&v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{RootDomain: "local.example.com", StorageClass: "local-sc"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	brood := &pdRoutedBrood{stored: map[string]json.RawMessage{}}
	deps.ConfigBrood = brood

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if len(brood.updates) != 1 {
		t.Fatalf("got %d updates, want 1", len(brood.updates))
	}
	body := brood.updates[0].body
	if body.Spec.RootDomain != "" {
		t.Errorf("rootDomain = %q, want empty: the local spec must not leak into the target", body.Spec.RootDomain)
	}
	if body.Spec.StorageClass != "fast" {
		t.Errorf("storageClass = %q, want fast", body.Spec.StorageClass)
	}
	if body.Name != "varroa-defaults" {
		t.Errorf("name = %q, want varroa-defaults", body.Name)
	}
	if body.ResourceVersion != "" {
		t.Errorf("resourceVersion = %q, want empty: a not-found base only creates", body.ResourceVersion)
	}
}

// A not-found read gives a base with no resourceVersion, so the write only
// creates. When another writer creates the object first, the write answers
// conflict, and the tool reads the target again and merges onto that object.
func TestMCPUpdateProvisioningDefaults_CreateConflictReadsAndMergesAgain(t *testing.T) {
	deps := adminDeps()
	brood := &pdRoutedBrood{
		stored:        map[string]json.RawMessage{},
		conflicts:     1,
		afterConflict: pdSpecRawAt(t, "varroa-defaults", "1", "raced.example.com"),
	}
	deps.ConfigBrood = brood

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if len(brood.reads) != 2 {
		t.Fatalf("base reads = %v, want two reads of member-b", brood.reads)
	}
	if len(brood.updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(brood.updates))
	}
	if got := brood.updates[0].body.ResourceVersion; got != "" {
		t.Errorf("first write resourceVersion = %q, want empty (create-only)", got)
	}
	second := brood.updates[1].body
	if second.ResourceVersion != "1" {
		t.Errorf("second write resourceVersion = %q, want 1", second.ResourceVersion)
	}
	if second.Spec.RootDomain != "raced.example.com" {
		t.Errorf("second write rootDomain = %q, want the other writer's value", second.Spec.RootDomain)
	}
	if second.Spec.StorageClass != "fast" {
		t.Errorf("second write storageClass = %q, want fast", second.Spec.StorageClass)
	}
}

// The local cluster has the same race. The first read misses although the
// object exists, so the create through the local operator conflicts and the
// second round merges onto the object.
func TestMCPUpdateProvisioningDefaults_LocalCreateRaceReadsAndMergesAgain(t *testing.T) {
	deps, fake, member, _ := memberDeps(t)
	if err := fake.Seed(&v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{RootDomain: "raced.example.com"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gvr, err := crdstore.GVRFor[v1alpha1.ProvisioningDefaults]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	fake.FailNext("get", gvr, apierrors.NewNotFound(gvr.GroupResource(), "varroa-defaults"))

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored after a create race: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.ProvisioningDefaults](context.Background(), fake, "varroa-defaults", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.RootDomain != "raced.example.com" {
		t.Errorf("rootDomain = %q, want the other writer's value kept", got.Spec.RootDomain)
	}
	if got.Spec.StorageClass != "fast" {
		t.Errorf("storageClass = %q, want fast", got.Spec.StorageClass)
	}
	if len(member.localWrites) != 2 || len(member.updates) != 0 {
		t.Errorf("local writes = %v, member writes = %v, want two local writes only", member.localWrites, member.updates)
	}
}

// A transient read error must fail the call. Starting from empty would write
// an object with only the patch fields over the target's real spec.
func TestMCPUpdateProvisioningDefaults_TransientReadErrorWritesNothing(t *testing.T) {
	deps := adminDeps()
	brood := &pdRoutedBrood{readErr: errors.New("bus timeout")}
	deps.ConfigBrood = brood

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if !tr.IsError {
		t.Fatalf("want a tool error on a transient read failure, got %v", tr.Content)
	}
	if len(brood.updates) != 0 {
		t.Errorf("got %d updates, want none after a failed read", len(brood.updates))
	}
}

// A conflict on the write makes the tool read the target again and merge its
// arguments onto the newer object. The second write carries the newer version.
func TestMCPUpdateProvisioningDefaults_ConflictReadsAndMergesAgain(t *testing.T) {
	deps := adminDeps()
	newer, err := json.Marshal(v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults", ResourceVersion: "2"},
		Spec: v1alpha1.ProvisioningDefaultsSpec{
			RootDomain:         "newer.example.com",
			IngressAnnotations: map[string]string{"kubernetes.io/ingress.class": "other"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	brood := &pdRoutedBrood{
		stored:        map[string]json.RawMessage{"member-b": pdSpecRawAt(t, "varroa-defaults", "1", "member-b.example.com")},
		conflicts:     1,
		afterConflict: newer,
	}
	deps.ConfigBrood = brood

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if len(brood.reads) != 2 {
		t.Fatalf("base reads = %v, want two reads of member-b", brood.reads)
	}
	if len(brood.updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(brood.updates))
	}
	if got := brood.updates[0].body.ResourceVersion; got != "1" {
		t.Errorf("first write resourceVersion = %q, want 1", got)
	}
	second := brood.updates[1].body
	if second.ResourceVersion != "2" {
		t.Errorf("second write resourceVersion = %q, want 2", second.ResourceVersion)
	}
	if second.Spec.RootDomain != "newer.example.com" {
		t.Errorf("second write rootDomain = %q, want the newer object's value", second.Spec.RootDomain)
	}
	if second.Spec.IngressAnnotations["kubernetes.io/ingress.class"] != "other" {
		t.Errorf("second write lost the other writer's annotation: %v", second.Spec.IngressAnnotations)
	}
	if second.Spec.StorageClass != "fast" {
		t.Errorf("second write storageClass = %q, want fast", second.Spec.StorageClass)
	}
}

// Every round conflicts, so the tool stops after the bounded attempts and asks
// the caller to run the call again.
func TestMCPUpdateProvisioningDefaults_ConflictOnEveryAttemptAsksForARetry(t *testing.T) {
	deps := adminDeps()
	newer := pdSpecRawAt(t, "varroa-defaults", "2", "newer.example.com")
	brood := &pdRoutedBrood{
		stored:        map[string]json.RawMessage{"member-b": pdSpecRawAt(t, "varroa-defaults", "1", "member-b.example.com")},
		conflicts:     5,
		afterConflict: newer,
	}
	deps.ConfigBrood = brood

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "cluster": "member-b", "storageClass": "fast",
	})
	if !tr.IsError {
		t.Fatalf("want a tool error after every attempt conflicts, got %v", tr.Content)
	}
	if len(brood.updates) != updateWriteAttempts {
		t.Errorf("got %d updates, want %d", len(brood.updates), updateWriteAttempts)
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "call update_provisioning_defaults again") {
		t.Errorf("error text = %v, want it to ask for a new call", tr.Content)
	}
}

// A conflict from the local operator starts a new round in the same way as a
// member conflict.
func TestMCPUpdateProvisioningDefaults_LocalConflictReadsAndMergesAgain(t *testing.T) {
	deps, fake, member, _ := memberDeps(t)
	seedLocalPD(t, fake)
	gvr, err := crdstore.GVRFor[v1alpha1.ProvisioningDefaults]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	fake.FailNext("update", gvr, apierrors.NewConflict(gvr.GroupResource(), "varroa-defaults", errors.New("stale resourceVersion")))

	tr := updatePDCall(t, deps, map[string]interface{}{
		"name": "varroa-defaults", "storageClass": "fast",
	})
	if tr.IsError {
		t.Fatalf("tool errored after a store conflict: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.ProvisioningDefaults](context.Background(), fake, "varroa-defaults", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.StorageClass != "fast" {
		t.Errorf("storageClass = %q, want fast", got.Spec.StorageClass)
	}
	if got.Spec.RootDomain != "local.example.com" {
		t.Errorf("rootDomain = %q, want the stored value kept", got.Spec.RootDomain)
	}
	if len(member.localWrites) != 2 || len(member.updates) != 0 {
		t.Errorf("local writes = %v, member writes = %v, want two local writes only", member.localWrites, member.updates)
	}
}

// vpRoutedBrood serves one JenkinsVersionProfile and records every read and
// write. A conflict first stores afterConflict, as a second writer would have.
type vpRoutedBrood struct {
	api.ConfigBrood
	stored        json.RawMessage
	getErr        error
	reads         int
	writes        int
	bodies        []json.RawMessage
	conflicts     int
	afterConflict json.RawMessage
}

func (b *vpRoutedBrood) GetVersionProfile(_ context.Context, _, _ string) (json.RawMessage, error) {
	b.reads++
	if b.getErr != nil {
		return nil, b.getErr
	}
	return b.stored, nil
}

func (b *vpRoutedBrood) UpdateVersionProfile(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	b.writes++
	b.bodies = append(b.bodies, obj)
	if b.conflicts > 0 {
		b.conflicts--
		b.stored = b.afterConflict
		return nil, &api.BroodError{Code: bus.CodeConflict, Msg: "changed or was deleted after it was read"}
	}
	return obj, nil
}

// vpProfileRaw returns a stored profile named 2.479 at resourceVersion rv.
func vpProfileRaw(t *testing.T, rv string, spec v1alpha1.JenkinsVersionProfileSpec) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "2.479", ResourceVersion: rv},
		Spec:       spec,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// vpBody decodes the name, resourceVersion and spec of a forwarded profile.
func vpBody(t *testing.T, raw json.RawMessage) (name, rv string, spec v1alpha1.JenkinsVersionProfileSpec) {
	t.Helper()
	var body v1alpha1.JenkinsVersionProfile
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body.Name, body.ResourceVersion, body.Spec
}

func updateVPCall(t *testing.T, deps *api.Dependencies, args map[string]interface{}) toolResult {
	t.Helper()
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "update_jenkins_version_profile",
		"arguments": args,
	}, mcpAdminClaims)
	return parseToolResult(t, resp.Result)
}

func TestUpdateJenkinsVersionProfile_SendsReadVersionAndMerges(t *testing.T) {
	deps := adminDeps()
	brood := &vpRoutedBrood{stored: vpProfileRaw(t, "5", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts"})}
	deps.ConfigBrood = brood

	tr := updateVPCall(t, deps, map[string]interface{}{
		"name": "2.479", "object": `{"spec":{"recommended":true}}`,
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if brood.writes != 1 {
		t.Fatalf("writes = %d, want 1", brood.writes)
	}
	_, rv, spec := vpBody(t, brood.bodies[0])
	if rv != "5" {
		t.Errorf("write resourceVersion = %q, want the read's 5", rv)
	}
	if spec.Channel != "lts" {
		t.Errorf("channel = %q, want the stored lts", spec.Channel)
	}
	if !spec.Recommended {
		t.Errorf("recommended = false, want the merged true")
	}
}

func TestUpdateJenkinsVersionProfile_MergesAgainAfterConflict(t *testing.T) {
	deps := adminDeps()
	brood := &vpRoutedBrood{
		stored:        vpProfileRaw(t, "5", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts"}),
		conflicts:     1,
		afterConflict: vpProfileRaw(t, "6", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts", EOL: "2030-01-01"}),
	}
	deps.ConfigBrood = brood

	tr := updateVPCall(t, deps, map[string]interface{}{
		"name": "2.479", "object": `{"spec":{"recommended":true}}`,
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if brood.reads != 2 || brood.writes != 2 {
		t.Fatalf("reads = %d, writes = %d, want 2 and 2", brood.reads, brood.writes)
	}
	_, rv, spec := vpBody(t, brood.bodies[1])
	if rv != "6" {
		t.Errorf("second write resourceVersion = %q, want the newer 6", rv)
	}
	if spec.EOL != "2030-01-01" {
		t.Errorf("second write lost the other writer's eol: %q", spec.EOL)
	}
	if !spec.Recommended {
		t.Errorf("second write recommended = false, want the merged true")
	}
}

// Every round conflicts, so the tool stops after the bounded attempts and asks
// the caller to run the call again.
func TestUpdateJenkinsVersionProfile_StopsAfterThreeConflicts(t *testing.T) {
	deps := adminDeps()
	brood := &vpRoutedBrood{
		stored:        vpProfileRaw(t, "5", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts"}),
		conflicts:     10,
		afterConflict: vpProfileRaw(t, "6", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts"}),
	}
	deps.ConfigBrood = brood

	tr := updateVPCall(t, deps, map[string]interface{}{"name": "2.479", "object": `{"spec":{"recommended":true}}`})
	if !tr.IsError {
		t.Fatalf("tool succeeded, want an error after the bounded attempts")
	}
	if !strings.Contains(toolText(tr), "changed during each of 3 attempts: call update_jenkins_version_profile again") {
		t.Errorf("error %q, want the retry request", toolText(tr))
	}
	if brood.writes != 3 {
		t.Errorf("writes = %d, want 3", brood.writes)
	}
}

func TestUpdateJenkinsVersionProfile_NotFoundWritesNothing(t *testing.T) {
	deps := adminDeps()
	brood := &vpRoutedBrood{getErr: &api.BroodError{Code: bus.CodeNotFound, Msg: "not found"}}
	deps.ConfigBrood = brood

	tr := updateVPCall(t, deps, map[string]interface{}{"name": "2.479", "object": `{"spec":{}}`})
	if !tr.IsError {
		t.Fatalf("tool succeeded, want a not-found error")
	}
	if !strings.HasPrefix(toolText(tr), "version profile not found") {
		t.Errorf("error %q, want the not-found prefix", toolText(tr))
	}
	if brood.writes != 0 {
		t.Errorf("writes = %d, want 0", brood.writes)
	}
}

func TestUpdateJenkinsVersionProfile_IgnoresObjectVersion(t *testing.T) {
	deps := adminDeps()
	brood := &vpRoutedBrood{stored: vpProfileRaw(t, "5", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts"})}
	deps.ConfigBrood = brood

	tr := updateVPCall(t, deps, map[string]interface{}{
		"name": "2.479", "object": `{"metadata":{"resourceVersion":"1"},"spec":{"recommended":true}}`,
	})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	_, rv, _ := vpBody(t, brood.bodies[0])
	if rv != "5" {
		t.Errorf("write resourceVersion = %q, want the read's 5, not the object's 1", rv)
	}
}

func TestMergeVersionProfileObject(t *testing.T) {
	base := vpProfileRaw(t, "5", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts", EOL: "2030-01-01"})

	merged, err := mergeVersionProfileObject(base, []byte(`{"spec":{"eol":null}}`), "2.479")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	_, rv, spec := vpBody(t, merged)
	if spec.EOL != "" {
		t.Errorf("eol = %q, want the null patch to remove it", spec.EOL)
	}
	if spec.Channel != "lts" || rv != "5" {
		t.Errorf("channel %q rv %q, want the stored lts and 5", spec.Channel, rv)
	}

	forced, err := mergeVersionProfileObject(base, []byte(`{"metadata":{"name":"other"}}`), "2.479")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if name, _, _ := vpBody(t, forced); name != "2.479" {
		t.Errorf("name = %q, want the addressed 2.479", name)
	}

	if _, err := mergeVersionProfileObject(base, []byte(`[1]`), "2.479"); err == nil {
		t.Errorf("a non-object patch merged without error")
	}
	if _, err := mergeVersionProfileObject(base, []byte(`null`), "2.479"); err == nil {
		t.Errorf("a null patch merged without error")
	}
}

// An update that names no object changes nothing, so it writes nothing.
func TestUpdateJenkinsVersionProfile_EmptyPatchWritesNothing(t *testing.T) {
	deps := adminDeps()
	brood := &vpRoutedBrood{stored: vpProfileRaw(t, "5", v1alpha1.JenkinsVersionProfileSpec{Channel: "lts"})}
	deps.ConfigBrood = brood

	tr := updateVPCall(t, deps, map[string]interface{}{"name": "2.479"})
	if tr.IsError {
		t.Fatalf("tool errored: %v", tr.Content)
	}
	if brood.writes != 0 {
		t.Errorf("writes = %d, want 0 for an empty patch", brood.writes)
	}
}
