package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

const (
	userPasswordSentinel     = "sentinel-plaintext-password"
	userPasswordHashSentinel = "sentinel-hashed-password"
)

// userDeps seeds two users: one fully populated — including the two credential
// fields the summary must never carry — and one who has never logged in (nil
// Status.LastLogin).
func userDeps(t *testing.T) *api.Dependencies {
	t.Helper()
	store := crdstore.NewFake()
	lastLogin := metav1.NewTime(time.Date(2026, 8, 1, 12, 30, 45, 0, time.UTC))
	crdstore.MustSeed(store, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "ns"},
		Spec: v1alpha1.UserSpec{
			Email:       "u1@example.com",
			DisplayName: "User One",
			Password:    userPasswordSentinel,
		},
		Status: v1alpha1.UserStatus{
			LastLogin: &lastLogin,
			Credentials: &v1alpha1.UserCredentials{
				PasswordHash: userPasswordHashSentinel,
			},
		},
	})
	crdstore.MustSeed(store, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "u2", Namespace: "ns"},
		Spec:       v1alpha1.UserSpec{Email: "u2@example.com"},
		// Status.LastLogin deliberately left nil: this user has never logged in.
	})
	// list_users is admin-gated, so the deps need a real Authorizer: the handler
	// calls IsAdmin unconditionally, and an Authorizer whose resolver is unset
	// panics rather than denying. Mirrors adminDeps in tools_leak_test.go.
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

// listUsersResult calls list_users and returns the decoded structuredContent.
func listUsersResult(t *testing.T, args map[string]interface{}) map[string]any {
	t.Helper()
	handler := NewHandler(userDeps(t))
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_users",
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_users returned error: %v", tr.Content)
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

func findItem(t *testing.T, sc map[string]any, name string) map[string]any {
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
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("no item named %q in result: %v", name, sc)
	return nil
}

// The credential fields are the reason Task 8 exists: spec.password is
// write-only and status.credentials.passwordHash is the hash, and neither
// belongs in a listing. Sanitization already strips them at the resultJSON
// chokepoint, so the summary must not project them either — and the test pins
// both sentinels absent so the belt survives even if a future cleanup moves
// sanitization.
func TestSummarizeUser_DropsCredentialsKeepsIdentity(t *testing.T) {
	lastLogin := metav1.NewTime(time.Date(2026, 8, 1, 12, 30, 45, 0, time.UTC))
	got := summarizeUser(&v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "varroa"},
		Spec: v1alpha1.UserSpec{
			Email:       "u1@example.com",
			DisplayName: "User One",
			Password:    userPasswordSentinel,
		},
		Status: v1alpha1.UserStatus{
			LastLogin: &lastLogin,
			Credentials: &v1alpha1.UserCredentials{
				PasswordHash: userPasswordHashSentinel,
			},
		},
	})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), userPasswordSentinel) {
		t.Errorf("summary carries spec.password: %s", b)
	}
	if strings.Contains(string(b), userPasswordHashSentinel) {
		t.Errorf("summary carries status.credentials.passwordHash: %s", b)
	}
	for _, want := range []string{"u1", "varroa", "User One", "u1@example.com", "2026-08-01T12:30:45Z"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("summary dropped %s: %s", want, b)
		}
	}
}

// Status.LastLogin is a *metav1.Time and nil for a user who has never logged
// in. A nil guard must turn that into an omitted field, not a JSON null or a
// zero time.
func TestSummarizeUser_NeverLoggedInOmitsLastLogin(t *testing.T) {
	got := summarizeUser(&v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "u2", Namespace: "varroa"},
		Spec:       v1alpha1.UserSpec{Email: "u2@example.com"},
		// Status.LastLogin nil.
	})
	if got.LastLogin != "" {
		t.Errorf("LastLogin = %q, want empty for a user who never logged in", got.LastLogin)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "lastLogin") {
		t.Errorf("never-logged-in summary must omit lastLogin: %s", b)
	}
}

// The default list result must be the summary projection: identity fields
// plus the last login, never the credential fields or raw CR internals.
func TestListUsers_DefaultsToSummary(t *testing.T) {
	sc := listUsersResult(t, map[string]interface{}{"namespace": "ns"})
	// Select by name rather than by position: the store returns users in
	// nondeterministic order, so asserting against items[0] passes in isolation
	// and fails intermittently in a full package run.
	item := itemNamed(t, sc, "u1")

	allowed := map[string]bool{
		"name": true, "namespace": true, "displayName": true,
		"email": true, "lastLogin": true,
	}
	for k := range item {
		if !allowed[k] {
			t.Errorf("summary contains unexpected key %q; summary must not carry CR internals", k)
		}
	}
	for _, k := range []string{"name", "namespace", "displayName", "email"} {
		if item[k] == nil || item[k] == "" {
			t.Errorf("summary missing %q: %v", k, item)
		}
	}
	if item["lastLogin"] != "2026-08-01T12:30:45Z" {
		t.Errorf("lastLogin = %v, want RFC3339 UTC 2026-08-01T12:30:45Z", item["lastLogin"])
	}
	if sc["count"] != float64(2) {
		t.Errorf("count = %v, want 2", sc["count"])
	}
	// The credential fields must be absent from the whole default payload, not
	// just from the struct: belt-and-braces over the summary projection.
	b, _ := json.Marshal(sc)
	if strings.Contains(string(b), userPasswordSentinel) {
		t.Errorf("list payload carries spec.password: %s", b)
	}
	if strings.Contains(string(b), userPasswordHashSentinel) {
		t.Errorf("list payload carries status.credentials.passwordHash: %s", b)
	}
	for _, k := range []string{"metadata", "spec", "status"} {
		if _, present := item[k]; present {
			t.Errorf("summary must not contain %q", k)
		}
	}
}

// A user who never logged in surfaces as a summary without lastLogin, so a
// caller can spot dormant accounts at a glance.
func TestListUsers_NeverLoggedInOmitsLastLogin(t *testing.T) {
	item := findItem(t, listUsersResult(t, map[string]interface{}{"namespace": "ns"}), "u2")
	if _, present := item["lastLogin"]; present {
		t.Errorf("never-logged-in user must omit lastLogin, got %v", item)
	}
	if item["email"] != "u2@example.com" {
		t.Errorf("email = %v, want u2@example.com", item["email"])
	}
}

// verbose is an escape hatch from the projection: the full CR. The chokepoint
// still runs, so even the full object must not leak the sentinel credentials.
func TestListUsers_VerboseReturnsFullCR(t *testing.T) {
	sc := listUsersResult(t, map[string]interface{}{"namespace": "ns", "verbose": true})
	item := firstItem(t, sc)

	if _, ok := item["metadata"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	if _, ok := item["spec"]; !ok {
		t.Errorf("verbose must return full resources, got %v", item)
	}
	b, _ := json.Marshal(sc)
	if strings.Contains(string(b), userPasswordSentinel) {
		t.Errorf("verbose payload still leaks spec.password; SanitizeObject should strip it: %s", b)
	}
	if strings.Contains(string(b), userPasswordHashSentinel) {
		t.Errorf("verbose payload still leaks passwordHash; SanitizeObject should strip it: %s", b)
	}
}

// A declared outputSchema that cannot accept the tool's own output is worse
// than none: strict clients reject the result outright. Validate both modes
// against the real declared schema.
func TestListUsers_OutputMatchesDeclaredSchema(t *testing.T) {
	var schemaDoc any
	if err := json.Unmarshal(userListOutputSchema, &schemaDoc); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("list_users.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := c.Compile("list_users.json")
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
			sc := listUsersResult(t, tc.args)
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
func TestUserListSchema_UsesAnyOfNotOneOf(t *testing.T) {
	if strings.Contains(string(userListOutputSchema), `"oneOf"`) {
		t.Error("item schema must use anyOf: a summary matches both branches, " +
			"and oneOf requires exactly one match, so it would reject valid output")
	}
	if !strings.Contains(string(userListOutputSchema), `"anyOf"`) {
		t.Error("item schema must declare anyOf over the summary and full-CR shapes")
	}
}

// Every user handler calls deps.Authorizer.IsAdmin unconditionally. On an
// incompletely-wired server Authorizer is nil, and each handler must answer a
// tool error rather than dereference nil and panic (#472).
func TestUserTools_AuthorizerNilReturnsError(t *testing.T) {
	deps := &api.Dependencies{Client: &stubClient{}, Store: crdstore.NewFake()}
	handler := NewHandler(deps)
	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"list_users", map[string]interface{}{}},
		{"create_user", map[string]interface{}{"namespace": "ns", "name": "u1"}},
		{"update_user", map[string]interface{}{"namespace": "ns", "name": "u1"}},
		{"delete_user", map[string]interface{}{"namespace": "ns", "name": "u1"}},
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
