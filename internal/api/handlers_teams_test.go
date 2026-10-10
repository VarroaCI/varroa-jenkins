package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// teamPipelinesTenantTestServer builds a Server whose Authorizer resolves
// role bindings, so a caller can hold a tenant-scoped pipelines grant on
// "payments" without holding full administrative access. subject names the
// bound user; roleRef names the VarroaRole the binding uses.
func teamPipelinesTenantTestServer(t *testing.T, subject, roleRef string) (*Server, *crdstore.Fake, context.Context) {
	t.Helper()
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-reader"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read"}}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pipelines-admin"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: []string{"read", "admin"}}},
		}},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: subject + "-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: subject}},
			RoleRef:  roleRef,
			Scope:    &v1alpha1.VarroaRoleBindingScope{Tenants: []v1alpha1.TenantScope{{Name: "payments"}}},
		},
	}
	resolver := testResolver(roles, []*v1alpha1.VarroaRoleBinding{binding})
	authz := NewAuthorizer(resolver, false)

	client := newFakeResourceClient()
	store := storeFromFake(client)
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      store,
		Logger:     slog.Default(),
		Authorizer: authz,
	})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: subject})
	return srv, store, ctx
}

// adminTeamTestServer builds a Server whose Authorizer admits the caller
// "test-admin" as a full admin, for the pipelines-mapping tests below.
func adminTeamTestServer(t *testing.T) (*Server, *crdstore.Fake, context.Context) {
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

func TestTeamHandlers_NonAdmin(t *testing.T) {
	// nil Authorizer → 403 on all endpoints.
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client: client,
		Store:  storeFromFake(client),
		Logger: slog.Default(),
	})

	tests := []struct {
		name    string
		method  string
		path    string
		body    io.Reader
		handler http.HandlerFunc
	}{
		{"list teams", http.MethodGet, "/teams", nil, srv.HandleTeams},
		{"create team", http.MethodPost, "/teams", bytes.NewReader([]byte(`{"name":"t","members":["u"],"namespaces":["ns"]}`)), srv.HandleTeams},
		{"get team", http.MethodGet, "/teams/my-team", nil, srv.HandleTeamDispatch},
		{"delete team", http.MethodDelete, "/teams/my-team", nil, srv.HandleTeamDispatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, tt.body)
			req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{}))
			w := httptest.NewRecorder()
			tt.handler(w, req)
			resp := w.Result()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("expected 403, got %d", resp.StatusCode)
			}
		})
	}
}

func TestTeamHandlers_AdminCreate(t *testing.T) {
	// With an Authorizer that uses an admin-capable resolver, admin can create.
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
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      storeFromFake(client),
		Logger:     slog.Default(),
		Authorizer: authz,
	})

	// Test create with valid request
	body := `{"name":"new-team","members":["alice"],"namespaces":["ns1"]}`
	req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(body)))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "test-admin"}))
	w := httptest.NewRecorder()
	srv.HandleTeams(w, req)
	resp := w.Result()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

func TestTeamHandlers_AdminCreate_Validation(t *testing.T) {
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
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      storeFromFake(client),
		Logger:     slog.Default(),
		Authorizer: authz,
	})

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "empty members and subjects",
			body:       `{"name":"t","namespaces":["ns"]}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "admin roleRef",
			body:       `{"name":"t","members":["u"],"namespaces":["ns"],"roleRef":"admin"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty namespaces",
			body:       `{"name":"t","members":["u"],"namespaces":[]}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "test-admin"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(tt.body)))
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			srv.HandleTeams(w, req)
			resp := w.Result()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

// --- 11.5: spec.pipelines and status.placement round trip ---

// TestTeamHandlers_PipelinesFieldsStoredOnPUT asserts task 11.5's write
// direction: a PUT carrying spec.pipelines stores every field.
func TestTeamHandlers_PipelinesFieldsStoredOnPUT(t *testing.T) {
	srv, store, ctx := adminTeamTestServer(t)

	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
	})

	body := `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1"],
		"pipelines": {
			"profile": "lts",
			"cellPool": "solo",
			"agentNamespace": "ns1",
			"composedBundleRef": {"name": "payments-bundle", "namespace": "ns1"}
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	if stored.Spec.Pipelines == nil {
		t.Fatal("stored Team carries no spec.pipelines after PUT")
	}
	if stored.Spec.Pipelines.Profile != "lts" || stored.Spec.Pipelines.CellPool != "solo" ||
		stored.Spec.Pipelines.AgentNamespace != "ns1" ||
		stored.Spec.Pipelines.ComposedBundleRef == nil ||
		stored.Spec.Pipelines.ComposedBundleRef.Name != "payments-bundle" ||
		stored.Spec.Pipelines.ComposedBundleRef.Namespace != "ns1" {
		t.Fatalf("stored spec.pipelines = %+v, want every field carried", stored.Spec.Pipelines)
	}
}

// TestTeamHandlers_PipelinesAndPlacementReadOnGET asserts task 11.5's read
// direction: the detail response reports both spec.pipelines and
// status.placement for an already-placed pipelines tenant.
func TestTeamHandlers_PipelinesAndPlacementReadOnGET(t *testing.T) {
	srv, store, ctx := adminTeamTestServer(t)

	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"ns1"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile:        "lts",
				CellPool:       "solo",
				AgentNamespace: "ns1",
				ComposedBundleRef: &v1alpha1.ComposedBundleRef{
					Name: "payments-bundle", Namespace: "ns1",
				},
			},
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{
				Cluster: "core", Cell: "cell-a", CellNamespace: "ns1", Generation: 4,
			},
		},
	})

	getReq := httptest.NewRequest(http.MethodGet, "/teams/payments", nil)
	getReq = getReq.WithContext(ctx)
	getW := httptest.NewRecorder()
	srv.HandleTeamDispatch(getW, getReq)
	if getW.Result().StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getW.Result().StatusCode)
	}

	var got teamEntry
	if err := json.Unmarshal(getW.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Pipelines == nil || got.Pipelines.Profile != "lts" || got.Pipelines.CellPool != "solo" ||
		got.Pipelines.AgentNamespace != "ns1" || got.Pipelines.ComposedBundleRef == nil ||
		got.Pipelines.ComposedBundleRef.Name != "payments-bundle" || got.Pipelines.ComposedBundleRef.Namespace != "ns1" {
		t.Fatalf("response pipelines = %+v, want spec.pipelines reported in full", got.Pipelines)
	}
	if got.Placement == nil || got.Placement.Cluster != "core" || got.Placement.Cell != "cell-a" ||
		got.Placement.CellNamespace != "ns1" || got.Placement.Generation != 4 {
		t.Fatalf("response placement = %+v, want status.placement reported", got.Placement)
	}
}

// TestTeamHandlers_PipelinesLedgerSettingsRoundTrip asserts task 10.4: a PUT
// carrying the seven Team.spec.pipelines ledger and rehydration settings
// stores every one of them, and a GET reports status.rehydration in full.
func TestTeamHandlers_PipelinesLedgerSettingsRoundTrip(t *testing.T) {
	srv, store, ctx := adminTeamTestServer(t)

	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
	})

	body := `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1"],
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"rehydrateBuilds": 5,
			"rehydrateDeadline": "45m0s",
			"interruptedRetry": "automatic",
			"concurrency": "cancelInProgress",
			"cronBackstop": false,
			"cronCatchUp": "none",
			"cronGrace": "10m0s"
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	p := stored.Spec.Pipelines
	if p == nil {
		t.Fatal("stored Team carries no spec.pipelines after PUT")
	}
	if p.RehydrateBuilds == nil || *p.RehydrateBuilds != 5 {
		t.Errorf("stored rehydrateBuilds = %v, want 5", p.RehydrateBuilds)
	}
	if p.RehydrateDeadline == nil || p.RehydrateDeadline.Duration != 45*time.Minute {
		t.Errorf("stored rehydrateDeadline = %v, want 45m", p.RehydrateDeadline)
	}
	if p.InterruptedRetry != "automatic" {
		t.Errorf("stored interruptedRetry = %q, want automatic", p.InterruptedRetry)
	}
	if p.Concurrency != "cancelInProgress" {
		t.Errorf("stored concurrency = %q, want cancelInProgress", p.Concurrency)
	}
	if p.CronBackstop == nil || *p.CronBackstop != false {
		t.Errorf("stored cronBackstop = %v, want false", p.CronBackstop)
	}
	if p.CronCatchUp != "none" {
		t.Errorf("stored cronCatchUp = %q, want none", p.CronCatchUp)
	}
	if p.CronGrace == nil || p.CronGrace.Duration != 10*time.Minute {
		t.Errorf("stored cronGrace = %v, want 10m", p.CronGrace)
	}

	// Seed status.rehydration directly (TeamReconciler's own write path;
	// this test only asserts the read projection) and confirm the GET
	// response carries every field.
	stored.Status.Rehydration = &v1alpha1.TeamRehydrationStatus{
		Generation:   3,
		CellInstance: "pod-uid-1",
		Records:      12,
		Truncated:    true,
		UnseededJobs: 2,
		Deferred:     []string{"payments/api/main"},
		Attempts:     4,
		LastAttempt:  &metav1.Time{Time: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)},
	}
	if err := crdstore.Apply[v1alpha1.Team](context.Background(), store, stored); err != nil {
		t.Fatalf("seed status.rehydration: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/teams/payments", nil)
	getReq = getReq.WithContext(ctx)
	getW := httptest.NewRecorder()
	srv.HandleTeamDispatch(getW, getReq)
	if getW.Result().StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getW.Result().StatusCode)
	}

	var got teamEntry
	if err := json.Unmarshal(getW.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Pipelines == nil {
		t.Fatal("response carries no pipelines")
	}
	if got.Pipelines.RehydrateBuilds == nil || *got.Pipelines.RehydrateBuilds != 5 {
		t.Errorf("response rehydrateBuilds = %v, want 5", got.Pipelines.RehydrateBuilds)
	}
	if got.Pipelines.RehydrateDeadline == nil || got.Pipelines.RehydrateDeadline.Duration != 45*time.Minute {
		t.Errorf("response rehydrateDeadline = %v, want 45m", got.Pipelines.RehydrateDeadline)
	}
	if got.Pipelines.InterruptedRetry != "automatic" {
		t.Errorf("response interruptedRetry = %q, want automatic", got.Pipelines.InterruptedRetry)
	}
	if got.Pipelines.Concurrency != "cancelInProgress" {
		t.Errorf("response concurrency = %q, want cancelInProgress", got.Pipelines.Concurrency)
	}
	if got.Pipelines.CronBackstop == nil || *got.Pipelines.CronBackstop != false {
		t.Errorf("response cronBackstop = %v, want false", got.Pipelines.CronBackstop)
	}
	if got.Pipelines.CronCatchUp != "none" {
		t.Errorf("response cronCatchUp = %q, want none", got.Pipelines.CronCatchUp)
	}
	if got.Pipelines.CronGrace == nil || got.Pipelines.CronGrace.Duration != 10*time.Minute {
		t.Errorf("response cronGrace = %v, want 10m", got.Pipelines.CronGrace)
	}

	if got.Rehydration == nil {
		t.Fatal("response carries no status.rehydration")
	}
	r := got.Rehydration
	if r.Generation != 3 || r.CellInstance != "pod-uid-1" || r.Records != 12 || !r.Truncated ||
		r.UnseededJobs != 2 || len(r.Deferred) != 1 || r.Deferred[0] != "payments/api/main" ||
		r.Attempts != 4 || r.LastAttempt == nil {
		t.Fatalf("response rehydration = %+v, want status.rehydration reported in full", r)
	}
}

// TestTeamHandlers_DoorCredentialsNeverServed asserts task 11.5's other
// half: status.pipelines.webhookToken and .webhookSecret never appear in
// a Team read response, even though the CRD carries them.
func TestTeamHandlers_DoorCredentialsNeverServed(t *testing.T) {
	srv, store, ctx := adminTeamTestServer(t)

	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
		Status: v1alpha1.TeamStatus{
			Pipelines: &v1alpha1.TeamPipelinesStatus{
				WebhookToken:  "super-secret-token",
				WebhookSecret: "super-secret-secret",
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/payments", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", w.Result().StatusCode)
	}

	raw := w.Body.String()
	for _, secret := range []string{"super-secret-token", "super-secret-secret", "webhookToken", "webhookSecret"} {
		if bytes.Contains([]byte(raw), []byte(secret)) {
			t.Errorf("response body carries %q, want the door credentials never served: %s", secret, raw)
		}
	}

	// The same guarantee holds on the list endpoint.
	listReq := httptest.NewRequest(http.MethodGet, "/teams", nil)
	listReq = listReq.WithContext(ctx)
	listW := httptest.NewRecorder()
	srv.HandleTeams(listW, listReq)
	listRaw := listW.Body.String()
	for _, secret := range []string{"super-secret-token", "super-secret-secret", "webhookToken", "webhookSecret"} {
		if bytes.Contains([]byte(listRaw), []byte(secret)) {
			t.Errorf("list response body carries %q, want the door credentials never served: %s", secret, listRaw)
		}
	}
}

// --- 8.3: spec.pipelines.exports round trip and authorization ---

// TestTeamHandlers_ExportsRoundTrip asserts task 8.1/8.3's data path: a PUT
// carrying two exports entries (one job grant, one locks grant) stores both
// in full, and the GET response reports them with no field dropped.
func TestTeamHandlers_ExportsRoundTrip(t *testing.T) {
	srv, store, ctx := adminTeamTestServer(t)

	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
	})

	body := `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1"],
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"exports": [
				{"job": "release/*", "callers": ["payments", "search"]},
				{"locks": ["deploy-prod", "staging/*"], "callers": ["payments"]}
			]
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	if len(stored.Spec.Pipelines.Exports) != 2 {
		t.Fatalf("stored exports = %+v, want 2 entries", stored.Spec.Pipelines.Exports)
	}
	if stored.Spec.Pipelines.Exports[0].Job != "release/*" ||
		len(stored.Spec.Pipelines.Exports[0].Callers) != 2 ||
		stored.Spec.Pipelines.Exports[0].Callers[0] != "payments" ||
		stored.Spec.Pipelines.Exports[0].Callers[1] != "search" {
		t.Fatalf("stored exports[0] = %+v, want the job grant carried in full", stored.Spec.Pipelines.Exports[0])
	}
	if len(stored.Spec.Pipelines.Exports[1].Locks) != 2 ||
		stored.Spec.Pipelines.Exports[1].Locks[0] != "deploy-prod" ||
		stored.Spec.Pipelines.Exports[1].Locks[1] != "staging/*" ||
		len(stored.Spec.Pipelines.Exports[1].Callers) != 1 ||
		stored.Spec.Pipelines.Exports[1].Callers[0] != "payments" {
		t.Fatalf("stored exports[1] = %+v, want the locks grant carried in full", stored.Spec.Pipelines.Exports[1])
	}

	getReq := httptest.NewRequest(http.MethodGet, "/teams/payments", nil)
	getReq = getReq.WithContext(ctx)
	getW := httptest.NewRecorder()
	srv.HandleTeamDispatch(getW, getReq)
	if getW.Result().StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getW.Result().StatusCode)
	}

	var got teamEntry
	if err := json.Unmarshal(getW.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Pipelines == nil || len(got.Pipelines.Exports) != 2 {
		t.Fatalf("response exports = %+v, want 2 entries reported", got.Pipelines)
	}
	if got.Pipelines.Exports[0].Job != "release/*" ||
		len(got.Pipelines.Exports[0].Callers) != 2 {
		t.Fatalf("response exports[0] = %+v, want the job grant reported in full", got.Pipelines.Exports[0])
	}
	if len(got.Pipelines.Exports[1].Locks) != 2 || len(got.Pipelines.Exports[1].Callers) != 1 {
		t.Fatalf("response exports[1] = %+v, want the locks grant reported in full", got.Pipelines.Exports[1])
	}
}

// TestTeamHandlers_GetTeam_ReaderWithoutTenantRight_Forbidden asserts task
// 8.3's read gate: a caller holding no CanReadPipeline grant on the Team's
// own tenant, and no administrative access, is refused.
func TestTeamHandlers_GetTeam_ReaderWithoutTenantRight_Forbidden(t *testing.T) {
	// A claims with no role binding at all: neither IsAdmin nor
	// CanReadPipeline on "payments" holds.
	client := newFakeResourceClient()
	noGrantStore := storeFromFake(client)
	crdstore.MustSeed(noGrantStore, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
	})
	authz := NewAuthorizer(testResolver(nil, nil), false)
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      noGrantStore,
		Logger:     slog.Default(),
		Authorizer: authz,
	})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "outsider"})

	req := httptest.NewRequest(http.MethodGet, "/teams/payments", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("GET status = %d, want 403", w.Result().StatusCode)
	}
}

// TestTeamHandlers_GetTeam_TenantReaderAllowed asserts the read side of the
// same gate: a caller holding CanReadPipeline on the Team's own tenant, but
// no administrative access, is allowed.
func TestTeamHandlers_GetTeam_TenantReaderAllowed(t *testing.T) {
	srv, store, ctx := teamPipelinesTenantTestServer(t, "reader", "pipelines-reader")
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Members: []string{"alice"}, Namespaces: []string{"ns1"},
			Pipelines: &v1alpha1.TeamPipelinesSpec{
				Profile: "lts", AgentNamespace: "ns1",
				Exports: []v1alpha1.TeamExport{{Job: "release/*", Callers: []string{"search"}}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/payments", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}
}

// TestTeamHandlers_UpdateExports_WriterWithoutPipelinesAdmin_Forbidden
// asserts task 8.3's write gate: a caller holding CanReadPipeline (read
// only, not admin) on the Team's own tenant is refused a write, and the
// stored Team is unchanged.
func TestTeamHandlers_UpdateExports_WriterWithoutPipelinesAdmin_Forbidden(t *testing.T) {
	srv, store, ctx := teamPipelinesTenantTestServer(t, "reader", "pipelines-reader")
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
	})

	body := `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1"],
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"exports": [{"job": "release/*", "callers": ["search"]}]
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("PUT status = %d, want 403; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	if stored.Spec.Pipelines != nil {
		t.Fatalf("stored Team gained spec.pipelines after a refused write: %+v", stored.Spec.Pipelines)
	}
}

// TestTeamHandlers_UpdateExports_TenantAdminAllowed asserts the write side
// of the same gate: a caller holding the tenant's pipelines:admin
// capability, but no administrative access, may write exports.
func TestTeamHandlers_UpdateExports_TenantAdminAllowed(t *testing.T) {
	srv, store, ctx := teamPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"ns1"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"},
		},
	})

	body := `{
		"name": "payments",
		"members": ["alice"],
		"namespaces": ["ns1"],
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"exports": [{"job": "release/*", "callers": ["search"]}]
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	if stored.Spec.Pipelines == nil || len(stored.Spec.Pipelines.Exports) != 1 ||
		stored.Spec.Pipelines.Exports[0].Job != "release/*" {
		t.Fatalf("stored exports = %+v, want the grant carried", stored.Spec.Pipelines)
	}
}

// A tenant pipelines admin who changes any field beside exports is
// refused: the tenant right reaches exports only, never members, roleRef,
// or namespaces.
func TestTeamHandlers_UpdateExports_TenantAdminCannotChangeOtherFields(t *testing.T) {
	srv, store, ctx := teamPipelinesTenantTestServer(t, "tenant-admin", "pipelines-admin")
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"ns1"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "lts", AgentNamespace: "ns1"},
		},
	})

	body := `{
		"name": "payments",
		"members": ["alice", "mallory"],
		"namespaces": ["ns1"],
		"roleRef": "operator",
		"pipelines": {
			"profile": "lts",
			"agentNamespace": "ns1",
			"exports": [{"job": "release/*", "callers": ["search"]}]
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body)))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleTeamDispatch(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("PUT status = %d, want 403; body=%s", w.Result().StatusCode, w.Body.String())
	}
	stored, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", "")
	if err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	if len(stored.Spec.Members) != 1 || stored.Spec.RoleRef != "" {
		t.Fatalf("stored spec changed: %+v", stored.Spec)
	}
}

// TestTeamHandlers_PipelinesRequiredFields asserts that a pipelines block
// missing a required field is refused with a 400 that names the field, and
// that nothing is written.
func TestTeamHandlers_PipelinesRequiredFields(t *testing.T) {
	const complete = `"pipelines": {"profile": "lts", "agentNamespace": "ns1"%s}`
	cases := []struct {
		name     string
		pipeline string
		wantMsg  string
	}{
		{"no profile", `"pipelines": {"agentNamespace": "ns1"}`, "pipelines.profile"},
		{"no agentNamespace", `"pipelines": {"profile": "lts"}`, "pipelines.agentNamespace"},
		{"agentNamespace outside namespaces", `"pipelines": {"profile": "lts", "agentNamespace": "other"}`, "must be one of namespaces"},
		{"export without callers", `"pipelines": {"profile": "lts", "agentNamespace": "ns1", "exports": [{"job": "a/*"}]}`, "pipelines.exports[0].callers"},
	}
	for _, tc := range cases {
		t.Run("POST "+tc.name, func(t *testing.T) {
			srv, store, ctx := adminTeamTestServer(t)
			body := `{"name": "payments", "members": ["alice"], "namespaces": ["ns1"], ` + tc.pipeline + `}`
			req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(body))).WithContext(ctx)
			w := httptest.NewRecorder()
			srv.HandleTeams(w, req)
			if w.Result().StatusCode != http.StatusBadRequest {
				t.Fatalf("POST status = %d, want 400; body=%s", w.Result().StatusCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Errorf("POST body = %s, want it to name %q", w.Body.String(), tc.wantMsg)
			}
			if _, err := crdstore.Get[v1alpha1.Team](context.Background(), store, "payments", ""); err == nil {
				t.Error("refused POST stored a Team")
			}
		})
		t.Run("PUT "+tc.name, func(t *testing.T) {
			srv, store, ctx := adminTeamTestServer(t)
			crdstore.MustSeed(store, &v1alpha1.Team{
				ObjectMeta: metav1.ObjectMeta{Name: "payments"},
				Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, Namespaces: []string{"ns1"}},
			})
			body := `{"name": "payments", "members": ["alice"], "namespaces": ["ns1"], ` + tc.pipeline + `}`
			req := httptest.NewRequest(http.MethodPut, "/teams/payments", bytes.NewReader([]byte(body))).WithContext(ctx)
			w := httptest.NewRecorder()
			srv.HandleTeamDispatch(w, req)
			if w.Result().StatusCode != http.StatusBadRequest {
				t.Fatalf("PUT status = %d, want 400; body=%s", w.Result().StatusCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Errorf("PUT body = %s, want it to name %q", w.Body.String(), tc.wantMsg)
			}
		})
	}

	t.Run("complete block is accepted", func(t *testing.T) {
		srv, _, ctx := adminTeamTestServer(t)
		body := `{"name": "payments", "members": ["alice"], "namespaces": ["ns1"], ` +
			strings.Replace(complete, "%s", `, "exports": [{"job": "a/*", "callers": ["ops"]}]`, 1) + `}`
		req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleTeams(w, req)
		if w.Result().StatusCode != http.StatusCreated {
			t.Fatalf("POST status = %d, want 201; body=%s", w.Result().StatusCode, w.Body.String())
		}
	})
}

// TestTeamHandlers_ApiserverInvalidAnswers400 asserts that an apiserver
// validation refusal becomes a 400 with its message, and that any other
// store failure stays a 500.
func TestTeamHandlers_ApiserverInvalidAnswers400(t *testing.T) {
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	body := `{"name": "payments", "members": ["alice"], "namespaces": ["ns1"], "pipelines": {"profile": "lts", "agentNamespace": "ns1"}}`

	t.Run("Invalid becomes 400", func(t *testing.T) {
		srv, store, ctx := adminTeamTestServer(t)
		store.FailNext("create", gvr, apierrors.NewInvalid(
			schema.GroupKind{Group: "varroa.dev", Kind: "Team"}, "payments",
			field.ErrorList{field.Required(field.NewPath("spec", "pipelines", "profile"), "")}))
		req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleTeams(w, req)
		if w.Result().StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", w.Result().StatusCode, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "spec.pipelines.profile") {
			t.Errorf("body = %s, want it to name spec.pipelines.profile", w.Body.String())
		}
	})

	t.Run("other store failure stays 500", func(t *testing.T) {
		srv, store, ctx := adminTeamTestServer(t)
		store.FailNext("create", gvr, errors.New("boom"))
		req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleTeams(w, req)
		if w.Result().StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%s", w.Result().StatusCode, w.Body.String())
		}
	})
}
