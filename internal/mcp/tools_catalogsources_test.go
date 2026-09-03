package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// scopedDeveloperDeps builds api.Dependencies with an Authorizer whose caller
// is bound to a varroa:developer VarroaRoleBinding scoped to namespace
// "team-a", granting catalogsources create/update/delete.
func scopedDeveloperDeps() *api.Dependencies {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"catalogsources"}, Verbs: []string{"read", "create", "update", "delete"}},
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
	store := crdstore.NewFake()
	// Pre-seed a catalog source so update/delete tests can find it.
	crdstore.MustSeed(store, &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "team-a"},
		Spec:       v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/repo.git"},
	})
	return &api.Dependencies{
		Client:     &stubClient{},
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		Store:      store,
	}
}

var mcpScopedDevClaims = &auth.Claims{Subject: "dev-user"}

// requireAccessDenied asserts the tool result is specifically the authz
// denial, not some unrelated error (tool wiring, stub client, ...).
func requireAccessDenied(t *testing.T, tr toolResult) {
	t.Helper()
	if !tr.IsError {
		t.Fatal("expected access-denied error, got success")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "access denied") {
		t.Errorf("expected access-denied message, got: %v", tr.Content)
	}
}

func TestMCPCreateCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
			"repoURL":   "https://example.com/repo.git",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("create in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
			"repoURL":   "https://example.com/repo.git",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

func TestMCPUpdateCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("update in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

func TestMCPUpdateCatalogSource_FractionalSyncIntervalIsRejected(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace":           "team-a",
			"name":                "src",
			"syncIntervalSeconds": 60.5,
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("fractional syncIntervalSeconds must be rejected, got success: %v", tr.Content)
	}
	if !strings.Contains(fmt.Sprint(tr.Content), "whole number") {
		t.Errorf("error should name the fractional value problem, got: %v", tr.Content)
	}
}

func TestMCPDeleteCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "delete_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("delete in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "delete_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

func TestMCPSyncCatalogSource_ScopedDeveloper(t *testing.T) {
	deps := scopedDeveloperDeps()
	deps.ConfigBrood = &syncRecordingConfigBrood{}
	handler := NewHandler(deps)

	ownNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "sync_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, ownNs.Result)
	if tr.IsError {
		t.Errorf("sync in bound namespace should succeed, got error: %v", tr.Content)
	}

	otherNs := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "sync_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-b",
			"name":      "src",
		},
	}, mcpScopedDevClaims)
	requireAccessDenied(t, parseToolResult(t, otherNs.Result))
}

// syncRecordingConfigBrood records SyncCatalogSource calls. It embeds the
// interface so the other 32 methods need no implementation; any unexpected
// call nil-panics loudly.
type syncRecordingConfigBrood struct {
	api.ConfigBrood
	calls []string
}

func (s *syncRecordingConfigBrood) SyncCatalogSource(_ context.Context, cluster, ns, name string) error {
	s.calls = append(s.calls, cluster+"/"+ns+"/"+name)
	return nil
}

func TestMCPSyncCatalogSource_CallsConfigBrood(t *testing.T) {
	cb := &syncRecordingConfigBrood{}
	deps := adminDeps()
	deps.ConfigBrood = cb

	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "sync_catalog_source",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "src1"},
	}, mcpAdminClaims)

	if len(cb.calls) != 1 {
		t.Fatalf("SyncCatalogSource called %d times, want 1", len(cb.calls))
	}
	if cb.calls[0] != "core/team-a/src1" {
		t.Errorf("call = %q, want core/team-a/src1", cb.calls[0])
	}
	_ = resp
}

func TestMCPSyncCatalogSource_NilConfigBroodErrors(t *testing.T) {
	deps := adminDeps()
	deps.ConfigBrood = nil

	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "sync_catalog_source",
		"arguments": map[string]interface{}{"namespace": "team-a", "name": "src1"},
	}, mcpAdminClaims)

	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "config brood not configured") {
		t.Errorf("expected config-brood error, got %s", b)
	}
}

// catalogSourceDeps seeds one git-backed catalog source whose status carries
// the fields Task 5 projects (phase, itemCount, observedRevision).
func catalogSourceDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "cs1", Namespace: "ns"},
		Spec: v1alpha1.CatalogSourceSpec{
			RepoURL:  "https://example.com/catalog.git",
			Revision: "v1.0.0",
			Trusted:  true,
		},
		Status: v1alpha1.CatalogSourceStatus{
			Phase:            v1alpha1.CatalogSyncReady,
			ItemCount:        7,
			ObservedRevision: "v1.0.0",
		},
	})
	return &api.Dependencies{Client: &stubClient{}, Store: store}
}

// listCatalogSourcesResult calls list_catalog_sources and returns the decoded
// structuredContent.
func listCatalogSourcesResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(catalogSourceDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_catalog_sources",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_catalog_sources returned error: %v", tr.Content)
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

// The projection exists to drop spec internals (path, secretRef,
// syncIntervalSeconds) and status.conditions. Identity, where the source points,
// and the item count must survive.
func TestSummarizeCatalogSource_ProjectsPopulatedFieldsOnly(t *testing.T) {
	got := summarizeCatalogSource(&v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "cs1", Namespace: "varroa"},
		Spec: v1alpha1.CatalogSourceSpec{
			RepoURL:             "https://example.com/catalog.git",
			Revision:            "v1.0.0",
			Path:                "some/catalog/path",
			SyncIntervalSeconds: 300,
			SecretRef:           "git-creds",
			Trusted:             true,
		},
		Status: v1alpha1.CatalogSourceStatus{
			Phase:            v1alpha1.CatalogSyncReady,
			ObservedRevision: "v1.0.0",
			ItemCount:        42,
			Message:          "synced cleanly",
			Conditions: []v1alpha1.TemplateCatalogCondition{
				{Type: "Synced", Status: metav1.ConditionTrue},
			},
		},
	})
	if got.ItemCount != 42 {
		t.Errorf("ItemCount = %d, want 42", got.ItemCount)
	}
	if !got.Trusted {
		t.Error("Trusted = false, want true")
	}
	if got.Phase != "Ready" {
		t.Errorf("Phase = %q, want Ready (CatalogSyncPhase must be string-cast)", got.Phase)
	}
	b, _ := json.Marshal(got)
	// The payload the projection exists to drop.
	for _, gone := range []string{"git-creds", "syncIntervalSeconds", "some/catalog/path", "conditions", "synced cleanly"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("summary still carries %q: %s", gone, b)
		}
	}
	// The fields the summary claims must survive.
	for _, want := range []string{"cs1", "varroa", "https://example.com/catalog.git", "v1.0.0"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("summary dropped %s: %s", want, b)
		}
	}
}

// Before the first sync LastSyncTime is nil; the summarizer must neither panic
// nor emit a zero/epoch timestamp, and omitempty must drop the key entirely.
func TestSummarizeCatalogSource_NilLastSyncTime(t *testing.T) {
	cs := &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "cs1", Namespace: "varroa"},
		Spec:       v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/catalog.git"},
		Status:     v1alpha1.CatalogSourceStatus{Phase: v1alpha1.CatalogSyncPending},
	}
	if cs.Status.LastSyncTime != nil {
		t.Fatal("test precondition: LastSyncTime must start nil")
	}
	got := summarizeCatalogSource(cs)
	if got.LastSyncTime != "" {
		t.Errorf("LastSyncTime = %q, want empty before first sync", got.LastSyncTime)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "lastSyncTime") {
		t.Errorf("nil LastSyncTime must be omitted by omitempty: %s", b)
	}
}

// LastSyncTime is rendered RFC3339 in UTC, not the local offset — the same
// format as createdAt in the controller summary.
func TestSummarizeCatalogSource_FormatsLastSyncTimeRFC3339UTC(t *testing.T) {
	when := metav1.NewTime(time.Date(2026, 8, 3, 14, 30, 45, 0, time.FixedZone("CET", 3600)))
	got := summarizeCatalogSource(&v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "cs1", Namespace: "varroa"},
		Spec:       v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/catalog.git"},
		Status:     v1alpha1.CatalogSourceStatus{LastSyncTime: &when},
	})
	// 14:30:45+01:00 is 13:30:45Z.
	if got.LastSyncTime != "2026-08-03T13:30:45Z" {
		t.Errorf("LastSyncTime = %q, want 2026-08-03T13:30:45Z", got.LastSyncTime)
	}
}

// A source may be OCI-backed instead of git-backed (the CRD allows exactly one
// of repoURL/ociRef). Project whichever is set; the empty alternative must not
// leak a misleading field name.
func TestSummarizeCatalogSource_ProjectsOCIRefWhenSet(t *testing.T) {
	got := summarizeCatalogSource(&v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "cs-oci", Namespace: "varroa"},
		Spec:       v1alpha1.CatalogSourceSpec{OCIRef: "ghcr.io/example/catalog:latest"},
		Status:     v1alpha1.CatalogSourceStatus{ItemCount: 3},
	})
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), "ghcr.io/example/catalog:latest") {
		t.Errorf("summary dropped ociRef: %s", b)
	}
	if strings.Contains(string(b), "repoURL") {
		t.Errorf("repoURL must be omitted when only ociRef is set: %s", b)
	}
}

// The default list result must be the summary projection: identity plus the
// projected fields, never raw CR internals.
func TestListCatalogSources_DefaultsToSummary(t *testing.T) {
	item := firstItem(t, listCatalogSourcesResult(t, map[string]interface{}{"namespace": "ns"}))

	allowed := map[string]bool{
		"name": true, "namespace": true, "repoURL": true, "ociRef": true,
		"revision": true, "phase": true, "itemCount": true, "trusted": true,
		"lastSyncTime": true, "observedRevision": true,
	}
	for k := range item {
		if !allowed[k] {
			t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
		}
	}
	for _, k := range []string{"name", "namespace", "repoURL", "phase", "observedRevision"} {
		if item[k] == nil || item[k] == "" {
			t.Errorf("summary missing %q: %v", k, item)
		}
	}
	// JSON-decoded numbers come back as float64.
	if item["itemCount"] != float64(7) {
		t.Errorf("itemCount = %v (%T), want 7", item["itemCount"], item["itemCount"])
	}
	if item["trusted"] != true {
		t.Errorf("trusted = %v, want true", item["trusted"])
	}
	for _, k := range []string{"metadata", "spec", "status"} {
		if _, present := item[k]; present {
			t.Errorf("summary must not contain %q", k)
		}
	}
}

// verbose is an escape hatch from the projection: the full CR, internals intact.
func TestListCatalogSources_VerboseReturnsFullCR(t *testing.T) {
	item := firstItem(t, listCatalogSourcesResult(t, map[string]interface{}{
		"namespace": "ns", "verbose": true,
	}))

	if _, ok := item["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	if _, ok := item["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	b, _ := json.Marshal(item)
	if !strings.Contains(string(b), "https://example.com/catalog.git") {
		t.Errorf("verbose result must keep the full spec: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListCatalogSources_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(catalogSourceListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_catalog_sources.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_catalog_sources.json")
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
			sc := listCatalogSourcesResult(t, tc.args)
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
// summary branch and the open object branch.
func TestCatalogSourceListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(catalogSourceListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(catalogSourceListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}

// An OCI-backed source must be reachable from MCP: ociRef, trusted and
// syncIntervalSeconds are all spec fields REST accepts, and a tool that cannot
// name them leaves an agent unable to create anything but a plain git source.
func TestCreateCatalogSource_OCI(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace":           "team-a",
			"name":                "team-catalog",
			"ociRef":              "ghcr.io/example/team-catalog:v1",
			"trusted":             true,
			"syncIntervalSeconds": float64(120),
			"secretRef":           "ghcr-pull",
		},
	}, mcpScopedDevClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("create returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "team-catalog", "team-a")
	if err != nil {
		t.Fatalf("get created source: %v", err)
	}
	want := v1alpha1.CatalogSourceSpec{
		OCIRef:              "ghcr.io/example/team-catalog:v1",
		Trusted:             true,
		SyncIntervalSeconds: 120,
		SecretRef:           "ghcr-pull",
	}
	if !reflect.DeepEqual(got.Spec, want) {
		t.Fatalf("spec = %+v, want %+v", got.Spec, want)
	}
}

// The CRD's CEL rule rejects both shapes, but an apply rejection reaches an
// agent as an opaque admission error. Catch it in the tool and write nothing.
func TestCreateCatalogSource_RequiresExactlyOneSource(t *testing.T) {
	for name, tc := range map[string]struct {
		args    map[string]interface{}
		wantMsg string
	}{
		"neither source": {
			args:    map[string]interface{}{"namespace": "team-a", "name": "no-source"},
			wantMsg: "exactly one of repoURL or ociRef",
		},
		"both sources": {
			args: map[string]interface{}{
				"namespace": "team-a", "name": "no-source",
				"repoURL": "https://example.com/repo.git",
				"ociRef":  "ghcr.io/example/team-catalog:v1",
			},
			wantMsg: "only one of repoURL or ociRef",
		},
	} {
		t.Run(name, func(t *testing.T) {
			deps := scopedDeveloperDeps()
			handler := NewHandler(deps)
			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      "create_catalog_source",
				"arguments": tc.args,
			}, mcpScopedDevClaims)

			tr := parseToolResult(t, resp.Result)
			if !tr.IsError {
				t.Fatal("expected a validation error")
			}
			if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, tc.wantMsg) {
				t.Errorf("error = %v, want it to contain %q", tr.Content, tc.wantMsg)
			}
			if _, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "no-source", "team-a"); err == nil {
				t.Error("a rejected create must write nothing")
			}
		})
	}
}

// With WithInputSchemaValidation an undeclared argument never reaches the
// handler, so a field the handler applies but the schema omits is unreachable.
// repoURL must also stop being required, or an OCI-only create cannot be made.
func TestCatalogSourceTools_DeclareOCIArgs(t *testing.T) {
	wantTypes := map[string]string{
		"ociRef":              "string",
		"trusted":             "boolean",
		"syncIntervalSeconds": "number",
	}
	seen := 0
	for _, tool := range liveTools(t) {
		if tool.Name != "create_catalog_source" && tool.Name != "update_catalog_source" {
			continue
		}
		seen++
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("unmarshal inputSchema for %s: %v", tool.Name, err)
		}
		for arg, wantType := range wantTypes {
			prop, ok := schema.Properties[arg].(map[string]any)
			if !ok {
				t.Errorf("%s does not declare %s", tool.Name, arg)
				continue
			}
			if prop["type"] != wantType {
				t.Errorf("%s.%s type = %v, want %s", tool.Name, arg, prop["type"], wantType)
			}
		}
		if tool.Name == "create_catalog_source" {
			for _, r := range schema.Required {
				if r == "repoURL" {
					t.Error("create_catalog_source must not require repoURL: an OCI source has none")
				}
			}
		}
	}
	if seen != 2 {
		t.Fatalf("found %d catalog source write tools, want 2", seen)
	}
}

// Switching kinds must clear the abandoned kind's fields, or the stored spec
// violates the CRD's at-most-one rule and the next apply is rejected.
func TestUpdateCatalogSource_SwitchesGitToOCI(t *testing.T) {
	deps := scopedDeveloperDeps()
	crdstore.MustSeed(deps.Store.(*crdstore.Fake), &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "team-a"},
		Spec: v1alpha1.CatalogSourceSpec{
			RepoURL:   "https://example.com/repo.git",
			Revision:  "main",
			Path:      "catalog",
			SecretRef: "git-creds",
			Trusted:   true,
		},
	})
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
			"ociRef":    "ghcr.io/example/team-catalog:v1",
		},
	}, mcpScopedDevClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "src", "team-a")
	if err != nil {
		t.Fatalf("get updated source: %v", err)
	}
	want := v1alpha1.CatalogSourceSpec{
		OCIRef:    "ghcr.io/example/team-catalog:v1",
		Path:      "catalog",
		SecretRef: "git-creds",
		Trusted:   true,
	}
	if !reflect.DeepEqual(got.Spec, want) {
		t.Fatalf("spec = %+v, want %+v", got.Spec, want)
	}
}

func TestUpdateCatalogSource_RejectsBothSources(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
			"repoURL":   "https://example.com/other.git",
			"ociRef":    "ghcr.io/example/team-catalog:v1",
		},
	}, mcpScopedDevClaims)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("expected a validation error")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "only one of repoURL or ociRef") {
		t.Errorf("error = %v, want it to name the one-source rule", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "src", "team-a")
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if got.Spec.RepoURL != "https://example.com/repo.git" || got.Spec.OCIRef != "" {
		t.Errorf("spec = %+v, want the seeded git source untouched", got.Spec)
	}
}

// trusted and syncIntervalSeconds follow key presence, not truthiness: an
// omitted flag preserves the stored value and an explicit false clears it.
// Reading the value alone would make trusted a one-way door.
func TestUpdateCatalogSource_TrustedAndIntervalByPresence(t *testing.T) {
	newDeps := func(t *testing.T) *api.Dependencies {
		t.Helper()
		deps := scopedDeveloperDeps()
		crdstore.MustSeed(deps.Store.(*crdstore.Fake), &v1alpha1.CatalogSource{
			ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "team-a"},
			Spec: v1alpha1.CatalogSourceSpec{
				RepoURL:             "https://example.com/repo.git",
				Trusted:             true,
				SyncIntervalSeconds: 600,
			},
		})
		return deps
	}

	t.Run("omitted preserves both", func(t *testing.T) {
		deps := newDeps(t)
		resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
			"name": "update_catalog_source",
			"arguments": map[string]interface{}{
				"namespace": "team-a", "name": "src", "path": "catalog",
			},
		}, mcpScopedDevClaims)
		if tr := parseToolResult(t, resp.Result); tr.IsError {
			t.Fatalf("update returned error: %v", tr.Content)
		}
		got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "src", "team-a")
		if err != nil {
			t.Fatalf("get source: %v", err)
		}
		if !got.Spec.Trusted || got.Spec.SyncIntervalSeconds != 600 {
			t.Errorf("spec = %+v, want trusted and interval preserved", got.Spec)
		}
		if got.Spec.Path != "catalog" {
			t.Errorf("path = %q, want catalog", got.Spec.Path)
		}
	})

	t.Run("explicit false clears trusted", func(t *testing.T) {
		deps := newDeps(t)
		resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
			"name": "update_catalog_source",
			"arguments": map[string]interface{}{
				"namespace": "team-a", "name": "src", "trusted": false,
			},
		}, mcpScopedDevClaims)
		if tr := parseToolResult(t, resp.Result); tr.IsError {
			t.Fatalf("update returned error: %v", tr.Content)
		}
		got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "src", "team-a")
		if err != nil {
			t.Fatalf("get source: %v", err)
		}
		if got.Spec.Trusted {
			t.Error("trusted: false must clear the stored flag")
		}
		if got.Spec.SyncIntervalSeconds != 600 {
			t.Errorf("syncIntervalSeconds = %d, want 600 preserved", got.Spec.SyncIntervalSeconds)
		}
	})
}

func TestMCPCreateCatalogSource_ReservedNameRejected(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      v1alpha1.UpdateCenterCatalogSourceName,
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("creating the reserved source must be rejected, got success: %v", tr.Content)
	}
	if !strings.Contains(fmt.Sprint(tr.Content), "reconciled by the operator") {
		t.Errorf("error should say the operator owns the reserved source, got: %v", tr.Content)
	}
}

func TestMCPUpdateCatalogSource_OCIRefClearsRevisionPassedAlongside(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "src",
			"ociRef":    "ghcr.io/acme/catalog:1.0.0",
			"revision":  "main",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update failed: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "src", "team-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.OCIRef != "ghcr.io/acme/catalog:1.0.0" || got.Spec.RepoURL != "" || got.Spec.Revision != "" {
		t.Errorf("switching to ociRef must clear git fields even when revision rides along: %+v", got.Spec)
	}
}

func TestMCPUpdateCatalogSource_ReservedNameRejected(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "update_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      v1alpha1.UpdateCenterCatalogSourceName,
			"trusted":   true,
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError || !strings.Contains(fmt.Sprint(tr.Content), "cannot be edited") {
		t.Fatalf("editing the reserved source must be rejected, got: %v", tr.Content)
	}
}

func TestMCPCreateCatalogSource_OCIRefDropsRevision(t *testing.T) {
	deps := scopedDeveloperDeps()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "create_catalog_source",
		"arguments": map[string]interface{}{
			"namespace": "team-a",
			"name":      "oci-src",
			"ociRef":    "ghcr.io/acme/catalog:1.0.0",
			"revision":  "main",
		},
	}, mcpScopedDevClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("create failed: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), deps.Store, "oci-src", "team-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Revision != "" {
		t.Errorf("revision must not ride along with an OCI source on create: %+v", got.Spec)
	}
}
