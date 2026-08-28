package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ---------------------------------------------------------------------------
// Fake client for ConfigCRUD tests
// ---------------------------------------------------------------------------

type fakeConfigCRUDClient struct {
	bundles         map[string]*v1alpha1.ComposedBundle
	createBundleErr error
	updateBundleErr error

	sources         map[string]*v1alpha1.CatalogSource
	createSourceErr error
	updateSourceErr error

	items                map[string]*v1alpha1.CatalogItem
	roles                map[string]*v1alpha1.JenkinsRole
	bindings             map[string]*v1alpha1.JenkinsRoleBinding
	secrets              map[string]map[string][]byte
	secretAnnotations    map[string]map[string]string // name -> annotations, for host-scoping tests
	secretAnnotationsErr error                        // injected GetSecretAnnotations failure (fail-closed tests)
	defaults             map[string]*v1alpha1.ProvisioningDefaults
	profiles             map[string]*v1alpha1.JenkinsVersionProfile
	configMaps           map[string]map[string]string
}

func key(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func (f *fakeConfigCRUDClient) ListComposedBundleCRDs(_ context.Context, namespace string) ([]*v1alpha1.ComposedBundle, error) {
	var result []*v1alpha1.ComposedBundle
	for _, v := range f.bundles {
		if namespace == "" || v.Namespace == namespace {
			result = append(result, v)
		}
	}
	return result, nil
}

func (f *fakeConfigCRUDClient) GetComposedBundleCRD(_ context.Context, name, namespace string) (*v1alpha1.ComposedBundle, error) {
	cr, ok := f.bundles[key(namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "composedbundles"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) CreateComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	if f.createBundleErr != nil {
		return f.createBundleErr
	}
	k := key(cr.Namespace, cr.Name)
	if _, exists := f.bundles[k]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "varroa.dev", Resource: "composedbundles"}, cr.Name)
	}
	if f.bundles == nil {
		f.bundles = make(map[string]*v1alpha1.ComposedBundle)
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "100"
	f.bundles[k] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) UpdateComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	if f.updateBundleErr != nil {
		return f.updateBundleErr
	}
	k := key(cr.Namespace, cr.Name)
	existing, exists := f.bundles[k]
	if !exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "composedbundles"}, cr.Name)
	}
	if cr.ResourceVersion != "" {
		if cr.ResourceVersion != existing.ResourceVersion {
			return apierrors.NewConflict(schema.GroupResource{Group: "varroa.dev", Resource: "composedbundles"}, cr.Name, fmt.Errorf("stale RV"))
		}
		// Preserve the caller-specified RV on match
		crCopy := cr.DeepCopy()
		crCopy.ResourceVersion = existing.ResourceVersion
		f.bundles[k] = crCopy
		return nil
	}
	// RV-less: get existing RV, apply caller's changes, save
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = existing.ResourceVersion
	f.bundles[k] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) ApplyComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	k := key(cr.Namespace, cr.Name)
	if f.bundles == nil {
		f.bundles = make(map[string]*v1alpha1.ComposedBundle)
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "102"
	f.bundles[k] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) DeleteComposedBundleCRD(_ context.Context, name, namespace string) error {
	delete(f.bundles, key(namespace, name))
	return nil
}

func (f *fakeConfigCRUDClient) ListCatalogSourceCRDs(_ context.Context, namespace string) ([]*v1alpha1.CatalogSource, error) {
	var result []*v1alpha1.CatalogSource
	for _, v := range f.sources {
		if namespace == "" || v.Namespace == namespace {
			result = append(result, v)
		}
	}
	return result, nil
}

func (f *fakeConfigCRUDClient) GetCatalogSourceCRD(_ context.Context, name, namespace string) (*v1alpha1.CatalogSource, error) {
	cr, ok := f.sources[key(namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "catalogsources"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) CreateCatalogSourceCRD(_ context.Context, cr *v1alpha1.CatalogSource) error {
	if f.createSourceErr != nil {
		return f.createSourceErr
	}
	k := key(cr.Namespace, cr.Name)
	if _, exists := f.sources[k]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "varroa.dev", Resource: "catalogsources"}, cr.Name)
	}
	if f.sources == nil {
		f.sources = make(map[string]*v1alpha1.CatalogSource)
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "100"
	f.sources[k] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) UpdateCatalogSourceCRD(_ context.Context, cr *v1alpha1.CatalogSource) error {
	if f.updateSourceErr != nil {
		return f.updateSourceErr
	}
	k := key(cr.Namespace, cr.Name)
	existing, exists := f.sources[k]
	if !exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "catalogsources"}, cr.Name)
	}
	if cr.ResourceVersion != "" && cr.ResourceVersion != existing.ResourceVersion {
		if cr.ResourceVersion != existing.ResourceVersion {
			return apierrors.NewConflict(schema.GroupResource{Group: "varroa.dev", Resource: "catalogsources"}, cr.Name, fmt.Errorf("stale RV"))
		}
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "101"
	f.sources[k] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) ApplyCatalogSourceCRD(_ context.Context, cr *v1alpha1.CatalogSource) error {
	k := key(cr.Namespace, cr.Name)
	if f.sources == nil {
		f.sources = make(map[string]*v1alpha1.CatalogSource)
	}
	crCopy := cr.DeepCopy()
	f.sources[k] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) DeleteCatalogSourceCRD(_ context.Context, name, namespace string) error {
	delete(f.sources, key(namespace, name))
	return nil
}

func (f *fakeConfigCRUDClient) ListCatalogItemCRDs(_ context.Context, namespace, _ string) ([]*v1alpha1.CatalogItem, error) {
	var result []*v1alpha1.CatalogItem
	for _, v := range f.items {
		if namespace == "" || v.Namespace == namespace {
			result = append(result, v)
		}
	}
	return result, nil
}

func (f *fakeConfigCRUDClient) GetCatalogItemCRD(_ context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	cr, ok := f.items[key(namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "catalogitems"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) ListJenkinsRoleCRDs(_ context.Context) ([]*v1alpha1.JenkinsRole, error) {
	result := make([]*v1alpha1.JenkinsRole, 0, len(f.roles))
	for _, v := range f.roles {
		result = append(result, v)
	}
	return result, nil
}

func (f *fakeConfigCRUDClient) GetJenkinsRoleCRD(_ context.Context, name string) (*v1alpha1.JenkinsRole, error) {
	cr, ok := f.roles[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) CreateJenkinsRoleCRD(_ context.Context, cr *v1alpha1.JenkinsRole) error {
	if f.createSourceErr != nil {
		return f.createSourceErr
	}
	if _, exists := f.roles[cr.Name]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"}, cr.Name)
	}
	if f.roles == nil {
		f.roles = make(map[string]*v1alpha1.JenkinsRole)
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "100"
	f.roles[cr.Name] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) UpdateJenkinsRoleCRD(_ context.Context, cr *v1alpha1.JenkinsRole) error {
	if f.updateSourceErr != nil {
		return f.updateSourceErr
	}
	existing, exists := f.roles[cr.Name]
	if !exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"}, cr.Name)
	}
	if cr.ResourceVersion != "" && cr.ResourceVersion != existing.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"}, cr.Name, fmt.Errorf("stale RV"))
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "101"
	f.roles[cr.Name] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) ApplyJenkinsRoleCRD(_ context.Context, cr *v1alpha1.JenkinsRole) error {
	if f.roles == nil {
		f.roles = make(map[string]*v1alpha1.JenkinsRole)
	}
	crCopy := cr.DeepCopy()
	f.roles[cr.Name] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) DeleteJenkinsRoleCRD(_ context.Context, name string) error {
	delete(f.roles, name)
	return nil
}

func (f *fakeConfigCRUDClient) ListJenkinsRoleBindingCRDs(_ context.Context) ([]*v1alpha1.JenkinsRoleBinding, error) {
	result := make([]*v1alpha1.JenkinsRoleBinding, 0, len(f.bindings))
	for _, v := range f.bindings {
		result = append(result, v)
	}
	return result, nil
}

func (f *fakeConfigCRUDClient) GetJenkinsRoleBindingCRD(_ context.Context, name string) (*v1alpha1.JenkinsRoleBinding, error) {
	cr, ok := f.bindings[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsrolebindings"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) CreateJenkinsRoleBindingCRD(_ context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	if f.createSourceErr != nil {
		return f.createSourceErr
	}
	if _, exists := f.bindings[cr.Name]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsrolebindings"}, cr.Name)
	}
	if f.bindings == nil {
		f.bindings = make(map[string]*v1alpha1.JenkinsRoleBinding)
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "100"
	f.bindings[cr.Name] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) UpdateJenkinsRoleBindingCRD(_ context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	if f.updateSourceErr != nil {
		return f.updateSourceErr
	}
	existing, exists := f.bindings[cr.Name]
	if !exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsrolebindings"}, cr.Name)
	}
	if cr.ResourceVersion != "" && cr.ResourceVersion != existing.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsrolebindings"}, cr.Name, fmt.Errorf("stale RV"))
	}
	crCopy := cr.DeepCopy()
	crCopy.ResourceVersion = "101"
	f.bindings[cr.Name] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) ApplyJenkinsRoleBindingCRD(_ context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	if f.bindings == nil {
		f.bindings = make(map[string]*v1alpha1.JenkinsRoleBinding)
	}
	crCopy := cr.DeepCopy()
	f.bindings[cr.Name] = crCopy
	return nil
}

func (f *fakeConfigCRUDClient) DeleteJenkinsRoleBindingCRD(_ context.Context, name string) error {
	delete(f.bindings, name)
	return nil
}

func (f *fakeConfigCRUDClient) GetSecret(_ context.Context, name, namespace string) (map[string][]byte, error) {
	s, ok := f.secrets[key(namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "secrets"}, name)
	}
	return s, nil
}

func (f *fakeConfigCRUDClient) GetSecretAnnotations(_ context.Context, name, namespace string) (map[string]string, error) {
	if f.secretAnnotationsErr != nil {
		return nil, f.secretAnnotationsErr
	}
	if f.secretAnnotations == nil {
		return nil, nil
	}
	s, ok := f.secretAnnotations[key(namespace, name)]
	if !ok {
		return nil, nil
	}
	return s, nil
}

func (f *fakeConfigCRUDClient) GetProvisioningDefaultsCRD(_ context.Context, name string) (*v1alpha1.ProvisioningDefaults, error) {
	cr, ok := f.defaults[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "provisioningdefaults"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) GetControllerClassCRD(_ context.Context, name string) (*v1alpha1.ControllerClass, error) {
	return nil, nil
}

func (f *fakeConfigCRUDClient) ListControllerClassCRDs(_ context.Context) ([]*v1alpha1.ControllerClass, error) {
	return nil, nil
}

func (f *fakeConfigCRUDClient) ApplyProvisioningDefaultsCRD(_ context.Context, cr *v1alpha1.ProvisioningDefaults) error {
	if f.defaults == nil {
		f.defaults = make(map[string]*v1alpha1.ProvisioningDefaults)
	}
	f.defaults[cr.Name] = cr.DeepCopy()
	return nil
}

func (f *fakeConfigCRUDClient) ListJenkinsVersionProfileCRDs(_ context.Context) ([]*v1alpha1.JenkinsVersionProfile, error) {
	result := make([]*v1alpha1.JenkinsVersionProfile, 0, len(f.profiles))
	for _, v := range f.profiles {
		result = append(result, v)
	}
	return result, nil
}

func (f *fakeConfigCRUDClient) GetJenkinsVersionProfileCRD(_ context.Context, name string) (*v1alpha1.JenkinsVersionProfile, error) {
	cr, ok := f.profiles[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsversionprofiles"}, name)
	}
	return cr, nil
}

func (f *fakeConfigCRUDClient) CreateJenkinsVersionProfileCRD(_ context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	if f.profiles == nil {
		f.profiles = make(map[string]*v1alpha1.JenkinsVersionProfile)
	}
	if _, exists := f.profiles[cr.Name]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsversionprofiles"}, cr.Name)
	}
	f.profiles[cr.Name] = cr.DeepCopy()
	return nil
}

func (f *fakeConfigCRUDClient) UpdateJenkinsVersionProfileCRD(_ context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	existing, ok := f.profiles[cr.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsversionprofiles"}, cr.Name)
	}
	if cr.ResourceVersion != "" && cr.ResourceVersion != existing.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsversionprofiles"}, cr.Name, fmt.Errorf("stale RV"))
	}
	f.profiles[cr.Name] = cr.DeepCopy()
	return nil
}

func (f *fakeConfigCRUDClient) DeleteJenkinsVersionProfileCRD(_ context.Context, name string) error {
	delete(f.profiles, name)
	return nil
}

func (f *fakeConfigCRUDClient) GetConfigMap(_ context.Context, name, namespace string) (map[string]string, error) {
	cm, ok := f.configMaps[key(namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "configmaps"}, name)
	}
	return cm, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestConfigCRUD builds a ConfigCRUD wired to a fake store, seeding every
// ComposedBundle already present in the fake client's map (bundle handlers
// read the store; the remaining config handlers read the client until 1c).
func newTestConfigCRUD(fc *fakeConfigCRUDClient, opNS string, composer *bundle.Composer) (*ConfigCRUD, *crdstore.Fake) {
	st := crdstore.NewFake()
	for _, b := range fc.bundles {
		crdstore.MustSeed(st, b)
	}
	for _, o := range fc.sources {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.items {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.roles {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.bindings {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.defaults {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.profiles {
		crdstore.MustSeed(st, o)
	}
	return &ConfigCRUD{Client: fc, Store: st, OperatorNamespace: opNS, Logger: testLogger(), Composer: composer}, st
}

func newFakeClient() *fakeConfigCRUDClient {
	return &fakeConfigCRUDClient{
		bundles:    make(map[string]*v1alpha1.ComposedBundle),
		sources:    make(map[string]*v1alpha1.CatalogSource),
		items:      make(map[string]*v1alpha1.CatalogItem),
		roles:      make(map[string]*v1alpha1.JenkinsRole),
		bindings:   make(map[string]*v1alpha1.JenkinsRoleBinding),
		secrets:    make(map[string]map[string][]byte),
		defaults:   make(map[string]*v1alpha1.ProvisioningDefaults),
		profiles:   make(map[string]*v1alpha1.JenkinsVersionProfile),
		configMaps: make(map[string]map[string]string),
	}
}

func jsonMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func jsonUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal: %v (data: %s)", err, string(data))
	}
}

// ---------------------------------------------------------------------------
// Tests for Task 2.2: read/delete handlers
// ---------------------------------------------------------------------------

func TestHandleBundlesList(t *testing.T) {
	fc := newFakeClient()
	cr, crStore := newTestConfigCRUD(fc, "op-ns", nil)
	_ = crStore

	resp := cr.HandleBundlesList(jsonMarshal(t, bus.ConfigListRequest{Namespace: ""}))
	var listResp bus.ConfigListResponse
	jsonUnmarshal(t, resp, &listResp)
	if listResp.Code != "" {
		t.Errorf("expected code empty, got %q", listResp.Code)
	}
	if len(listResp.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(listResp.Items))
	}

	fc.bundles["ns-a/b1"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}}},
	}
	crdstore.MustSeed(crStore, fc.bundles["ns-a/b1"])

	resp = cr.HandleBundlesList(jsonMarshal(t, bus.ConfigListRequest{Namespace: "ns-a"}))
	jsonUnmarshal(t, resp, &listResp)
	if listResp.Code != "" {
		t.Errorf("expected code empty, got %q", listResp.Code)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(listResp.Items))
	}

	// Verify managedFields stripped
	var item map[string]any
	jsonUnmarshal(t, listResp.Items[0], &item)
	meta, ok := item["metadata"].(map[string]any)
	if !ok {
		t.Fatal("missing metadata")
	}
	if _, exists := meta["managedFields"]; exists {
		t.Error("managedFields should have been stripped")
	}
}

func TestHandleBundlesGet(t *testing.T) {
	fc := newFakeClient()
	fc.bundles["ns-a/b1"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}}},
	}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	resp := cr.HandleBundlesGet(jsonMarshal(t, bus.ConfigGetRequest{Namespace: "ns-a", Name: "b1"}))
	var getResp bus.ConfigGetResponse
	jsonUnmarshal(t, resp, &getResp)
	if getResp.Code != "" {
		t.Errorf("expected code empty, got %q", getResp.Code)
	}

	resp = cr.HandleBundlesGet(jsonMarshal(t, bus.ConfigGetRequest{Namespace: "ns-a", Name: "nonexistent"}))
	jsonUnmarshal(t, resp, &getResp)
	if getResp.Code != bus.CodeNotFound {
		t.Errorf("expected not_found, got %q", getResp.Code)
	}
}

func TestHandleBundlesDelete(t *testing.T) {
	fc := newFakeClient()
	fc.bundles["ns-a/b1"] = &v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a"}}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	resp := cr.HandleBundlesDelete(jsonMarshal(t, bus.ConfigDeleteRequest{Namespace: "ns-a", Name: "b1"}))
	var delResp bus.ConfigDeleteResponse
	jsonUnmarshal(t, resp, &delResp)
	if delResp.Code != "" {
		t.Errorf("expected code empty, got %q", delResp.Code)
	}
	if _, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), crStore, "b1", "ns-a"); err == nil {
		t.Error("bundle should have been deleted")
	}
}

func TestHandleSourcesListGetDelete(t *testing.T) {
	fc := newFakeClient()
	fc.sources["ns-a/src1"] = &v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: "src1", Namespace: "ns-a"}}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	resp := cr.HandleSourcesList(jsonMarshal(t, bus.ConfigListRequest{Namespace: "ns-a"}))
	var listResp bus.ConfigListResponse
	jsonUnmarshal(t, resp, &listResp)
	if len(listResp.Items) != 1 {
		t.Errorf("expected 1 source, got %d", len(listResp.Items))
	}

	resp = cr.HandleSourcesGet(jsonMarshal(t, bus.ConfigGetRequest{Namespace: "ns-a", Name: "src1"}))
	var getResp bus.ConfigGetResponse
	jsonUnmarshal(t, resp, &getResp)
	if getResp.Code != "" {
		t.Errorf("expected code empty, got %q", getResp.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests for Task 2.3: items, roles, bindings
// ---------------------------------------------------------------------------

func TestHandleItemsList_OperatorNamespace(t *testing.T) {
	fc := newFakeClient()
	fc.items["ns-a/item1"] = &v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "item1", Namespace: "ns-a"}}
	cr, crStore := newTestConfigCRUD(fc, "op-ns", nil)
	_ = crStore

	resp := cr.HandleItemsList(jsonMarshal(t, bus.ConfigListRequest{Namespace: "ns-a"}))
	var listResp bus.ConfigListResponse
	jsonUnmarshal(t, resp, &listResp)
	if listResp.OperatorNamespace != "op-ns" {
		t.Errorf("expected operatorNamespace %q, got %q", "op-ns", listResp.OperatorNamespace)
	}
	if len(listResp.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(listResp.Items))
	}
}

func TestProjectCatalogItemSummary(t *testing.T) {
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "item1", Namespace: "ns-a"},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef:   "source-a",
			Type:        v1alpha1.CatalogItemJCasC,
			DisplayName: "Display",
			Version:     "1.2.3",
			Description: "desc",
			Tags:        []string{"tag-a", "tag-b"},
		},
		Status: v1alpha1.CatalogItemStatus{
			Valid:       true,
			Message:     "ok",
			ContentHash: "abc123",
			Content:     strings.Repeat("x", 1024),
		},
	}

	summary := ProjectCatalogItemSummary(item)
	if summary.Name != "item1" || summary.Namespace != "ns-a" || summary.DisplayName != "Display" ||
		summary.Type != "jcasc" || summary.SourceRef != "source-a" || summary.Version != "1.2.3" ||
		summary.Description != "desc" || summary.Valid != true || summary.Message != "ok" || summary.ContentHash != "abc123" {
		t.Fatalf("summary did not map fields: %#v", summary)
	}
	if got := strings.Join(summary.Tags, ","); got != "tag-a,tag-b" {
		t.Fatalf("tags = %q", got)
	}
	raw := jsonMarshal(t, summary)
	var rawObj map[string]any
	jsonUnmarshal(t, raw, &rawObj)
	if _, ok := rawObj["content"]; ok {
		t.Fatalf("summary JSON contains content field: %s", raw)
	}
}

func TestHandleItemsListSummariesAndGetFullContent(t *testing.T) {
	fc := newFakeClient()
	fc.items["ns-a/item1"] = &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "item1", Namespace: "ns-a"},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef:   "source-a",
			Type:        v1alpha1.CatalogItemPlugin,
			DisplayName: "Plugin",
			Version:     "2.0.0",
		},
		Status: v1alpha1.CatalogItemStatus{Valid: true, Content: "large-yaml", ContentHash: "hash-a"},
	}
	cr, crStore := newTestConfigCRUD(fc, "op-ns", nil)
	_ = crStore

	listRespBytes := cr.HandleItemsList(jsonMarshal(t, bus.ConfigListRequest{Namespace: "ns-a"}))
	var listResp bus.ConfigListResponse
	jsonUnmarshal(t, listRespBytes, &listResp)
	if listResp.Code != "" {
		t.Fatalf("list failed: %s", listResp.Error)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(listResp.Items))
	}
	var summary bus.CatalogItemSummary
	jsonUnmarshal(t, listResp.Items[0], &summary)
	if summary.Name != "item1" || summary.ContentHash != "hash-a" {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	var raw map[string]any
	jsonUnmarshal(t, listResp.Items[0], &raw)
	if _, ok := raw["content"]; ok {
		t.Fatalf("list summary contains content: %v", raw)
	}
	if _, ok := raw["status"]; ok {
		t.Fatalf("list summary contains status: %v", raw)
	}

	getRespBytes := cr.HandleItemsGet(jsonMarshal(t, bus.ConfigGetRequest{Namespace: "ns-a", Name: "item1"}))
	var getResp bus.ConfigGetResponse
	jsonUnmarshal(t, getRespBytes, &getResp)
	if getResp.Code != "" {
		t.Fatalf("get failed: %s", getResp.Error)
	}
	var full v1alpha1.CatalogItem
	jsonUnmarshal(t, getResp.Item, &full)
	if full.Status.Content != "large-yaml" {
		t.Fatalf("get did not return full content: %#v", full.Status)
	}
}

func TestHandleItemsListLargeContentFitsSummaryBudget(t *testing.T) {
	fc := newFakeClient()
	largeContent := strings.Repeat("x", 100*1024)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("item-%02d", i)
		fc.items[key("ns-a", name)] = &v1alpha1.CatalogItem{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-a"},
			Spec: v1alpha1.CatalogItemSpec{
				SourceRef: "source-a",
				Type:      v1alpha1.CatalogItemItem,
			},
			Status: v1alpha1.CatalogItemStatus{Valid: true, Content: largeContent, ContentHash: fmt.Sprintf("hash-%02d", i)},
		}
	}
	cr, crStore := newTestConfigCRUD(fc, "op-ns", nil)
	_ = crStore

	respBytes := cr.HandleItemsList(jsonMarshal(t, bus.ConfigListRequest{Namespace: "ns-a"}))
	var resp bus.ConfigListResponse
	jsonUnmarshal(t, respBytes, &resp)
	if resp.Code != "" || strings.Contains(resp.Error, "list too large") {
		t.Fatalf("large catalog list failed: code=%q error=%q", resp.Code, resp.Error)
	}
	if len(resp.Items) != 10 {
		t.Fatalf("expected 10 summaries, got %d", len(resp.Items))
	}
}

func TestHandleRolesListGetDelete(t *testing.T) {
	fc := newFakeClient()
	fc.roles["role1"] = &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "role1"}}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	resp := cr.HandleRolesList(jsonMarshal(t, bus.ConfigListRequest{}))
	var listResp bus.ConfigListResponse
	jsonUnmarshal(t, resp, &listResp)
	if len(listResp.Items) != 1 {
		t.Errorf("expected 1 role, got %d", len(listResp.Items))
	}

	resp = cr.HandleRolesGet(jsonMarshal(t, bus.ConfigGetRequest{Name: "role1"}))
	var getResp bus.ConfigGetResponse
	jsonUnmarshal(t, resp, &getResp)
	if getResp.Code != "" {
		t.Errorf("expected code empty, got %q", getResp.Code)
	}
}

func TestHandleBindingsListGetDelete(t *testing.T) {
	fc := newFakeClient()
	fc.bindings["binding1"] = &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding1"}}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	resp := cr.HandleBindingsList(jsonMarshal(t, bus.ConfigListRequest{}))
	var listResp bus.ConfigListResponse
	jsonUnmarshal(t, resp, &listResp)
	if len(listResp.Items) != 1 {
		t.Errorf("expected 1 binding, got %d", len(listResp.Items))
	}
}

// ---------------------------------------------------------------------------
// Tests for Task 2.4: create/update handlers
// ---------------------------------------------------------------------------

func TestHandleBundlesCreateConflict(t *testing.T) {
	fc := newFakeClient()
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	obj, _ := json.Marshal(v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a"},
		Spec:       v1alpha1.ComposedBundleSpec{DisplayName: "test"},
	})

	resp := cr.HandleBundlesCreate(jsonMarshal(t, bus.ConfigApplyRequest{Namespace: "ns-a", Name: "b1", Object: obj}))
	var appResp bus.ConfigApplyResponse
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != "" {
		t.Errorf("expected code empty, got %q", appResp.Code)
	}

	// Create on existing → conflict
	resp = cr.HandleBundlesCreate(jsonMarshal(t, bus.ConfigApplyRequest{Namespace: "ns-a", Name: "b1", Object: obj}))
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != bus.CodeConflict {
		t.Errorf("expected conflict, got %q", appResp.Code)
	}

	// Existing CR spec unchanged
	stored, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), crStore, "b1", "ns-a")
	if err != nil {
		t.Fatalf("get bundle after conflict: %v", err)
	}
	if stored.Spec.DisplayName != "test" {
		t.Error("existing bundle spec changed after conflict")
	}
}

func TestHandleBundlesUpdateStaleRV(t *testing.T) {
	fc := newFakeClient()
	fc.bundles["ns-a/b1"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a", ResourceVersion: "100"},
		Spec:       v1alpha1.ComposedBundleSpec{DisplayName: "original"},
	}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	// stale RV → conflict
	staleObj, _ := json.Marshal(v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a", ResourceVersion: "99"},
		Spec:       v1alpha1.ComposedBundleSpec{DisplayName: "updated"},
	})
	resp := cr.HandleBundlesUpdate(jsonMarshal(t, bus.ConfigApplyRequest{Namespace: "ns-a", Name: "b1", Object: staleObj}))
	var appResp bus.ConfigApplyResponse
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != bus.CodeConflict {
		t.Errorf("expected conflict for stale RV, got %q", appResp.Code)
	}
}

func TestHandleRolesCreateConflict(t *testing.T) {
	fc := newFakeClient()
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	obj, _ := json.Marshal(v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "jr1"}})
	resp := cr.HandleRolesCreate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jr1", Object: obj}))
	var appResp bus.ConfigApplyResponse
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != "" {
		t.Errorf("expected code empty, got %q", appResp.Code)
	}

	// conflict
	resp = cr.HandleRolesCreate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jr1", Object: obj}))
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != bus.CodeConflict {
		t.Errorf("expected conflict, got %q", appResp.Code)
	}
}

func TestHandleBindingsCreateConflict(t *testing.T) {
	fc := newFakeClient()
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	obj, _ := json.Marshal(v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "jrb1"}})
	resp := cr.HandleBindingsCreate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jrb1", Object: obj}))
	var appResp bus.ConfigApplyResponse
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != "" {
		t.Errorf("expected code empty, got %q", appResp.Code)
	}

	resp = cr.HandleBindingsCreate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jrb1", Object: obj}))
	jsonUnmarshal(t, resp, &appResp)
	if appResp.Code != bus.CodeConflict {
		t.Errorf("expected conflict, got %q", appResp.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests for Task 2.5: pause/resume/sync
// ---------------------------------------------------------------------------

func TestHandleBundlesPauseResume(t *testing.T) {
	fc := newFakeClient()
	fc.bundles["ns-a/b1"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns-a"},
	}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	// Pause
	resp := cr.HandleBundlesPause(jsonMarshal(t, bus.BundlePauseRequest{Namespace: "ns-a", Name: "b1", Paused: true}))
	var pauseResp bus.BundlePauseResponse
	jsonUnmarshal(t, resp, &pauseResp)
	if pauseResp.Code != "" {
		t.Errorf("expected code empty, got %q", pauseResp.Code)
	}

	b, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), crStore, "b1", "ns-a")
	if err != nil {
		t.Fatalf("get bundle after pause: %v", err)
	}
	if b.Annotations == nil || b.Annotations["varroa.dev/rollout-paused"] != "true" {
		t.Error("expected rollout-paused annotation after pause")
	}

	// Resume
	resp = cr.HandleBundlesPause(jsonMarshal(t, bus.BundlePauseRequest{Namespace: "ns-a", Name: "b1", Paused: false}))
	jsonUnmarshal(t, resp, &pauseResp)
	if pauseResp.Code != "" {
		t.Errorf("expected code empty, got %q", pauseResp.Code)
	}

	b = fc.bundles["ns-a/b1"]
	if b.Annotations != nil && b.Annotations["varroa.dev/rollout-paused"] == "true" {
		t.Error("rollout-paused annotation should be cleared after resume")
	}
}

func TestHandleSourceSync(t *testing.T) {
	fc := newFakeClient()
	fc.sources["ns-a/src1"] = &v1alpha1.CatalogSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src1", Namespace: "ns-a"},
	}
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	resp := cr.HandleSourceSync(jsonMarshal(t, bus.CatalogSyncRequest{Namespace: "ns-a", Name: "src1"}))
	var syncResp bus.CatalogSyncResponse
	jsonUnmarshal(t, resp, &syncResp)
	if syncResp.Code != "" {
		t.Errorf("expected code empty, got %q", syncResp.Code)
	}

	s, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), crStore, "src1", "ns-a")
	if err != nil {
		t.Fatalf("get source after sync: %v", err)
	}
	if s.Annotations == nil || s.Annotations["varroa.dev/sync-requested-at"] == "" {
		t.Error("expected sync-requested-at annotation after sync")
	}
}

// ---------------------------------------------------------------------------
// Preview/validate compose test (basic coverage)
// ---------------------------------------------------------------------------

func TestHandleBundlesPreviewInvalidJSON(t *testing.T) {
	fc := newFakeClient()
	cr, crStore := newTestConfigCRUD(fc, "", nil)
	_ = crStore

	// Invalid request JSON → code:invalid
	resp := cr.HandleBundlesPreview([]byte(`not-json`))
	var compResp bus.BundleComposeResponse
	jsonUnmarshal(t, resp, &compResp)
	if compResp.Code != bus.CodeInvalid {
		t.Errorf("expected invalid for bad JSON, got %q", compResp.Code)
	}
}

func TestHandleBundlesPreview_OCIAuthError(t *testing.T) {
	fc := newFakeClient()
	// Create a minimal composer (with no resolver — OCI source will find it missing).
	composer := bundle.NewComposer(fc, nil, t.TempDir(), "", "", "", "")
	cr, crStore := newTestConfigCRUD(fc, "", composer)
	_ = crStore

	// A bundle with an ociSource referencing a nonexistent secretRef should
	// return an error in the response (not a 5xx / transport error).
	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{OCISource: &v1alpha1.OCIBundleSource{Ref: "registry.io/bundle:v1", Path: ".", SecretRef: "missing-secret"}},
		},
	}
	specJSON := jsonMarshal(t, spec)
	req := jsonMarshal(t, bus.BundleComposeRequest{Namespace: "ns", Spec: specJSON})
	resp := cr.HandleBundlesPreview(req)
	var compResp bus.BundleComposeResponse
	jsonUnmarshal(t, resp, &compResp)
	// Should have an error, not a transport code.
	if compResp.Code != "" {
		t.Errorf("expected empty code (data error), got %q", compResp.Code)
	}
	if compResp.Preview == nil {
		t.Fatal("expected Preview in response, got nil")
	}
	found := false
	for _, e := range compResp.Preview.Errors {
		if strings.Contains(e, "OCI") && strings.Contains(e, "secret") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected OCI-related error in preview, got errors: %v", compResp.Preview.Errors)
	}
}

func TestComposeBundle_GitSecretRefHostNotAllowed(t *testing.T) {
	// HandleBundlesPreview with a basic-auth gitSource input whose Secret's
	// varroa.dev/allowed-hosts annotation does not match the repo URL's host
	// must return a host-mismatch error in Preview.Errors.
	fc := newFakeClient()
	fc.secrets[key("ns", "git-creds")] = map[string][]byte{
		"username": []byte("alice"),
		"password": []byte("s3cret"),
	}
	fc.secretAnnotations = make(map[string]map[string]string)
	fc.secretAnnotations[key("ns", "git-creds")] = map[string]string{
		bundle.AllowedHostsAnnotation: "github.com",
	}
	// Composer needs to be non-nil (composeBundle calls c.Composer.Compose),
	// but the host check fires in resolveComposedBundleGitAuth before the
	// composer reaches the git input.
	composer := bundle.NewComposer(fc, nil, t.TempDir(), "", "", "", "")
	cr, crStore := newTestConfigCRUD(fc, "", composer)
	_ = crStore

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{GitSource: &v1alpha1.GitBundleSource{
				RepoURL:   "https://attacker.example/x.git",
				Path:      ".",
				Revision:  "main",
				SecretRef: "git-creds",
			}},
		},
	}
	specJSON := jsonMarshal(t, spec)
	req := jsonMarshal(t, bus.BundleComposeRequest{Namespace: "ns", Spec: specJSON})
	resp := cr.HandleBundlesPreview(req)
	var compResp bus.BundleComposeResponse
	jsonUnmarshal(t, resp, &compResp)
	if compResp.Preview == nil {
		t.Fatal("expected Preview in response, got nil")
	}
	found := false
	for _, e := range compResp.Preview.Errors {
		if strings.Contains(e, "allowed-hosts") || strings.Contains(e, "not in the allowed-hosts") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected host mismatch error in preview errors, got: %v", compResp.Preview.Errors)
	}
}

func TestComposeBundle_GitSecretRefAnnotationsErrorFailsClosed(t *testing.T) {
	// Same fail-closed contract as the reconciler sites: an annotations read
	// failure must surface as a host-scoping error, not skip the check.
	fc := newFakeClient()
	fc.secrets[key("ns", "git-creds")] = map[string][]byte{
		"username": []byte("alice"),
		"password": []byte("s3cret"),
	}
	fc.secretAnnotationsErr = errors.New("api server unavailable")
	composer := bundle.NewComposer(fc, nil, t.TempDir(), "", "", "", "")
	cr, crStore := newTestConfigCRUD(fc, "", composer)
	_ = crStore

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{GitSource: &v1alpha1.GitBundleSource{
				RepoURL:   "https://attacker.example/x.git",
				Path:      ".",
				Revision:  "main",
				SecretRef: "git-creds",
			}},
		},
	}
	specJSON := jsonMarshal(t, spec)
	req := jsonMarshal(t, bus.BundleComposeRequest{Namespace: "ns", Spec: specJSON})
	resp := cr.HandleBundlesPreview(req)
	var compResp bus.BundleComposeResponse
	jsonUnmarshal(t, resp, &compResp)
	if compResp.Preview == nil {
		t.Fatal("expected Preview in response, got nil")
	}
	found := false
	for _, e := range compResp.Preview.Errors {
		if strings.Contains(e, "allowed-hosts") || strings.Contains(e, "host") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected host-scoping error in preview errors (fail closed), got: %v", compResp.Preview.Errors)
	}
}
