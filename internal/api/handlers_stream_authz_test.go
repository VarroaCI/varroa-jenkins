package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// --------------------------------------------------------------------------
// Common test fixtures
// --------------------------------------------------------------------------

// namespaceScopedRoleBinding returns a viewer role+binding scoped to team-a.
func namespaceScopedRoleBinding() (*v1alpha1.VarroaRole, *v1alpha1.VarroaRoleBinding) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-scoped"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Namespaces: []string{"team-a"},
			},
		},
	}
	return role, binding
}

// clusterWideRoleBinding returns a viewer role+binding with cluster-wide scope.
func clusterWideRoleBinding() (*v1alpha1.VarroaRole, *v1alpha1.VarroaRoleBinding) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer-cluster"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer-cluster",
			// Scope: nil means cluster-wide
		},
	}
	return role, binding
}

// userAClaims returns standard claims for user-a.
func userAClaims() *auth.Claims {
	return &auth.Claims{Subject: "user-a", Email: "user-a@example.com"}
}

// authContext embeds claims into a background context.
func authContext(claims *auth.Claims) context.Context {
	return auth.ContextWithClaims(context.Background(), claims)
}

// canceledContext returns a cancel-optimized context for SSE tests.
func canceledContext(claims *auth.Claims) context.Context {
	ctx, cancel := context.WithCancel(authContext(claims))
	cancel()
	return ctx
}

// seededController returns a Controller CR with wakeToken in status.
func seededController(name, namespace string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.ControllerSpec{
			Version:    "2.570.0",
			PowerState: "Running",
		},
		Status: v1alpha1.ControllerStatus{
			Phase:     "Running",
			WakeToken: "super-secret-wake-token-12345",
		},
	}
}

// newTicketIssuerForTest creates a real TicketIssuer for test use.
func newTicketIssuerForTest(t *testing.T) *auth.TicketIssuer {
	t.Helper()
	signer, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	return auth.NewTicketIssuer(signer, "https://bff.test", 30*time.Second)
}

// --------------------------------------------------------------------------
// #404 — handleControllerLogs
// --------------------------------------------------------------------------

func TestHandleControllerLogs_ForbiddenOutsideScope(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer: NewAuthorizer(r, false),
		Logger:     slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-b/ctrl1/logs", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleControllerLogs(w, req, "", "team-b", "ctrl1")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleControllerLogs_AllowedInScope(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	lb := logbuffer.New(100)
	srv := NewServer(&Dependencies{
		Authorizer: NewAuthorizer(r, false),
		LogBuffer:  lb,
		Logger:     slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/ctrl1/logs", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleControllerLogs(w, req, "", "team-a", "ctrl1")

	// Must not be 403 — the handler may 200 (empty logs from LogBuffer) or 501
	// (remote cluster), but not forbidden.
	if w.Code == http.StatusForbidden {
		t.Fatalf("expected non-403 for in-scope controller, got 403: %s", w.Body.String())
	}
}

// TestHandleControllerLogs_TicketAuthed_AllowedViaPreferredUsernameBinding guards
// against ticket-authed SSE regressing to 403 when the caller's only RBAC path is a
// kind:User binding on preferred_username. Stream tickets must carry that claim
// through Mint → Verify, or the handler's per-request re-check sees a degraded
// subject set and rejects an otherwise-authorized caller.
func TestHandleControllerLogs_TicketAuthed_AllowedViaPreferredUsernameBinding(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "preferred-username-scoped"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			// Only this binding grants access — the caller's opaque OIDC subject has none.
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice-preferred"}},
			RoleRef:  "viewer",
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Namespaces: []string{"team-a"},
			},
		},
	}
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	lb := logbuffer.New(100)
	srv := NewServer(&Dependencies{
		Authorizer: NewAuthorizer(r, false),
		LogBuffer:  lb,
		Logger:     slog.Default(),
	})

	full := &auth.Claims{
		Subject:           "google-oauth2|12345",
		Email:             "alice@example.com",
		Name:              "Alice Example",
		PreferredUsername: "alice-preferred",
		Groups:            []string{"devs"},
	}
	signer, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	iss := auth.NewTicketIssuer(signer, "https://bff.test", 30*time.Second)
	ver := auth.NewTicketVerifier(signer.PublicKey(), "https://bff.test")

	ticket, _, err := iss.Mint(full, "controller:core/team-a/ctrl1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := ver.Verify(context.Background(), ticket, "controller:core/team-a/ctrl1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.PreferredUsername != "alice-preferred" {
		t.Fatalf("ticket Verify dropped PreferredUsername: %+v", claims)
	}

	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/ctrl1/logs", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleControllerLogs(w, req, "core", "team-a", "ctrl1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for preferred_username-bound caller via ticket, got %d: %s", w.Code, w.Body.String())
	}
}

// --------------------------------------------------------------------------
// #405 — handleControllerYAML
// --------------------------------------------------------------------------

func TestHandleControllerYAML_ForbiddenOutsideScope(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer: NewAuthorizer(r, false),
		Logger:     slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-b/ctrl1/yaml", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleControllerYAML(w, req, "", "team-b", "ctrl1")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleControllerYAML_StripsWakeToken(t *testing.T) {
	role, binding := clusterWideRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	fc := newFakeResourceClient()
	fc.controllers = map[string]*v1alpha1.Controller{
		"ctrl1": seededController("ctrl1", "team-a"),
	}
	srv := NewServer(&Dependencies{
		Authorizer: NewAuthorizer(r, false),
		Client:     fc,
		Store:      storeFromFake(fc),
		Logger:     slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/ctrl1/yaml", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleControllerYAML(w, req, "", "team-a", "ctrl1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, "wakeToken") {
		t.Fatalf("response should not contain 'wakeToken', got:\n%s", body)
	}
	if strings.Contains(body, "super-secret-wake-token-12345") {
		t.Fatalf("response should not contain the wake token value, got:\n%s", body)
	}
}

// --------------------------------------------------------------------------
// #405 — handleCreateController local-fallback
// --------------------------------------------------------------------------

func TestHandleCreateController_BroodRoute_OmitsWakeTokenShape(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "creator"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read", "create"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-creator"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "creator",
		},
	}
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	fc := newFakeResourceClient()
	fc.controllers = make(map[string]*v1alpha1.Controller)
	fc.namespaces["team-a"] = true
	srv := NewServer(&Dependencies{
		Authorizer:        NewAuthorizer(r, false),
		Client:            fc,
		Store:             storeFromFake(fc),
		Logger:            slog.Default(),
		Brood:             newFakeBrood(fc),
		OperatorNamespace: "varroa-system",
	})

	claims := userAClaims()
	body := `{"metadata":{"name":"newctrl","namespace":"team-a"},"apiVersion":"varroa.dev/v1alpha1","kind":"Controller","spec":{"version":"2.570.0","powerState":"Running"}}`
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/controllers/team-a/newctrl", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleCreateController(w, req, "core", "team-a")

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var resp controllerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected valid controllerDetailResponse JSON, got error: %v; body: %s", err, w.Body.String())
	}
	if resp.Name != "newctrl" {
		t.Errorf("expected name 'newctrl', got %q", resp.Name)
	}
	if resp.Namespace != "team-a" {
		t.Errorf("expected namespace 'team-a', got %q", resp.Namespace)
	}
	if resp.Cluster != "core" {
		t.Errorf("expected cluster 'core', got %q", resp.Cluster)
	}
	// Ensure no raw CR fields leak through.
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "managedFields") {
		t.Errorf("response should not contain 'managedFields', got:\n%s", bodyStr)
	}
}

// --------------------------------------------------------------------------
// #405 — handleUpdateController local-fallback
// --------------------------------------------------------------------------

func TestHandleUpdateController_BroodRoute_StripsWakeToken(t *testing.T) {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "editor"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read", "update"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-editor"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "editor",
		},
	}
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	fc := newFakeResourceClient()
	fc.controllers = map[string]*v1alpha1.Controller{
		"ctrl1": seededController("ctrl1", "team-a"),
	}
	srv := NewServer(&Dependencies{
		Authorizer:        NewAuthorizer(r, false),
		Client:            fc,
		Store:             storeFromFake(fc),
		Logger:            slog.Default(),
		Brood:             newFakeBrood(fc),
		OperatorNamespace: "varroa-system",
	})

	claims := userAClaims()
	body := `{"spec":{"powerState":"Stopped"}}`
	req := httptest.NewRequest(http.MethodPatch, "/clusters/core/controllers/team-a/ctrl1", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	srv.handleUpdateController(w, req, "core", "team-a", "ctrl1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp controllerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected valid controllerDetailResponse JSON, got error: %v; body: %s", err, w.Body.String())
	}

	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "wakeToken") {
		t.Fatalf("response should not contain 'wakeToken', got:\n%s", bodyStr)
	}
}

// --------------------------------------------------------------------------
// #409 — handleMiteStreamSSE
// --------------------------------------------------------------------------

func TestHandleMiteStreamSSE_ForbiddenOutsideScope(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	bc := sse.NewBroadcaster()
	srv := NewServer(&Dependencies{
		Authorizer:  NewAuthorizer(r, false),
		Broadcaster: bc,
		Logger:      slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-b/ctrl1/mite/stream", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleMiteStreamSSE(w, req, "core", "team-b", "ctrl1")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatal("handler should not have entered the streaming branch for forbidden callers")
	}
}

func TestHandleMiteStreamSSE_AllowedInScope(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	bc := sse.NewBroadcaster()
	srv := NewServer(&Dependencies{
		Authorizer:  NewAuthorizer(r, false),
		Broadcaster: bc,
		Logger:      slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/controllers/team-a/ctrl1/mite/stream", nil).
		WithContext(canceledContext(claims))
	w := httptest.NewRecorder()
	srv.handleMiteStreamSSE(w, req, "core", "team-a", "ctrl1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("expected Content-Type text/event-stream for allowed caller")
	}
}

// --------------------------------------------------------------------------
// #408 — handleBroodStreamSSE (new wrapper)
// --------------------------------------------------------------------------

func TestHandleBroodStreamSSE_ForbiddenForNamespaceScopedUser(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	bc := sse.NewBroadcaster()
	srv := NewServer(&Dependencies{
		Authorizer:  NewAuthorizer(r, false),
		Broadcaster: bc,
		Logger:      slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/stream/brood", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleBroodStreamSSE(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleBroodStreamSSE_AllowedForClusterWideBinding(t *testing.T) {
	role, binding := clusterWideRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	bc := sse.NewBroadcaster()
	srv := NewServer(&Dependencies{
		Authorizer:  NewAuthorizer(r, false),
		Broadcaster: bc,
		Logger:      slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/stream/brood", nil).
		WithContext(canceledContext(claims))
	w := httptest.NewRecorder()
	srv.handleBroodStreamSSE(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("expected Content-Type text/event-stream for cluster-wide caller")
	}
}

func TestHandleBroodStreamSSE_NilAuthorizerDenies(t *testing.T) {
	bc := sse.NewBroadcaster()
	srv := NewServer(&Dependencies{
		Authorizer:  nil,
		Broadcaster: bc,
		Logger:      slog.Default(),
	})

	claims := userAClaims()
	req := httptest.NewRequest(http.MethodGet, "/stream/brood", nil).
		WithContext(authContext(claims))
	w := httptest.NewRecorder()
	srv.handleBroodStreamSSE(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden with nil Authorizer, got %d: %s", w.Code, w.Body.String())
	}
}

// --------------------------------------------------------------------------
// #408 — handleStreamTicket mint-time authz
// --------------------------------------------------------------------------

func TestHandleStreamTicket_BroodScope_ForbiddenWithoutClusterWideBinding(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer:   NewAuthorizer(r, false),
		TicketIssuer: newTicketIssuerForTest(t),
		Logger:       slog.Default(),
	})

	claims := userAClaims()
	body := `{"scope":"brood"}`
	req := httptest.NewRequest(http.MethodPost, "/stream/ticket", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for brood scope without cluster-wide binding, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleStreamTicket_BroodScope_AllowedForClusterWideBinding(t *testing.T) {
	role, binding := clusterWideRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer:   NewAuthorizer(r, false),
		TicketIssuer: newTicketIssuerForTest(t),
		Logger:       slog.Default(),
	})

	claims := userAClaims()
	body := `{"scope":"brood"}`
	req := httptest.NewRequest(http.MethodPost, "/stream/ticket", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for brood scope with cluster-wide binding, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleStreamTicket_ActivityScope_AllowedForNamespaceScopedCaller(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer:   NewAuthorizer(r, false),
		TicketIssuer: newTicketIssuerForTest(t),
		Logger:       slog.Default(),
	})

	claims := userAClaims()
	body := `{"scope":"activity"}`
	req := httptest.NewRequest(http.MethodPost, "/stream/ticket", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for activity scope (any authenticated caller), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleStreamTicket_ControllerScope_ForbiddenOutsideNamespace(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer:   NewAuthorizer(r, false),
		TicketIssuer: newTicketIssuerForTest(t),
		Logger:       slog.Default(),
	})

	claims := userAClaims()
	body := `{"scope":"controller:core/team-b/ctrl1"}`
	req := httptest.NewRequest(http.MethodPost, "/stream/ticket", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for controller scope outside namespace, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleStreamTicket_BroodopScope_ForbiddenOutsideNamespace(t *testing.T) {
	role, binding := namespaceScopedRoleBinding()
	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	srv := NewServer(&Dependencies{
		Authorizer:   NewAuthorizer(r, false),
		TicketIssuer: newTicketIssuerForTest(t),
		Logger:       slog.Default(),
	})

	claims := userAClaims()
	body := `{"scope":"broodop:team-b/op1"}`
	req := httptest.NewRequest(http.MethodPost, "/stream/ticket", strings.NewReader(body)).
		WithContext(authContext(claims))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for broodop scope outside namespace, got %d: %s", w.Code, w.Body.String())
	}
}
