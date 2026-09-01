package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// fakeBroodClient implements controller.ResourceClient for busBrood tests.
type fakeBroodClient struct {
	controllers map[string]*v1alpha1.Controller
	store       *crdstore.Fake
}

func newFakeBroodClient() *fakeBroodClient {
	return &fakeBroodClient{
		store: crdstore.NewFake(), controllers: make(map[string]*v1alpha1.Controller)}
}

func (f *fakeBroodClient) ListControllerCRDs(_ context.Context, namespace string) ([]*v1alpha1.Controller, error) {
	var out []*v1alpha1.Controller
	for _, cr := range f.controllers {
		if namespace == "" || cr.Namespace == namespace {
			out = append(out, cr)
		}
	}
	return out, nil
}

// Stub methods to satisfy controller.ResourceClient.
func (f *fakeBroodClient) GetControllerCRD(_ context.Context, _, _ string) (*v1alpha1.Controller, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyControllerSpecSSA(_ context.Context, _, _ string, _ map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	return nil, nil, nil
}
func (f *fakeBroodClient) ApplyControllerSpecSSAIfExists(_ context.Context, _, _ string, _ map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	return nil, nil, nil
}
func (f *fakeBroodClient) SetHibernated(_ context.Context, _, _ string, _ bool) (bool, error) {
	return false, nil
}
func (f *fakeBroodClient) DeleteControllerCRD(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) DeleteControllerPod(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) GetControllerPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetComposedBundleCRD(_ context.Context, _, _ string) (*v1alpha1.ComposedBundle, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (f *fakeBroodClient) DeleteComposedBundleCRD(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) ListVarroaRoleCRDs(_ context.Context) ([]*v1alpha1.VarroaRole, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetVarroaRoleCRD(_ context.Context, _ string) (*v1alpha1.VarroaRole, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyVarroaRoleCRD(_ context.Context, _ *v1alpha1.VarroaRole) error {
	return nil
}
func (f *fakeBroodClient) DeleteVarroaRoleCRD(_ context.Context, _ string) error { return nil }
func (f *fakeBroodClient) ListVarroaRoleBindingCRDs(_ context.Context) ([]*v1alpha1.VarroaRoleBinding, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetVarroaRoleBindingCRD(_ context.Context, _ string) (*v1alpha1.VarroaRoleBinding, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyVarroaRoleBindingCRD(_ context.Context, _ *v1alpha1.VarroaRoleBinding) error {
	return nil
}
func (f *fakeBroodClient) DeleteVarroaRoleBindingCRD(_ context.Context, _ string) error { return nil }
func (f *fakeBroodClient) ListJenkinsRoleCRDs(_ context.Context) ([]*v1alpha1.JenkinsRole, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetJenkinsRoleCRD(_ context.Context, _ string) (*v1alpha1.JenkinsRole, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (f *fakeBroodClient) DeleteJenkinsRoleCRD(_ context.Context, _ string) error { return nil }
func (f *fakeBroodClient) ListJenkinsRoleBindingCRDs(_ context.Context) ([]*v1alpha1.JenkinsRoleBinding, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetJenkinsRoleBindingCRD(_ context.Context, _ string) (*v1alpha1.JenkinsRoleBinding, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}
func (f *fakeBroodClient) DeleteJenkinsRoleBindingCRD(_ context.Context, _ string) error { return nil }
func (f *fakeBroodClient) ListPodTemplateCRDs(_ context.Context, _ string) ([]*v1alpha1.PodTemplate, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetPodTemplateCRD(_ context.Context, _, _ string) (*v1alpha1.PodTemplate, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyPodTemplateCRD(_ context.Context, _ *v1alpha1.PodTemplate) error {
	return nil
}
func (f *fakeBroodClient) DeletePodTemplateCRD(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) ListCatalogSourceCRDs(_ context.Context, _ string) ([]*v1alpha1.CatalogSource, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetCatalogSourceCRD(_ context.Context, _, _ string) (*v1alpha1.CatalogSource, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (f *fakeBroodClient) DeleteCatalogSourceCRD(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) PatchCatalogSourceStatus(_ context.Context, _, _ string, _ *v1alpha1.CatalogSourceStatus) error {
	return nil
}
func (f *fakeBroodClient) ListCatalogItemCRDs(_ context.Context, _, _ string) ([]*v1alpha1.CatalogItem, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetCatalogItemCRD(_ context.Context, _, _ string) (*v1alpha1.CatalogItem, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyCatalogItemCRD(_ context.Context, _ *v1alpha1.CatalogItem) error {
	return nil
}
func (f *fakeBroodClient) DeleteCatalogItemCRD(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) PatchCatalogItemStatus(_ context.Context, _, _ string, _ *v1alpha1.CatalogItemStatus) error {
	return nil
}
func (f *fakeBroodClient) ListComposedBundleCRDs(_ context.Context, _ string) ([]*v1alpha1.ComposedBundle, error) {
	return nil, nil
}
func (f *fakeBroodClient) PatchComposedBundleStatus(_ context.Context, _, _ string, _ *v1alpha1.ComposedBundleStatus) error {
	return nil
}
func (f *fakeBroodClient) ListBroodOperationCRDs(_ context.Context, _ string) ([]*v1alpha1.BroodOperation, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetBroodOperationCRD(_ context.Context, _, _ string) (*v1alpha1.BroodOperation, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyBroodOperationCRD(_ context.Context, _ *v1alpha1.BroodOperation) error {
	return nil
}
func (f *fakeBroodClient) DeleteBroodOperationCRD(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) PatchBroodOperationStatus(_ context.Context, _, _ string, _ *v1alpha1.BroodOperationStatus) error {
	return nil
}
func (f *fakeBroodClient) GetProvisioningDefaultsCRD(_ context.Context, _ string) (*v1alpha1.ProvisioningDefaults, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyProvisioningDefaultsCRD(_ context.Context, _ *v1alpha1.ProvisioningDefaults) error {
	return nil
}
func (f *fakeBroodClient) ListJenkinsVersionProfileCRDs(_ context.Context) ([]*v1alpha1.JenkinsVersionProfile, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetJenkinsVersionProfileCRD(_ context.Context, _ string) (*v1alpha1.JenkinsVersionProfile, error) {
	return nil, nil
}
func (f *fakeBroodClient) CreateJenkinsVersionProfileCRD(_ context.Context, _ *v1alpha1.JenkinsVersionProfile) error {
	return nil
}
func (f *fakeBroodClient) UpdateJenkinsVersionProfileCRD(_ context.Context, _ *v1alpha1.JenkinsVersionProfile) error {
	return nil
}
func (f *fakeBroodClient) DeleteJenkinsVersionProfileCRD(_ context.Context, _ string) error {
	return nil
}
func (f *fakeBroodClient) PatchJenkinsVersionProfileStatus(_ context.Context, _ string, _ *v1alpha1.JenkinsVersionProfileStatus) error {
	return nil
}
func (f *fakeBroodClient) GetUserCRD(_ context.Context, _, _ string) (*v1alpha1.User, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyUserCRD(_ context.Context, _ *v1alpha1.User) error { return nil }
func (f *fakeBroodClient) ListUserCRDs(_ context.Context, _ string) ([]*v1alpha1.User, error) {
	return nil, nil
}
func (f *fakeBroodClient) PatchUserStatus(_ context.Context, _, _ string, _ *v1alpha1.UserStatus) error {
	return nil
}
func (f *fakeBroodClient) ClearUserPassword(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) DeleteUserCRD(_ context.Context, _, _ string) error     { return nil }
func (f *fakeBroodClient) ListGroupCRDs(_ context.Context) ([]*v1alpha1.Group, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetGroupCRD(_ context.Context, _ string) (*v1alpha1.Group, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyGroupCRD(_ context.Context, _ *v1alpha1.Group) error { return nil }
func (f *fakeBroodClient) DeleteGroupCRD(_ context.Context, _ string) error         { return nil }
func (f *fakeBroodClient) ListTeamCRDs(_ context.Context) ([]*v1alpha1.Team, error) { return nil, nil }
func (f *fakeBroodClient) GetTeamCRD(_ context.Context, _ string) (*v1alpha1.Team, error) {
	return nil, nil
}
func (f *fakeBroodClient) ApplyTeamCRD(_ context.Context, _ *v1alpha1.Team) error { return nil }
func (f *fakeBroodClient) DeleteTeamCRD(_ context.Context, _ string) error        { return nil }
func (f *fakeBroodClient) PatchTeamStatus(_ context.Context, _ string, _ *v1alpha1.TeamStatus) error {
	return nil
}
func (f *fakeBroodClient) StreamPodLogs(_ context.Context, _, _, _ string, _ int64, _ bool) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeBroodClient) CreateService(_ context.Context, _, _ string, _ int32, _ string) error {
	return nil
}
func (f *fakeBroodClient) CreateServiceAccount(_ context.Context, _, _ string) error { return nil }
func (f *fakeBroodClient) CreateAgentRBAC(_ context.Context, _, _ string) error      { return nil }
func (f *fakeBroodClient) CreateStatefulSet(_ context.Context, _ controller.StatefulSetSpec) error {
	return nil
}
func (f *fakeBroodClient) UpdateStatefulSetOIDCEnv(_ context.Context, _, _, _, _, _, _, _, _, _, _, _ string) error {
	return nil
}
func (f *fakeBroodClient) EnsureStatefulSetPodLabel(_ context.Context, _, _, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeBroodClient) IsStatefulSetReady(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeBroodClient) GetStatefulSetPluginsChecksum(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeBroodClient) GetStatefulSetImages(_ context.Context, _, _ string) (map[string]string, map[string]string, error) {
	return nil, nil, nil
}
func (f *fakeBroodClient) GetStatefulSetContainerSpecs(_ context.Context, _, _ string) (string, string, *corev1.ResourceRequirements, *corev1.ResourceRequirements, string, map[string]string, bool, error) {
	return "", "", nil, nil, "", nil, false, nil
}
func (f *fakeBroodClient) ScaleStatefulSet(_ context.Context, _, _ string, _ int32) error { return nil }
func (f *fakeBroodClient) CreateSecret(_ context.Context, _, _ string, _ map[string]string, _ map[string][]byte) error {
	return nil
}
func (f *fakeBroodClient) CreateSecretExclusive(_ context.Context, _, _ string, _ map[string]string, _ map[string][]byte) error {
	return nil
}
func (f *fakeBroodClient) CreateOrUpdateSecret(_ context.Context, _, _ string, _ map[string][]byte) error {
	return nil
}
func (f *fakeBroodClient) PatchSecretData(_ context.Context, _, _ string, _ map[string][]byte) error {
	return nil
}
func (f *fakeBroodClient) GetSecret(_ context.Context, _, _ string) (map[string][]byte, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetSecretAnnotations(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}
func (f *fakeBroodClient) ListSecrets(_ context.Context, _, _ string) ([]map[string][]byte, error) {
	return nil, nil
}
func (f *fakeBroodClient) CopyImagePullSecret(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeBroodClient) CreateIngress(_ context.Context, _, _, _, _, _, _ string, _ map[string]string, _ string) error {
	return nil
}
func (f *fakeBroodClient) CreateOrUpdateConfigMap(_ context.Context, _, _ string, _ map[string]string, _ ...metav1.OwnerReference) error {
	return nil
}
func (f *fakeBroodClient) GetConfigMap(_ context.Context, _, _ string) (map[string]string, error) {
	return nil, nil
}
func (f *fakeBroodClient) RemoveConfigMapLabel(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeBroodClient) UpdateConfigMapData(_ context.Context, _, _ string, _ map[string]string) error {
	return nil
}
func (f *fakeBroodClient) DeleteResource(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeBroodClient) DeleteSecret(_ context.Context, _, _ string) error      { return nil }
func (f *fakeBroodClient) EnsureWakeEndpointSlice(_ context.Context, _, _ string, _ []string, _ int32) error {
	return nil
}
func (f *fakeBroodClient) DeleteWakeEndpointSlice(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeBroodClient) ListOperatorPodIPs(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetLiveResource(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (f *fakeBroodClient) CreateOrUpdateConfigMapWithOwner(_ context.Context, _, _ string, _ map[string]string, _ metav1.OwnerReference) error {
	return nil
}
func (f *fakeBroodClient) CreateOrUpdateOwnedConfigMap(_ context.Context, _, _ string, _ map[string]string, _ map[string]string) error {
	return nil
}
func (f *fakeBroodClient) RESTConfig() *rest.Config         { return nil }
func (f *fakeBroodClient) DynamicClient() dynamic.Interface { return nil }
func (f *fakeBroodClient) PatchControllerFinalizers(_ context.Context, _, _ string, _ []string) error {
	return nil
}
func (f *fakeBroodClient) PatchControllerStatus(_ context.Context, _, _ string, _ *v1alpha1.ControllerStatus) error {
	return nil
}
func (f *fakeBroodClient) PatchControllerAnnotations(_ context.Context, _, _ string, _ map[string]*string) error {
	return nil
}
func (f *fakeBroodClient) ListIngressHosts(_ context.Context) (map[string][]string, error) {
	return nil, nil
}
func (f *fakeBroodClient) ListResourceQuotas(_ context.Context, _ string) ([]corev1.ResourceQuota, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetNamespace(_ context.Context, _ string) (*corev1.Namespace, error) {
	return nil, nil
}
func (f *fakeBroodClient) GetControllerClassCRD(_ context.Context, _ string) (*v1alpha1.ControllerClass, error) {
	return nil, nil
}
func (f *fakeBroodClient) ListControllerClassCRDs(_ context.Context) ([]*v1alpha1.ControllerClass, error) {
	return nil, nil
}

// --- Tests ---

func TestBusBrood_ListAllMergeLocalAndRemote(t *testing.T) {
	fc := newFakeBroodClient()
	fc.controllers["team-a/local-ctrl"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
	}
	crdstore.MustSeed(fc.store, fc.controllers["team-a/local-ctrl"])
	bf := &busBrood{
		localCluster: "core",
		client:       fc,
		store:        fc.store,
		logger:       slog.Default(),
	}
	bf.membershipCache = []bus.ClusterInfo{{Name: "dev-cluster"}}
	bf.membershipCacheTime = time.Now()

	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "remote-ctrl", Namespace: "team-a"}}
		raw, _ := json.Marshal(cr)
		resp := bus.ControllersListResponse{Items: []json.RawMessage{raw}}
		return json.Marshal(resp)
	}

	cc, cs, err := bf.ListAll(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(cc) != 2 {
		t.Fatalf("expected 2 controllers, got %d", len(cc))
	}
	if len(cs) != 2 {
		t.Fatalf("expected 2 cluster statuses, got %d", len(cs))
	}
	foundLocal, foundRemote := false, false
	for _, c := range cc {
		if c.Cluster == "core" && c.CR.Name == "local-ctrl" {
			foundLocal = true
		}
		if c.Cluster == "dev-cluster" && c.CR.Name == "remote-ctrl" {
			foundRemote = true
		}
	}
	if !foundLocal || !foundRemote {
		t.Fatal("expected both local and remote controllers")
	}
}

func TestBusBrood_ListAllRemoteError(t *testing.T) {
	fc := newFakeBroodClient()
	fc.controllers["team-a/local-ctrl"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
	}
	crdstore.MustSeed(fc.store, fc.controllers["team-a/local-ctrl"])
	bf := &busBrood{
		localCluster: "core",
		client:       fc,
		store:        fc.store,
		logger:       slog.Default(),
	}
	bf.membershipCache = []bus.ClusterInfo{
		{Name: "dev-cluster"},
		{Name: "prod-east"},
	}
	bf.membershipCacheTime = time.Now()

	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		if subject == "operator.dev-cluster.controllers.list" {
			return json.Marshal(bus.ControllersListResponse{Items: []json.RawMessage{}})
		}
		return nil, errors.New("timeout")
	}

	cc, cs, err := bf.ListAll(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(cc) != 1 || cc[0].Cluster != "core" {
		t.Fatalf("expected 1 core controller, got %d", len(cc))
	}
	if len(cs) != 3 {
		t.Fatalf("expected 3 cluster statuses, got %d", len(cs))
	}
	if cs[0].Name != "core" || !cs[0].OK {
		t.Fatal("expected core first and ok")
	}
	foundDown := false
	for _, s := range cs {
		if s.Name == "prod-east" && !s.OK && s.Error != "" {
			foundDown = true
		}
	}
	if !foundDown {
		t.Fatal("expected prod-east to have ok=false with error")
	}
}

func TestBusBrood_ListAllClusterFilterLocal(t *testing.T) {
	fc := newFakeBroodClient()
	fc.controllers["team-a/local-ctrl"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "local-ctrl", Namespace: "team-a"},
	}
	crdstore.MustSeed(fc.store, fc.controllers["team-a/local-ctrl"])
	bf := &busBrood{
		localCluster: "core",
		client:       fc,
		store:        fc.store,
		logger:       slog.Default(),
	}
	bf.membershipCache = []bus.ClusterInfo{{Name: "dev-cluster"}}
	bf.membershipCacheTime = time.Now()

	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		t.Fatal("request should not be called when clusterFilter=core")
		return nil, nil
	}

	cc, cs, err := bf.ListAll(context.Background(), "", "core")
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(cc) != 1 || cc[0].Cluster != "core" {
		t.Fatalf("expected core controller, got %d", len(cc))
	}
	if len(cs) != 1 || !cs[0].OK {
		t.Fatal("expected 1 ok status")
	}
}

func TestBusBrood_BroodErrorCodeMapping(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		wantCode string
	}{
		{name: "not_found", code: bus.CodeNotFound, wantCode: bus.CodeNotFound},
		{name: "conflict", code: bus.CodeConflict, wantCode: bus.CodeConflict},
		{name: "invalid", code: bus.CodeInvalid, wantCode: bus.CodeInvalid},
		{name: "internal", code: bus.CodeInternal, wantCode: bus.CodeInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bf := &busBrood{
				localCluster: "core",
				logger:       slog.Default(),
				client:       newFakeBroodClient(),
			}
			bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
				resp := bus.ControllersGetResponse{Code: tt.code, Error: "test error"}
				return json.Marshal(resp)
			}
			_, err := bf.Get(context.Background(), "dev-cluster", "team-a", "test-ctrl")
			if err == nil {
				t.Fatal("expected error")
			}
			var fe *BroodError
			if !errors.As(err, &fe) {
				t.Fatalf("expected *BroodError, got %T: %v", err, err)
			}
			if fe.Code != tt.wantCode {
				t.Fatalf("expected code %q, got %q", tt.wantCode, fe.Code)
			}
		})
	}
}

// TestBroodErrorConflicts verifies that BroodError.Conflicts survives the
// error round-trip through busBrood.Update.
func TestBroodErrorConflicts(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		logger:       slog.Default(),
		client:       newFakeBroodClient(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp := bus.ControllersUpdateResponse{
			Code:  bus.CodeConflict,
			Error: "field conflict",
			Conflicts: []bus.FieldConflict{
				{Field: ".spec.resources", Manager: "other-manager", Message: `conflict with "other-manager"`},
			},
		}
		return json.Marshal(resp)
	}
	_, _, _, err := bf.Update(context.Background(), "dev-cluster", "team-a", "test-ctrl", json.RawMessage(`{"spec":{"version":"2.0"}}`), "varroa-ui", false)
	if err == nil {
		t.Fatal("expected error")
	}
	var fe *BroodError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *BroodError, got %T: %v", err, err)
	}
	if len(fe.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(fe.Conflicts))
	}
	if fe.Conflicts[0].Field != ".spec.resources" || fe.Conflicts[0].Manager != "other-manager" {
		t.Errorf("Conflicts[0] = %+v, want {Field:.spec.resources Manager:other-manager}", fe.Conflicts[0])
	}
}

// --- Drain/DrainCancel/StateOf tests ---

func TestBusBrood_DrainSuccess(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp := bus.ClusterDrainResponse{State: "draining"}
		return json.Marshal(resp)
	}
	state, err := bf.Drain(context.Background(), "dev-cluster", "admin")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if state != "draining" {
		t.Errorf("state = %q, want %q", state, "draining")
	}
}

func TestBusBrood_DrainConflict(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp := bus.ClusterDrainResponse{Error: "cluster is not draining", Code: bus.CodeConflict}
		return json.Marshal(resp)
	}
	_, err := bf.Drain(context.Background(), "dev-cluster", "admin")
	if err == nil {
		t.Fatal("expected error")
	}
	var fe *BroodError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *BroodError, got %T: %v", err, err)
	}
	if fe.Code != bus.CodeConflict {
		t.Errorf("code = %q, want %q", fe.Code, bus.CodeConflict)
	}
}

func TestBusBrood_DrainUnreachable(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		return nil, errors.New("nats timeout")
	}
	_, err := bf.Drain(context.Background(), "dev-cluster", "admin")
	if err == nil {
		t.Fatal("expected error")
	}
	var ec *ErrClusterUnreachable
	if !errors.As(err, &ec) {
		t.Fatalf("expected *ErrClusterUnreachable, got %T: %v", err, err)
	}
	if ec.Cluster != "dev-cluster" {
		t.Errorf("cluster = %q, want %q", ec.Cluster, "dev-cluster")
	}
}

func TestBusBrood_DrainCancelSuccess(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp := bus.ClusterDrainCancelResponse{State: "active"}
		return json.Marshal(resp)
	}
	state, err := bf.DrainCancel(context.Background(), "dev-cluster", "admin")
	if err != nil {
		t.Fatalf("DrainCancel: %v", err)
	}
	if state != "active" {
		t.Errorf("state = %q, want %q", state, "active")
	}
}

func TestBusBrood_StateOfLocalCluster(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	state, err := bf.StateOf(context.Background(), "core")
	if err != nil {
		t.Fatalf("StateOf: %v", err)
	}
	if state != bus.ClusterStateActive {
		t.Errorf("state = %q, want %q", state, bus.ClusterStateActive)
	}
}

func TestBusBrood_StateOfRemoteCluster(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.membershipCache = []bus.ClusterInfo{
		{Name: "dev-cluster", State: bus.ClusterStateDraining},
	}
	bf.membershipCacheTime = time.Now()

	state, err := bf.StateOf(context.Background(), "dev-cluster")
	if err != nil {
		t.Fatalf("StateOf: %v", err)
	}
	if state != bus.ClusterStateDraining {
		t.Errorf("state = %q, want %q", state, bus.ClusterStateDraining)
	}
}

func TestBusBrood_StateOfUnknownCluster(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.membershipCache = []bus.ClusterInfo{} // empty cache, no members
	bf.membershipCacheTime = time.Now()
	_, err := bf.StateOf(context.Background(), "unknown")
	if err == nil {
		t.Fatal("expected error for unknown cluster")
	}
}

func TestBusBrood_SelfRowDefaultsActive(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.membershipCache = []bus.ClusterInfo{} // no members; just self row
	bf.membershipCacheTime = time.Now()
	clusters, err := bf.Clusters(context.Background())
	if err != nil {
		t.Fatalf("Clusters: %v", err)
	}
	if len(clusters) == 0 {
		t.Fatal("expected at least self row")
	}
	if clusters[0].State != bus.ClusterStateActive {
		t.Errorf("self row State = %q, want %q", clusters[0].State, bus.ClusterStateActive)
	}
}

// Stub methods for new ResourceClient interface methods (add-remote-config-authoring).
func (f *fakeBroodClient) CreateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (f *fakeBroodClient) UpdateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (f *fakeBroodClient) CreateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (f *fakeBroodClient) UpdateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (f *fakeBroodClient) CreateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (f *fakeBroodClient) UpdateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (f *fakeBroodClient) CreateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}
func (f *fakeBroodClient) UpdateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}

func (f *fakeBroodClient) PatchUpdateCenterStatus(_ context.Context, _ string, _ *v1alpha1.UpdateCenterStatus) error {
	return nil
}

func (f *fakeBroodClient) GetUpdateCenter(_ context.Context, _ string) (*v1alpha1.UpdateCenter, error) {
	return nil, nil
}

func (f *fakeBroodClient) GetPVC(_ context.Context, _, _ string) (*corev1.PersistentVolumeClaim, error) {
	return nil, nil
}

func TestBusBrood_DiscoverNamespaces_RemoteHappy(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp := bus.NamespacesListResponse{
			ManagedNamespaces: []string{"ns-a", "ns-b"},
			CuratedNamespaces: []string{"team-a"},
			CuratedDefault:    "team-a",
		}
		return json.Marshal(resp)
	}
	r, err := bf.DiscoverNamespaces(context.Background(), "dev-cluster")
	if err != nil {
		t.Fatalf("DiscoverNamespaces: %v", err)
	}
	if len(r.ManagedNamespaces) != 2 || r.ManagedNamespaces[0] != "ns-a" {
		t.Errorf("managed = %v, want [ns-a ns-b]", r.ManagedNamespaces)
	}
	if len(r.CuratedNamespaces) != 1 || r.CuratedNamespaces[0] != "team-a" {
		t.Errorf("curated = %v, want [team-a]", r.CuratedNamespaces)
	}
	if r.CuratedDefault != "team-a" {
		t.Errorf("curatedDefault = %q, want team-a", r.CuratedDefault)
	}
}

func TestBusBrood_DiscoverNamespaces_Timeout(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		return nil, io.ErrUnexpectedEOF
	}
	_, err := bf.DiscoverNamespaces(context.Background(), "dev-cluster")
	var unreachable *ErrClusterUnreachable
	if !errors.As(err, &unreachable) {
		t.Fatalf("expected ErrClusterUnreachable, got %T: %v", err, err)
	}
	if unreachable.Cluster != "dev-cluster" {
		t.Errorf("cluster = %q, want dev-cluster", unreachable.Cluster)
	}
}

func TestBusBrood_DiscoverNamespaces_StructuredError(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp := bus.NamespacesListResponse{Error: "provisioning defaults read failed", Code: bus.CodeInternal}
		return json.Marshal(resp)
	}
	_, err := bf.DiscoverNamespaces(context.Background(), "dev-cluster")
	var fe *BroodError
	if !errors.As(err, &fe) {
		t.Fatalf("expected BroodError, got %T: %v", err, err)
	}
	if fe.Code != bus.CodeInternal {
		t.Errorf("code = %q, want %q", fe.Code, bus.CodeInternal)
	}
}

func TestBusBrood_DiscoverNamespaces_LocalCluster(t *testing.T) {
	bf := &busBrood{
		localCluster: "core",
		client:       newFakeBroodClient(),
		logger:       slog.Default(),
	}
	bf.request = func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
		t.Fatal("request should not be called for local cluster")
		return nil, nil
	}
	_, err := bf.DiscoverNamespaces(context.Background(), "core")
	if err == nil {
		t.Fatal("expected error for local cluster call")
	}
}
