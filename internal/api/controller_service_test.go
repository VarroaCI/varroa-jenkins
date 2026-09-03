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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := testSvc(crdstore.NewFake())
			svc.DashboardHost = tt.dashboard
			serr := svc.ValidateIngress(tt.spec, tt.forCreate, tt.cluster, "ctl")
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
	resp := s.controllerDetail(cr, "core")
	if resp.Attention == nil {
		t.Fatal("detail response carries no attention")
	}
	if resp.Attention.Kind != AttentionReconcileBlocked {
		t.Fatalf("Attention.Kind = %q, want %q", resp.Attention.Kind, AttentionReconcileBlocked)
	}
}
