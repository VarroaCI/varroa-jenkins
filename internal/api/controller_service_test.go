package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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

func TestControllerService_ApplyUpdate_ShortCircuits(t *testing.T) {
	svc := testSvc(crdstore.NewFake())
	ctx := context.Background()

	t.Run("routing mode immutable", func(t *testing.T) {
		existing := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Mode: "subdomain"}}}
		updated := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{IngressSpec: &v1alpha1.IngressSpec{Mode: "path"}}}
		result, serr := svc.ApplyUpdate(ctx, existing, updated, nil, "ns", "ctl", false)
		if result != nil || serr == nil || serr.Status != http.StatusBadRequest || serr.Message != "ingressSpec.mode is immutable" {
			t.Fatalf("result=%v serr=%+v, want immutability 400", result, serr)
		}
	})
	t.Run("missing bundle rejected before preflight", func(t *testing.T) {
		existing := &v1alpha1.Controller{}
		updated := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "ghost"},
		}}
		result, serr := svc.ApplyUpdate(ctx, existing, updated, nil, "ns", "ctl", false)
		if result != nil || serr == nil || serr.Status != http.StatusBadRequest {
			t.Fatalf("result=%v serr=%+v, want bundle 400", result, serr)
		}
	})
}
