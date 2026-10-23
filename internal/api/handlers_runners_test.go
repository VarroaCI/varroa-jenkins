package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// runnerAdminTestServer builds a Server whose Authorizer admits
// "test-admin" as full admin (mirrors adminTenantTestServer in
// handlers_tenants_test.go).
func runnerAdminTestServer(t *testing.T) (*Server, *crdstore.Fake, context.Context) {
	t.Helper()
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		},
	}
	adminBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-admin"}},
			RoleRef:  "admin",
		},
	}
	resolver := testResolver([]*v1alpha1.VarroaRole{adminRole}, []*v1alpha1.VarroaRoleBinding{adminBinding})
	authz := NewAuthorizer(resolver, false)

	client := newFakeResourceClient()
	store := storeFromFake(client)
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      store,
		Logger:     slog.Default(),
		Authorizer: authz,
	})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "test-admin"})
	return srv, store, ctx
}

// runnerFleetOnlyTestServer builds a Server whose caller "fleet-user" holds
// only a cluster-wide runners rule with the given verbs — the "runners"
// gate alone, no tenant grant on any Tenant.
func runnerFleetOnlyTestServer(t *testing.T, verbs []string) (*Server, *crdstore.Fake, context.Context) {
	t.Helper()
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "runners-fleet"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"runners"}, Verbs: verbs}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "fleet-user"}},
			RoleRef:  "runners-fleet",
			// Scope: nil = cluster-wide (grants the runners verb fleet-wide,
			// but carries no tenant scope, so no pipelines grant follows).
		},
	}
	resolver := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	authz := NewAuthorizer(resolver, false)

	client := newFakeResourceClient()
	store := storeFromFake(client)
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      store,
		Logger:     slog.Default(),
		Authorizer: authz,
	})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "fleet-user"})
	return srv, store, ctx
}

// runnerTenantTestServer builds a Server whose caller "tenant-user" holds a
// cluster-wide runners rule (verbs) plus a tenant-scoped pipelines grant on
// "payments", narrowed by jobGlobs when non-nil.
func runnerTenantTestServer(t *testing.T, runnerVerbs []string, pipelinesVerbs []string, jobGlobs []string) (*Server, *crdstore.Fake, context.Context) {
	t.Helper()
	fleetRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "runners-fleet"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"runners"}, Verbs: runnerVerbs}},
		},
	}
	pipelinesRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pipelines-tenant"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: pipelinesVerbs}},
		},
	}
	fleetBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "tenant-user"}},
			RoleRef:  "runners-fleet",
		},
	}
	tenantBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "tenant-user"}},
			RoleRef:  "pipelines-tenant",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: jobGlobs}}},
		},
	}
	resolver := testResolver(
		[]*v1alpha1.VarroaRole{fleetRole, pipelinesRole},
		[]*v1alpha1.VarroaRoleBinding{fleetBinding, tenantBinding},
	)
	authz := NewAuthorizer(resolver, false)

	client := newFakeResourceClient()
	store := storeFromFake(client)
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      store,
		Logger:     slog.Default(),
		Authorizer: authz,
	})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "tenant-user"})
	return srv, store, ctx
}

func seedRunner(t *testing.T, store *crdstore.Fake, r *v1alpha1.Runner) {
	t.Helper()
	crdstore.MustSeed(store, r)
}

func seedRunnerTenant(t *testing.T, store *crdstore.Fake) {
	t.Helper()
	crdstore.MustSeed(store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TenantSpec{Members: []string{"someone"}, Namespaces: []string{"payments"}},
	})
}

// --- 7.3: list, get, create, patch, delete ---

func TestRunnerHandlers_AdminCRUD(t *testing.T) {
	srv, store, ctx := runnerAdminTestServer(t)
	seedRunnerTenant(t, store)

	// create
	createBody := `{"name":"win-1","spec":{"tenant":"payments","launcher":"inbound","remoteFS":"/var/j"}}`
	req := httptest.NewRequest(http.MethodPost, "/runners", bytes.NewReader([]byte(createBody))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunners(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Result().StatusCode, w.Body.String())
	}

	// list
	req = httptest.NewRequest(http.MethodGet, "/runners", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	srv.HandleRunners(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w.Result().StatusCode)
	}
	var listResp struct {
		Items []runnerEntry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list: decode: %v", err)
	}
	if len(listResp.Items) != 1 || listResp.Items[0].Name != "win-1" {
		t.Fatalf("list: expected [win-1], got %+v", listResp.Items)
	}

	// get
	req = httptest.NewRequest(http.MethodGet, "/runners/win-1", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Result().StatusCode)
	}
	var got runnerEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("get: decode: %v", err)
	}
	if got.Spec.Tenant != "payments" || got.Spec.Launcher != "inbound" {
		t.Fatalf("get: unexpected spec %+v", got.Spec)
	}

	// patch
	patchBody := `{"spec":{"numExecutors":3}}`
	req = httptest.NewRequest(http.MethodPatch, "/runners/win-1", bytes.NewReader([]byte(patchBody))).WithContext(ctx)
	w = httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	updated, err := crdstore.Get[v1alpha1.Runner](context.Background(), store, "win-1", "")
	if err != nil {
		t.Fatalf("patch: get after patch: %v", err)
	}
	if updated.Spec.NumExecutors != 3 {
		t.Errorf("patch: expected numExecutors=3, got %d", updated.Spec.NumExecutors)
	}

	// delete
	req = httptest.NewRequest(http.MethodDelete, "/runners/win-1", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", w.Result().StatusCode)
	}
	if _, err := crdstore.Get[v1alpha1.Runner](context.Background(), store, "win-1", ""); err == nil {
		t.Fatal("delete: expected runner to be gone")
	}
}

func TestRunnerHandlers_CreateUnknownTenantRefused(t *testing.T) {
	srv, _, ctx := runnerAdminTestServer(t)
	// No Tenant "payments" seeded.
	body := `{"name":"win-1","spec":{"tenant":"payments","launcher":"inbound","remoteFS":"/var/j"}}`
	req := httptest.NewRequest(http.MethodPost, "/runners", bytes.NewReader([]byte(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunners(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

func TestRunnerHandlers_PatchImmutableFieldsRefused(t *testing.T) {
	srv, store, ctx := runnerAdminTestServer(t)
	seedRunnerTenant(t, store)
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
	})

	cases := []string{
		`{"spec":{"tenant":"other-team"}}`,
		`{"spec":{"launcher":"ssh"}}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPatch, "/runners/win-1", bytes.NewReader([]byte(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleRunnerDispatch(w, req)
		if w.Result().StatusCode != http.StatusBadRequest {
			t.Errorf("patch %s: expected 400, got %d: %s", body, w.Result().StatusCode, w.Body.String())
		}
	}
}

// --- 7.3a: the tenant check beside the runners verb ---

func TestRunnerHandlers_FleetVerbWithoutTenantGrant_CreateForbidden(t *testing.T) {
	srv, store, ctx := runnerFleetOnlyTestServer(t, []string{"read", "create", "update", "delete"})
	seedRunnerTenant(t, store)

	body := `{"name":"win-1","spec":{"tenant":"payments","launcher":"inbound","remoteFS":"/var/j"}}`
	req := httptest.NewRequest(http.MethodPost, "/runners", bytes.NewReader([]byte(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunners(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if _, err := crdstore.Get[v1alpha1.Runner](context.Background(), store, "win-1", ""); err == nil {
		t.Fatal("expected no runner to be created")
	}
}

func TestRunnerHandlers_FleetVerbWithoutTenantGrant_UpdateForbidden(t *testing.T) {
	srv, store, ctx := runnerFleetOnlyTestServer(t, []string{"read", "create", "update", "delete"})
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
	})

	body := `{"spec":{"numExecutors":3}}`
	req := httptest.NewRequest(http.MethodPatch, "/runners/win-1", bytes.NewReader([]byte(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

func TestRunnerHandlers_FleetVerbWithoutTenantGrant_DeleteForbidden(t *testing.T) {
	srv, store, ctx := runnerFleetOnlyTestServer(t, []string{"read", "create", "update", "delete"})
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
	})

	req := httptest.NewRequest(http.MethodDelete, "/runners/win-1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if _, err := crdstore.Get[v1alpha1.Runner](context.Background(), store, "win-1", ""); err != nil {
		t.Fatal("expected the runner to still exist")
	}
}

func TestRunnerHandlers_ListFiltersUnreadableTenants(t *testing.T) {
	srv, store, ctx := runnerFleetOnlyTestServer(t, []string{"read"})
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunners(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (filtered, not refused), got %d", w.Result().StatusCode)
	}
	var listResp struct {
		Items []runnerEntry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listResp.Items) != 0 {
		t.Fatalf("expected the payments runner to be omitted, got %+v", listResp.Items)
	}
}

func TestRunnerHandlers_EmptyJobPathPassesNarrowedGrant(t *testing.T) {
	// tenant-user holds a "payments" grant narrowed to jobs "foo/*". A
	// runner carries no job path, so the narrowed grant must still admit
	// the read and write checks.
	srv, store, ctx := runnerTenantTestServer(t, []string{"read", "create", "update", "delete"}, []string{"read", "write"}, []string{"foo/*"})
	seedRunnerTenant(t, store)
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/win-1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("read: expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}

	patchBody := `{"spec":{"numExecutors":2}}`
	req = httptest.NewRequest(http.MethodPatch, "/runners/win-1", bytes.NewReader([]byte(patchBody))).WithContext(ctx)
	w = httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("write: expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

// --- 7.4: token redaction ---

func TestRunnerHandlers_TokenRedaction(t *testing.T) {
	srv, store, ctx := runnerAdminTestServer(t)
	seedRunnerTenant(t, store)
	seedRunner(t, store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
		Status:     v1alpha1.RunnerStatus{Token: "deadbeefdeadbeefdeadbeefdeadbeef", Connected: true},
	})

	// admin (holds runners:create) sees the token.
	req := httptest.NewRequest(http.MethodGet, "/runners/win-1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleRunnerDispatch(w, req)
	var adminView runnerEntry
	if err := json.Unmarshal(w.Body.Bytes(), &adminView); err != nil {
		t.Fatalf("admin: decode: %v", err)
	}
	if adminView.Status.Token == "" {
		t.Error("admin (runners:create) should see status.token")
	}

	// A viewer-style caller (read only, no create) must not see the token.
	viewerSrv, viewerStore, viewerCtx := runnerTenantTestServer(t, []string{"read"}, []string{"read", "write"}, nil)
	seedRunnerTenant(t, viewerStore)
	seedRunner(t, viewerStore, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "win-1"},
		Spec:       v1alpha1.RunnerSpec{Tenant: "payments", Launcher: "inbound", RemoteFS: "/var/j"},
		Status:     v1alpha1.RunnerStatus{Token: "deadbeefdeadbeefdeadbeefdeadbeef", Connected: true},
	})
	req = httptest.NewRequest(http.MethodGet, "/runners/win-1", nil).WithContext(viewerCtx)
	w = httptest.NewRecorder()
	viewerSrv.HandleRunnerDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("viewer: expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var viewerView runnerEntry
	if err := json.Unmarshal(w.Body.Bytes(), &viewerView); err != nil {
		t.Fatalf("viewer: decode: %v", err)
	}
	if viewerView.Status.Token != "" {
		t.Error("a caller without runners:create must not see status.token")
	}
	if !viewerView.Status.Connected {
		t.Error("redaction must clear only token, not the rest of status")
	}
}

// --- Unauthenticated / misconfigured Authorizer ---

func TestRunnerHandlers_NilAuthorizer(t *testing.T) {
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client: client,
		Store:  storeFromFake(client),
		Logger: slog.Default(),
	})

	tests := []struct {
		name       string
		method     string
		path       string
		handler    http.HandlerFunc
		wantStatus int
	}{
		{"list", http.MethodGet, "/runners", srv.HandleRunners, http.StatusOK}, // filtered to empty, not refused
		{"create", http.MethodPost, "/runners", srv.HandleRunners, http.StatusForbidden},
		{"get", http.MethodGet, "/runners/win-1", srv.HandleRunnerDispatch, http.StatusForbidden},
		{"delete", http.MethodDelete, "/runners/win-1", srv.HandleRunnerDispatch, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "someone"}))
			w := httptest.NewRecorder()
			tt.handler(w, req)
			if w.Result().StatusCode != tt.wantStatus {
				t.Errorf("expected %d, got %d", tt.wantStatus, w.Result().StatusCode)
			}
		})
	}
}
