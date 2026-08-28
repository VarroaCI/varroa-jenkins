package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// These are the assertions #467 lacked. The bug was not that the strip logic
// was wrong — there was no strip logic on this surface at all — and a green
// suite never noticed, because nothing asserted on what a tool result must NOT
// contain. Every check below is a negative one, stated at the MCP tool boundary
// rather than on the sanitizer in isolation.

const (
	testWakeToken    = "wake-token-that-must-never-ship"
	testPasswordHash = "$2a$10$hash-that-must-never-ship"
	testPassword     = "plaintext-that-must-never-ship"
)

// adminDeps builds Dependencies whose caller is a global admin, seeded with a
// hibernated Controller and a User carrying credentials.
func adminDeps() *api.Dependencies {
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

	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hibernated",
			Namespace:       "ns",
			ResourceVersion: "999",
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "varroa-ui"}},
		},
		Spec: v1alpha1.ControllerSpec{Version: "2.516.3"},
		Status: v1alpha1.ControllerStatus{
			Phase:     "Hibernated",
			WakeToken: testWakeToken,
		},
	})
	crdstore.MustSeed(store, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "ns"},
		Spec:       v1alpha1.UserSpec{Password: testPassword},
		Status: v1alpha1.UserStatus{
			Credentials: &v1alpha1.UserCredentials{PasswordHash: testPasswordHash},
		},
	})

	return &api.Dependencies{
		Client:     &stubClient{},
		Authorizer: api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false),
		Store:      store,
	}
}

var mcpAdminClaims = &auth.Claims{Subject: "admin-user"}

// callToolRaw invokes a tool and returns its whole result serialized, so
// assertions can search the entire payload — structuredContent and text content
// alike. A leak that appears in only one of the two is still a leak.
func callToolRaw(t *testing.T, deps *api.Dependencies, tool string, args map[string]interface{}) string {
	t.Helper()
	handler := NewHandler(deps)
	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      tool,
		"arguments": args,
	}, mcpAdminClaims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("%s returned error: %v", tool, tr.Content)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal %s result: %v", tool, err)
	}
	return string(b)
}

func requireAbsent(t *testing.T, tool, payload string, forbidden ...string) {
	t.Helper()
	for _, f := range forbidden {
		if strings.Contains(payload, f) {
			t.Errorf("%s result must not contain %q\nfull result:\n%s", tool, f, payload)
		}
	}
}

func TestMCPGetController_DoesNotLeakWakeToken(t *testing.T) {
	got := callToolRaw(t, adminDeps(), "get_controller", map[string]interface{}{
		"name":      "hibernated",
		"namespace": "ns",
	})
	requireAbsent(t, "get_controller", got, "wakeToken", testWakeToken, "managedFields")
	if !strings.Contains(got, "hibernated") {
		t.Errorf("get_controller returned nothing useful:\n%s", got)
	}
}

func TestMCPListControllers_DoesNotLeakWakeToken(t *testing.T) {
	got := callToolRaw(t, adminDeps(), "list_controllers", map[string]interface{}{
		"namespace": "ns",
	})
	requireAbsent(t, "list_controllers", got, "wakeToken", testWakeToken, "managedFields")
}

// The leak class is wider than the one field #467 names: closing it per-domain
// would have left this path open.
func TestMCPListUsers_DoesNotLeakCredentials(t *testing.T) {
	got := callToolRaw(t, adminDeps(), "list_users", map[string]interface{}{
		"namespace": "ns",
	})
	requireAbsent(t, "list_users", got, "passwordHash", testPasswordHash, testPassword)
	if !strings.Contains(got, "alice") {
		t.Errorf("list_users returned nothing useful:\n%s", got)
	}
}

func TestMCPGetUser_DoesNotLeakCredentials(t *testing.T) {
	got := callToolRaw(t, adminDeps(), "get_user", map[string]interface{}{
		"name":      "alice",
		"namespace": "ns",
	})
	requireAbsent(t, "get_user", got, "passwordHash", testPasswordHash, testPassword)
}
