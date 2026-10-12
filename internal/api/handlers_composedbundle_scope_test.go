package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// scopeTestConfigBrood is a ConfigBrood that records calls.
type scopeTestConfigBrood struct {
	createCalled  bool
	updateCalled  bool
	deleteCalled  bool
	composeCalled bool
	operatorNs    string

	createCatalogSourceCalled bool
	updateCatalogSourceCalled bool
	deleteCatalogSourceCalled bool
	syncCatalogSourceCalled   bool

	// bundles, sources and items are returned by the List* methods.
	bundles []json.RawMessage
	sources []json.RawMessage
	items   []json.RawMessage
}

// scopeTestObject returns a minimal object that carries the namespace and
// name the Get* methods were called with.
func scopeTestObject(ns, name string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"metadata": map[string]string{"namespace": ns, "name": name}})
	return b
}

func (f *scopeTestConfigBrood) ListComposedBundles(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	return f.bundles, nil
}
func (f *scopeTestConfigBrood) GetComposedBundle(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return scopeTestObject(ns, name), nil
}
func (f *scopeTestConfigBrood) CreateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.createCalled = true
	return obj, nil
}
func (f *scopeTestConfigBrood) UpdateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.updateCalled = true
	return obj, nil
}
func (f *scopeTestConfigBrood) DeleteComposedBundle(ctx context.Context, cluster, ns, name string) error {
	f.deleteCalled = true
	return nil
}
func (f *scopeTestConfigBrood) PauseComposedBundle(ctx context.Context, cluster, ns, name string, paused bool) error {
	return nil
}
func (f *scopeTestConfigBrood) ComposeBundle(ctx context.Context, cluster, ns string, spec json.RawMessage) (*bus.BundleComposePreview, error) {
	f.composeCalled = true
	return &bus.BundleComposePreview{}, nil
}
func (f *scopeTestConfigBrood) ListCatalogItems(ctx context.Context, cluster, ns string, filter CatalogItemFilter) ([]json.RawMessage, string, error) {
	return f.items, f.operatorNs, nil
}
func (f *scopeTestConfigBrood) GetCatalogItem(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return scopeTestObject(ns, name), nil
}
func (f *scopeTestConfigBrood) ListCatalogSources(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	return f.sources, nil
}
func (f *scopeTestConfigBrood) GetCatalogSource(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return scopeTestObject(ns, name), nil
}
func (f *scopeTestConfigBrood) CreateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.createCatalogSourceCalled = true
	return obj, nil
}
func (f *scopeTestConfigBrood) UpdateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	f.updateCatalogSourceCalled = true
	return obj, nil
}
func (f *scopeTestConfigBrood) DeleteCatalogSource(ctx context.Context, cluster, ns, name string) error {
	f.deleteCatalogSourceCalled = true
	return nil
}
func (f *scopeTestConfigBrood) SyncCatalogSource(ctx context.Context, cluster, ns, name string) error {
	f.syncCatalogSourceCalled = true
	return nil
}
func (f *scopeTestConfigBrood) ListJenkinsRoles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) GetJenkinsRole(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) CreateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) UpdateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) DeleteJenkinsRole(ctx context.Context, cluster, name string) error {
	return nil
}
func (f *scopeTestConfigBrood) ListJenkinsRoleBindings(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) GetJenkinsRoleBinding(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) CreateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) UpdateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) DeleteJenkinsRoleBinding(ctx context.Context, cluster, name string) error {
	return nil
}

func (f *scopeTestConfigBrood) GetProvisioningDefaults(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) UpdateProvisioningDefaults(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *scopeTestConfigBrood) ListVersionProfiles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) GetVersionProfile(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) CreateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *scopeTestConfigBrood) UpdateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *scopeTestConfigBrood) DeleteVersionProfile(ctx context.Context, cluster, name string) error {
	return nil
}
func (f *scopeTestConfigBrood) ViewVersionProfiles(ctx context.Context, cluster string) ([]bus.VersionProfileView, error) {
	return nil, nil
}

// TestDispatchComposedBundles_ClusterScoped verifies that the cluster-scoped
// composedbundle dispatch routes correctly with a fake ConfigBrood.
func TestDispatchComposedBundles_ClusterScoped(t *testing.T) {
	brood := &scopeTestConfigBrood{}
	srv := NewServer(&Dependencies{
		Authorizer:  adminTestAuthorizer(),
		ConfigBrood: brood,
		Logger:      slog.Default(),
	})

	body, _ := json.Marshal(map[string]string{"name": "test"})

	// Create a bundle on "core" cluster
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/ns1", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"ns1"})
	if w.Code != http.StatusCreated {
		t.Errorf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if !brood.createCalled {
		t.Error("ConfigBrood.CreateComposedBundle was not called")
	}

	// Validate
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/validate?namespace=ns1", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"validate"})
	if w.Code != http.StatusOK {
		t.Errorf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// scopedBundleReaderAuthorizer builds an Authorizer for the dev-user caller
// whose developer binding is scoped to team-a and grants composedbundles read.
func scopedBundleReaderAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: []string{"composedbundles"}, Verbs: []string{"read", "create", "update"}},
			}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-developer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "dev-user"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	return NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

// scopedBundleListNamespaces returns the namespaces of the bundles in a list
// response body.
func scopedBundleListNamespaces(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode list %s: %v", body, err)
	}
	out := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		out = append(out, it.Metadata.Namespace)
	}
	return out
}

func scopeTestBundleRaw(ns, name string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"metadata": map[string]string{"namespace": ns, "name": name}})
	return b
}

func TestDispatchComposedBundles_ScopedDeveloper_ListFiltersNamespaces(t *testing.T) {
	brood := &scopeTestConfigBrood{
		operatorNs: "varroa-system",
		bundles: []json.RawMessage{
			scopeTestBundleRaw("team-a", "a"),
			scopeTestBundleRaw("team-b", "b"),
			scopeTestBundleRaw("varroa-system", "starter"),
		},
	}
	srv := NewServer(&Dependencies{
		Authorizer:        scopedBundleReaderAuthorizer(),
		ConfigBrood:       brood,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/composedbundles", nil)
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := scopedBundleListNamespaces(t, w.Body.Bytes())
	if len(got) != 2 || got[0] != "team-a" || got[1] != "varroa-system" {
		t.Errorf("list namespaces = %v, want [team-a varroa-system]", got)
	}
}

func TestDispatchComposedBundles_ScopedDeveloper_GetOtherNamespaceForbidden(t *testing.T) {
	srv := NewServer(&Dependencies{
		Authorizer:        scopedBundleReaderAuthorizer(),
		ConfigBrood:       &scopeTestConfigBrood{},
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/composedbundles/team-b/b", nil)
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), scopedDevClaims)), "core", []string{"team-b", "b"})

	if w.Code != http.StatusForbidden {
		t.Errorf("get other-ns: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDispatchComposedBundles_Admin_ListSeesAll(t *testing.T) {
	brood := &scopeTestConfigBrood{
		bundles: []json.RawMessage{
			scopeTestBundleRaw("team-a", "a"),
			scopeTestBundleRaw("team-b", "b"),
		},
	}
	srv := NewServer(&Dependencies{
		Authorizer:  adminTestAuthorizer(),
		ConfigBrood: brood,
		Logger:      slog.Default(),
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/composedbundles", nil)
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := scopedBundleListNamespaces(t, w.Body.Bytes()); len(got) != 2 {
		t.Errorf("admin list namespaces = %v, want both namespaces", got)
	}
}
