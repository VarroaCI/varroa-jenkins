package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
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
}

func (f *scopeTestConfigBrood) ListComposedBundles(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) GetComposedBundle(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return nil, nil
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
	return nil, f.operatorNs, nil
}
func (f *scopeTestConfigBrood) GetCatalogItem(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) ListCatalogSources(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *scopeTestConfigBrood) GetCatalogSource(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return nil, nil
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
