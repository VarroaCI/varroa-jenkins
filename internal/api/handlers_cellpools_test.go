package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellPoolTestServer builds a server whose caller holds cellpools:read and
// cellpools:update through a cluster-wide binding, the shape both gates
// require.
func cellPoolTestServer(t *testing.T, scope *v1alpha1.VarroaRoleBindingScope) (*Server, *crdstore.Fake, context.Context) {
	t.Helper()
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"cellpools"}, Verbs: []string{"read", "update"}}},
		},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-operator"}},
			RoleRef:  "operator",
			Scope:    scope,
		},
	}
	authz := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding}), false)

	client := newFakeResourceClient()
	store := storeFromFake(client)
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      store,
		Logger:     slog.Default(),
		Authorizer: authz,
	})
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "test-operator"})
	return srv, store, ctx
}

func seedCellPool(store *crdstore.Fake, name string, warm int32) {
	crdstore.MustSeed(store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:        "lts-2-555",
			Warm:              warm,
			MaxCells:          6,
			MaxTenantsPerCell: 10,
		},
	})
}

// TestCellPools_ListSortedByName covers "An operator lists the pools".
func TestCellPools_ListSortedByName(t *testing.T) {
	srv, store, ctx := cellPoolTestServer(t, nil)
	seedCellPool(store, "lts-2-555", 1)
	seedCellPool(store, "big", 2)

	req := httptest.NewRequest(http.MethodGet, "/cellpools", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPools(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	var got struct {
		Items []cellPoolEntry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	if got.Items[0].Name != "big" || got.Items[1].Name != "lts-2-555" {
		t.Fatalf("items must sort by name, got %s then %s", got.Items[0].Name, got.Items[1].Name)
	}
	if got.Items[0].Spec.ProfileRef != "lts-2-555" || got.Items[0].Spec.Warm != 2 {
		t.Errorf("spec = %+v, want the stored values", got.Items[0].Spec)
	}
}

// TestCellPools_NamespaceScopedBindingRefused covers "A namespace-scoped
// binding does not satisfy the gate".
func TestCellPools_NamespaceScopedBindingRefused(t *testing.T) {
	srv, store, ctx := cellPoolTestServer(t, &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}})
	seedCellPool(store, "lts-2-555", 1)

	req := httptest.NewRequest(http.MethodGet, "/cellpools", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPools(w, req)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Result().StatusCode)
	}
}

// TestCellPools_MissingPoolIsA404 covers "A missing pool is a 404".
func TestCellPools_MissingPoolIsA404(t *testing.T) {
	srv, _, ctx := cellPoolTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/cellpools/nope", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Result().StatusCode)
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := env["error"]; !ok {
		t.Errorf("response must use the uniform error envelope, got %v", env)
	}
}

// TestCellPools_ScaleEditAccepted covers "A scale edit is accepted": the
// named dial changes and every other spec field stays.
func TestCellPools_ScaleEditAccepted(t *testing.T) {
	srv, store, ctx := cellPoolTestServer(t, nil)
	seedCellPool(store, "lts-2-555", 1)

	body := []byte(`{"spec": {"warm": 3}}`)
	req := httptest.NewRequest(http.MethodPatch, "/cellpools/lts-2-555", bytes.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Result().StatusCode, w.Body.String())
	}

	stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
	if err != nil {
		t.Fatalf("get stored pool: %v", err)
	}
	if stored.Spec.Warm != 3 {
		t.Errorf("warm = %d, want 3", stored.Spec.Warm)
	}
	if stored.Spec.ProfileRef != "lts-2-555" || stored.Spec.MaxCells != 6 || stored.Spec.MaxTenantsPerCell != 10 {
		t.Errorf("every other spec field must be unchanged, got %+v", stored.Spec)
	}
}

// TestCellPools_StructuralEditRefused covers "A structural edit is refused"
// and the unknown-key rule.
func TestCellPools_StructuralEditRefused(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "profileRef", body: `{"spec": {"profileRef": "x"}}`},
		{name: "cellTemplate", body: `{"spec": {"cellTemplate": {}}}`},
		{name: "unknown key", body: `{"spec": {"nope": 1}}`},
		{name: "no recognised key", body: `{"spec": {}}`},
		{name: "no spec", body: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, store, ctx := cellPoolTestServer(t, nil)
			seedCellPool(store, "lts-2-555", 1)

			req := httptest.NewRequest(http.MethodPatch, "/cellpools/lts-2-555",
				bytes.NewReader([]byte(tt.body))).WithContext(ctx)
			w := httptest.NewRecorder()
			srv.HandleCellPoolDispatch(w, req)
			if w.Result().StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Result().StatusCode, w.Body.String())
			}
			stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
			if err != nil {
				t.Fatalf("get stored pool: %v", err)
			}
			if stored.Spec.Warm != 1 || stored.Spec.ProfileRef != "lts-2-555" {
				t.Errorf("a refused patch must write nothing, got %+v", stored.Spec)
			}
		})
	}
}

// TestCellPools_OutOfRangeDialRefused covers "An out-of-range dial is refused
// before the write": the BFF repeats the CRD's numeric checks, so a bad
// PATCH returns the uniform 400 envelope instead of a raw apiserver 422.
func TestCellPools_OutOfRangeDialRefused(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "negative warm", body: `{"spec": {"warm": -1}}`},
		{name: "negative maxCells", body: `{"spec": {"maxCells": -1}}`},
		{name: "zero maxTenantsPerCell", body: `{"spec": {"maxTenantsPerCell": 0}}`},
		{name: "cap below warm", body: `{"spec": {"maxCells": 1}}`},
		{name: "unparsable duration", body: `{"spec": {"unreadyGracePeriod": "soon"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, store, ctx := cellPoolTestServer(t, nil)
			seedCellPool(store, "lts-2-555", 4)

			req := httptest.NewRequest(http.MethodPatch, "/cellpools/lts-2-555",
				bytes.NewReader([]byte(tt.body))).WithContext(ctx)
			w := httptest.NewRecorder()
			srv.HandleCellPoolDispatch(w, req)
			if w.Result().StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Result().StatusCode, w.Body.String())
			}
			stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "lts-2-555", "")
			if err != nil {
				t.Fatalf("get stored pool: %v", err)
			}
			if stored.Spec.Warm != 4 {
				t.Errorf("a refused patch must write nothing, got warm %d", stored.Spec.Warm)
			}
		})
	}
}

// TestCellPools_ConcurrentWriteRetriedThen409 covers "A concurrent write is
// retried": the handler answers 409 only after three failed attempts.
func TestCellPools_ConcurrentWriteRetriedThen409(t *testing.T) {
	srv, store, ctx := cellPoolTestServer(t, nil)
	seedCellPool(store, "lts-2-555", 1)

	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	store.FailAlways("update", gvr, apierrors.NewConflict(
		schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, "lts-2-555", nil))

	body := []byte(`{"spec": {"warm": 3}}`)
	req := httptest.NewRequest(http.MethodPatch, "/cellpools/lts-2-555", bytes.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.HandleCellPoolDispatch(w, req)
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Result().StatusCode, w.Body.String())
	}
}

// TestCellPools_NoCreateRoute covers "Creating a pool over the API is not
// possible": the collection handler serves GET alone.
func TestCellPools_NoCreateRoute(t *testing.T) {
	srv, _, ctx := cellPoolTestServer(t, nil)

	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(method, "/cellpools", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleCellPools(w, req)
		if w.Result().StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /cellpools: status = %d, want 405", method, w.Result().StatusCode)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(method, "/cellpools/lts-2-555", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.HandleCellPoolDispatch(w, req)
		if w.Result().StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /cellpools/{name}: status = %d, want 405", method, w.Result().StatusCode)
		}
	}
}

// The read path carries all three cellTemplate keys, miteResources included.
func TestCellPoolEntryFrom_CarriesMiteResources(t *testing.T) {
	pool := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lts-2-555"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef: "lts-2-555",
			CellTemplate: &v1alpha1.CellTemplate{
				Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
				},
				MiteResources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				PodOverrides: &v1alpha1.PodOverrides{JvmOpts: "-XX:MaxRAMPercentage=50"},
			},
		},
	}
	tmpl := cellPoolEntryFrom(pool).Spec.CellTemplate
	if tmpl == nil {
		t.Fatal("cellTemplate missing from the read")
	}
	if got := string(tmpl.MiteResources); got != `{"limits":{"memory":"128Mi"}}` {
		t.Errorf("miteResources = %s", got)
	}
	if got := string(tmpl.Resources); got != `{"limits":{"memory":"6Gi"}}` {
		t.Errorf("resources = %s", got)
	}
	if got := string(tmpl.PodOverrides); got != `{"jvmOpts":"-XX:MaxRAMPercentage=50"}` {
		t.Errorf("podOverrides = %s", got)
	}
}

// TestCellPoolEntryFrom_CarriesLibraries checks that the read projection
// carries each pool library's declaration, and the credential Secret name
// only.
func TestCellPoolEntryFrom_CarriesLibraries(t *testing.T) {
	pool := &v1alpha1.CellPool{
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef: "lts-2-555",
			Libraries: []v1alpha1.CellPoolLibrary{{
				Name:           "platform-lib",
				Remote:         "ssh://git@example.com/platform.git",
				DefaultVersion: "main",
				CredentialRef:  "platform-lib-key",
				Implicit:       true,
			}},
		},
	}
	libs := cellPoolEntryFrom(pool).Spec.Libraries
	if len(libs) != 1 {
		t.Fatalf("libraries = %d, want 1", len(libs))
	}
	got := libs[0]
	if got.Name != "platform-lib" || got.CredentialRef != "platform-lib-key" || !got.Implicit || got.AllowVersionOverride {
		t.Fatalf("library entry = %+v", got)
	}
}
