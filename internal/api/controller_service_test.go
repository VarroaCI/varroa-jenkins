package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func testSvc(store *crdstore.Fake) *ControllerService {
	return &ControllerService{
		Store:        store,
		LocalCluster: "core",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestControllerService_ValidateIngress(t *testing.T) {
	tests := []struct {
		name       string
		spec       *v1alpha1.IngressSpec
		forCreate  bool
		cluster    string
		dashboard  string
		headless   bool
		wantStatus int // 0 = accepted
	}{
		{name: "nil spec accepted"},
		{name: "bad mode rejected", spec: &v1alpha1.IngressSpec{Mode: "wildcard"}, wantStatus: http.StatusBadRequest},
		{name: "subdomain accepted", spec: &v1alpha1.IngressSpec{Mode: "subdomain", Host: "x.example.org"}},
		{
			name: "path mode on remote cluster rejected at create",
			spec: &v1alpha1.IngressSpec{Mode: "path", Host: "dash.example.org"}, forCreate: true, cluster: "member",
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "path mode host must match dashboard at create",
			spec: &v1alpha1.IngressSpec{Mode: "path", Host: "other.example.org"}, forCreate: true, cluster: "core", dashboard: "dash.example.org",
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "path mode local with matching host accepted",
			spec: &v1alpha1.IngressSpec{Mode: "path", Host: "dash.example.org"}, forCreate: true, cluster: "core", dashboard: "dash.example.org",
		},
		{
			name: "path mode not gated on update",
			spec: &v1alpha1.IngressSpec{Mode: "path", Host: "anything.example.org"}, forCreate: false, cluster: "member", dashboard: "dash.example.org",
		},
		{
			name: "headless skips the shared-host check",
			spec: &v1alpha1.IngressSpec{Mode: "path", Host: "other.example.org"}, forCreate: true, cluster: "core", dashboard: "dash.example.org",
			headless: true,
		},
		{
			name: "headless still rejects a bad mode",
			spec: &v1alpha1.IngressSpec{Mode: "wildcard"}, headless: true,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := testSvc(crdstore.NewFake())
			svc.DashboardHost = tt.dashboard
			serr := svc.ValidateIngress(tt.spec, tt.forCreate, tt.cluster, "ctl", tt.headless)
			if tt.wantStatus == 0 && serr != nil {
				t.Fatalf("unexpected rejection: %v", serr)
			}
			if tt.wantStatus != 0 && (serr == nil || serr.Status != tt.wantStatus) {
				t.Fatalf("serr = %+v, want status %d", serr, tt.wantStatus)
			}
		})
	}
}

func TestControllerService_ValidateBundleRef(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "team-a"},
	})
	svc := testSvc(store)
	ctx := context.Background()

	if serr := svc.ValidateBundleRef(ctx, nil, "team-a"); serr != nil {
		t.Fatalf("nil ref must pass, got %v", serr)
	}
	if serr := svc.ValidateBundleRef(ctx, &v1alpha1.ComposedBundleRef{Name: "b1"}, "team-a"); serr != nil {
		t.Fatalf("existing bundle in caller namespace must pass, got %v", serr)
	}
	if serr := svc.ValidateBundleRef(ctx, &v1alpha1.ComposedBundleRef{Name: "b1", Namespace: "team-a"}, "elsewhere"); serr != nil {
		t.Fatalf("explicit namespace must win over caller namespace, got %v", serr)
	}
	serr := svc.ValidateBundleRef(ctx, &v1alpha1.ComposedBundleRef{Name: "missing"}, "team-a")
	if serr == nil || serr.Status != http.StatusBadRequest {
		t.Fatalf("missing bundle must 400, got %+v", serr)
	}
}

// TestControllerUpdate_MergePatchContentTypeStillRoutesToBrood (5.7): the
// frontend sends Content-Type: application/merge-patch+json, but the content
// type does not decide semantics — the handler decodes the JSON body and
// forwards it to the bus. The local direct-apply fallback is gone, so this
// also pins that a PATCH routes through Brood.Update with the spec untouched.
func TestControllerUpdate_MergePatchContentTypeStillRoutesToBrood(t *testing.T) {
	base := newFakeResourceClient()
	base.controllers = map[string]*v1alpha1.Controller{}
	base.namespaces["team-a"] = true
	base.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "lts"},
	}
	brood := newFakeBrood(base).(*fakeBrood)
	srv := NewServer(&Dependencies{
		Client:            base,
		Store:             storeFromFake(base),
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		Brood:             brood,
	})

	adminClaims := &auth.Claims{Subject: "admin", Groups: []string{"admins"}}
	req := httptest.NewRequest(http.MethodPatch, "/controllers/team-a/ci",
		strings.NewReader(`{"spec":{"resources":{"requests":{"cpu":"100m"}}}}`))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	srv.handleUpdateController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var sent map[string]any
	if err := json.Unmarshal(brood.updatePatch, &sent); err != nil {
		t.Fatalf("unmarshal patch sent to Brood: %v", err)
	}
	got, _ := json.Marshal(sent["spec"])
	want, _ := json.Marshal(map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}}})
	if string(got) != string(want) {
		t.Fatalf("spec forwarded to Brood = %s, want %s", got, want)
	}
}

// TestControllerUpdate_UnappliedRemovalsReportedViaBrood (5.7): the handler
// surfaces the unappliedRemovals the operator reports. With the local
// direct-apply fallback deleted, there is a single route — the bus — so this
// replaces the old brood-vs-local parity test.
func TestControllerUpdate_UnappliedRemovalsReportedViaBrood(t *testing.T) {
	bb := newFakeResourceClient()
	bb.controllers = map[string]*v1alpha1.Controller{
		"ci": {
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{Version: "2.479"},
		},
	}
	bb.namespaces["team-a"] = true
	srv := NewServer(&Dependencies{
		Client:            bb,
		Store:             storeFromFake(bb),
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		Brood:             newFakeBrood(bb),
	})

	claims := &auth.Claims{Subject: "admin", Groups: []string{"admins"}}
	req := httptest.NewRequest(http.MethodPatch, "/controllers/team-a/ci",
		strings.NewReader(`{"spec":{"version":null}}`))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	srv.handleUpdateController(w, req.WithContext(contextWithClaims(req.Context(), claims)), "core", "team-a", "ci")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, _ := json.Marshal(body["unappliedRemovals"])
	if string(raw) != `[{"field":"spec.version"}]` {
		t.Fatalf("unappliedRemovals = %s, want %s", raw, `[{"field":"spec.version"}]`)
	}
}

// TestControllerDetail_CarriesAttention pins the detail builder to the same
// attention projection the list builder uses — they are separate code paths.
func TestControllerDetail_CarriesAttention(t *testing.T) {
	s := &Server{deps: &Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  crdstore.NewFake(),
	}}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns1"},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseProvisioning,
			Conditions: []v1alpha1.ControllerCondition{{
				Type:    v1alpha1.ConditionReconcileBlocked,
				Status:  metav1.ConditionTrue,
				Reason:  "PluginConflict",
				Message: "plugin kubernetes requested at A conflicts with profile lock B",
			}},
		},
	}
	resp := s.controllerDetail(context.Background(), cr, "core")
	if resp.Attention == nil {
		t.Fatal("detail response carries no attention")
	}
	if resp.Attention.Kind != AttentionReconcileBlocked {
		t.Fatalf("Attention.Kind = %q, want %q", resp.Attention.Kind, AttentionReconcileBlocked)
	}
}

func TestControllerDetail_CellProjection(t *testing.T) {
	s := &Server{deps: &Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  crdstore.NewFake(),
	}}
	unready := metav1.NewTime(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns1"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{
			Cell: &v1alpha1.CellStatus{
				Tenants:                  []string{"payments", "search"},
				AppliedTenants:           []string{"payments"},
				Warm:                     true,
				Evacuate:                 true,
				UnreadySince:             &unready,
				RolledAt:                 &unready,
				Instance:                 "uid-new",
				PreviousInstance:         "uid-old",
				PreviousInstanceLastSeen: &unready,
				TenantWarnings: []v1alpha1.TenantWarning{{
					Tenant: "search", Reason: "ItemsSkipped", Message: "bundle unreadable",
				}},
				Rehydration: []v1alpha1.CellRehydrationEntry{{Tenant: "payments"}},
			},
		},
	}
	resp := s.controllerDetail(context.Background(), cr, "core")
	if resp.Kind != "cell" {
		t.Fatalf("Kind = %q, want cell", resp.Kind)
	}
	if resp.Cell == nil {
		t.Fatal("detail response carries no cell projection")
	}
	if len(resp.Cell.Tenants) != 2 || resp.Cell.AppliedTenants[0] != "payments" {
		t.Errorf("tenants = %v, appliedTenants = %v", resp.Cell.Tenants, resp.Cell.AppliedTenants)
	}
	if !resp.Cell.Warm || !resp.Cell.Evacuate {
		t.Error("warm and evacuate did not survive the projection")
	}
	if resp.Cell.Instance != "uid-new" || resp.Cell.PreviousInstance != "uid-old" {
		t.Errorf("instances = %q, %q", resp.Cell.Instance, resp.Cell.PreviousInstance)
	}
	if resp.Cell.UnreadySince != "2026-02-03T04:05:06Z" {
		t.Errorf("unreadySince = %q", resp.Cell.UnreadySince)
	}
	if len(resp.Cell.TenantWarnings) != 1 || resp.Cell.TenantWarnings[0].Reason != "ItemsSkipped" {
		t.Errorf("tenantWarnings = %v", resp.Cell.TenantWarnings)
	}

	// The projection declares ten keys and no rehydration key.
	raw, err := json.Marshal(resp.Cell)
	if err != nil {
		t.Fatalf("marshal cell: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode cell: %v", err)
	}
	if _, ok := keys["rehydration"]; ok {
		t.Error("the cell projection carries a rehydration key")
	}
	want := []string{
		"tenants", "warm", "evacuate", "unreadySince", "rolledAt",
		"appliedTenants", "tenantWarnings", "instance", "previousInstance",
		"previousInstanceLastSeen",
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("the cell projection carries no %q key", k)
		}
	}
	if len(keys) != len(want) {
		t.Errorf("the cell projection carries %d keys, want %d", len(keys), len(want))
	}
}

func TestControllerDetail_StatefulCarriesNoCell(t *testing.T) {
	s := &Server{deps: &Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  crdstore.NewFake(),
	}}
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "ns1"}}
	resp := s.controllerDetail(context.Background(), cr, "core")
	if resp.Kind != "stateful" {
		t.Fatalf("Kind = %q, want stateful", resp.Kind)
	}
	if resp.Cell != nil {
		t.Error("a controller with no cell role carries a cell projection")
	}
}

// TestBuildCellStatusJSON_PendingSignatures proves the detail projection
// carries the pending signatures and their requesters.
func TestBuildCellStatusJSON_PendingSignatures(t *testing.T) {
	cr := &v1alpha1.Controller{
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{
			PendingSignatures: []v1alpha1.CellPendingSignature{{
				Signature: "method a b",
				Dangerous: true,
				Requesters: []v1alpha1.CellSignatureRequester{
					{Tenant: "team-a", JobPath: "team-a/build", BuildNumber: 3, RunID: "r9"},
				},
			}},
			PendingSignaturesTotal: 4,
		}},
	}
	out := buildCellStatusJSON(cr)
	if out == nil || len(out.PendingSignatures) != 1 {
		t.Fatalf("pending signatures not projected: %+v", out)
	}
	sig := out.PendingSignatures[0]
	if sig.Signature != "method a b" || !sig.Dangerous {
		t.Fatalf("signature = %+v", sig)
	}
	if len(sig.Requesters) != 1 || sig.Requesters[0].JobPath != "team-a/build" || sig.Requesters[0].RunID != "r9" {
		t.Fatalf("requesters = %+v", sig.Requesters)
	}
	if out.PendingSignaturesTotal != 4 {
		t.Fatalf("total = %d, want 4", out.PendingSignaturesTotal)
	}
}
