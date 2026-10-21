package mcp

import (
	"context"
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
// tool error rather than dereference nil and panic.
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

// deleteUserDeps returns deps over store with an admin Authorizer, the same
// shape userDeps uses. The admin binding lives in the resolver, not the store.
func deleteUserDeps(store crdstore.Backend) *api.Dependencies {
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

// TestDeleteUser_StripsBindings pins that delete_user runs the same
// deprovision step as the REST route: bindings naming the user lose that
// subject, and a binding left empty is deleted.
func TestDeleteUser_StripsBindings(t *testing.T) {
	store := crdstore.NewFake()
	subj := func(kind, name string) v1alpha1.SubjectRef {
		return v1alpha1.SubjectRef{Kind: kind, Name: name}
	}
	crdstore.MustSeed(store,
		&v1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "alice",
				Namespace: "ns",
				Labels:    map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
				Annotations: map[string]string{
					v1alpha1.AnnotationOIDCSubject:           "subj-1",
					v1alpha1.AnnotationOIDCPreferredUsername: "alice-preferred",
				},
			},
			Spec: v1alpha1.UserSpec{Email: "alice@example.com"},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-sub"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "subj-1")}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-shared"},
			Spec: v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{
				subj("User", "alice@example.com"), subj("Group", "devs"),
			}},
		},
		&v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "jrb-pu"},
			Spec:       v1alpha1.JenkinsRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice-preferred")}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-other"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "bob")}},
		},
	)
	ctx := context.Background()

	resp := mcpRequest(t, NewHandler(deleteUserDeps(store)), "tools/call", map[string]interface{}{
		"name":      "delete_user",
		"arguments": map[string]interface{}{"namespace": "ns", "name": "alice"},
	}, mcpAdminClaims)
	if tr := parseToolResult(t, resp.Result); tr.IsError {
		t.Fatalf("delete_user returned error: %v", tr.Content)
	}

	if _, err := crdstore.Get[v1alpha1.User](ctx, store, "alice", "ns"); err == nil {
		t.Errorf("User alice still exists after delete_user")
	}
	if _, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-sub", ""); err == nil {
		t.Errorf("vrb-sub named only the user and should have been deleted")
	}
	if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, store, "jrb-pu", ""); err == nil {
		t.Errorf("jrb-pu named only the preferred username and should have been deleted")
	}
	shared, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-shared", "")
	if err != nil {
		t.Fatalf("vrb-shared: %v", err)
	}
	if len(shared.Spec.Subjects) != 1 || shared.Spec.Subjects[0] != subj("Group", "devs") {
		t.Errorf("vrb-shared subjects = %+v, want only Group/devs", shared.Spec.Subjects)
	}
	other, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-other", "")
	if err != nil {
		t.Fatalf("vrb-other: %v", err)
	}
	if len(other.Spec.Subjects) != 1 || other.Spec.Subjects[0] != subj("User", "bob") {
		t.Errorf("vrb-other subjects = %+v, want only User/bob", other.Spec.Subjects)
	}
}

// TestDeleteUser_MissingUserIsError pins the tool error for an absent user,
// the same outcome the REST route gives as 404.
func TestDeleteUser_MissingUserIsError(t *testing.T) {
	resp := mcpRequest(t, NewHandler(deleteUserDeps(crdstore.NewFake())), "tools/call", map[string]interface{}{
		"name":      "delete_user",
		"arguments": map[string]interface{}{"namespace": "ns", "name": "ghost"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("delete_user on a missing user must return a tool error, got success: %v", tr.Content)
	}
	if got := toolText(tr); !strings.Contains(got, "user not found") {
		t.Errorf("error text = %q, want it to contain %q", got, "user not found")
	}
}

// TestDeleteUser_EmitsUserDeleted pins that delete_user publishes the shared
// user.deleted event built by api.UserActivityEvent.
func TestDeleteUser_EmitsUserDeleted(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "ns"},
		Spec:       v1alpha1.UserSpec{Email: "alice@example.com"},
	})
	sink := &recordingSink{}
	deps := deleteUserDeps(store)
	deps.ActivityPublisher = sink

	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "delete_user",
		"arguments": map[string]interface{}{"namespace": "ns", "name": "alice"},
	}, mcpAdminClaims)
	if tr := parseToolResult(t, resp.Result); tr.IsError {
		t.Fatalf("delete_user returned error: %v", tr.Content)
	}

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(sink.events), sink.events)
	}
	got := sink.events[0]
	want := api.UserActivityEvent(api.UserDeleted, "ns", "alice")
	if got.Type != want.Type || got.Message != want.Message || got.Namespace != want.Namespace {
		t.Errorf("event = %+v, want type/message/namespace of %+v", got, want)
	}
	if got.Source != mcpSource {
		t.Errorf("Source = %q, want %q", got.Source, mcpSource)
	}
}

// TestUpdateUser_ConcurrentWriteKeepsFormerIdentifiers pins that a write
// between the read and the update of update_user survives it.
func TestUpdateUser_ConcurrentWriteKeepsFormerIdentifiers(t *testing.T) {
	ctx := context.Background()
	deps := userDeps(t)
	st := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "ns", ResourceVersion: "1"},
	})
	gvr, _ := crdstore.GVRFor[v1alpha1.User]()
	fired := false
	st.SetAfterGet(gvr, func(ns, name string) {
		if fired {
			return
		}
		fired = true
		if _, err := st.PatchObjectMetaAt(ctx, gvr, ns, name, "1", map[string]any{
			"annotations": map[string]any{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
		}); err != nil {
			t.Errorf("concurrent patch: %v", err)
		}
	})

	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      "update_user",
		"arguments": map[string]interface{}{"namespace": "ns", "name": "u1", "email": "new@example.com"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("update_user returned a tool error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.User](ctx, st, "u1", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] != `["bob"]` {
		t.Errorf("former identifiers annotation = %q, want the concurrent value", got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers])
	}
	if got.Spec.Email != "new@example.com" {
		t.Errorf("email = %q, want new@example.com", got.Spec.Email)
	}
}

// TestUpdateUser_NotFound pins that update_user on a missing user answers a
// tool error that starts with "user not found".
func TestUpdateUser_NotFound(t *testing.T) {
	resp := mcpRequest(t, NewHandler(deleteUserDeps(crdstore.NewFake())), "tools/call", map[string]interface{}{
		"name":      "update_user",
		"arguments": map[string]interface{}{"namespace": "ns", "name": "ghost", "email": "x@example.com"},
	}, mcpAdminClaims)
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("update_user on a missing user must return a tool error, got success: %v", tr.Content)
	}
	if got := toolText(tr); !strings.HasPrefix(got, "user not found") {
		t.Errorf("error text = %q, want prefix %q", got, "user not found")
	}
}
