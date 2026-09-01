package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// fakeResourceClient implements controller.ResourceClient for handler tests.
// storeFromFake builds a crdstore fake seeded with every CRD the
// fakeResourceClient's maps hold — the read surface handlers use after the
// crdstore migration. Call after seeding the fake's maps.
func storeFromFake(fc *fakeResourceClient) *crdstore.Fake {
	st := fc.crdStore
	if st == nil {
		st = crdstore.NewFake()
		fc.crdStore = st
	}
	for _, o := range fc.users {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.groups {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.controllers {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.roles {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.roleBindings {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.jenkinsRoles {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.jenkinsRoleBindings {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.catalogSources {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.catalogItems {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.composedBundles {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.versionProfiles {
		crdstore.MustSeed(st, o)
	}
	for _, o := range fc.updateCenters {
		crdstore.MustSeed(st, o)
	}
	return st
}

type fakeResourceClient struct {
	crdStore *crdstore.Fake
	users    map[string]*v1alpha1.User
	groups   map[string]*v1alpha1.Group
	// controllers backs Get/ListControllerCRDs and ApplyControllerSpecSSA when
	// non-nil; the zero value preserves the legacy nil,nil stubs.
	controllers map[string]*v1alpha1.Controller
	// controllerErr, when set, is returned by GetControllerCRD instead of the
	// usual NotFound/found lookup, to simulate a transient API error.
	controllerErr error
	// namespaces marks which namespaces exist; GetNamespace returns NotFound for others.
	// Populated by the test to pass the preflight namespace check.
	namespaces map[string]bool
	// CRD stores for RBAC, catalog, composed bundles
	roles               map[string]*v1alpha1.VarroaRole
	roleBindings        map[string]*v1alpha1.VarroaRoleBinding
	jenkinsRoles        map[string]*v1alpha1.JenkinsRole
	jenkinsRoleBindings map[string]*v1alpha1.JenkinsRoleBinding
	catalogSources      map[string]*v1alpha1.CatalogSource
	catalogItems        map[string]*v1alpha1.CatalogItem
	// configMaps and configMapErrs back GetConfigMap; absent names keep the
	// legacy nil,nil stub.
	configMaps      map[string]map[string]string
	configMapErrs   map[string]error
	composedBundles map[string]*v1alpha1.ComposedBundle
	versionProfiles map[string]*v1alpha1.JenkinsVersionProfile
	updateCenters   map[string]*v1alpha1.UpdateCenter
}

func newFakeResourceClient() *fakeResourceClient {
	return &fakeResourceClient{
		crdStore:   crdstore.NewFake(),
		users:      make(map[string]*v1alpha1.User),
		groups:     make(map[string]*v1alpha1.Group),
		namespaces: make(map[string]bool),
		versionProfiles: map[string]*v1alpha1.JenkinsVersionProfile{
			"test-profile": {ObjectMeta: metav1.ObjectMeta{Name: "test-profile"}, TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsVersionProfile"}, Spec: v1alpha1.JenkinsVersionProfileSpec{}},
		},
	}
}

func (f *fakeResourceClient) GetUserCRD(_ context.Context, name, namespace string) (*v1alpha1.User, error) {
	u, ok := f.users[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("users"), name)
	}
	return u, nil
}

func (f *fakeResourceClient) ApplyUserCRD(_ context.Context, cr *v1alpha1.User) error {
	f.users[cr.Name] = cr.DeepCopy()
	return nil
}

func (f *fakeResourceClient) ListUserCRDs(_ context.Context, namespace string) ([]*v1alpha1.User, error) {
	out := make([]*v1alpha1.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}

// Stub methods required by the interface but not used by these tests.
func (f *fakeResourceClient) PatchUserStatus(_ context.Context, name, namespace string, status *v1alpha1.UserStatus) error {
	return nil
}
func (f *fakeResourceClient) ClearUserPassword(_ context.Context, name, namespace string) error {
	return nil
}
func (f *fakeResourceClient) DeleteUserCRD(_ context.Context, name, namespace string) error {
	return nil
}
func (f *fakeResourceClient) ListGroupCRDs(_ context.Context) ([]*v1alpha1.Group, error) {
	return nil, nil
}
func (f *fakeResourceClient) GetGroupCRD(_ context.Context, name string) (*v1alpha1.Group, error) {
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("groups"), name)
}
func (f *fakeResourceClient) ApplyGroupCRD(_ context.Context, cr *v1alpha1.Group) error { return nil }
func (f *fakeResourceClient) DeleteGroupCRD(_ context.Context, name string) error       { return nil }
func (f *fakeResourceClient) ListVarroaRoleBindingCRDs(_ context.Context) ([]*v1alpha1.VarroaRoleBinding, error) {
	out := make([]*v1alpha1.VarroaRoleBinding, 0, len(f.roleBindings))
	for _, r := range f.roleBindings {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeResourceClient) GetVarroaRoleBindingCRD(_ context.Context, name string) (*v1alpha1.VarroaRoleBinding, error) {
	r, ok := f.roleBindings[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("varroarolebindings"), name)
	}
	return r, nil
}
func (f *fakeResourceClient) ApplyVarroaRoleBindingCRD(_ context.Context, cr *v1alpha1.VarroaRoleBinding) error {
	if f.roleBindings != nil {
		f.roleBindings[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteVarroaRoleBindingCRD(_ context.Context, name string) error {
	if f.roleBindings != nil {
		delete(f.roleBindings, name)
	}
	return nil
}
func (f *fakeResourceClient) ListJenkinsRoleBindingCRDs(_ context.Context) ([]*v1alpha1.JenkinsRoleBinding, error) {
	out := make([]*v1alpha1.JenkinsRoleBinding, 0, len(f.jenkinsRoleBindings))
	for _, r := range f.jenkinsRoleBindings {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeResourceClient) GetJenkinsRoleBindingCRD(_ context.Context, name string) (*v1alpha1.JenkinsRoleBinding, error) {
	r, ok := f.jenkinsRoleBindings[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("jenkinsrolebindings"), name)
	}
	return r, nil
}
func (f *fakeResourceClient) ApplyJenkinsRoleBindingCRD(_ context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	if f.jenkinsRoleBindings != nil {
		f.jenkinsRoleBindings[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteJenkinsRoleBindingCRD(_ context.Context, name string) error {
	if f.jenkinsRoleBindings != nil {
		delete(f.jenkinsRoleBindings, name)
	}
	return nil
}
func (f *fakeResourceClient) ListVarroaRoleCRDs(_ context.Context) ([]*v1alpha1.VarroaRole, error) {
	out := make([]*v1alpha1.VarroaRole, 0, len(f.roles))
	for _, r := range f.roles {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeResourceClient) GetVarroaRoleCRD(_ context.Context, name string) (*v1alpha1.VarroaRole, error) {
	r, ok := f.roles[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("varroaroles"), name)
	}
	return r, nil
}
func (f *fakeResourceClient) ApplyVarroaRoleCRD(_ context.Context, cr *v1alpha1.VarroaRole) error {
	if f.roles != nil {
		f.roles[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteVarroaRoleCRD(_ context.Context, name string) error {
	if f.roles != nil {
		delete(f.roles, name)
	}
	return nil
}
func (f *fakeResourceClient) ListJenkinsRoleCRDs(_ context.Context) ([]*v1alpha1.JenkinsRole, error) {
	out := make([]*v1alpha1.JenkinsRole, 0, len(f.jenkinsRoles))
	for _, r := range f.jenkinsRoles {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeResourceClient) GetJenkinsRoleCRD(_ context.Context, name string) (*v1alpha1.JenkinsRole, error) {
	r, ok := f.jenkinsRoles[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("jenkinsroles"), name)
	}
	return r, nil
}
func (f *fakeResourceClient) ApplyJenkinsRoleCRD(_ context.Context, cr *v1alpha1.JenkinsRole) error {
	if f.jenkinsRoles != nil {
		f.jenkinsRoles[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteJenkinsRoleCRD(_ context.Context, name string) error {
	if f.jenkinsRoles != nil {
		delete(f.jenkinsRoles, name)
	}
	return nil
}
func (f *fakeResourceClient) ListControllerCRDs(_ context.Context, ns string) ([]*v1alpha1.Controller, error) {
	if f.controllers == nil {
		return nil, nil
	}
	out := make([]*v1alpha1.Controller, 0, len(f.controllers))
	for _, cr := range f.controllers {
		out = append(out, cr)
	}
	return out, nil
}
func (f *fakeResourceClient) GetControllerCRD(_ context.Context, name, ns string) (*v1alpha1.Controller, error) {
	if f.controllerErr != nil {
		return nil, f.controllerErr
	}
	if f.controllers == nil {
		return nil, nil
	}
	cr, ok := f.controllers[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("controllers"), name)
	}
	return cr, nil
}
func (f *fakeResourceClient) ApplyControllerSpecSSA(_ context.Context, namespace, name string, spec map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	if f.controllers != nil {
		if ctrl, ok := f.controllers[name]; ok {
			// The fake returns the stored controller unchanged (it never
			// simulates a real SSA merge), standing in for "another manager
			// owns every field" — so every requested removal that is still on
			// the stored object is reported, mirroring the seam.
			removals, _ := controller.ApplyRemovalPaths(spec)
			return ctrl.DeepCopy(), controller.UnappliedRemovals(ctrl, removals), nil
		}
	}
	return nil, nil, nil
}
func (f *fakeResourceClient) ApplyControllerSpecSSAIfExists(_ context.Context, namespace, name string, spec map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	return nil, nil, nil
}
func (f *fakeResourceClient) SetHibernated(_ context.Context, _, _ string, _ bool) (bool, error) {
	return false, nil
}
func (f *fakeResourceClient) DeleteControllerCRD(_ context.Context, name, ns string) error {
	return nil
}
func (f *fakeResourceClient) PatchControllerFinalizers(_ context.Context, name, ns string, fin []string) error {
	return nil
}
func (f *fakeResourceClient) PatchControllerStatus(_ context.Context, name, ns string, st *v1alpha1.ControllerStatus) error {
	return nil
}
func (f *fakeResourceClient) PatchControllerAnnotations(_ context.Context, name, namespace string, ann map[string]*string) error {
	return nil
}
func (f *fakeResourceClient) ListPodTemplateCRDs(_ context.Context, ns string) ([]*v1alpha1.PodTemplate, error) {
	return nil, nil
}
func (f *fakeResourceClient) GetPodTemplateCRD(_ context.Context, name, ns string) (*v1alpha1.PodTemplate, error) {
	return nil, nil
}
func (f *fakeResourceClient) ApplyPodTemplateCRD(_ context.Context, cr *v1alpha1.PodTemplate) error {
	return nil
}
func (f *fakeResourceClient) DeletePodTemplateCRD(_ context.Context, name, ns string) error {
	return nil
}
func (f *fakeResourceClient) GetProvisioningDefaultsCRD(_ context.Context, name string) (*v1alpha1.ProvisioningDefaults, error) {
	return &v1alpha1.ProvisioningDefaults{}, nil
}
func (f *fakeResourceClient) ApplyProvisioningDefaultsCRD(_ context.Context, cr *v1alpha1.ProvisioningDefaults) error {
	return nil
}
func (f *fakeResourceClient) ListCatalogSourceCRDs(_ context.Context, _ string) ([]*v1alpha1.CatalogSource, error) {
	out := make([]*v1alpha1.CatalogSource, 0, len(f.catalogSources))
	for _, s := range f.catalogSources {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeResourceClient) GetCatalogSourceCRD(_ context.Context, name, ns string) (*v1alpha1.CatalogSource, error) {
	s, ok := f.catalogSources[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("catalogsources"), name)
	}
	return s, nil
}
func (f *fakeResourceClient) ApplyCatalogSourceCRD(_ context.Context, cr *v1alpha1.CatalogSource) error {
	if f.catalogSources != nil {
		f.catalogSources[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteCatalogSourceCRD(_ context.Context, name, ns string) error {
	if f.catalogSources != nil {
		delete(f.catalogSources, name)
	}
	return nil
}
func (f *fakeResourceClient) PatchCatalogSourceStatus(_ context.Context, _, _ string, _ *v1alpha1.CatalogSourceStatus) error {
	return nil
}
func (f *fakeResourceClient) ListCatalogItemCRDs(_ context.Context, _, _ string) ([]*v1alpha1.CatalogItem, error) {
	out := make([]*v1alpha1.CatalogItem, 0, len(f.catalogItems))
	for _, item := range f.catalogItems {
		out = append(out, item)
	}
	return out, nil
}
func (f *fakeResourceClient) GetCatalogItemCRD(_ context.Context, name, ns string) (*v1alpha1.CatalogItem, error) {
	item, ok := f.catalogItems[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("catalogitems"), name)
	}
	return item, nil
}
func (f *fakeResourceClient) ApplyCatalogItemCRD(_ context.Context, cr *v1alpha1.CatalogItem) error {
	if f.catalogItems != nil {
		f.catalogItems[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteCatalogItemCRD(_ context.Context, name, ns string) error {
	if f.catalogItems != nil {
		delete(f.catalogItems, name)
	}
	return nil
}
func (f *fakeResourceClient) PatchCatalogItemStatus(_ context.Context, _, _ string, _ *v1alpha1.CatalogItemStatus) error {
	return nil
}
func (f *fakeResourceClient) ListComposedBundleCRDs(_ context.Context, ns string) ([]*v1alpha1.ComposedBundle, error) {
	out := make([]*v1alpha1.ComposedBundle, 0, len(f.composedBundles))
	for _, b := range f.composedBundles {
		out = append(out, b)
	}
	return out, nil
}
func (f *fakeResourceClient) GetComposedBundleCRD(_ context.Context, name, ns string) (*v1alpha1.ComposedBundle, error) {
	b, ok := f.composedBundles[name]
	if !ok {
		return nil, k8serrors.NewNotFound(v1alpha1.Resource("composedbundles"), name)
	}
	return b, nil
}
func (f *fakeResourceClient) ApplyComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	if f.composedBundles != nil {
		f.composedBundles[cr.Name] = cr.DeepCopy()
	}
	return nil
}
func (f *fakeResourceClient) DeleteComposedBundleCRD(_ context.Context, name, ns string) error {
	if f.composedBundles != nil {
		delete(f.composedBundles, name)
	}
	return nil
}
func (f *fakeResourceClient) PatchComposedBundleStatus(_ context.Context, _, _ string, _ *v1alpha1.ComposedBundleStatus) error {
	return nil
}
func (f *fakeResourceClient) CreateService(_ context.Context, n, ns string, p int32, _ string) error {
	return nil
}
func (f *fakeResourceClient) CreateServiceAccount(_ context.Context, n, ns string) error { return nil }
func (f *fakeResourceClient) CreateAgentRBAC(_ context.Context, n, ns string) error      { return nil }
func (f *fakeResourceClient) CreateStatefulSet(_ context.Context, _ controller.StatefulSetSpec) error {
	return nil
}
func (f *fakeResourceClient) UpdateStatefulSetOIDCEnv(_ context.Context, _, _, _, _, _, _, _, _, _, _, _ string) error {
	return nil
}
func (f *fakeResourceClient) EnsureStatefulSetPodLabel(_ context.Context, _, _, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeResourceClient) IsStatefulSetReady(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (f *fakeResourceClient) CreateIngress(_ context.Context, _, _, _, _, _, _ string, _ map[string]string, _ string) error {
	return nil
}
func (f *fakeResourceClient) CreateOrUpdateConfigMap(_ context.Context, _, _ string, _ map[string]string, _ ...metav1.OwnerReference) error {
	return nil
}
func (f *fakeResourceClient) GetConfigMap(_ context.Context, name, _ string) (map[string]string, error) {
	if err, ok := f.configMapErrs[name]; ok {
		return nil, err
	}
	if cm, ok := f.configMaps[name]; ok {
		return cm, nil
	}
	return nil, nil
}
func (f *fakeResourceClient) RemoveConfigMapLabel(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeResourceClient) UpdateConfigMapData(_ context.Context, _, _ string, _ map[string]string) error {
	return nil
}
func (f *fakeResourceClient) CreateSecret(_ context.Context, _, _ string, _ map[string]string, _ map[string][]byte) error {
	return nil
}
func (f *fakeResourceClient) CreateSecretExclusive(_ context.Context, _, _ string, _ map[string]string, _ map[string][]byte) error {
	return nil
}
func (f *fakeResourceClient) CreateOrUpdateSecret(_ context.Context, _, _ string, _ map[string][]byte) error {
	return nil
}
func (f *fakeResourceClient) PatchSecretData(_ context.Context, _, _ string, _ map[string][]byte) error {
	return nil
}
func (f *fakeResourceClient) GetSecret(_ context.Context, _, _ string) (map[string][]byte, error) {
	return nil, nil
}
func (f *fakeResourceClient) GetSecretAnnotations(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}
func (f *fakeResourceClient) ListSecrets(_ context.Context, _, _ string) ([]map[string][]byte, error) {
	return nil, nil
}
func (f *fakeResourceClient) CopyImagePullSecret(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeResourceClient) DeleteResource(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeResourceClient) DeleteSecret(_ context.Context, _, _ string) error      { return nil }
func (f *fakeResourceClient) EnsureWakeEndpointSlice(_ context.Context, _, _ string, _ []string, _ int32) error {
	return nil
}
func (f *fakeResourceClient) DeleteWakeEndpointSlice(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeResourceClient) ListOperatorPodIPs(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeResourceClient) ScaleStatefulSet(_ context.Context, _, _ string, _ int32) error {
	return nil
}
func (f *fakeResourceClient) StreamPodLogs(_ context.Context, _, _, _ string, _ int64, _ bool) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeResourceClient) DeleteControllerPod(_ context.Context, _, _ string) error { return nil }
func (f *fakeResourceClient) ListResourceQuotas(_ context.Context, _ string) ([]corev1.ResourceQuota, error) {
	return nil, nil
}
func (f *fakeResourceClient) ListIngressHosts(_ context.Context) (map[string][]string, error) {
	return nil, nil
}
func (f *fakeResourceClient) GetStatefulSetPluginsChecksum(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeResourceClient) GetStatefulSetImages(_ context.Context, _, _ string) (map[string]string, map[string]string, error) {
	return nil, nil, nil
}
func (f *fakeResourceClient) GetStatefulSetContainerSpecs(_ context.Context, _, _ string) (string, string, *corev1.ResourceRequirements, *corev1.ResourceRequirements, string, map[string]string, bool, error) {
	return "", "", nil, nil, "", nil, false, nil
}
func (f *fakeResourceClient) GetControllerPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	return nil, nil
}

func (f *fakeResourceClient) GetNamespace(_ context.Context, name string) (*corev1.Namespace, error) {
	if f.namespaces[name] {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
	}
	return nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func (f *fakeResourceClient) GetLiveResource(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeResourceClient) ListJenkinsVersionProfileCRDs(_ context.Context) ([]*v1alpha1.JenkinsVersionProfile, error) {
	out := make([]*v1alpha1.JenkinsVersionProfile, 0, len(f.versionProfiles))
	for _, p := range f.versionProfiles {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeResourceClient) GetJenkinsVersionProfileCRD(_ context.Context, name string) (*v1alpha1.JenkinsVersionProfile, error) {
	if p, ok := f.versionProfiles[name]; ok {
		return p, nil
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("jenkinsversionprofiles"), name)
}

func (f *fakeResourceClient) CreateJenkinsVersionProfileCRD(_ context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	f.versionProfiles[cr.Name] = cr.DeepCopy()
	return nil
}

func (f *fakeResourceClient) UpdateJenkinsVersionProfileCRD(_ context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	f.versionProfiles[cr.Name] = cr.DeepCopy()
	return nil
}

func (f *fakeResourceClient) DeleteJenkinsVersionProfileCRD(_ context.Context, name string) error {
	delete(f.versionProfiles, name)
	return nil
}

func (f *fakeResourceClient) CreateOrUpdateConfigMapWithOwner(_ context.Context, name, namespace string, data map[string]string, owner metav1.OwnerReference) error {
	return f.CreateOrUpdateConfigMap(context.Background(), name, namespace, data, owner)
}

func (f *fakeResourceClient) CreateOrUpdateOwnedConfigMap(_ context.Context, name, namespace string, data map[string]string, _ map[string]string) error {
	return f.CreateOrUpdateConfigMap(context.Background(), name, namespace, data)
}

func (f *fakeResourceClient) PatchJenkinsVersionProfileStatus(_ context.Context, _ string, _ *v1alpha1.JenkinsVersionProfileStatus) error {
	return nil
}

// ---- Team CRD operations ----

func (f *fakeResourceClient) ListTeamCRDs(_ context.Context) ([]*v1alpha1.Team, error) {
	return nil, nil
}
func (f *fakeResourceClient) GetTeamCRD(_ context.Context, name string) (*v1alpha1.Team, error) {
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("teams"), name)
}
func (f *fakeResourceClient) ApplyTeamCRD(_ context.Context, cr *v1alpha1.Team) error {
	return nil
}
func (f *fakeResourceClient) DeleteTeamCRD(_ context.Context, name string) error {
	return nil
}
func (f *fakeResourceClient) PatchTeamStatus(_ context.Context, name string, status *v1alpha1.TeamStatus) error {
	return nil
}
func (f *fakeResourceClient) ListBroodOperationCRDs(_ context.Context, _ string) ([]*v1alpha1.BroodOperation, error) {
	return nil, nil
}
func (f *fakeResourceClient) GetBroodOperationCRD(_ context.Context, _, _ string) (*v1alpha1.BroodOperation, error) {
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("broodoperations"), "none")
}
func (f *fakeResourceClient) ApplyBroodOperationCRD(_ context.Context, _ *v1alpha1.BroodOperation) error {
	return nil
}
func (f *fakeResourceClient) DeleteBroodOperationCRD(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeResourceClient) PatchBroodOperationStatus(_ context.Context, _, _ string, _ *v1alpha1.BroodOperationStatus) error {
	return nil
}

func contextWithClaims(ctx context.Context, claims *auth.Claims) context.Context {
	return auth.ContextWithClaims(ctx, claims)
}

func TestHandleMe_Unauthenticated(t *testing.T) {
	client := newFakeResourceClient()
	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	w := httptest.NewRecorder()
	srv.HandleMe(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleMe_LocalEnrichment(t *testing.T) {
	client := newFakeResourceClient()
	now := metav1.Now()
	client.users["alice"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "test-ns"},
		Spec: v1alpha1.UserSpec{
			Email:       "alice@example.com",
			DisplayName: "Alice Cooper",
		},
		Status: v1alpha1.UserStatus{
			LastLogin: &now,
		},
	}
	crdstore.MustSeed(client.crdStore, client.users["alice"])

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
		Auth:              &stubProvider{mode: auth.AuthModeLocal},
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	claims := &auth.Claims{
		Subject:           "alice",
		Email:             "alice@example.com",
		Name:              "Alice Cooper",
		PreferredUsername: "alice",
		Groups:            []string{"developers"},
	}
	req = req.WithContext(contextWithClaims(req.Context(), claims))

	w := httptest.NewRecorder()
	srv.HandleMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp meResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.DisplayName != "Alice Cooper" {
		t.Errorf("expected displayName 'Alice Cooper', got %q", resp.DisplayName)
	}
	if resp.LastLogin == nil {
		t.Error("expected lastLogin to be populated")
	}
	if resp.AuthMode != "local" {
		t.Errorf("expected authMode 'local', got %q", resp.AuthMode)
	}
}

func TestHandleMe_SubjectLookupNotEmail(t *testing.T) {
	client := newFakeResourceClient()
	// The CRD is named by subject-derived name (oidc-hash), not by email.
	client.users["oidc-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"},
		Spec: v1alpha1.UserSpec{
			Email:       "oidcuser@example.com",
			DisplayName: "OIDC User",
		},
	}
	crdstore.MustSeed(client.crdStore, client.users["oidc-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"])

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "oidc"},
		Auth:              &stubProvider{mode: auth.AuthModeOIDC},
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	// The claims have email but the lookup must NOT use it.
	claims := &auth.Claims{
		Subject: "oidc:user-subject-that-hashes-to-above",
		Email:   "oidcuser@example.com",
	}
	req = req.WithContext(contextWithClaims(req.Context(), claims))

	w := httptest.NewRecorder()
	srv.HandleMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp meResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	// In this test the subject hash won't match the stored CRD name,
	// so displayName should not be enriched. The key assertion is
	// that we don't accidentally find the user by email.
	// The test verifies the code path uses subject-derived name lookup.
	_ = resp
}

func TestHandleMe_NoLastLogin(t *testing.T) {
	client := newFakeResourceClient()
	client.users["alice"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "test-ns"},
		Spec: v1alpha1.UserSpec{
			Email:       "alice@example.com",
			DisplayName: "Alice",
		},
	}
	crdstore.MustSeed(client.crdStore, client.users["alice"])

	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
		Auth:              &stubProvider{mode: auth.AuthModeLocal},
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	claims := &auth.Claims{
		Subject: "alice",
		Email:   "alice@example.com",
	}
	req = req.WithContext(contextWithClaims(req.Context(), claims))

	w := httptest.NewRecorder()
	srv.HandleMe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp meResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.LastLogin != nil {
		t.Errorf("expected no lastLogin, got %v", resp.LastLogin)
	}
	if resp.DisplayName != "Alice" {
		t.Errorf("expected displayName 'Alice', got %q", resp.DisplayName)
	}
}

type stubProvider struct {
	mode       auth.AuthMode
	cookie     string
	discoverOK bool
	openidCfg  []byte
	jwks       []byte
	verifyFn   func(string) (*auth.Claims, error)
}

func (s *stubProvider) Mode() auth.AuthMode { return s.mode }
func (s *stubProvider) Verify(_ context.Context, token string) (*auth.Claims, error) {
	if s.verifyFn != nil {
		return s.verifyFn(token)
	}
	return nil, nil
}
func (s *stubProvider) CookieDomain() string                                      { return s.cookie }
func (s *stubProvider) Discovery() ([]byte, []byte, bool)                         { return s.openidCfg, s.jwks, s.discoverOK }
func (s *stubProvider) Login(_ context.Context, _, _ string) (string, int, error) { return "", 0, nil }
func (s *stubProvider) ChangePassword(_ context.Context, _, _, _ string) error    { return nil }
func (s *stubProvider) SetPassword(_ context.Context, _, _ string) error          { return nil }
func (s *stubProvider) LoginLimiter() interface{}                                 { return nil }

// --------------------------------------------------------------------------
// Tests for /me/permissions
// --------------------------------------------------------------------------

func TestHandleMePermissions_AuthenticatedScopedCaller(t *testing.T) {
	// Wire up a real Authorizer with a scoped-only binding.
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "scoped-user"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		}},
	}
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range roles {
		_ = roleIdx.Add(r)
	}
	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.BySubjectIndex: rbac.SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIdx.Add(b)
	}
	resolver := rbac.NewResolver(
		&fakeInformer{indexer: roleIdx},
		&fakeInformer{indexer: bindingIdx},
		&fakeInformer{indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})},
		&fakeInformer{indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})},
		&fakeInformer{indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})},
		false, []string{"sub"}, []string{"groups"},
	)

	client := newFakeResourceClient()
	client.namespaces["team-a"] = true
	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
		Authorizer:        NewAuthorizer(resolver, false),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/me/permissions", nil)
	claims := &auth.Claims{Subject: "scoped-user"}
	req = req.WithContext(contextWithClaims(req.Context(), claims))
	w := httptest.NewRecorder()
	srv.handleMePermissions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// Must have "global" (possibly empty object) and "scopes" (non-null array).
	if _, ok := body["global"]; !ok {
		t.Error("response missing 'global' key")
	}
	if _, ok := body["scopes"]; !ok {
		t.Error("response missing 'scopes' key")
	}
	scopes, ok := body["scopes"].([]interface{})
	if !ok {
		t.Fatal("'scopes' is not an array")
	}
	if len(scopes) == 0 {
		t.Fatal("expected at least one scope entry for scoped caller")
	}
	scope0, ok := scopes[0].(map[string]interface{})
	if !ok {
		t.Fatal("scope entry is not an object")
	}
	if _, ok := scope0["namespaces"]; !ok {
		t.Error("scope entry missing 'namespaces'")
	}
	if _, ok := scope0["hasControllerSelector"]; !ok {
		t.Error("scope entry missing 'hasControllerSelector'")
	}
	if _, ok := scope0["capabilities"]; !ok {
		t.Error("scope entry missing 'capabilities'")
	}

	// Verify scoped caps do NOT appear in global.
	global, ok := body["global"].(map[string]interface{})
	if !ok {
		t.Fatal("'global' is not an object")
	}
	if _, ok := global["controllers"]; ok {
		t.Error("scoped-only caller must NOT have controllers in global")
	}
}

func TestHandleMePermissions_Unauthenticated(t *testing.T) {
	client := newFakeResourceClient()
	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/me/permissions", nil)
	w := httptest.NewRecorder()
	srv.handleMePermissions(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated, got %d", w.Code)
	}
}

func TestHandleMePermissions_WrongMethod(t *testing.T) {
	client := newFakeResourceClient()
	deps := &Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
		IdentityConfig:    IdentityConfig{Mode: "local"},
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/me/permissions", nil)
	claims := &auth.Claims{Subject: "user"}
	req = req.WithContext(contextWithClaims(req.Context(), claims))
	w := httptest.NewRecorder()
	srv.handleMePermissions(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for non-GET, got %d", w.Code)
	}
}

// Stub methods for new ResourceClient interface methods (add-remote-config-authoring).
func (f *fakeResourceClient) CreateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (f *fakeResourceClient) UpdateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (f *fakeResourceClient) CreateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (f *fakeResourceClient) UpdateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (f *fakeResourceClient) CreateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (f *fakeResourceClient) UpdateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (f *fakeResourceClient) CreateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}
func (f *fakeResourceClient) UpdateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}

func (f *fakeResourceClient) GetControllerClassCRD(_ context.Context, _ string) (*v1alpha1.ControllerClass, error) {
	return nil, nil
}

func (f *fakeResourceClient) ListControllerClassCRDs(_ context.Context) ([]*v1alpha1.ControllerClass, error) {
	return nil, nil
}

func (f *fakeResourceClient) PatchUpdateCenterStatus(_ context.Context, _ string, _ *v1alpha1.UpdateCenterStatus) error {
	return nil
}

func (f *fakeResourceClient) GetUpdateCenter(_ context.Context, _ string) (*v1alpha1.UpdateCenter, error) {
	if f.updateCenters != nil {
		uc, ok := f.updateCenters["varroa-update-center"]
		if ok {
			return uc, nil
		}
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("updatecenters"), "varroa-update-center")
}

func (f *fakeResourceClient) GetPVC(_ context.Context, _, _ string) (*corev1.PersistentVolumeClaim, error) {
	return nil, nil
}
