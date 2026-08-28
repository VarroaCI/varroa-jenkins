package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func TestHandleDeployableNamespaces_GET(t *testing.T) {
	// Create a minimal RBAC setup so the authorizer returns a non-empty result.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide → unrestricted
			},
		},
	}

	resolver := testResolver(roles, bindings)
	authorizer := NewAuthorizer(resolver, false)

	client := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
		provisioning: &v1alpha1.ProvisioningDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
			Spec: v1alpha1.ProvisioningDefaultsSpec{
				DefaultNamespace: "team-a",
				Namespaces:       []string{"team-a", "team-b"},
			},
		},
	}

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromProvisioning(client),
		Authorizer:        authorizer,
		ManagedNamespaces: "", // cluster-wide mode (empty string)
		Logger:            slog.Default(),
	}
	srv := NewServer(deps)

	// Inject claims into context
	claims := &auth.Claims{Subject: "admin-user"}
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/namespaces/deployable", nil)
	ctx := auth.ContextWithClaims(req.Context(), claims)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "core")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Namespaces == nil {
		t.Error("namespaces must not be null")
	}
	if len(resp.Namespaces) == 0 {
		t.Error("expected non-empty namespaces for unrestricted user with curated list")
	}
	if !resp.AllowFreeform {
		t.Error("expected allowFreeform=true for unrestricted+cluster-wide")
	}
}

func TestHandleDeployableNamespaces_GET_ScopedMode(t *testing.T) {
	// Scoped mode: unrestricted user + managed namespaces set
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
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

	resolver := testResolver(roles, bindings)
	authorizer := NewAuthorizer(resolver, false)

	client := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
		provisioning: &v1alpha1.ProvisioningDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		},
	}

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromProvisioning(client),
		Authorizer:        authorizer,
		ManagedNamespaces: "team-a team-b", // scoped mode: two managed namespaces
		Logger:            slog.Default(),
	}
	srv := NewServer(deps)

	claims := &auth.Claims{Subject: "admin-user"}
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/namespaces/deployable", nil)
	ctx := auth.ContextWithClaims(req.Context(), claims)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "core")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Namespaces == nil {
		t.Error("namespaces must not be null")
	}
	if len(resp.Namespaces) != 2 {
		t.Errorf("expected 2 namespaces, got %v", resp.Namespaces)
	}
	if resp.AllowFreeform {
		t.Error("expected allowFreeform=false in scoped mode")
	}
}

func TestHandleDeployableNamespaces_POST_Returns405(t *testing.T) {
	client := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
	}
	deps := &Dependencies{
		Client: client,
		Store:  storeFromProvisioning(client),
		Logger: slog.Default(),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/clusters/core/namespaces/deployable", nil)
	w := httptest.NewRecorder()
	srv.HandleDeployableNamespaces(w, req, "core")

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeployableNamespaces_GET_NoClaims(t *testing.T) {
	// No claims → should still return 200 with empty namespaces (fail-closed)
	client := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
		provisioning: &v1alpha1.ProvisioningDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		},
	}
	deps := &Dependencies{
		Client:            client,
		Store:             storeFromProvisioning(client),
		Authorizer:        NewAuthorizer(nil, false),
		ManagedNamespaces: "",
		Logger:            slog.Default(),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/clusters/core/namespaces/deployable", nil)
	w := httptest.NewRecorder()
	srv.HandleDeployableNamespaces(w, req, "core")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for nil claims, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Namespaces == nil {
		t.Error("namespaces must not be null even for nil claims")
	}
	if len(resp.Namespaces) != 0 {
		t.Errorf("expected empty namespaces for nil claims, got %v", resp.Namespaces)
	}
	if resp.AllowFreeform {
		t.Error("expected allowFreeform=false for nil claims")
	}
}

func TestHandleDeployableNamespaces_RemoteReachable(t *testing.T) {
	// Remote cluster: fake Brood returns target inputs that differ from core env.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
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

	resolver := testResolver(roles, bindings)
	authorizer := NewAuthorizer(resolver, false)

	client := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
		provisioning: &v1alpha1.ProvisioningDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
			Spec: v1alpha1.ProvisioningDefaultsSpec{
				DefaultNamespace: "core-ns",
				Namespaces:       []string{"core-ns"},
			},
		},
	}

	brood := newFakeBrood(&client.fakeResourceClient)
	ff := brood.(*fakeBrood)
	ff.discoverNamespacesResp = &bus.NamespacesListResponse{
		ManagedNamespaces: []string{"edge-a", "edge-b"},
		CuratedNamespaces: []string{"edge-a"},
		CuratedDefault:    "edge-a",
	}

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromProvisioning(client),
		Authorizer:        authorizer,
		ManagedNamespaces: "core-mgmt", // different from remote
		Logger:            slog.Default(),
		Brood:             brood,
	}
	srv := NewServer(deps)

	claims := &auth.Claims{Subject: "admin-user"}
	req := httptest.NewRequest(http.MethodGet, "/clusters/dev-cluster/namespaces/deployable", nil)
	ctx := auth.ContextWithClaims(req.Context(), claims)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "dev-cluster")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Target inputs must win: edge-a, edge-b (managed), NOT core-mgmt
	if len(resp.Namespaces) != 2 || resp.Namespaces[0] != "edge-a" || resp.Namespaces[1] != "edge-b" {
		t.Errorf("namespaces = %v, want [edge-a edge-b] (target managed set)", resp.Namespaces)
	}
	if resp.DefaultNamespace != "edge-a" {
		t.Errorf("defaultNamespace = %q, want edge-a", resp.DefaultNamespace)
	}
	if resp.AllowFreeform {
		t.Error("expected allowFreeform=false (scoped mode via remote managed set)")
	}
	if resp.Degraded {
		t.Error("expected degraded=false for reachable remote")
	}
}

func TestHandleDeployableNamespaces_RemoteUnreachable_Unrestricted(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
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

	resolver := testResolver(roles, bindings)
	authorizer := NewAuthorizer(resolver, false)

	brood := newFakeBrood(newFakeResourceClient())
	ff := brood.(*fakeBrood)
	ff.discoverNamespacesErr = &ErrClusterUnreachable{Cluster: "dev-cluster", Err: io.ErrUnexpectedEOF}

	deps := &Dependencies{
		Client:            newFakeResourceClient(),
		Authorizer:        authorizer,
		ManagedNamespaces: "",
		Logger:            slog.Default(),
		Brood:             brood,
	}
	srv := NewServer(deps)

	claims := &auth.Claims{Subject: "admin-user"}
	req := httptest.NewRequest(http.MethodGet, "/clusters/dev-cluster/namespaces/deployable", nil)
	ctx := auth.ContextWithClaims(req.Context(), claims)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "dev-cluster")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Unrestricted + cluster-wide (nil managed) + unreachable → empty, freeform=true, degraded=true
	if len(resp.Namespaces) != 0 {
		t.Errorf("namespaces = %v, want []", resp.Namespaces)
	}
	if !resp.AllowFreeform {
		t.Error("expected allowFreeform=true for unrestricted + unreachable")
	}
	if !resp.Degraded {
		t.Error("expected degraded=true")
	}
}

func TestHandleDeployableNamespaces_RemoteUnreachable_Restricted(t *testing.T) {
	// Restricted: explicit scopes via binding with namespaces.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"create"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "dev",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a", "team-b"},
				},
			},
		},
	}

	resolver := testResolver(roles, bindings)
	authorizer := NewAuthorizer(resolver, false)

	brood := newFakeBrood(newFakeResourceClient())
	ff := brood.(*fakeBrood)
	ff.discoverNamespacesErr = &ErrClusterUnreachable{Cluster: "dev-cluster", Err: io.ErrUnexpectedEOF}

	deps := &Dependencies{
		Client:            newFakeResourceClient(),
		Authorizer:        authorizer,
		ManagedNamespaces: "",
		Logger:            slog.Default(),
		Brood:             brood,
	}
	srv := NewServer(deps)

	claims := &auth.Claims{Subject: "dev-user"}
	req := httptest.NewRequest(http.MethodGet, "/clusters/dev-cluster/namespaces/deployable", nil)
	ctx := auth.ContextWithClaims(req.Context(), claims)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "dev-cluster")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Restricted + unreachable → explicit set, no freeform, degraded=true
	if len(resp.Namespaces) != 2 {
		t.Errorf("namespaces = %v, want [team-a team-b]", resp.Namespaces)
	}
	if resp.AllowFreeform {
		t.Error("expected allowFreeform=false for restricted")
	}
	if !resp.Degraded {
		t.Error("expected degraded=true")
	}
}

func TestHandleDeployableNamespaces_RemoteUnreachable_NilClaims(t *testing.T) {
	brood := newFakeBrood(newFakeResourceClient())
	ff := brood.(*fakeBrood)
	ff.discoverNamespacesErr = &ErrClusterUnreachable{Cluster: "dev-cluster", Err: io.ErrUnexpectedEOF}

	deps := &Dependencies{
		Client:     newFakeResourceClient(),
		Authorizer: NewAuthorizer(nil, false),
		Logger:     slog.Default(),
		Brood:      brood,
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/clusters/dev-cluster/namespaces/deployable", nil)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "dev-cluster")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Namespaces) != 0 {
		t.Errorf("namespaces = %v, want []", resp.Namespaces)
	}
	if resp.AllowFreeform {
		t.Error("expected allowFreeform=false for nil claims")
	}
	if !resp.Degraded {
		t.Error("expected degraded=true")
	}
}

func TestHandleDeployableNamespaces_RemoteStructuredErrorDegrades(t *testing.T) {
	brood := newFakeBrood(newFakeResourceClient())
	ff := brood.(*fakeBrood)
	ff.discoverNamespacesErr = &BroodError{Code: bus.CodeInternal, Msg: "operator error"}

	deps := &Dependencies{
		Client:     newFakeResourceClient(),
		Authorizer: NewAuthorizer(nil, false),
		Logger:     slog.Default(),
		Brood:      brood,
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/clusters/dev-cluster/namespaces/deployable", nil)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "dev-cluster")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp DeployableNamespaces
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Degraded {
		t.Error("expected degraded=true for BroodError")
	}
}

func TestHandleDeployableNamespaces_LocalClusterNeverCallsDiscover(t *testing.T) {
	brood := newFakeBrood(newFakeResourceClient())
	ff := brood.(*fakeBrood)

	fpc := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
		provisioning: &v1alpha1.ProvisioningDefaults{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
			Spec: v1alpha1.ProvisioningDefaultsSpec{
				DefaultNamespace: "team-a",
				Namespaces:       []string{"team-a", "team-b"},
			},
		},
	}
	deps := &Dependencies{
		Client:            fpc,
		Store:             storeFromProvisioning(fpc),
		Authorizer:        NewAuthorizer(nil, false),
		ManagedNamespaces: "",
		Logger:            slog.Default(),
		Brood:             brood,
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/clusters/core/namespaces/deployable", nil)
	w := httptest.NewRecorder()

	srv.HandleDeployableNamespaces(w, req, "core")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if ff.discoverNamespacesCalled {
		t.Error("DiscoverNamespaces should NOT be called for local cluster")
	}
}

func TestParseManagedNamespaces(t *testing.T) {
	// Empty string → nil (cluster-wide)
	if r := parseManagedNamespaces(""); r != nil {
		t.Errorf("expected nil for empty string, got %v", r)
	}
	// Space-separated
	r := parseManagedNamespaces("team-a team-b")
	if len(r) != 2 || r[0] != "team-a" || r[1] != "team-b" {
		t.Errorf("expected [team-a team-b], got %v", r)
	}
	// Comma-separated
	r = parseManagedNamespaces("team-a,team-b")
	if len(r) != 2 || r[0] != "team-a" || r[1] != "team-b" {
		t.Errorf("expected [team-a team-b], got %v", r)
	}
	// Mixed
	r = parseManagedNamespaces("team-a, team-b team-c")
	if len(r) != 3 || r[0] != "team-a" || r[1] != "team-b" || r[2] != "team-c" {
		t.Errorf("expected [team-a team-b team-c], got %v", r)
	}
	// Drops empties
	r = parseManagedNamespaces("team-a,,  team-b")
	if len(r) != 2 || r[0] != "team-a" || r[1] != "team-b" {
		t.Errorf("expected [team-a team-b], got %v", r)
	}
}
