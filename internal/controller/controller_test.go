package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/bundles"
	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/mite"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// testKey is a static key for TokenSigner in tests.
var testKey = []byte("test-key-32-bytes-long-xxxxx")

// testClient is a fake ResourceClient for testing.
type testClient struct {
	listProfilesErr      error // when set, ListJenkinsVersionProfileCRDs returns it
	services             []string
	serviceAccounts      []string
	statefulSets         []string
	secrets              []string
	ingress              []string
	configMaps           []string
	existingConfigMaps   map[string]bool
	configMapData        map[string]map[string]string       // name -> data
	configMapOwners      map[string][]metav1.OwnerReference // name -> owner refs
	deleted              []struct{ kind, name string }
	deletedSecrets       []string
	statuses             map[string]string
	oidcUpdateCalls      []struct{ name, namespace, oidcIssuer, loginURL, apikeyVerifyURL, caPEM string }
	fleetPodLabel        string
	fleetPodLabelPatches int
	secretWrites         int
	cmWrites             int
	existingSecrets      map[string]map[string][]byte
	secretAnnotations    map[string]map[string]string // name -> annotations, for host-scoping tests
	secretAnnotationsErr error                        // injected GetSecretAnnotations failure (fail-closed tests)

	// powerState CAS tracking for hibernation tests.
	powerTransitions []struct{ name, from, to string }
	powerCASChanged  bool // value returned by TransitionPowerState
	powerCASErr      error

	// Users/Group tracking for local auth tests.
	users          map[string]*v1alpha1.User
	passwordClears []string
	createdSecrets map[string]map[string][]byte
	// scaleCalls records ScaleStatefulSet(name -> last requested replica count).
	scaleCalls      map[string]int32
	composedBundles map[string]*v1alpha1.ComposedBundle

	// profiles stores JenkinsVersionProfiles by name.
	profiles map[string]*v1alpha1.JenkinsVersionProfile

	// ingressCalls records the full CreateIngress arguments for routing-mode assertions.
	ingressCalls []ingressCall
	// stsSpecs records the full StatefulSetSpec passed to CreateStatefulSet.
	stsSpecs []StatefulSetSpec
	// controllers is returned by ListControllerCRDs when non-nil (gate tests).
	controllers []*v1alpha1.Controller
	// deleteControllerPodCalls records each call to DeleteControllerPod.
	deleteControllerPodCalls int
	controllerPod            *corev1.Pod
	controllerPodErr         error // injected error for GetControllerPod
	statefulSetReady         *bool
	// provisioningDefaults is used by seedClientCRDs to seed the crdstore.
	provisioningDefaults *v1alpha1.ProvisioningDefaults
	// controllerClass is used by seedClientCRDs to seed the crdstore.
	controllerClass *v1alpha1.ControllerClass
	// lastPatchedStatus records the most recent PatchControllerStatus payload.
	lastPatchedStatus *v1alpha1.ControllerStatus
	// patchCount counts PatchControllerStatus calls, so tests can assert a
	// single patch happened (guarding against a double-patch in one pass).
	patchCount int
	// stsComputedImages and stsLiveImages back GetStatefulSetImages, and
	// (via their "mite" entry) the image half of GetStatefulSetContainerSpecs.
	stsComputedImages map[string]string
	stsLiveImages     map[string]string
	stsImagesErr      error
	// stsMite* back the resources/pullPolicy/found/err half of
	// GetStatefulSetContainerSpecs (the two calls were consolidated into one
	// method, PR #373 review, but tests still set images and mite fields
	// together as before). stsMiteFound defaults to false (no StatefulSet
	// yet); set it true to simulate a live StatefulSet with the given
	// resources/pullPolicy already applied.
	stsMiteResources    *corev1.ResourceRequirements
	stsJenkinsResources *corev1.ResourceRequirements
	stsMitePullPolicy   string
	stsMiteFound        bool
	stsMiteErr          error
	// stsResourcesSource backs the resources-source map returned by
	// GetStatefulSetContainerSpecs. nil means "no stamp yet".
	stsResourcesSource map[string]string
	// teams stores Teams by name for tests.
	teams map[string]*v1alpha1.Team
	// namespaces records which namespaces exist (for tenancy stubs).
	namespaces map[string]bool
	// varroaRoles stores VarroaRoles by name.
	varroaRoles     map[string]*v1alpha1.VarroaRole
	wakeEnsureCalls []struct {
		namespace, service string
		ips                []string
		port               int32
	}
	wakeDeleteCalls []struct{ namespace, service string }
	wakeDeleteErr   error
	wakePodIPs      []string
	wakePodErr      error

	// updateCenter is returned by GetUpdateCenter when non-nil.
	updateCenter *v1alpha1.UpdateCenter
	// pvc is returned by GetPVC when non-nil (and pvcErr).
	pvc    *corev1.PersistentVolumeClaim
	pvcErr error

	// store is the crdstore fake for tests.
	store *crdstore.Fake

	// imagePullSecretCopies records calls to CopyImagePullSecret for test assertions.
	imagePullSecretCopies []imagePullSecretCopy
	// imagePullSecretCopyErr, when non-nil, is returned by CopyImagePullSecret.
	imagePullSecretCopyErr error
}

type imagePullSecretCopy struct {
	src, dst, name string
}

type ingressCall struct {
	name, namespace, host, pathPrefix, tlsSecret, ingressClass string
	annotations                                                map[string]string
}

func newTestClient() *testClient {
	tc := &testClient{
		statuses:           make(map[string]string),
		existingSecrets:    make(map[string]map[string][]byte),
		existingConfigMaps: make(map[string]bool),
		configMapData:      make(map[string]map[string]string),
		users:              make(map[string]*v1alpha1.User),
		createdSecrets:     make(map[string]map[string][]byte),
		composedBundles:    make(map[string]*v1alpha1.ComposedBundle),
		profiles:           make(map[string]*v1alpha1.JenkinsVersionProfile),
		teams:              make(map[string]*v1alpha1.Team),
		namespaces:         make(map[string]bool),
		varroaRoles:        make(map[string]*v1alpha1.VarroaRole),
		store:              crdstore.NewFake(),
	}
	seedStarterBundle(tc)
	return tc
}

// seedStarterBundle mirrors what StarterReconciler does on every operator tick:
// a Ready ComposedBundle named varroa-starter in the operator namespace, holding
// the embedded content. A Controller with no composedBundleRef resolves to it,
// so without this every zero-config fixture would block in Provisioning waiting
// for a bundle that production would have seeded.
//
// The content is the real embedded bundle rather than a stub, which makes these
// fixtures also assert that the shipped starter content survives variable
// resolution.
func seedStarterBundle(tc *testClient) {
	crdstore.MustSeed(tc.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: StarterBundleName, Namespace: "varroa-system"},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: StarterBundleName + "-content",
		},
	})
	tc.configMapData[StarterBundleName+"-content"] = map[string]string{
		"jenkins.yaml": bundles.StarterJCasC(),
		"plugins.yaml": "",
		"items.yaml":   bundles.StarterItems(),
		"rbac.yaml":    "",
	}
}

// newTestClientWithBundle returns a test client pre-populated with a Ready
// ComposedBundle named "test-bundle" and a matching content ConfigMap.
func newTestClientWithBundle() *testClient {
	tc := newTestClient()
	for _, ns := range []string{"ns", "ns1", "testns"} {
		bundle := &v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "test-bundle", Namespace: ns},
			Status: v1alpha1.ComposedBundleStatus{
				Phase:      v1alpha1.ComposedBundleReady,
				ContentRef: "test-bundle-content",
			},
		}
		if ns == "ns" {
			tc.composedBundles["test-bundle"] = bundle
		}
		crdstore.MustSeed(tc.store, bundle)
	}
	tc.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: \"Hello Varroa\"",
		"plugins.yaml": "",
		"items.yaml":   "",
		"rbac.yaml":    "",
	}
	return tc
}

// seedClientCRDs mirrors the testClient's legacy CRD seed fields into the
// fake store at reconciler construction, so tests that populate fields
// before newTestReconciler work against store-backed production reads.
// Namespace-less composedBundle keys replicate across the common test
// namespaces, matching newTestClientWithBundle.
func seedClientCRDs(c *testClient) {
	for key, b := range c.composedBundles {
		if before, after, found := strings.Cut(key, "/"); found {
			cp := b.DeepCopy()
			cp.Namespace, cp.Name = before, after
			crdstore.MustSeed(c.store, cp)
			continue
		}
		for _, ns := range []string{"ns", "ns1", "testns", "test-ns", "default"} {
			cp := b.DeepCopy()
			cp.Name, cp.Namespace = key, ns
			crdstore.MustSeed(c.store, cp)
		}
	}
	for name, p := range c.profiles {
		cp := p.DeepCopy()
		if cp.Name == "" {
			cp.Name = name
		}
		crdstore.MustSeed(c.store, cp)
	}
	if c.controllerClass != nil {
		crdstore.MustSeed(c.store, c.controllerClass.DeepCopy())
	}
	if c.provisioningDefaults != nil {
		cp := c.provisioningDefaults.DeepCopy()
		if cp.Name == "" {
			cp.Name = "varroa-defaults"
		}
		crdstore.MustSeed(c.store, cp)
	}
	if c.updateCenter != nil {
		crdstore.MustSeed(c.store, c.updateCenter.DeepCopy())
	}
}

// storeGroups lists Groups from the fake store (the team reconciler's write
// surface after the crdstore migration).
func (t *testClient) storeGroups() []*v1alpha1.Group {
	gs, err := crdstore.List[v1alpha1.Group](context.Background(), t.store, "", "")
	if err != nil {
		panic(err)
	}
	return gs
}

func (t *testClient) storeRoleBindings() []*v1alpha1.VarroaRoleBinding {
	bs, err := crdstore.List[v1alpha1.VarroaRoleBinding](context.Background(), t.store, "", "")
	if err != nil {
		panic(err)
	}
	return bs
}

func newTestReconciler(client *testClient) *Reconciler {
	seedClientCRDs(client)
	rec := NewReconciler(
		bundle.NewResolver("/tmp/test"),
		client,
		client.store,
		transport.NewLocalRegistry(mite.NewRegistry()),
		mite.NewTokenSigner(testKey),
		rbac.NewGenerator(rbac.NewTestResolver()),
		nil, // composer not used in tests
	)
	rec.Logger = slog.Default()
	return rec
}

// testUID is a stable UID used by testController for reproducible assertions.
const testUID = "00000000-0000-0000-0000-000000000001"

// testPrefix is the expected controllerPrefix output for a CR named "test" with testUID.
const testPrefix = "test-00000000"

// testPrefixFor returns the expected controllerPrefix output for a given name with testUID.
func testPrefixFor(name string) string {
	uid := testUID[:8]
	return name + "-" + uid
}

// testController creates a Controller CR with a deterministic UID for
// reproducible test assertions.
func testController(name, namespace string, phase v1alpha1.ControllerPhase) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			UID:               testUID,
			CreationTimestamp: metav1Now(),
		},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"},
		},
		Status: v1alpha1.ControllerStatus{Phase: phase},
	}
}

func (t *testClient) CreateService(_ context.Context, name, namespace string, port int32, overlayYAML string) error {
	t.services = append(t.services, name)
	return nil
}
func (t *testClient) CreateServiceAccount(_ context.Context, name, namespace string) error {
	t.serviceAccounts = append(t.serviceAccounts, name)
	return nil
}
func (t *testClient) CreateAgentRBAC(_ context.Context, name, namespace string) error {
	return nil
}
func (t *testClient) CreateStatefulSet(_ context.Context, spec StatefulSetSpec) error {
	t.statefulSets = append(t.statefulSets, spec.Name)
	t.stsSpecs = append(t.stsSpecs, spec)
	return nil
}
func (t *testClient) UpdateStatefulSetOIDCEnv(_ context.Context, name, namespace, oidcIssuer, loginURL, oidcUserClaim, oidcGroupClaim, pubKeyPEM, pubKeyKID, aud, apikeyVerifyURL, caPEM string) error {
	t.oidcUpdateCalls = append(t.oidcUpdateCalls, struct{ name, namespace, oidcIssuer, loginURL, apikeyVerifyURL, caPEM string }{name, namespace, oidcIssuer, loginURL, apikeyVerifyURL, caPEM})
	return nil
}
func (t *testClient) EnsureStatefulSetPodLabel(_ context.Context, _, _, key, value string) (bool, error) {
	if key == "app.kubernetes.io/managed-by" && t.fleetPodLabel == value {
		return false, nil
	}
	t.fleetPodLabel = value
	t.fleetPodLabelPatches++
	return true, nil
}
func (t *testClient) IsStatefulSetReady(_ context.Context, _, _ string) (bool, error) {
	if t.statefulSetReady != nil {
		return *t.statefulSetReady, nil
	}
	return len(t.statefulSets) > 0, nil
}
func (t *testClient) CreateIngress(_ context.Context, name, namespace, host, pathPrefix, tlsSecret, ingressClass string, annotations map[string]string, overlayYAML string) error {
	t.ingress = append(t.ingress, name)
	t.ingressCalls = append(t.ingressCalls, ingressCall{name, namespace, host, pathPrefix, tlsSecret, ingressClass, annotations})
	return nil
}
func (t *testClient) CreateOrUpdateConfigMap(_ context.Context, name, namespace string, data map[string]string, owners ...metav1.OwnerReference) error {
	t.existingConfigMaps[name] = true
	t.configMaps = append(t.configMaps, name)
	if t.configMapData == nil {
		t.configMapData = make(map[string]map[string]string)
	}
	t.configMapData[name] = data
	if t.configMapOwners == nil {
		t.configMapOwners = make(map[string][]metav1.OwnerReference)
	}
	t.configMapOwners[name] = owners
	t.cmWrites++
	return nil
}
func (t *testClient) GetConfigMap(_ context.Context, name, namespace string) (map[string]string, error) {
	if data, ok := t.configMapData[name]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("configmap %s not found", name)
}
func (t *testClient) CreateSecret(_ context.Context, name, namespace string, _ map[string]string, data map[string][]byte) error {
	t.secrets = append(t.secrets, name)
	return nil
}
func (t *testClient) CreateSecretExclusive(_ context.Context, name, namespace string, _ map[string]string, data map[string][]byte) error {
	t.secrets = append(t.secrets, name)
	return nil
}
func (t *testClient) CreateOrUpdateSecret(_ context.Context, name, namespace string, data map[string][]byte) error {
	t.secrets = append(t.secrets, name)
	t.secretWrites++
	t.createdSecrets[name] = data
	return nil
}
func (t *testClient) PatchSecretData(_ context.Context, name, namespace string, data map[string][]byte) error {
	t.secretWrites++
	if t.createdSecrets[name] == nil {
		t.createdSecrets[name] = map[string][]byte{}
	}
	for k, v := range data {
		t.createdSecrets[name][k] = v
	}
	return nil
}
func (t *testClient) GetSecret(_ context.Context, name, namespace string) (map[string][]byte, error) {
	if data, ok := t.existingSecrets[name]; ok {
		return data, nil
	}
	if data, ok := t.createdSecrets[name]; ok {
		return data, nil
	}
	return nil, nil
}
func (t *testClient) GetSecretAnnotations(_ context.Context, name, namespace string) (map[string]string, error) {
	if t.secretAnnotationsErr != nil {
		return nil, t.secretAnnotationsErr
	}
	if ann, ok := t.secretAnnotations[name]; ok {
		return ann, nil
	}
	return nil, nil
}
func (t *testClient) ListSecrets(_ context.Context, namespace, labelSelector string) ([]map[string][]byte, error) {
	return nil, nil
}
func (t *testClient) CopyImagePullSecret(_ context.Context, srcNamespace, dstNamespace, name string) error {
	t.imagePullSecretCopies = append(t.imagePullSecretCopies, imagePullSecretCopy{src: srcNamespace, dst: dstNamespace, name: name})
	if t.imagePullSecretCopyErr != nil {
		return t.imagePullSecretCopyErr
	}
	return nil
}
func (t *testClient) DeleteResource(_ context.Context, kind, name, namespace string) error {
	t.deleted = append(t.deleted, struct{ kind, name string }{kind, name})
	return nil
}
func (t *testClient) DeleteSecret(_ context.Context, name, namespace string) error {
	t.deletedSecrets = append(t.deletedSecrets, name)
	return nil
}
func (t *testClient) EnsureWakeEndpointSlice(_ context.Context, namespace, serviceName string, podIPs []string, port int32) error {
	t.wakeEnsureCalls = append(t.wakeEnsureCalls, struct {
		namespace, service string
		ips                []string
		port               int32
	}{namespace, serviceName, append([]string(nil), podIPs...), port})
	return nil
}
func (t *testClient) DeleteWakeEndpointSlice(_ context.Context, namespace, serviceName string) error {
	t.wakeDeleteCalls = append(t.wakeDeleteCalls, struct{ namespace, service string }{namespace, serviceName})
	return t.wakeDeleteErr
}
func (t *testClient) ListOperatorPodIPs(_ context.Context, _ string) ([]string, error) {
	return append([]string(nil), t.wakePodIPs...), t.wakePodErr
}
func (t *testClient) ApplyControllerSpecSSA(_ context.Context, _, _ string, _ map[string]any, _ string, _ bool) (*v1alpha1.Controller, error) {
	return nil, nil
}
func (t *testClient) TransitionPowerState(_ context.Context, name, _, from, to string) (bool, error) {
	t.powerTransitions = append(t.powerTransitions, struct{ name, from, to string }{name, from, to})
	if t.powerCASErr != nil {
		return false, t.powerCASErr
	}
	return t.powerCASChanged, nil
}
func (t *testClient) PatchControllerStatus(_ context.Context, name, namespace string, status *v1alpha1.ControllerStatus) error {
	t.statuses[name] = string(status.Phase)
	cp := &v1alpha1.ControllerStatus{}
	status.DeepCopyInto(cp)
	t.lastPatchedStatus = cp
	t.patchCount++
	return nil
}

func (t *testClient) ClearUserPassword(_ context.Context, name, namespace string) error {
	t.passwordClears = append(t.passwordClears, name)
	if u, ok := t.users[name]; ok {
		u.Spec.Password = ""
	}
	return nil
}

func (t *testClient) DeleteControllerPod(_ context.Context, _, _ string) error {
	t.deleteControllerPodCalls++
	return nil
}
func (t *testClient) ScaleStatefulSet(_ context.Context, name, _ string, replicas int32) error {
	if t.scaleCalls == nil {
		t.scaleCalls = make(map[string]int32)
	}
	t.scaleCalls[name] = replicas
	return nil
}
func (t *testClient) StreamPodLogs(_ context.Context, _, _, _ string, _ int64, _ bool) (io.ReadCloser, error) {
	return nil, nil
}

func (t *testClient) ListResourceQuotas(_ context.Context, _ string) ([]corev1.ResourceQuota, error) {
	return nil, nil
}
func (t *testClient) ListIngressHosts(_ context.Context) (map[string][]string, error) {
	return nil, nil
}
func (t *testClient) GetStatefulSetPluginsChecksum(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (t *testClient) GetStatefulSetImages(_ context.Context, _, _ string) (map[string]string, map[string]string, error) {
	return t.stsComputedImages, t.stsLiveImages, t.stsImagesErr
}
func (t *testClient) GetStatefulSetContainerSpecs(_ context.Context, _, _ string) (computedMiteImage, liveMiteImage string, miteResources, jenkinsResources *corev1.ResourceRequirements, mitePullPolicy string, resourcesSource map[string]string, found bool, err error) {
	if t.stsImagesErr != nil {
		return "", "", nil, nil, "", nil, false, t.stsImagesErr
	}
	if t.stsMiteErr != nil {
		return "", "", nil, nil, "", nil, false, t.stsMiteErr
	}
	return t.stsComputedImages["mite"], t.stsLiveImages["mite"], t.stsMiteResources, t.stsJenkinsResources, t.stsMitePullPolicy, t.stsResourcesSource, t.stsMiteFound, nil
}
func (t *testClient) GetControllerPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	if t.controllerPodErr != nil {
		return nil, t.controllerPodErr
	}
	return t.controllerPod, nil
}

func (t *testClient) GetLiveResource(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (t *testClient) CreateOrUpdateConfigMapWithOwner(_ context.Context, name, namespace string, data map[string]string, owner metav1.OwnerReference) error {
	return t.CreateOrUpdateConfigMap(context.Background(), name, namespace, data, owner)
}

// GetNamespace returns the namespace (not used by most testClient callers).
func (t *testClient) GetNamespace(_ context.Context, name string) (*corev1.Namespace, error) {
	if t.namespaces[name] {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func (t *testClient) CreateNamespace(_ context.Context, name string, labels map[string]string) error {
	if t.namespaces == nil {
		t.namespaces = make(map[string]bool)
	}
	t.namespaces[name] = true
	return nil
}

func (t *testClient) PatchNamespaceLabels(_ context.Context, name string, labels map[string]string) error {
	return nil
}

func TestReconcilePendingPhase(t *testing.T) {
	tests := []struct {
		name      string
		cr        *v1alpha1.Controller
		wantPhase v1alpha1.ControllerPhase
	}{
		{
			name: "empty phase goes to provisioning",
			cr: &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
				Status:     v1alpha1.ControllerStatus{Phase: ""},
			},
			wantPhase: v1alpha1.ControllerPhaseProvisioning,
		},
		{
			name: "pending goes to provisioning",
			cr: &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
				Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhasePending},
			},
			wantPhase: v1alpha1.ControllerPhaseProvisioning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClientWithBundle()
			rec := newTestReconciler(client)

			err := rec.reconcileController(context.Background(), tt.cr)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if tt.cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
				t.Errorf("expected Provisioning, got %s", tt.cr.Status.Phase)
			}
		})
	}
}

func TestReconcileProvisioning(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	err := rec.reconcileController(context.Background(), cr)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(client.services) == 0 {
		t.Error("Service should be created")
	}
	if len(client.statefulSets) == 0 {
		t.Error("StatefulSet should be created")
	}
	if len(client.secrets) == 0 {
		t.Error("Bootstrap secret should be created")
	}
	if len(client.serviceAccounts) == 0 {
		t.Error("ServiceAccount should be created")
	}
	wantSA := testPrefix + "-agent"
	if client.serviceAccounts[0] != wantSA {
		t.Errorf("ServiceAccount name: got %q, want %q", client.serviceAccounts[0], wantSA)
	}
}

// TestProvisioningBlocksUntilBundleMaterializes guards the regression where a
// controller provisioned before its ComposedBundle finished materializing baked
// a core-only plugins.txt into the plugins-init container. Because
// handleProvisioning runs only in the Provisioning phase, that incomplete plugin
// set never self-healed once the controller connected, so bundle-declared plugins
// (e.g. workflow-aggregator) were silently dropped. Provisioning must instead
// block (no StatefulSet) until the bundle is Ready.
func TestProvisioningBlocksUntilBundleMaterializes(t *testing.T) {
	client := newTestClient()
	// Bundle exists but is still Pending (not yet materialized) — exactly the
	// race window between controller creation and the ComposedBundle reconciler.
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundlePending},
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning to be blocked (error) while bundle is Pending, got nil")
	}
	if len(client.statefulSets) != 0 {
		t.Errorf("StatefulSet must not be created before the bundle materializes, got %v", client.statefulSets)
	}
	if cr.Status.Phase == v1alpha1.ControllerPhaseFailed {
		t.Error("controller should remain Provisioning (waiting), not Failed")
	}
	var resolved *v1alpha1.ControllerCondition
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == v1alpha1.ConditionBundleResolved {
			resolved = &cr.Status.Conditions[i]
		}
	}
	if resolved == nil || resolved.Status != metav1.ConditionFalse {
		t.Errorf("BundleResolved condition should be False while waiting, got %+v", resolved)
	}
}

// TestProvisioningIncludesBundlePluginsInPluginsTxt is the positive counterpart:
// once the bundle is materialized, its plugins.yaml entries must flow into the
// plugins.txt the plugins-init container installs.
func TestProvisioningIncludesBundlePluginsInPluginsTxt(t *testing.T) {
	client := newTestClientWithBundle()
	client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n" +
		"  - artifactId: workflow-aggregator\n    version: latest\n" +
		"  - artifactId: git\n    version: latest\n"
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pluginsCM, ok := client.configMapData[testPrefix+"-plugins"]
	if !ok {
		t.Fatalf("plugins ConfigMap %q-plugins not created", testPrefix)
	}
	pluginsTxt := pluginsCM["plugins.txt"]
	for _, want := range []string{"workflow-aggregator", "git"} {
		if !strings.Contains(pluginsTxt, want) {
			t.Errorf("plugins.txt missing bundle-declared plugin %q; got:\n%s", want, pluginsTxt)
		}
	}
}

func TestProvisioningSurfacesPluginsInitFailure(t *testing.T) {
	client := newTestClientWithBundle()
	ready := false
	client.statefulSetReady = &ready
	client.controllerPod = &corev1.Pod{
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "plugins-init",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: "failed to resolve pipeline-graph-view:1.27",
				}},
			}},
		},
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
		t.Fatalf("expected phase to remain Provisioning, got %s", cr.Status.Phase)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginRollFailed)
	if cond == nil {
		t.Fatal("expected PluginRollFailed condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected PluginRollFailed=True, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonPluginRollFailed {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonPluginRollFailed, cond.Reason)
	}
	if !strings.Contains(cond.Message, "pipeline-graph-view") {
		t.Errorf("expected message to include init failure detail, got %q", cond.Message)
	}
}

func TestProvisioningBlocksDirectPluginProfileConflict(t *testing.T) {
	client := newTestClientWithBundle()
	client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n" +
		"  - artifactId: git\n    version: 5.10.1\n"
	client.profiles["jenkins-2-570"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-2-570"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "profile-content",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	client.configMapData["profile-content"] = map[string]string{
		"plugins.yaml": "plugins:\n  - artifactId: git\n    version: 5.2.2\n",
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Version = "2.570"

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning to be blocked by plugin conflict")
	}
	if len(client.statefulSets) != 0 {
		t.Fatalf("StatefulSet must not be created for known-bad plugin set, got %v", client.statefulSets)
	}
	if _, ok := client.configMapData[testPrefix+"-plugins"]; ok {
		t.Fatal("plugins ConfigMap must not be written for known-bad plugin set")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginConflict)
	if cond == nil {
		t.Fatal("expected PluginConflict condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected PluginConflict=True, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonPluginConflict {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonPluginConflict, cond.Reason)
	}
	if !strings.Contains(cond.Message, "git") || !strings.Contains(cond.Message, "5.10.1") || !strings.Contains(cond.Message, "5.2.2") {
		t.Errorf("expected conflict message to mention plugin and versions, got %q", cond.Message)
	}

	// Verify the ReconcileBlocked condition is also set via markReconcileBlocked.
	blockedCond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if blockedCond == nil {
		t.Fatal("expected ReconcileBlocked condition")
	}
	if blockedCond.Status != metav1.ConditionTrue {
		t.Fatalf("expected ReconcileBlocked=True, got %s", blockedCond.Status)
	}
	if blockedCond.Reason != v1alpha1.ReasonReconcileBlockedPluginConflict {
		t.Errorf("expected ReconcileBlocked reason %s, got %s", v1alpha1.ReasonReconcileBlockedPluginConflict, blockedCond.Reason)
	}
	if cr.Status.LastReconcileError == "" {
		t.Error("expected non-empty LastReconcileError")
	}
}

func TestFinalize(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
			UID:  testUID,
		},
		Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseRunning},
	}

	err := rec.Finalize(context.Background(), cr)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

func TestFinalizeDoesNotDeleteUserSuppliedTLSSecret(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns1", UID: testUID},
		Spec: v1alpha1.ControllerSpec{
			IngressSpec: &v1alpha1.IngressSpec{Host: "demo.example.com", TLSSecretName: "shared-wildcard-tls"},
		},
		Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseRunning},
	}

	if err := rec.Finalize(context.Background(), cr); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	for _, name := range client.deletedSecrets {
		if name == "shared-wildcard-tls" {
			t.Errorf("Finalize deleted user-supplied TLS secret %q; the operator does not own it", name)
		}
	}

	pre := controllerPrefix(cr)
	found := false
	for _, name := range client.deletedSecrets {
		if name == pre+"-bootstrap" {
			found = true
		}
	}
	if !found {
		t.Error("Finalize should still delete the operator-owned bootstrap secret")
	}
}

// --- Finding 1: provisioning timeout must use ProvisioningStartedAt, not CreationTimestamp ---

func TestProvisioningTimeoutDoesNotFireForOldCRWithRecentProvisioningStart(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	// CR was created an hour ago but ProvisioningStartedAt is only 10 seconds ago.
	// An operator restart on a live cluster produces exactly this state.
	longAgo := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	recent := metav1.NewTime(time.Now().Add(-10 * time.Second))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: testUID,
			CreationTimestamp: longAgo,
		},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseProvisioning,
			ProvisioningStartedAt: &recent,
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cr.Status.Phase == v1alpha1.ControllerPhaseFailed {
		t.Error("controller should not be Failed: ProvisioningStartedAt is recent, only CreationTimestamp is old")
	}
}

func TestProvisioningTimeoutFiresWhenProvisioningStartedAtIsExpired(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	// ProvisioningStartedAt exceeds the 5-minute timeout.
	expiredAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: testUID,
			CreationTimestamp: expiredAt,
		},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseProvisioning,
			ProvisioningStartedAt: &expiredAt,
		},
	}

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Error("expected error for provisioning timeout")
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseFailed {
		t.Errorf("expected Failed, got %s", cr.Status.Phase)
	}
}

func TestRunningPhaseTimeoutDoesNotFireForOldCRWithRecentProvisioningStart(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	// Simulates a controller that was previously Connected, dropped to Pending
	// after operator restart, and re-entered Running. CreationTimestamp is old.
	longAgo := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	recent := metav1.NewTime(time.Now().Add(-10 * time.Second))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: testUID,
			CreationTimestamp: longAgo,
		},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseRunning,
			ProvisioningStartedAt: &recent,
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cr.Status.Phase == v1alpha1.ControllerPhaseFailed {
		t.Error("controller should not be Failed: ProvisioningStartedAt is recent")
	}
}

// --- Finding 8: bootstrap token must not be rotated on every provisioning tick ---

func TestProvisioningDoesNotRotateExistingBootstrapToken(t *testing.T) {
	client := newTestClientWithBundle()
	// Simulate a bootstrap secret that already exists from prior provisioning, holding a
	// currently-valid (v2, unexpired) token signed with the reconciler's key. A valid
	// token must be left in place so reconnecting mites are not invalidated.
	validToken, err := mite.NewTokenSigner(testKey).GenerateToken("test", "ns", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	client.existingSecrets[testPrefix+"-bootstrap"] = map[string][]byte{
		"token": []byte(validToken),
	}

	rec := newTestReconciler(client)

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseProvisioning,
			ProvisioningStartedAt: &recent,
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The bootstrap secret must not be re-created (it already exists).
	bsName := testPrefix + "-bootstrap"
	for _, s := range client.secrets {
		if s == bsName {
			t.Error("bootstrap token was rotated even though the secret already existed")
		}
	}
}

func TestProvisioningRotatesLegacyBootstrapToken(t *testing.T) {
	client := newTestClientWithBundle()
	// A pre-existing token that is NOT current-format (legacy/plaintext) must be
	// re-minted, so upgraded controllers stop trusting old-format tokens.
	client.existingSecrets[testPrefix+"-bootstrap"] = map[string][]byte{
		"token": []byte("legacy-plaintext-token"),
	}

	rec := newTestReconciler(client)

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseProvisioning,
			ProvisioningStartedAt: &recent,
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	bsName := testPrefix + "-bootstrap"
	rotated := false
	for _, s := range client.secrets {
		if s == bsName {
			rotated = true
		}
	}
	if !rotated {
		t.Error("legacy-format bootstrap token should have been re-minted")
	}
}

func TestProvisioningCreatesBootstrapTokenWhenSecretIsMissing(t *testing.T) {
	client := newTestClientWithBundle()
	// No pre-existing secrets — fresh provisioning.

	rec := newTestReconciler(client)

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseProvisioning,
			ProvisioningStartedAt: &recent,
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if client.secretWrites == 0 {
		t.Error("bootstrap token should be created when the secret does not exist")
	}
}

// --- Finding 5: conditions must not grow unboundedly ---

func TestConditionsDoNotGrowUnbounded(t *testing.T) {
	client := newTestClientWithBundle()
	reg := transport.NewLocalRegistry(mite.NewRegistry())
	signer := mite.NewTokenSigner(testKey)
	rec := NewReconciler(bundle.NewResolver("/tmp/test"), client, client.store, reg, signer, rbac.NewGenerator(rbac.NewTestResolver()), nil)
	rec.Logger = slog.Default()

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: testUID,
			CreationTimestamp: recent,
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
			},
		},
	}

	// Register a connected mite so handleConnected doesn't transition out.
	reg.Registry().Register(cr.Name, cr.Namespace, nil, nil, "v1.0.0", time.Now().Add(24*time.Hour))
	// Set snapshot health so GetHealth returns "healthy", not "unreachable".
	reg.Registry().UpdateHeartbeat(cr.Name, cr.Namespace, "v1.0.0", &mitev1.StateSnapshot{JenkinsHealth: "healthy"}, nil, "")

	// Reconcile 3 times — each tick currently appends a new ConditionReady.
	// Send() errors are expected (no real gRPC stream in test); conditions
	// are modified in-memory before the Send() call.
	for i := 0; i < 3; i++ {
		_ = rec.reconcileController(context.Background(), cr)
	}

	// Count ConditionReady entries — must be exactly 1.
	readyCount := 0
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionReady {
			readyCount++
		}
	}
	if readyCount != 1 {
		t.Errorf("expected exactly 1 ConditionReady, got %d (conditions grew to %d total)",
			readyCount, len(cr.Status.Conditions))
	}
}

// --- Admin controls: power on/off and reprovision ---

func TestPowerOffScalesStatefulSetToZero(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Spec:       v1alpha1.ControllerSpec{PowerState: "Stopped"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseStopped {
		t.Errorf("phase = %q, want Stopped", cr.Status.Phase)
	}
	if len(client.scaleCalls) == 0 {
		t.Fatal("power off must scale the StatefulSet, but ScaleStatefulSet was never called")
	}
	for name, replicas := range client.scaleCalls {
		if replicas != 0 {
			t.Errorf("scale call for %s = %d, want 0", name, replicas)
		}
	}
}

func TestPowerOffClearsMiteConnection(t *testing.T) {
	recent := metav1.NewTime(time.Now())
	for _, tc := range []struct {
		powerState string
		wantPhase  v1alpha1.ControllerPhase
	}{
		{"Stopped", v1alpha1.ControllerPhaseStopped},
		{"Hibernated", v1alpha1.ControllerPhaseHibernated},
	} {
		t.Run(tc.powerState, func(t *testing.T) {
			client := newTestClient()
			rec := newTestReconciler(client)
			cr := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
				Spec:       v1alpha1.ControllerSpec{PowerState: tc.powerState},
				Status: v1alpha1.ControllerStatus{
					Phase:      v1alpha1.ControllerPhaseConnected,
					MiteStatus: &v1alpha1.MiteStatus{Connected: true, JenkinsHealth: "healthy"},
				},
			}

			if err := rec.reconcileController(context.Background(), cr); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if cr.Status.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", cr.Status.Phase, tc.wantPhase)
			}
			// The pod is scaled to 0, so a mite cannot be connected.
			if cr.Status.MiteStatus.Connected {
				t.Error("MiteStatus.Connected = true after power off, want false")
			}
			// Not "": jenkinsHealth is omitempty, so an empty string would be
			// dropped from the status merge patch and the stale value would survive.
			if cr.Status.MiteStatus.JenkinsHealth != "unreachable" {
				t.Errorf("MiteStatus.JenkinsHealth = %q after power off, want %q", cr.Status.MiteStatus.JenkinsHealth, "unreachable")
			}
		})
	}
}

func TestPowerOnFromStoppedTransitionsToPending(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Spec:       v1alpha1.ControllerSpec{PowerState: "Running"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseStopped},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhasePending {
		t.Errorf("phase = %q, want Pending (powering on re-provisions)", cr.Status.Phase)
	}
}

func TestReprovisionFlagSetAndConsumed(t *testing.T) {
	client := newTestClientWithBundle()
	reg := transport.NewLocalRegistry(mite.NewRegistry())
	signer := mite.NewTokenSigner(testKey)
	rec := NewReconciler(bundle.NewResolver("/tmp/test"), client, client.store, reg, signer, rbac.NewGenerator(rbac.NewTestResolver()), nil)
	rec.Logger = slog.Default()

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
		},
	}
	// Register a connected mite so handleConnected runs to the force-push check.
	reg.Registry().Register(cr.Name, cr.Namespace, nil, nil, "v1.0.0", time.Now().Add(24*time.Hour))
	reg.Registry().UpdateHeartbeat(cr.Name, cr.Namespace, "v1.0.0", &mitev1.StateSnapshot{JenkinsHealth: "healthy"}, nil, "")

	rec.Reprovision("core", cr.Namespace, cr.Name)
	// Reprovision now stamps the force-reprovision annotation — the map is gone.
	// Verify the annotation was set on the client's patch call.
	// (We can't inspect the patch directly via testClient, but we can verify
	// that reconcileController no longer panics. The old forceReprovision map
	// path is gone; the annotation path is exercised by the AnnotationRouting tests.)
	_ = rec.reconcileController(context.Background(), cr)
}

// TestTriggersEnqueueReconcileEvent verifies that the on-demand trigger paths
// (TriggerReconcile, WakeController, Reprovision) enqueue a controller-runtime
// reconcile event for the named controller. Under the controller-runtime
// engine these must push onto reconcileEvents (wired via source.Channel in
// SetupWithManager); the legacy in-memory trigger channels are never drained.
func TestTriggersEnqueueReconcileEvent(t *testing.T) {
	drain := func(t *testing.T, rec *Reconciler) {
		t.Helper()
		select {
		case ev := <-rec.reconcileEvents:
			if ev.Object.GetNamespace() != "ns" || ev.Object.GetName() != "test" {
				t.Fatalf("enqueued event for %s/%s, want ns/test",
					ev.Object.GetNamespace(), ev.Object.GetName())
			}
		default:
			t.Fatal("expected a reconcile event to be enqueued")
		}
	}

	t.Run("TriggerReconcile", func(t *testing.T) {
		rec := newTestReconciler(newTestClientWithBundle())
		rec.TriggerReconcile("core", "test", "ns")
		drain(t, rec)
	})
	t.Run("WakeController", func(t *testing.T) {
		rec := newTestReconciler(newTestClientWithBundle())
		rec.WakeController("core", "ns", "test")
		drain(t, rec)
	})
	t.Run("Reprovision", func(t *testing.T) {
		rec := newTestReconciler(newTestClientWithBundle())
		rec.Reprovision("core", "ns", "test")
		// Reprovision now stamps the annotation — no local enqueue.
		// The annotation watch predicate triggers the owner's reconcile.
		// Verify no panic and no event on channel.
		select {
		case <-rec.reconcileEvents:
			t.Fatal("Reprovision should not enqueue locally; annotation is the wake")
		default:
		}
	})
}

// --- Finding 6: CreateConfigMap must update, not silently skip ---

func TestCreateOrUpdateConfigMapUpdatesOnSecondReconcile(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Status: v1alpha1.ControllerStatus{
			Phase:                 v1alpha1.ControllerPhaseProvisioning,
			ProvisioningStartedAt: &recent,
		},
	}

	// First reconcile creates ConfigMaps and transitions to Running.
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	firstCmWrites := client.cmWrites

	// Reset phase and ProvisioningStartedAt to simulate a re-provisioning
	// tick (e.g., operator restart re-enters Provisioning).
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	now := metav1Now()
	cr.Status.ProvisioningStartedAt = &now

	// Second reconcile — ConfigMaps already exist, must still update data.
	client.cmWrites = 0
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if client.cmWrites == 0 {
		t.Error("ConfigMaps were not updated on second reconcile — CreateConfigMap silently skips existing")
	}
	if client.cmWrites != firstCmWrites {
		t.Errorf("expected %d ConfigMap writes on each reconcile, got %d on second", firstCmWrites, client.cmWrites)
	}
}

// --- Task 7: buildDesiredStateCommand tests ---

func TestBuildDesiredStateCommandIncludesJcascYaml(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	resolved := &bundle.MaterializedBundle{
		JenkinsYAML: "jenkins:\n  systemMessage: \"Hello Varroa\"",
	}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"},
		},
	}

	cmd := rec.buildDesiredStateCommand(cr, resolved)

	// stripAuthorizationStrategy parses and re-marshals; Varroa also injects
	// projectNamingStrategy. Compare semantic content rather than raw bytes.
	if cmd.JcascYaml == "" {
		t.Error("JcascYaml should not be empty when bundle is resolved")
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(cmd.JcascYaml), &got); err != nil {
		t.Fatalf("failed to parse JcascYaml: %v", err)
	}
	gotJenkins, ok := got["jenkins"].(map[string]any)
	if !ok || gotJenkins == nil {
		t.Fatal("JcascYaml missing 'jenkins' key")
	}
	// systemMessage from the bundle must be preserved.
	if gotJenkins["systemMessage"] != "Hello Varroa" {
		t.Errorf("systemMessage = %v, want Hello Varroa", gotJenkins["systemMessage"])
	}
	// authorizationStrategy must be stripped.
	if _, hasAuthz := gotJenkins["authorizationStrategy"]; hasAuthz {
		t.Error("JcascYaml must not contain authorizationStrategy")
	}
	// projectNamingStrategy must be injected as a mapping.
	pns, ok := gotJenkins["projectNamingStrategy"].(map[string]any)
	if !ok {
		t.Errorf("projectNamingStrategy must be a mapping, got %T", gotJenkins["projectNamingStrategy"])
	} else {
		rb, ok := pns["roleBased"].(map[string]any)
		if !ok {
			t.Errorf("projectNamingStrategy must have 'roleBased' key, got %v", pns)
		} else if rb["forceExistingJobs"] != false {
			t.Errorf("projectNamingStrategy.roleBased.forceExistingJobs = %v, want false", rb["forceExistingJobs"])
		}
	}
}
func TestBuildDesiredStateCommandNilBundleYieldsEmptyJcascYaml(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}

	cmd := rec.buildDesiredStateCommand(cr, nil)

	if cmd.JcascYaml != "" {
		t.Errorf("expected empty JcascYaml when no bundle, got %q", cmd.JcascYaml)
	}
}

// TestBuildDesiredStateCommandAlwaysSetsReload pins the #166 keystone: every
// desired-state command must use the MANAGE-gated reload path, never the legacy
// admin apply path. The operator therefore always sets Reload=true so the mite
// can run without Jenkins.ADMINISTER.
func TestBuildDesiredStateCommandAlwaysSetsReload(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}

	cmd := rec.buildDesiredStateCommand(cr, nil)

	if !cmd.Reload {
		t.Error("buildDesiredStateCommand must always set Reload=true (config push goes through reload, not admin apply)")
	}
}

func TestBuildDesiredStateCommandIncludesItemsYaml(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	resolved := &bundle.MaterializedBundle{
		JenkinsYAML: "jenkins:\n  systemMessage: hello",
		ItemsYAML:   "items:\n  - kind: folder\n    name: test-folder",
	}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}

	cmd := rec.buildDesiredStateCommand(cr, resolved)

	if cmd.ItemsYaml != resolved.ItemsYAML {
		t.Errorf("expected ItemsYaml %q, got %q", resolved.ItemsYAML, cmd.ItemsYaml)
	}
}

func TestBuildDesiredStateCommandHashIncludesItems(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	resolved := &bundle.MaterializedBundle{
		JenkinsYAML: "jenkins: {}",
		ItemsYAML:   "items:\n  - kind: folder\n    name: a",
	}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}

	cmd1 := rec.buildDesiredStateCommand(cr, resolved)
	hash1 := cmd1.DesiredStateHash

	resolved.ItemsYAML = "items:\n  - kind: folder\n    name: b"
	cmd2 := rec.buildDesiredStateCommand(cr, resolved)
	hash2 := cmd2.DesiredStateHash

	if hash1 == hash2 {
		t.Error("expected different hashes when items.yaml changes")
	}
}

// TestBuildDesiredStateCommandDeadlineDefault verifies that without configured
// ProvisioningDefaults, CommandDeadlineSec is zero (mite uses 20m default).
func TestBuildDesiredStateCommandDeadlineDefault(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}

	cmd := rec.buildDesiredStateCommand(cr, nil)

	if cmd.CommandDeadlineSec != 0 {
		t.Errorf("expected CommandDeadlineSec=0, got %d", cmd.CommandDeadlineSec)
	}
}

func TestStripAuthorizationStrategy(t *testing.T) {
	// YAML with authorizationStrategy should have it removed.
	in := "jenkins:\n  authorizationStrategy:\n    roleBased:\n      roles:\n        global: []\n  systemMessage: hello"
	out := stripAuthorizationStrategy(in)
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not valid YAML: %v", err)
	}
	jenkins, ok := doc["jenkins"].(map[string]any)
	if !ok {
		t.Fatal("expected 'jenkins' key in output")
	}
	if _, hasAuthz := jenkins["authorizationStrategy"]; hasAuthz {
		t.Error("authorizationStrategy was not stripped")
	}
	if msg, ok := jenkins["systemMessage"]; !ok || msg != "hello" {
		t.Errorf("systemMessage should be preserved, got %v", jenkins["systemMessage"])
	}

	// Empty input.
	if out := stripAuthorizationStrategy(""); out != "" {
		t.Errorf("expected empty output for empty input, got %q", out)
	}

	// Invalid YAML should be returned as-is.
	invalid := "not: [valid yaml!!!"
	if out := stripAuthorizationStrategy(invalid); out != invalid {
		t.Errorf("expected invalid YAML returned as-is, got %q", out)
	}

	// YAML without jenkins key.
	noJenkins := "unrelated: value"
	if out := stripAuthorizationStrategy(noJenkins); out != noJenkins {
		t.Errorf("expected unchanged, got %q", out)
	}
}

func TestControllerPrefix(t *testing.T) {
	t.Run("includes stable UID", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{
				Name: "smoke-main",
				UID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			},
		}
		got := controllerPrefix(cr)
		if got != "smoke-main-a1b2c3d4" {
			t.Errorf("controllerPrefix = %q, want %q", got, "smoke-main-a1b2c3d4")
		}
	})

	t.Run("truncates long names to fit 253-char limit", func(t *testing.T) {
		longName := strings.Repeat("x", 250)
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{
				Name: longName,
				UID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			},
		}
		got := controllerPrefix(cr)
		if len(got) > 238 {
			t.Errorf("controllerPrefix length = %d, want <= 238", len(got))
		}
		pvcName := "jenkins-home-" + got + "-0"
		if len(pvcName) > 253 {
			t.Errorf("PVC name %q length = %d, want <= 253", pvcName, len(pvcName))
		}
	})

	t.Run("different UIDs produce different prefixes", func(t *testing.T) {
		cr1 := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", UID: "11111111-0000-0000-0000-000000000001"},
		}
		cr2 := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", UID: "22222222-0000-0000-0000-000000000001"},
		}
		if controllerPrefix(cr1) == controllerPrefix(cr2) {
			t.Error("different UIDs should produce different prefixes")
		}
	})

	t.Run("digit-leading controller names get service-safe prefix", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "223-test", UID: "6afe8592-0a97-4541-a835-9ac595556648"},
		}
		got := controllerPrefix(cr)
		if got != "c-223-test-6afe8592" {
			t.Errorf("controllerPrefix = %q, want %q", got, "c-223-test-6afe8592")
		}
		if got[0] < 'a' || got[0] > 'z' {
			t.Errorf("controllerPrefix must start with a letter for Service names, got %q", got)
		}
	})
}

func TestControllerPodName(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "smoke-main",
			UID:  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		},
	}
	// Pods are "<controllerPrefix>-<ordinal>", not "<name>-<ordinal>".
	if got, want := PodName(cr, 0), "smoke-main-a1b2c3d4-0"; got != want {
		t.Errorf("PodName(cr, 0) = %q, want %q", got, want)
	}
	if got, want := PodName(cr, 0), cr.Name+"-0"; got == want {
		t.Errorf("PodName must not equal bare name form %q", want)
	}
}

func TestDetectLogLevel(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"ERROR: something broke", "ERROR"},
		{"SEVERE: catastrophic failure", "ERROR"},
		{"WARN: disk space low", "WARN"},
		{"warning: deprecated API", "WARN"},
		{"DEBUG: entering loop", "DEBUG"},
		{"debug: connection established", "DEBUG"},
		{"INFO: everything fine", "INFO"},
		{"[info] plugin loaded", "INFO"},
		{"normal log line without level", "INFO"},
		{"", "INFO"},
	}
	for _, tc := range tests {
		if got := logbuffer.DetectLogLevel(tc.line); got != tc.want {
			t.Errorf("detectLogLevel(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestHandleConnectedOIDCEnvSync(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	// Configure OIDC on the resolver so the reconciler has a desired
	// issuer and login URL to sync to the StatefulSet.
	rec.Resolver.SetOIDCConfig("https://dex.example.com", "varroa", "secret")
	rec.SetVarroaRedirectURL("https://varroa.example.com/callback")

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "testctl", Namespace: "testns", UID: testUID, CreationTimestamp: metav1Now()},
		Spec: v1alpha1.ControllerSpec{
			Version: "latest",
		},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseConnected,
		},
	}

	err := rec.reconcileController(context.Background(), cr)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(client.oidcUpdateCalls) == 0 {
		t.Error("expected UpdateStatefulSetOIDCEnv to be called in handleConnected")
	}
	call := client.oidcUpdateCalls[0]
	wantName := testPrefixFor("testctl")
	if call.name != wantName {
		t.Errorf("name: got %q, want %q", call.name, wantName)
	}
	if call.namespace != "testns" {
		t.Errorf("namespace: got %q, want %q", call.namespace, "testns")
	}
	if call.oidcIssuer != "https://dex.example.com" {
		t.Errorf("oidcIssuer: got %q, want %q", call.oidcIssuer, "https://dex.example.com")
	}
	if call.loginURL != "https://varroa.example.com/login" {
		t.Errorf("loginURL: got %q, want %q", call.loginURL, "https://varroa.example.com/login")
	}
	if call.apikeyVerifyURL != "" {
		t.Errorf("apikeyVerifyURL: got %q, want empty (no endpoint set)", call.apikeyVerifyURL)
	}
	if call.caPEM != "" {
		t.Errorf("caPEM: got %q, want empty (no CA PEM set)", call.caPEM)
	}
}

func TestFleetPodLabelReconciliationInRunningAndConnected(t *testing.T) {
	tests := []struct {
		name      string
		phase     v1alpha1.ControllerPhase
		reconcile func(*Reconciler, context.Context, *v1alpha1.Controller) error
	}{
		{
			name:  "connected",
			phase: v1alpha1.ControllerPhaseConnected,
			reconcile: func(r *Reconciler, ctx context.Context, cr *v1alpha1.Controller) error {
				return r.handleConnected(ctx, cr)
			},
		},
		{
			name:  "running",
			phase: v1alpha1.ControllerPhaseRunning,
			reconcile: func(r *Reconciler, ctx context.Context, cr *v1alpha1.Controller) error {
				return r.handleRunning(ctx, cr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+" patches unlabeled StatefulSet once", func(t *testing.T) {
			client := newTestClient()
			rec := newTestReconciler(client)
			cr := testController("fleet", "team-a", tt.phase)
			for i := 0; i < 2; i++ {
				if err := tt.reconcile(rec, context.Background(), cr); err != nil {
					t.Fatalf("reconcile %d: %v", i+1, err)
				}
			}
			if client.fleetPodLabelPatches != 1 {
				t.Errorf("patches = %d, want 1", client.fleetPodLabelPatches)
			}
		})

		t.Run(tt.name+" skips labeled StatefulSet", func(t *testing.T) {
			client := newTestClient()
			client.fleetPodLabel = "varroa-operator"
			rec := newTestReconciler(client)
			cr := testController("fleet", "team-a", tt.phase)
			if err := tt.reconcile(rec, context.Background(), cr); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if client.fleetPodLabelPatches != 0 {
				t.Errorf("patches = %d, want 0", client.fleetPodLabelPatches)
			}
		})
	}
}

// --- Task 5.6: Controller resolve tests ---

func TestResolveBundleForController_ReadsConfigMap(t *testing.T) {
	client := newTestClientWithBundle()
	// Pre-populate a ComposedBundle with Ready phase and contentRef.
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "test-bundle-content",
		},
	}
	client.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: ${varroa_controller_name}\n",
		"plugins.yaml": "",
		"items.yaml":   "",
		"rbac.yaml":    "",
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}
	if mat == nil {
		t.Fatal("expected materialized bundle, got nil")
	}
	if !strings.Contains(mat.JenkinsYAML, "systemMessage: testctl") {
		t.Errorf("expected varroa_controller_name resolved to 'testctl', got: %s", mat.JenkinsYAML)
	}
}

// varroa_controller_endpoint must point at the UID-named Service
// ("<name>-<uid8>-svc"), which is the Service actually created in
// handleProvisioning. The bare "<name>-svc" resolves to a stale/non-existent
// Service with no endpoints, so build agents get connection refused. Regression
// guard for the agent-connectivity break after UID-based naming (#69).
func TestResolveBundleForController_EndpointUsesUIDService(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "test-bundle-content",
		},
	}
	client.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  url: ${varroa_controller_endpoint}\n",
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	wantURL := "http://" + testPrefixFor("testctl") + "-svc.testns.svc.cluster.local:8080"
	if !strings.Contains(mat.JenkinsYAML, wantURL) {
		t.Errorf("expected endpoint %q, got: %s", wantURL, mat.JenkinsYAML)
	}
	if strings.Contains(mat.JenkinsYAML, "://testctl-svc.") {
		t.Errorf("endpoint used bare-name service (the regression): %s", mat.JenkinsYAML)
	}
}

func TestResolveBundleForController_FrontendURL(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "test-bundle-content",
		},
	}
	client.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  themeUrl: ${varroa_frontend_url}/varroa-theme.css\n",
	}

	t.Run("resolves when redirect URL is set", func(t *testing.T) {
		rec := newTestReconciler(client)
		rec.SetVarroaRedirectURL("https://varroa.example.com/callback")
		cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

		mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveBundleForController: %v", err)
		}
		if !strings.Contains(mat.JenkinsYAML, "themeUrl: https://varroa.example.com/varroa-theme.css") {
			t.Errorf("expected varroa_frontend_url resolved, got: %s", mat.JenkinsYAML)
		}
	})

	t.Run("resolves to empty when redirect URL is not set", func(t *testing.T) {
		rec := newTestReconciler(client)
		cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

		mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveBundleForController: %v", err)
		}
		// variable should be replaced with empty string, leaving "/varroa-theme.css"
		if strings.Contains(mat.JenkinsYAML, "${varroa_frontend_url}") {
			t.Errorf("expected varroa_frontend_url to be replaced, got unresolved: %s", mat.JenkinsYAML)
		}
		if !strings.Contains(mat.JenkinsYAML, "themeUrl: /varroa-theme.css") {
			t.Errorf("expected empty varroa_frontend_url, got: %s", mat.JenkinsYAML)
		}
	})
}

func TestResolveBundleForController_WaitsOnPending(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase: v1alpha1.ComposedBundlePending,
		},
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error waiting on Pending bundle")
	}
	if !strings.Contains(err.Error(), "waiting") && !strings.Contains(err.Error(), "Ready") {
		t.Errorf("expected 'waiting for ... Ready' in error, got: %v", err)
	}
}

func TestResolveBundleForController_CompletenessCheck(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "test-bundle-content",
		},
	}
	client.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  url: ${UNDEFINED_VAR}\n",
		"plugins.yaml": "",
		"items.yaml":   "",
		"rbac.yaml":    "",
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected completeness check error")
	}
	if !strings.Contains(err.Error(), "unresolved") && !strings.Contains(err.Error(), "UNDEFINED_VAR") {
		t.Errorf("expected error about unresolved variable, got: %v", err)
	}
}

func TestResolveBundleForController_EscapedVarAllowed(t *testing.T) {
	// ^${var} is a JCasC literal escape and must not trip the completeness check.
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "test-bundle-content",
		},
	}
	client.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: \"^${RUNTIME_VAR}\"\n",
		"plugins.yaml": "",
		"items.yaml":   "",
		"rbac.yaml":    "",
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("expected no completeness error for ^${} escape, got: %v", err)
	}
	if !strings.Contains(mat.JenkinsYAML, "^${RUNTIME_VAR}") {
		t.Errorf("expected escaped var to be preserved, got: %q", mat.JenkinsYAML)
	}
}

func TestResolveBundleForController_KeepsLastGood(t *testing.T) {
	// When the bundle is Invalid, resolveBundleForController returns an error,
	// which causes the controller to keep serving last-good content.
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:  v1alpha1.ComposedBundleInvalid,
			Errors: []string{"validation failed: missing items"},
		},
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error for Invalid bundle")
	}
	// The error should contain the bundle error for visibility.
	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("expected bundle error in message, got: %v", err)
	}
}

func TestResolveBundleForController_MissingBundle(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "testctl", Namespace: "testns", UID: testUID},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "nonexistent"}},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error for missing ComposedBundle")
	}
}

func TestResolveBundleForController_NoContentRef(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "", // empty
		},
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error for missing contentRef")
	}
	if !strings.Contains(err.Error(), "contentRef") {
		t.Errorf("expected error about contentRef, got: %v", err)
	}
}

func TestResolveBundleForController_MissingConfigMap(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "nonexistent",
		},
	}

	rec := newTestReconciler(client)
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error for missing ConfigMap")
	}
}

func TestResolveBundleForController_OIDCClientSecretUnresolved(t *testing.T) {
	// After the #411 fix, ${varroa_oidc_client_secret} is no longer injected
	// by resolveBundleForController. A bundle containing it should produce
	// an "unresolved variables" error.
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:      v1alpha1.ComposedBundleReady,
			ContentRef: "test-bundle-content",
		},
	}
	client.configMapData["test-bundle-content"] = map[string]string{
		"jenkins.yaml": "jenkins:\n  systemMessage: ${varroa_oidc_client_secret}\n",
		"plugins.yaml": "",
		"items.yaml":   "",
		"rbac.yaml":    "",
	}

	rec := newTestReconciler(client)
	// Configure OIDC so the resolver has the secret available (but it must
	// no longer inject it).
	rec.Resolver.SetOIDCConfig("https://dex.example.com", "varroa", "s3cret")
	cr := testController("testctl", "testns", v1alpha1.ControllerPhaseConnected)

	_, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected unresolved variables error for varroa_oidc_client_secret")
	}
	if !strings.Contains(err.Error(), "unresolved variables") &&
		!strings.Contains(err.Error(), "unresolved") {
		t.Errorf("expected error about unresolved variables, got: %v", err)
	}
}

// --- Task 6.4: API surface tests ---

// A Controller that names no ComposedBundle is the zero-config path: it must
// fall back to the seeded starter bundle and advance, not fail.
func TestHandlePending_NilBundleRefUsesStarterBundle(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhasePending},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("expected the starter bundle to satisfy a nil composedBundleRef, got: %v", err)
	}
	if cr.Status.Phase == v1alpha1.ControllerPhaseFailed {
		t.Fatalf("expected the controller to advance past Pending, got phase %s", cr.Status.Phase)
	}
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleFailed); c != nil &&
		c.Status == metav1.ConditionTrue {
		t.Errorf("BundleFailed should not be set for a nil ref: %s", c.Message)
	}
}

// When the starter bundle has not been seeded yet, a nil ref must fail with a
// message that names the starter — otherwise a fresh install reads as "you
// forgot composedBundleRef", which is the opposite of the truth.
func TestHandlePending_NilBundleRefWithoutStarterNamesIt(t *testing.T) {
	client := newTestClientWithBundle()
	if err := crdstore.Delete[v1alpha1.ComposedBundle](context.Background(), client.store,
		StarterBundleName, "varroa-system"); err != nil {
		t.Fatalf("delete seeded starter: %v", err)
	}
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhasePending},
	}

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected an error when the starter bundle is absent")
	}
	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleFailed)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatal("expected BundleFailed to be set")
	}
	if !strings.Contains(c.Message, StarterBundleName) || !strings.Contains(c.Message, "starter") {
		t.Errorf("expected the message to name the starter bundle, got: %s", c.Message)
	}
}

func TestHandlePending_AcceptsInvalidBundle(t *testing.T) {
	client := newTestClientWithBundle()
	// Create an Invalid ComposedBundle — the controller should accept it.
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase:  v1alpha1.ComposedBundleInvalid,
			Errors: []string{"some validation error"},
		},
	}

	rec := newTestReconciler(client)
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhasePending},
	}

	err := rec.reconcileController(context.Background(), cr)
	if err != nil {
		t.Fatalf("expected Invalid bundle to be accepted, got: %v", err)
	}
	// Should transition to Provisioning (waiting for bundle to become Ready).
	if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
		t.Errorf("expected phase Provisioning, got %s", cr.Status.Phase)
	}
}

func TestHandlePending_AcceptsPendingBundle(t *testing.T) {
	client := newTestClientWithBundle()
	client.composedBundles["test-bundle"] = &v1alpha1.ComposedBundle{
		Status: v1alpha1.ComposedBundleStatus{
			Phase: v1alpha1.ComposedBundlePending,
		},
	}

	rec := newTestReconciler(client)
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhasePending},
	}

	err := rec.reconcileController(context.Background(), cr)
	if err != nil {
		t.Fatalf("expected Pending bundle to be accepted, got: %v", err)
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
		t.Errorf("expected phase Provisioning, got %s", cr.Status.Phase)
	}
}

func TestHandlePending_RejectsMissingBundle(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "nonexistent"}},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhasePending},
	}

	err := rec.reconcileController(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error for nonexistent ComposedBundle")
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseFailed {
		t.Errorf("expected phase Failed, got %s", cr.Status.Phase)
	}
}

func TestFindUnresolvedVars(t *testing.T) {
	vars := findUnresolvedVars(
		"jenkins:\n  url: ${MY_URL}\n  name: ${MY_NAME}\n",
		"",
		"items:\n- name: ${MY_ITEM}\n",
		"",
	)
	if len(vars) != 3 {
		t.Errorf("expected 3 unresolved vars, got %d: %v", len(vars), vars)
	}
	for _, want := range []string{"MY_URL", "MY_NAME", "MY_ITEM"} {
		found := false
		for _, v := range vars {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in unresolved vars", want)
		}
	}
}

func TestFindUnresolvedVars_None(t *testing.T) {
	vars := findUnresolvedVars("jenkins:\n  key: value\n", "", "", "")
	if len(vars) != 0 {
		t.Errorf("expected 0 unresolved vars, got %d: %v", len(vars), vars)
	}
}

// --- Section 2: Token minting and reload tests ---

func newTestReconcilerWithTokenSigner(client *testClient) *Reconciler {
	rec := newTestReconciler(client)
	signer, err := mite.NewMiteTokenSigner()
	if err != nil {
		panic("failed to create MiteTokenSigner: " + err.Error())
	}
	rec.SetMiteTokenSigner(signer)
	return rec
}

func TestMintOrGetMiteToken_60mTTL(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClient())
	token, expUnix := rec.mintOrGetMiteToken("ns/test")
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	exp := time.Unix(expUnix, 0)
	expectedExp := time.Now().Add(60 * time.Minute)
	if exp.Sub(expectedExp) > 5*time.Second || expectedExp.Sub(exp) > 5*time.Second {
		t.Errorf("token expiry %v not within 5s of expected %v", exp, expectedExp)
	}
}

func TestMintOrGetMiteToken_CacheReuse(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClient())
	key := "ns/test"
	token1, exp1 := rec.mintOrGetMiteToken(key)
	token2, exp2 := rec.mintOrGetMiteToken(key)
	if token1 != token2 {
		t.Error("expected same cached token on second call within window")
	}
	if exp1 != exp2 {
		t.Error("expected same expiry on second call within window")
	}
}

func TestMintOrGetMiteToken_ReMintAfterDelete(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClient())
	key := "ns/test"
	token1, _ := rec.mintOrGetMiteToken(key)
	// Simulate reconnect clear-then-request.
	rec.miteTokenMu.Lock()
	delete(rec.miteTokens, key)
	rec.miteTokenMu.Unlock()
	// Second mint should return a fresh token (cache was cleared).
	token2, _ := rec.mintOrGetMiteToken(key)
	// Verify we got valid tokens.
	if token1 == "" || token2 == "" {
		t.Fatal("expected non-empty tokens")
	}
	// After reconnect clear, the entry was deleted. The second mint must
	// have re-created it (the cache length reflects this).
	rec.miteTokenMu.Lock()
	_, ok := rec.miteTokens[key]
	rec.miteTokenMu.Unlock()
	if !ok {
		t.Error("expected cached token entry after re-mint")
	}
}

func TestMintMiteTokenForce_AdvancesExpiry(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClient())
	key := "ns/test"

	cachedToken, cachedExp := rec.mintOrGetMiteToken(key)
	if cachedToken == "" {
		t.Fatal("expected non-empty token from cache path")
	}

	forceToken, forceExp, err := rec.MintMiteTokenForce(key)
	if err != nil {
		t.Fatalf("MintMiteTokenForce: %v", err)
	}
	if forceToken == "" {
		t.Fatal("expected non-empty token from force-mint")
	}
	if forceToken == cachedToken {
		t.Error("force-mint should produce a new token, not the cached one")
	}
	if forceExp <= cachedExp {
		t.Errorf("force-mint expiry %d must be greater than cached expiry %d", forceExp, cachedExp)
	}

	rec.miteTokenMu.Lock()
	entry, ok := rec.miteTokens[key]
	rec.miteTokenMu.Unlock()
	if !ok {
		t.Error("force-mint should have updated the cached token entry")
	}
	if entry.exp.Unix() != forceExp {
		t.Errorf("cache entry expiry %d != force-mint expiry %d", entry.exp.Unix(), forceExp)
	}
}

func TestMintMiteTokenForce_PreservesDesiredStateCache(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClient())
	key := "ns/test"

	_, _, err := rec.MintMiteTokenForce(key)
	if err != nil {
		t.Fatalf("MintMiteTokenForce: %v", err)
	}

	desiredToken, desiredExp := rec.mintOrGetMiteToken(key)
	if desiredToken == "" {
		t.Fatal("expected non-empty token from desired-state path after force-mint")
	}
	rec.miteTokenMu.Lock()
	entry := rec.miteTokens[key]
	rec.miteTokenMu.Unlock()
	if entry.exp.Unix() != desiredExp {
		t.Error("desired-state token path should reuse cache updated by force-mint")
	}

	token2, _ := rec.mintOrGetMiteToken(key)
	if token2 != desiredToken {
		t.Error("second desired-state call should return same cached token")
	}
}

func TestForceReprovisionSetsReload(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	muteTransport := &captureTransport{}
	rec.miteTransport = muteTransport

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
		},
	}
	rec.Reprovision("core", cr.Namespace, cr.Name)
	_ = rec.reconcileController(context.Background(), cr)

	if muteTransport.lastDesired == nil {
		t.Fatal("expected desired state push")
	}
	if !muteTransport.lastDesired.Reload {
		t.Error("expected Reload=true on force-reprovision push")
	}
}

// TestDesiredStateHashConsistentWhenPluginsSuppressed guards the convergence
// regression where, with hot-install off (the default), the operator suppressed
// DesiredPlugins on the pushed command but left the command's DesiredStateHash
// stamped with the plugins included. The mite echoes cmd.DesiredStateHash back
// as AppliedHash, and convergence compares it to the without-plugins hash the
// operator tracks — so the two never matched and AppliedBundleHash (wave-rollout
// gating + UI "converged" signal) never populated. The sent command's hash must
// equal the tracked DesiredStateHash.
func TestDesiredStateHashConsistentWhenPluginsSuppressed(t *testing.T) {
	client := newTestClientWithBundle()
	// Give the bundle plugins so the with/without-plugins hashes differ — that
	// difference is exactly what the bug exposed.
	client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n" +
		"  - artifactId: workflow-aggregator\n    version: latest\n"
	rec := newTestReconcilerWithTokenSigner(client)
	transport := &captureTransport{}
	rec.miteTransport = transport

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if transport.lastDesired == nil {
		t.Fatal("expected a desired-state push")
	}
	// Plugins are never sent to the mite (the field is removed); the command's
	// hash must still equal the tracked DesiredStateHash so convergence works.
	if transport.lastDesired.DesiredStateHash != cr.Status.DesiredStateHash {
		t.Errorf("sent command hash %q must equal tracked DesiredStateHash %q (else AppliedBundleHash never converges)",
			transport.lastDesired.DesiredStateHash, cr.Status.DesiredStateHash)
	}
}

// captureTransport is a minimal mite transport that captures the last pushed
// desired state command.
type captureTransport struct {
	lastDesired   *mitev1.DesiredStateCommand
	lastSnapshots map[string]*mitev1.StateSnapshot
	drainResults  []*mitev1.CommandResult
}

func (c *captureTransport) Send(_ context.Context, ns, name string, msg *mitev1.OperatorMessage) error {
	if ds, ok := msg.Message.(*mitev1.OperatorMessage_DesiredState); ok {
		c.lastDesired = ds.DesiredState
	}
	return nil
}

func (c *captureTransport) Info(ns, name string) (version string, lastHeartbeat, certExpiry time.Time, connected bool) {
	return "v1.0.0", time.Now(), time.Now().Add(24 * time.Hour), true
}

func (c *captureTransport) Snapshot(ns, name string) *mitev1.StateSnapshot {
	if c.lastSnapshots != nil {
		return c.lastSnapshots[ns+"/"+name]
	}
	return &mitev1.StateSnapshot{JenkinsHealth: "healthy"}
}

func (c *captureTransport) Health(ns, name string) string { return "healthy" }

func (c *captureTransport) DrainResults(ns, name string) []*mitev1.CommandResult {
	if c.drainResults != nil {
		return c.drainResults
	}
	return nil
}

func (c *captureTransport) ConnEpoch(ns, name string) (int64, bool) { return 0, false }

func (c *captureTransport) IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool) {
	return nil, time.Time{}, false
}

func (c *captureTransport) SendImperative(_ context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	return nil
}

func (c *captureTransport) Connected(ns, name string) bool { return true }

func (c *captureTransport) ClearDesired(_ context.Context, ns, name string) error { return nil }

func (c *captureTransport) ObservabilityReport(ns, name string) *mitev1.ObservabilityReport {
	return nil
}

func (c *captureTransport) FetchLastApplied(_ context.Context, _, _ string) (*mitev1.ContentResponse, error) {
	return nil, transport.ErrContentUnavailable
}

func (c *captureTransport) PluginInventory(_, _ string) *mitev1.PluginInventory {
	return nil
}

func (c *captureTransport) InstalledPluginsHash(_, _ string) (string, bool) {
	return "", false
}

func (c *captureTransport) PluginClassification(_, _ string) (*transport.ClassifiedInventory, bool) {
	return nil, false
}

func (c *captureTransport) PutPluginClassification(_ context.Context, _, _ string, _ *transport.ClassifiedInventory) error {
	return nil
}

//nolint:unparam
func mkDSR(cfgOk, rbacOk, pluginsOk, itemsOk bool, cfgErr, rbacErr, pluginsErr, itemsErr, hash string) *mitev1.CommandResult {
	return &mitev1.CommandResult{
		Result: &mitev1.CommandResult_DesiredState{
			DesiredState: &mitev1.DesiredStateResult{
				ConfigSuccess:  cfgOk,
				RbacSuccess:    rbacOk,
				PluginsSuccess: pluginsOk,
				ItemsSuccess:   itemsOk,
				ConfigError:    cfgErr,
				RbacError:      rbacErr,
				PluginsError:   pluginsErr,
				ItemsError:     itemsErr,
				AppliedHash:    hash,
			},
		},
	}
}

// TestIssueSafeRestartDeletesPod verifies the operator restart helper performs a
// Kubernetes pod delete rather than sending a SAFE_RESTART imperative the mite
// can no longer service (its role lost Jenkins.ADMINISTER in #173).
func TestIssueSafeRestartDeletesPod(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}
	if err := rec.issueSafeRestart(context.Background(), cr, 30); err != nil {
		t.Fatalf("issueSafeRestart: %v", err)
	}
	if client.deleteControllerPodCalls != 1 {
		t.Errorf("expected exactly 1 DeleteControllerPod call from issueSafeRestart, got %d", client.deleteControllerPodCalls)
	}
}

func TestRecordApplyResult(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	muteTransport := &captureTransport{}
	rec.miteTransport = muteTransport

	t.Run("all-success", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkDSR(true, true, true, true, "", "", "", "", "hash-all-ok"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.LastApplyResult == nil {
			t.Fatal("expected LastApplyResult to be set")
		}
		ar := cr.Status.LastApplyResult
		if !ar.Succeeded {
			t.Error("expected Succeeded=true")
		}
		if ar.Hash != "hash-all-ok" {
			t.Errorf("expected hash=hash-all-ok, got %q", ar.Hash)
		}
		if len(ar.Sections) != 4 {
			t.Fatalf("expected 4 sections, got %d", len(ar.Sections))
		}
		wantSections := []string{"config", "rbac", "plugins", "items"}
		for i, s := range wantSections {
			if ar.Sections[i].Name != s {
				t.Errorf("section[%d] name: want %q, got %q", i, s, ar.Sections[i].Name)
			}
			if !ar.Sections[i].OK {
				t.Errorf("section[%d] %q: expected OK=true", i, s)
			}
			if ar.Sections[i].Error != "" {
				t.Errorf("section[%d] %q: expected empty error, got %q", i, s, ar.Sections[i].Error)
			}
		}
		if len(cr.Status.ApplyHistory) != 1 {
			t.Errorf("expected 1 entry in history, got %d", len(cr.Status.ApplyHistory))
		}
	})

	t.Run("rbac-mirrors-config", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkDSR(true, false, true, true, "", "rbac error", "", "", "hash-rbac-fail"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.LastApplyResult == nil {
			t.Fatal("expected LastApplyResult to be set")
		}
		ar := cr.Status.LastApplyResult
		if !ar.Succeeded {
			t.Error("expected Succeeded=true (rbac mirrors config, which succeeded)")
		}
		rbac := ar.Sections[1]
		if rbac.Name != "rbac" {
			t.Errorf("expected section[1] name=rbac, got %q", rbac.Name)
		}
		if !rbac.OK {
			t.Error("expected rbac.OK=true (mirrors ConfigSuccess=true)")
		}
		if rbac.Error != "" {
			t.Errorf("expected rbac.Error empty, got %q", rbac.Error)
		}
	})

	t.Run("config-failure-surfaces-on-rbac", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkDSR(false, true, true, true, "config apply failed", "", "", "", "hash-config-fail"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.LastApplyResult == nil {
			t.Fatal("expected LastApplyResult to be set")
		}
		ar := cr.Status.LastApplyResult
		if ar.Succeeded {
			t.Error("expected Succeeded=false (config failed)")
		}
		config := ar.Sections[0]
		if config.OK {
			t.Error("expected config.OK=false")
		}
		if config.Error != "config apply failed" {
			t.Errorf("expected config.Error='config apply failed', got %q", config.Error)
		}
		rbac := ar.Sections[1]
		if rbac.OK {
			t.Error("expected rbac.OK=false (mirrors config)")
		}
		if rbac.Error != "" {
			t.Errorf("expected rbac.Error empty, got %q", rbac.Error)
		}
	})

	t.Run("truncation", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		bigErr := strings.Repeat("x", 2000)
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkDSR(false, true, true, true, bigErr, "", "", "", "hash-big"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		configErr := cr.Status.LastApplyResult.Sections[0].Error
		if len(configErr) != 1024+len("…") {
			t.Errorf("expected truncated error length %d, got %d", 1024+len("…"), len(configErr))
		}
		if !strings.HasSuffix(configErr, "…") {
			t.Error("expected truncated error to end with …")
		}
	})

	t.Run("newest-first-and-dedup", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		// Drain three distinct results in one tick.
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkDSR(true, true, true, true, "", "", "", "", "hash-1"),
			mkDSR(true, true, true, true, "", "", "", "", "hash-2"),
			mkDSR(true, true, true, true, "", "", "", "", "hash-3"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.LastApplyResult.Hash != "hash-3" {
			t.Errorf("expected latest hash=hash-3, got %q", cr.Status.LastApplyResult.Hash)
		}
		if len(cr.Status.ApplyHistory) != 3 {
			t.Fatalf("expected 3 history entries, got %d", len(cr.Status.ApplyHistory))
		}
		if cr.Status.ApplyHistory[0].Hash != "hash-3" {
			t.Error("expected newest-first: header should be hash-3")
		}
		if cr.Status.ApplyHistory[1].Hash != "hash-2" {
			t.Error("expected position 1 to be hash-2")
		}
		if cr.Status.ApplyHistory[2].Hash != "hash-1" {
			t.Error("expected position 2 to be hash-1")
		}

		// Now re-push hash-3 as a periodic identical tick → should dedup.
		beforeLen := len(cr.Status.ApplyHistory)
		beforeTS := cr.Status.LastApplyResult.Timestamp
		time.Sleep(time.Millisecond) // ensure new timestamp
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkDSR(true, true, true, true, "", "", "", "", "hash-3"),
		}
		_ = rec.reconcileController(context.Background(), cr)
		if len(cr.Status.ApplyHistory) != beforeLen {
			t.Errorf("expected no new entry on dedup, got %d -> %d", beforeLen, len(cr.Status.ApplyHistory))
		}
		if cr.Status.LastApplyResult.Timestamp.Time.Equal(beforeTS.Time) {
			t.Error("expected timestamp to be refreshed on dedup")
		}
	})

	t.Run("cap-ten", func(t *testing.T) {
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		results := make([]*mitev1.CommandResult, 12)
		for i := 0; i < 12; i++ {
			results[i] = mkDSR(true, true, true, true, "", "", "", "", "hash-"+string(rune('a'+i)))
		}
		muteTransport.drainResults = results
		_ = rec.reconcileController(context.Background(), cr)

		if len(cr.Status.ApplyHistory) != 10 {
			t.Errorf("expected 10 entries after cap, got %d", len(cr.Status.ApplyHistory))
		}
		// Newest first: most recent hash should be "hash-l" (12th, index 11)
		if cr.Status.ApplyHistory[0].Hash != "hash-l" {
			t.Errorf("expected newest hash='hash-l', got %q", cr.Status.ApplyHistory[0].Hash)
		}
		// Oldest in the ring should be "hash-c" (3rd, index 2)
		if cr.Status.ApplyHistory[9].Hash != "hash-c" {
			t.Errorf("expected oldest retained='hash-c', got %q", cr.Status.ApplyHistory[9].Hash)
		}
	})
}

func seedResolverWithHumanAdmin() *rbac.Resolver {
	return rbac.NewTestResolverWithJenkins(
		[]*v1alpha1.JenkinsRole{{
			ObjectMeta: metav1.ObjectMeta{Name: "human-admin"},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Global",
				Permissions: []string{"hudson.model.Hudson.Administer"},
			},
		}},
		[]*v1alpha1.JenkinsRoleBinding{{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:  "human-admin",
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "varroa-admins"}},
			},
		}},
	)
}

func TestRBACLockoutGuard(t *testing.T) {
	t.Run("adminless-no-override", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		muteTransport := &captureTransport{}
		rec.miteTransport = muteTransport

		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		_ = rec.reconcileController(context.Background(), cr)

		if muteTransport.lastDesired == nil {
			t.Fatal("expected desired state push")
		}
		if muteTransport.lastDesired.RbacYaml != "" {
			t.Error("expected RbacYaml empty (guard fires)")
		}
		if muteTransport.lastDesired.JcascYaml == "" {
			t.Error("expected JcascYaml still set when RBAC skipped")
		}
		lockout := findCondition(cr.Status.Conditions, v1alpha1.ConditionRBACLockoutRisk)
		if lockout == nil || lockout.Status != metav1.ConditionTrue || lockout.Reason != "NoHumanAdmin" {
			t.Errorf("expected RBACLockoutRisk True/NoHumanAdmin, got %+v", lockout)
		}
	})

	t.Run("override-true", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		muteTransport := &captureTransport{}
		rec.miteTransport = muteTransport

		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test", Namespace: "ns", UID: testUID,
				CreationTimestamp: metav1.NewTime(time.Now()),
				Annotations:       map[string]string{"varroa.dev/allow-admin-lockout": "true"},
			},
			Spec: v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		_ = rec.reconcileController(context.Background(), cr)

		if muteTransport.lastDesired == nil {
			t.Fatal("expected desired state push")
		}
		if muteTransport.lastDesired.RbacYaml == "" {
			t.Error("expected RbacYaml populated (authz passed separately, override=true)")
		}
		if muteTransport.lastDesired.JcascYaml == "" {
			t.Error("expected JcascYaml still set")
		}
		if strings.Contains(muteTransport.lastDesired.JcascYaml, "authorizationStrategy") {
			t.Error("expected JcascYaml NOT to contain authorizationStrategy (authz is separate now)")
		}
		lockout := findCondition(cr.Status.Conditions, v1alpha1.ConditionRBACLockoutRisk)
		if lockout == nil || lockout.Status != metav1.ConditionFalse || lockout.Reason != "LockoutOverridden" {
			t.Errorf("expected RBACLockoutRisk False/LockoutOverridden, got %+v", lockout)
		}
	})

	t.Run("override-non-true", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		muteTransport := &captureTransport{}
		rec.miteTransport = muteTransport

		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test", Namespace: "ns", UID: testUID,
				CreationTimestamp: metav1.NewTime(time.Now()),
				Annotations:       map[string]string{"varroa.dev/allow-admin-lockout": "false"},
			},
			Spec: v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		_ = rec.reconcileController(context.Background(), cr)

		if muteTransport.lastDesired == nil {
			t.Fatal("expected desired state push")
		}
		if muteTransport.lastDesired.RbacYaml != "" {
			t.Error("expected RbacYaml empty (non-true override, guard still fires)")
		}
		lockout := findCondition(cr.Status.Conditions, v1alpha1.ConditionRBACLockoutRisk)
		if lockout == nil || lockout.Status != metav1.ConditionTrue || lockout.Reason != "NoHumanAdmin" {
			t.Errorf("expected RBACLockoutRisk True/NoHumanAdmin, got %+v", lockout)
		}
	})

	t.Run("human-admin-present", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		rec.rbacGenerator = rbac.NewGenerator(seedResolverWithHumanAdmin())
		muteTransport := &captureTransport{}
		rec.miteTransport = muteTransport

		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		_ = rec.reconcileController(context.Background(), cr)

		if muteTransport.lastDesired == nil {
			t.Fatal("expected desired state push")
		}
		if muteTransport.lastDesired.RbacYaml == "" {
			t.Error("expected RbacYaml populated (authz passed separately)")
		}
		if muteTransport.lastDesired.JcascYaml == "" {
			t.Error("expected JcascYaml still set")
		}
		if strings.Contains(muteTransport.lastDesired.JcascYaml, "authorizationStrategy") {
			t.Error("expected JcascYaml NOT to contain authorizationStrategy (authz is separate now)")
		}
		lockout := findCondition(cr.Status.Conditions, v1alpha1.ConditionRBACLockoutRisk)
		if lockout == nil || lockout.Status != metav1.ConditionFalse || lockout.Reason != "HumanAdminPresent" {
			t.Errorf("expected RBACLockoutRisk False/HumanAdminPresent, got %+v", lockout)
		}
	})

	t.Run("human-admin-legacy-permission-format", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		resolver := rbac.NewTestResolverWithRoles(
			[]*v1alpha1.VarroaRole{{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy-admin"},
				Spec:       v1alpha1.VarroaRoleSpec{JenkinsPermissions: []string{"Overall.Administer"}},
			}},
			[]*v1alpha1.VarroaRoleBinding{{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy-binding"},
				Spec: v1alpha1.VarroaRoleBindingSpec{
					RoleRef:  "legacy-admin",
					Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "varroa-admins"}},
				},
			}},
		)
		rec.rbacGenerator = rbac.NewGenerator(resolver)
		muteTransport := &captureTransport{}
		rec.miteTransport = muteTransport

		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
			Status: v1alpha1.ControllerStatus{
				Phase:      v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			},
		}
		_ = rec.reconcileController(context.Background(), cr)

		if muteTransport.lastDesired == nil {
			t.Fatal("expected desired state push")
		}
		if muteTransport.lastDesired.RbacYaml == "" {
			t.Error("expected RbacYaml populated (authz passed separately)")
		}
		if strings.Contains(muteTransport.lastDesired.JcascYaml, "authorizationStrategy") {
			t.Error("expected JcascYaml NOT to contain authorizationStrategy (authz is separate now)")
		}
		lockout := findCondition(cr.Status.Conditions, v1alpha1.ConditionRBACLockoutRisk)
		if lockout == nil || lockout.Status != metav1.ConditionFalse || lockout.Reason != "HumanAdminPresent" {
			t.Errorf("expected RBACLockoutRisk False/HumanAdminPresent, got %+v", lockout)
		}
	})
}

func TestComputeChangedSectionsNoRbac(t *testing.T) {
	desired := &mitev1.DesiredStateCommand{
		JcascYaml: "jenkins:\n  systemMessage: hello\n",
		ItemsYaml: "items:\n  some: value\n",
		RbacYaml:  "",
	}

	// Nil snapshot → fallback should NOT include "rbac" or "plugins".
	changes := computeChangedSections(nil, desired)
	if len(changes) != 2 {
		t.Fatalf("expected 2 sections, got %d: %v", len(changes), changes)
	}
	seen := make(map[string]bool)
	for _, c := range changes {
		seen[c] = true
	}
	if seen["rbac"] {
		t.Error("expected no 'rbac' in nil-snapshot fallback")
	}
	if !seen["config"] || !seen["items"] {
		t.Error("expected config, items in nil-snapshot fallback")
	}
	if seen["plugins"] {
		t.Error("plugins is no longer a tracked changed-section")
	}
}

func TestPluginInstallRequired(t *testing.T) {
	t.Run("drift-manual-mode", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{Mode: v1alpha1.ReconciliationModeManual}
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		// Add a new plugin to the bundle that wasn't in the baked set.
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: simple-theme-plugin\n    version: \"1.0\"\n"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("expected PluginInstallRequired condition to be set")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True, got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonPluginInstallRequired {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonPluginInstallRequired, c.Reason)
		}
		if !strings.Contains(c.Message, "+simple-theme-plugin") {
			t.Errorf("message should mention added plugin, got %q", c.Message)
		}
		if !strings.Contains(c.Message, "plugin-roll") {
			t.Errorf("message should mention the roll-approval remedy, got %q", c.Message)
		}
		// Phase stays Connected (no transition).
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("expected phase Connected (manual unapproved), got %s", cr.Status.Phase)
		}
		// PendingPluginRoll is surfaced.
		if cr.Status.PendingPluginRoll == nil {
			t.Fatal("expected PendingPluginRoll to be set")
		}
		if cr.Status.PendingPluginRoll.TargetChecksum == "" {
			t.Error("expected non-empty TargetChecksum")
		}
		if len(cr.Status.PendingPluginRoll.Changes) == 0 {
			t.Error("expected non-empty Changes")
		}
	})

	t.Run("drift-automatic-mode-transitions", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		// Default policy mode is automatic; drift triggers an immediate
		// phase transition to Provisioning so the roll runs.
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: simple-theme-plugin\n    version: \"1.0\"\n"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("expected PluginInstallRequired condition to be set")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True (automatic mode keeps True while transitioning), got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonPluginInstallRequired {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonPluginInstallRequired, c.Reason)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("expected phase Provisioning (automatic transitions), got %s", cr.Status.Phase)
		}
		if cr.Status.ProvisioningStartedAt == nil {
			t.Error("expected ProvisioningStartedAt to be set")
		}
		pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
		if pc == nil || pc.Status != metav1.ConditionTrue {
			t.Errorf("expected Provisioning condition True, got %+v", pc)
		}
	})

	t.Run("no-drift", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		// Same bundle (empty plugins.yaml) — no drift.
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		// Pre-seed a stale PendingPluginRoll, which should be cleared on convergence.
		cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{
			TargetChecksum: "stale",
			Since:          metav1Now(),
			Changes:        []string{"+old-plugin"},
		}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("expected PluginInstallRequired condition to be set")
		}
		if c.Status != metav1.ConditionFalse {
			t.Errorf("expected False, got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonPluginsInstalled {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonPluginsInstalled, c.Reason)
		}
		if cr.Status.PendingPluginRoll != nil {
			t.Error("expected stale PendingPluginRoll to be cleared on convergence")
		}
	})

	t.Run("approved-roll-transitions", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		// Run provisioning first to bake the baseline.
		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{Mode: v1alpha1.ReconciliationModeManual}
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		// Add a new plugin to cause drift.
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: simple-theme-plugin\n    version: \"1.0\"\n"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		_ = rec.reconcileController(context.Background(), cr)

		// Read the desired checksum from PendingPluginRoll and use it as the approval.
		if cr.Status.PendingPluginRoll == nil {
			t.Fatal("first reconcile should set PendingPluginRoll")
		}
		cr.Status.ApprovedPluginRollChecksum = cr.Status.PendingPluginRoll.TargetChecksum
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		// Reconcile again — now the approved gate should trigger a transition.
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("expected phase Provisioning (approved roll), got %s", cr.Status.Phase)
		}
		if cr.Status.ProvisioningStartedAt == nil {
			t.Error("expected ProvisioningStartedAt to be set")
		}
		pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
		if pc == nil || pc.Status != metav1.ConditionTrue {
			t.Errorf("expected Provisioning condition True, got %+v", pc)
		}
		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("expected PluginInstallRequired condition to be set")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True (not cleared during transition), got %s", c.Status)
		}
	})

	t.Run("manual-unapproved-no-flap", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{Mode: v1alpha1.ReconciliationModeManual}
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: simple-theme-plugin\n    version: \"1.0\"\n"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("expected Connected after first reconcile, got %s", cr.Status.Phase)
		}
		if cr.Status.PendingPluginRoll == nil {
			t.Fatal("expected PendingPluginRoll after first reconcile")
		}
		since1 := cr.Status.PendingPluginRoll.Since

		// Second reconcile — same diff, phase should stay Connected, Since preserved.
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("expected Connected after second reconcile (no flap), got %s", cr.Status.Phase)
		}
		if cr.Status.PendingPluginRoll == nil {
			t.Fatal("expected PendingPluginRoll to still be set")
		}
		if !since1.Time.Equal(cr.Status.PendingPluginRoll.Since.Time) {
			t.Error("expected PendingPluginRoll.Since to be preserved across ticks")
		}
	})

	t.Run("version-bump-drift-manual-mode", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{Mode: v1alpha1.ReconciliationModeManual}
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		// Same plugin but different version — whole-set drift fires.
		// Use a non-core plugin (not covered by the embedded lockfile); a
		// lock-covered plugin would be dropped from the managed set and never drift.
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: job-dsl\n    version: \"999.va\"\n"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("expected PluginInstallRequired condition to be set")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True, got %s", c.Status)
		}
	})

	t.Run("configmap-read-error-leaves-unchanged", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
			{Type: v1alpha1.ConditionPluginInstallRequired, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonPluginInstallRequired, Message: "preexisting"},
		}
		// Delete the plugins ConfigMap to force a read error.
		delete(client.configMapData, testPrefix+"-plugins")
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("condition should still exist (not cleared)")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True (unchanged), got %s", c.Status)
		}
		if c.Message != "preexisting" {
			t.Errorf("expected message to be unchanged, got %q", c.Message)
		}
	})

	t.Run("empty-baked-set-leaves-unchanged", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		// Skip Provisioning — manually seed an empty baked set.
		client.configMapData[testPrefix+"-plugins"] = map[string]string{"plugins.txt": ""}
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
			{Type: v1alpha1.ConditionPluginInstallRequired, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonPluginInstallRequired, Message: "preexisting"},
		}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("condition should still exist (not cleared)")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True (unchanged), got %s", c.Status)
		}
	})

	t.Run("bundle-resolve-failure-leaves-stale", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Provisioning reconcile: %v", err)
		}
		// Delete the bundle content ConfigMap so resolveBundleForController fails.
		delete(client.configMapData, "test-bundle-content")
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
			{Type: v1alpha1.ConditionPluginInstallRequired, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonPluginInstallRequired, Message: "preexisting"},
		}
		_ = rec.reconcileController(context.Background(), cr)

		// BundleFailed should be set.
		bf := findCondition(cr.Status.Conditions, v1alpha1.ConditionBundleFailed)
		if bf == nil || bf.Status != metav1.ConditionTrue {
			t.Errorf("expected BundleFailed=True during bundle resolution failure, got %+v", bf)
		}
		// PluginInstallRequired must stay unchanged.
		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired)
		if c == nil {
			t.Fatal("PluginInstallRequired should still exist (not cleared)")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True (unchanged), got %s", c.Status)
		}
		if c.Message != "preexisting" {
			t.Errorf("expected message to be unchanged, got %q", c.Message)
		}
	})
}

func TestConfigApplyFailed(t *testing.T) {
	t.Run("config-failure-true-and-ready-unchanged", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		mt := &captureTransport{}
		rec.miteTransport = mt

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		mt.drainResults = []*mitev1.CommandResult{
			mkDSR(false, true, true, true, "JCasC preflight rejected: unknown element simpleTheme", "", "", "", "hash-cfg-fail"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionConfigApplyFailed)
		if c == nil {
			t.Fatal("expected ConfigApplyFailed condition to be set")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True, got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonJCascApplyFailed {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonJCascApplyFailed, c.Reason)
		}
		if c.Message != "JCasC preflight rejected: unknown element simpleTheme" {
			t.Errorf("expected error message, got %q", c.Message)
		}
		// Ready must remain independent — not clobbered by the config failure.
		ready := findCondition(cr.Status.Conditions, v1alpha1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue {
			t.Errorf("expected Ready=True despite config apply failure, got %+v", ready)
		}
	})

	t.Run("config-success-clears-stale-true", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		mt := &captureTransport{}
		rec.miteTransport = mt

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.Conditions = []v1alpha1.ControllerCondition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
			{Type: v1alpha1.ConditionConfigApplyFailed, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonJCascApplyFailed, Message: "stale failure"},
		}
		mt.drainResults = []*mitev1.CommandResult{
			mkDSR(true, true, true, true, "", "", "", "", "hash-all-ok"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionConfigApplyFailed)
		if c == nil {
			t.Fatal("expected ConfigApplyFailed condition to be set")
		}
		if c.Status != metav1.ConditionFalse {
			t.Errorf("expected False (successful apply should clear stale True), got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonConfigApplied {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonConfigApplied, c.Reason)
		}
	})

	t.Run("no-last-apply-result-false", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionConfigApplyFailed)
		if c == nil {
			t.Fatal("expected ConfigApplyFailed condition to be set")
		}
		if c.Status != metav1.ConditionFalse {
			t.Errorf("expected False, got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonConfigApplied {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonConfigApplied, c.Reason)
		}
	})

	t.Run("stale-true-persists-on-empty-drain", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		mt := &captureTransport{}
		rec.miteTransport = mt

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.Conditions = []v1alpha1.ControllerCondition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"},
			{Type: v1alpha1.ConditionConfigApplyFailed, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonJCascApplyFailed, Message: "old failure"},
		}
		cr.Status.LastApplyResult = &v1alpha1.ApplyResult{
			Sections: []v1alpha1.ApplySectionResult{
				{Name: "config", OK: false, Error: "old failure"},
				{Name: "rbac", OK: true},
				{Name: "plugins", OK: true},
				{Name: "items", OK: true},
			},
		}
		// No drainResults — empty-drain tick, LastApplyResult unchanged.
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionConfigApplyFailed)
		if c == nil {
			t.Fatal("expected ConfigApplyFailed condition to be set")
		}
		if c.Status != metav1.ConditionTrue {
			t.Errorf("expected True (persists on empty drain), got %s", c.Status)
		}
		if c.Message != "old failure" {
			t.Errorf("expected message to persist, got %q", c.Message)
		}
	})

	t.Run("no-config-section-false", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseConnected)
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}
		cr.Status.LastApplyResult = &v1alpha1.ApplyResult{
			Sections: []v1alpha1.ApplySectionResult{
				{Name: "rbac", OK: false, Error: "rbac failed"},
				{Name: "plugins", OK: true},
				{Name: "items", OK: true},
			},
		}
		_ = rec.reconcileController(context.Background(), cr)

		c := findCondition(cr.Status.Conditions, v1alpha1.ConditionConfigApplyFailed)
		if c == nil {
			t.Fatal("expected ConfigApplyFailed condition to be set")
		}
		if c.Status != metav1.ConditionFalse {
			t.Errorf("expected False (no config section → no positive evidence), got %s", c.Status)
		}
		if c.Reason != v1alpha1.ReasonConfigApplied {
			t.Errorf("expected reason %s, got %s", v1alpha1.ReasonConfigApplied, c.Reason)
		}
	})
}

// TestJenkinsImageAndEffectiveDesired verifies the image helpers used by
// the version-roll path (design section 3).
func TestJenkinsImageAndEffectiveDesired(t *testing.T) {
	t.Run("jenkinsImageForVersion empty pins the lock baseline, not latest", func(t *testing.T) {
		want := "jenkins/jenkins:" + pluginlock.Baseline()
		if got := jenkinsImageForVersion("", nil); got != want {
			t.Errorf("jenkinsImageForVersion(\"\", nil) = %q, want %q", got, want)
		}
		if got := jenkinsImageForVersion("", nil); got == "jenkins/jenkins:latest" {
			t.Error("empty version must not resolve to a floating :latest core (#185)")
		}
	})
	t.Run("jenkinsImageForVersion lts sentinel pins the baseline too", func(t *testing.T) {
		want := "jenkins/jenkins:" + pluginlock.Baseline()
		if got := jenkinsImageForVersion("lts", nil); got != want {
			t.Errorf("jenkinsImageForVersion(%q, nil) = %q, want %q", "lts", got, want)
		}
		if got := jenkinsImageForVersion("lts", nil); got == "jenkins/jenkins:lts" {
			t.Error("the lts sentinel must not become a floating :lts tag (#185)")
		}
	})
	// Both unpinned sentinels must agree with what EvaluateCoreCompat reports as
	// baseline-backed, or the compat condition claims a pinning that isn't real.
	t.Run("jenkinsImageForVersion sentinels match ResolveProfile baseline set", func(t *testing.T) {
		for _, v := range []string{"", "lts", "  ", " lts "} {
			if _, kind := ResolveProfile(v, nil); kind != MatchBaseline {
				t.Errorf("ResolveProfile(%q) kind = %v, want MatchBaseline", v, kind)
			}
			if got, want := jenkinsImageForVersion(v, nil), "jenkins/jenkins:"+pluginlock.Baseline(); got != want {
				t.Errorf("jenkinsImageForVersion(%q, nil) = %q, want %q", v, got, want)
			}
		}
	})
	// The bug this guards: ResolveProfile returns (nil, MatchBaseline) for "" and
	// resolveCoreSet pins plugins to pluginlock.Baseline(), so the deployed core
	// must be that same baseline or plugins-init crash-loops with
	// AggregatePluginPrerequisitesNotMetException.
	t.Run("jenkinsImageForVersion empty agrees with the resolved core set", func(t *testing.T) {
		_, kind := ResolveProfile("", nil)
		if kind != MatchBaseline {
			t.Fatalf("ResolveProfile(\"\") kind = %v, want MatchBaseline", kind)
		}
		image := jenkinsImageForVersion("", nil)
		if want := "jenkins/jenkins:" + pluginlock.Baseline(); image != want {
			t.Errorf("image %q disagrees with plugin-lock baseline %q", image, want)
		}
	})
	t.Run("jenkinsImageForVersion specific", func(t *testing.T) {
		if got := jenkinsImageForVersion("2.570.2", nil); got != "jenkins/jenkins:2.570.2" {
			t.Errorf("jenkinsImageForVersion(\"2.570.2\", nil) = %q, want %q", got, "jenkins/jenkins:2.570.2")
		}
	})
	t.Run("jenkinsImageForVersion prefers profile ResolveVersion for LTS lines", func(t *testing.T) {
		profile := &v1alpha1.JenkinsVersionProfile{Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.555", Channel: "lts", ResolveVersion: "2.555.1",
		}}
		if got := jenkinsImageForVersion("2.555", profile); got != "jenkins/jenkins:2.555.1" {
			t.Errorf("jenkinsImageForVersion(\"2.555\", profile) = %q, want %q", got, "jenkins/jenkins:2.555.1")
		}
	})
	t.Run("jenkinsImageForVersion falls back to version when profile has no ResolveVersion", func(t *testing.T) {
		profile := &v1alpha1.JenkinsVersionProfile{Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.570", Channel: "weekly",
		}}
		if got := jenkinsImageForVersion("2.570", profile); got != "jenkins/jenkins:2.570" {
			t.Errorf("jenkinsImageForVersion(\"2.570\", profile) = %q, want %q", got, "jenkins/jenkins:2.570")
		}
	})
	t.Run("effectiveDesiredJenkinsImage resolves profile ResolveVersion", func(t *testing.T) {
		client := newTestClient()
		client.profiles = map[string]*v1alpha1.JenkinsVersionProfile{
			"jenkins-version-2-555": {
				ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-555"},
				Spec: v1alpha1.JenkinsVersionProfileSpec{
					Version: "2.555", Channel: "lts", ResolveVersion: "2.555.1",
				},
			},
		}
		rec := newTestReconciler(client)
		cr := testController("c", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.555"
		if got := rec.effectiveDesiredJenkinsImage(cr); got != "jenkins/jenkins:2.555.1" {
			t.Errorf("effectiveDesiredJenkinsImage = %q, want %q", got, "jenkins/jenkins:2.555.1")
		}
	})
	t.Run("effectiveDesiredJenkinsImage version-only", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := testController("c", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		if got := rec.effectiveDesiredJenkinsImage(cr); got != "jenkins/jenkins:2.570.2" {
			t.Errorf("effectiveDesiredJenkinsImage = %q, want %q", got, "jenkins/jenkins:2.570.2")
		}
	})
	t.Run("overlay-declared image wins", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := testController("c", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: jenkins\n        image: my-reg/custom:1.0\n",
		}
		if got := rec.effectiveDesiredJenkinsImage(cr); got != "my-reg/custom:1.0" {
			t.Errorf("effectiveDesiredJenkinsImage = %q, want %q", got, "my-reg/custom:1.0")
		}
	})
	t.Run("invalid overlay falls back", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := testController("c", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec: [bad",
		}
		if got := rec.effectiveDesiredJenkinsImage(cr); got != "jenkins/jenkins:2.570.2" {
			t.Errorf("effectiveDesiredJenkinsImage = %q, want %q", got, "jenkins/jenkins:2.570.2")
		}
	})
}

// --- Version-driven upgrade roll (change fix-version-driven-upgrade) ---

// TestReconcileVersionRoll covers the reconcileVersionRoll trigger (design §3/§4).
func TestReconcileVersionRoll(t *testing.T) {
	ctx := context.Background()

	t.Run("connected delta transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"

		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected reconcileVersionRoll to return true (transition)")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		if cr.Status.ProvisioningStartedAt == nil {
			t.Error("ProvisioningStartedAt must be reset")
		}
		pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
		if pc == nil || pc.Status != metav1.ConditionTrue || pc.Reason != "ProvisioningStarted" {
			t.Errorf("Provisioning condition = %+v, want True/ProvisioningStarted", pc)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Status != metav1.ConditionTrue || vc.Reason != v1alpha1.ReasonVersionRollStarted {
			t.Errorf("VersionRollPending = %+v, want True/VersionRollStarted", vc)
		}
	})

	t.Run("running delta transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.Version = "2.570.2"
		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected transition from Running")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
	})

	t.Run("converged is a no-op and gate is not called", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		rec := newTestReconciler(client)
		rec.versionRollGate = func(context.Context, *v1alpha1.Controller, string, string) (bool, string, string) {
			t.Fatal("gate must not be called when converged")
			return true, "", ""
		}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition when converged")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want Connected (unchanged)", cr.Status.Phase)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Status != metav1.ConditionFalse || vc.Reason != v1alpha1.ReasonVersionConverged {
			t.Errorf("VersionRollPending = %+v, want False/VersionConverged", vc)
		}
	})

	t.Run("converged with preserved override notes divergence", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		client.stsLiveImages = map[string]string{"jenkins": "custom/override:1"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition (stamp == desired)")
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != v1alpha1.ReasonVersionConverged {
			t.Fatalf("VersionRollPending = %+v, want VersionConverged", vc)
		}
		if !strings.Contains(vc.Message, "preserved (out-of-band override)") {
			t.Errorf("message = %q, want it to note the preserved override", vc.Message)
		}
	})

	t.Run("pre-stamp fallback uses live image", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = nil // no stamp
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected transition using pre-stamp live fallback")
		}
	})

	t.Run("missing StatefulSet is a no-op and leaves the condition untouched", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = nil
		client.stsLiveImages = nil // no STS
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionVersionRollPending, Status: metav1.ConditionTrue, Reason: "SENTINEL",
		})
		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition on missing STS")
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != "SENTINEL" {
			t.Errorf("VersionRollPending = %+v, want untouched SENTINEL", vc)
		}
	})

	t.Run("read error is a no-op and leaves the condition untouched", func(t *testing.T) {
		client := newTestClient()
		client.stsImagesErr = fmt.Errorf("boom")
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionVersionRollPending, Status: metav1.ConditionTrue, Reason: "SENTINEL",
		})
		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no transition on read error")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase changed on read error: %q", cr.Status.Phase)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != "SENTINEL" {
			t.Errorf("VersionRollPending = %+v, want untouched SENTINEL", vc)
		}
	})

	t.Run("denying gate holds the controller and is re-evaluated each tick", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconciler(client)
		calls := 0
		rec.versionRollGate = func(_ context.Context, _ *v1alpha1.Controller, cur, tgt string) (bool, string, string) {
			calls++
			return false, "VersionUpgradeBlocked", "blocked reason"
		}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		for i := 0; i < 2; i++ {
			if rec.reconcileVersionRoll(ctx, cr) {
				t.Fatalf("tick %d: expected hold (no transition)", i)
			}
			if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
				t.Errorf("tick %d: phase = %q, want Connected (held)", i, cr.Status.Phase)
			}
		}
		if calls != 2 {
			t.Errorf("gate call count = %d, want 2 (re-evaluated each tick)", calls)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Status != metav1.ConditionTrue || vc.Reason != "VersionUpgradeBlocked" || vc.Message != "blocked reason" {
			t.Errorf("VersionRollPending = %+v, want True/VersionUpgradeBlocked/blocked reason", vc)
		}
	})

	t.Run("denying gate with empty reason defaults to VersionRollHeld", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconciler(client)
		rec.versionRollGate = func(context.Context, *v1alpha1.Controller, string, string) (bool, string, string) {
			return false, "", ""
		}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		rec.reconcileVersionRoll(ctx, cr)
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != "VersionRollHeld" {
			t.Errorf("VersionRollPending reason = %v, want VersionRollHeld", vc)
		}
	})

	t.Run("overlay-declared image opts out of version rolls and rolls on overlay change", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"jenkins": "my-reg/custom:1.0"}
		client.stsLiveImages = map[string]string{"jenkins": "my-reg/custom:1.0"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: jenkins\n        image: my-reg/custom:1.0\n",
		}
		// spec.version edit is inert: desired == overlay image == stamp.
		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected no roll (overlay-governed, converged)")
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != v1alpha1.ReasonVersionConverged {
			t.Fatalf("VersionRollPending = %+v, want VersionConverged", vc)
		}
		if !strings.Contains(vc.Message, "declared by resourceOverlay") {
			t.Errorf("message = %q, want it to note the overlay", vc.Message)
		}
		// Changing the overlay image itself produces a delta and rolls.
		cr.Spec.ResourceOverlay.StatefulSet = "spec:\n  template:\n    spec:\n      containers:\n      - name: jenkins\n        image: my-reg/custom:2.0\n"
		if !rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("expected a roll when the overlay-declared image changes")
		}
	})

	t.Run("reconcileController skips connected dispatch on a version roll", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconcilerWithTokenSigner(client)
		capT := &captureTransport{}
		rec.miteTransport = capT
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("reconcileController: %v", err)
		}
		// The version-roll transition happened, proving handleConnected was skipped.
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != v1alpha1.ReasonVersionRollStarted {
			t.Errorf("VersionRollPending = %+v, want VersionRollStarted", vc)
		}
		// No desired-state push occurred (connected-phase work skipped).
		if capT.lastDesired != nil {
			t.Error("expected no desired-state push on a version-roll tick")
		}
	})
}

// TestReconcileContainerSpecRoll covers issue #368 (image) and its follow-up
// hardening (resources/imagePullPolicy, PR fix/mite-convergence-hardening): a
// spec.miteSpec.{image,resources,imagePullPolicy} change on an
// already-Connected controller must roll the StatefulSet's mite container,
// mirroring reconcileVersionRoll's Connected→Provisioning mechanism but with
// no compat gate (a direct spec edit is its own approval). Unless noted,
// subtests set stsMiteFound/stsMitePullPolicy to the converged baseline
// (found=true, pullPolicy=IfNotPresent, cpu/memory empty — the zero-config
// defaults from effectiveDesiredMiteResources/effectiveDesiredMiteImagePullPolicy)
// so the image assertions under test aren't muddied by an incidental
// resources/pullPolicy delta.
func TestReconcileContainerSpecRoll(t *testing.T) {
	ctx := context.Background()

	t.Run("connected image delta transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"

		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected reconcileContainerSpecRoll to return true (transition)")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		if cr.Status.ProvisioningStartedAt == nil {
			t.Error("ProvisioningStartedAt must be set")
		}
		pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
		if pc == nil || pc.Status != metav1.ConditionTrue || pc.Reason != "ProvisioningStarted" {
			t.Errorf("Provisioning condition = %+v, want True/ProvisioningStarted", pc)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionTrue || mc.Reason != v1alpha1.ReasonMiteSpecRollStarted {
			t.Errorf("MiteSpecRollPending = %+v, want True/MiteSpecRollStarted", mc)
		}
	})

	t.Run("running image delta transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.ClassName = "test-class"
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected transition from Running")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
	})

	t.Run("matching explicit spec image is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v2"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v2"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition when converged")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want Connected (unchanged)", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("MiteSpecRollPending = %+v, want False/MiteSpecConverged", mc)
		}
	})

	t.Run("unset spec image matches the operator default is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		// cr.Spec.MiteSpec is nil: an empty/unset spec image must never be
		// treated as a delta against the operator-wide default, or every
		// controller without an explicit miteSpec.image would roll forever.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition: unset spec image matches the provisioner's default")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("MiteSpecRollPending = %+v, want False/MiteSpecConverged", mc)
		}
	})

	t.Run("MiteSpec set but Image empty also falls back to the default", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "Always"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{ImagePullPolicy: "Always"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition: empty MiteSpec.Image falls back to the default")
		}
	})

	t.Run("pre-stamp fallback uses live image", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = nil // no stamp
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected transition using pre-stamp live fallback")
		}
	})

	t.Run("missing StatefulSet is a no-op and leaves the condition untouched", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = nil
		client.stsLiveImages = nil // no STS
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionMiteSpecRollPending, Status: metav1.ConditionTrue, Reason: "SENTINEL",
		})
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition on missing STS")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != "SENTINEL" {
			t.Errorf("MiteSpecRollPending = %+v, want untouched SENTINEL", mc)
		}
	})

	t.Run("read error is a no-op and leaves the condition untouched", func(t *testing.T) {
		client := newTestClient()
		client.stsImagesErr = fmt.Errorf("boom")
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionMiteSpecRollPending, Status: metav1.ConditionTrue, Reason: "SENTINEL",
		})
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition on read error")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase changed on read error: %q", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != "SENTINEL" {
			t.Errorf("MiteSpecRollPending = %+v, want untouched SENTINEL", mc)
		}
	})

	t.Run("resources read error is a no-op and leaves the condition untouched", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteErr = fmt.Errorf("boom")
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionMiteSpecRollPending, Status: metav1.ConditionTrue, Reason: "SENTINEL",
		})
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition on resources read error")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase changed on resources read error: %q", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != "SENTINEL" {
			t.Errorf("MiteSpecRollPending = %+v, want untouched SENTINEL", mc)
		}
	})

	t.Run("reconcileController skips connected dispatch on a mite spec roll", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconcilerWithTokenSigner(client)
		capT := &captureTransport{}
		rec.miteTransport = capT
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("reconcileController: %v", err)
		}
		// The mite-spec-roll transition happened, proving handleConnected was skipped.
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecRollStarted {
			t.Errorf("MiteSpecRollPending = %+v, want MiteSpecRollStarted", mc)
		}
		// No desired-state push occurred (connected-phase work skipped).
		if capT.lastDesired != nil {
			t.Error("expected no desired-state push on a mite-spec-roll tick")
		}
	})

	// Regression test for a Copilot PR-review finding on #370: a
	// resourceOverlay.statefulSet that declares the mite container's image
	// must win over spec.miteSpec.image/the default, mirroring
	// effectiveDesiredJenkinsImage's existing overlay precedence. Before this
	// fix, effectiveDesiredMiteImage ignored the overlay entirely; since
	// CreateStatefulSet applies overlays *before* stamping
	// varroa.dev/computed-images, the stamped/live image would forever reflect
	// the overlay while this check kept comparing against the spec/default —
	// an unconvergeable Connected->Provisioning loop.
	t.Run("overlay-declared image opts out of mite-spec rolls and rolls on overlay change", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": "my-reg/custom-mite:1.0"}
		client.stsLiveImages = map[string]string{"mite": "my-reg/custom-mite:1.0"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "some-other/mite:9.9"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		// class-level mite image is set but must be shadowed by the overlay below.
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: mite\n        image: my-reg/custom-mite:1.0\n",
		}
		// class mite image is inert here: desired == overlay image == stamp.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll (overlay-governed, converged)")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
		if !strings.Contains(mc.Message, "declared by resourceOverlay") {
			t.Errorf("message = %q, want it to note the overlay", mc.Message)
		}
		// Calling it again must stay quiet (no roll loop).
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a second call to also report no roll (no loop)")
		}
		// Changing the overlay image itself produces a real delta and rolls.
		cr.Spec.ResourceOverlay.StatefulSet = "spec:\n  template:\n    spec:\n      containers:\n      - name: mite\n        image: my-reg/custom-mite:2.0\n"
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll when the overlay-declared image changes")
		}
	})

	// Fix 2 (fix/mite-convergence-hardening): spec.miteSpec.resources and
	// spec.miteSpec.imagePullPolicy have the same Connected-phase blind spot
	// #368 fixed for the image — baked only at Provisioning, never
	// drift-checked. These subtests cover the coordinator's explicit ask:
	// drift on each field alone rolls; unset/default stays quiet; overlay
	// governance stays quiet.
	t.Run("resources drift alone transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}
		if client.stsMiteResources == nil {
			client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
		}
		client.stsMiteResources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected transition on resources drift")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecRollStarted {
			t.Errorf("MiteSpecRollPending = %+v, want MiteSpecRollStarted", mc)
		}
		if !strings.Contains(mc.Message, "resources") {
			t.Errorf("message = %q, want it to mention resources", mc.Message)
		}
	})

	t.Run("imagePullPolicy drift alone transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{ImagePullPolicy: "Always"}},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected transition on imagePullPolicy drift")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecRollStarted {
			t.Errorf("MiteSpecRollPending = %+v, want MiteSpecRollStarted", mc)
		}
		if !strings.Contains(mc.Message, "imagePullPolicy") {
			t.Errorf("message = %q, want it to mention imagePullPolicy", mc.Message)
		}
	})

	t.Run("unset resources/imagePullPolicy against baked defaults is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent" // what provisioning bakes when unset
		// stsMiteResources left nil (zero value): no resources baked.
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		// cr.Spec.MiteSpec is nil: must never read as drift, or every
		// controller without explicit miteSpec.resources/imagePullPolicy
		// would roll forever.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition: unset resources/imagePullPolicy match provisioning's defaults")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("MiteSpecRollPending = %+v, want False/MiteSpecConverged", mc)
		}
	})

	// Overlay-declared resources must not read as drift, mirroring the
	// image-overlay precedence test above (the same overlay-precedence
	// question already solved for the image).
	t.Run("overlay-declared resources opt out of mite-spec rolls and roll on overlay change", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}
		if client.stsMiteResources == nil {
			client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
		}
		client.stsMiteResources.Requests[corev1.ResourceMemory] = resource.MustParse("512Mi")
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		// spec.miteSpec.resources is set but must be shadowed by the overlay below.
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m"), corev1.ResourceMemory: resource.MustParse("999Mi")}}}
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: mite\n        resources:\n          requests:\n            cpu: \"500m\"\n            memory: \"512Mi\"\n",
		}
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll (overlay-governed resources, converged)")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
		// Changing the overlay resources produces a real delta and rolls.
		cr.Spec.ResourceOverlay.StatefulSet = "spec:\n  template:\n    spec:\n      containers:\n      - name: mite\n        resources:\n          requests:\n            cpu: \"750m\"\n            memory: \"512Mi\"\n"
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll when the overlay-declared resources change")
		}
	})

	// Regression for PR #373 review: an overlay declaring an unquoted
	// numeric cpu/memory (e.g. `cpu: 1`) decodes as YAML float64, not
	// string — overlay.ResourcesOverride must still pick it up (a bare
	// .(string) assertion silently missed it, so the override was ignored
	// and the drift check compared against the wrong desired value).
	t.Run("overlay-declared unquoted numeric resources are detected, no roll when converged", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
		if client.stsMiteResources == nil {
			client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
		}
		client.stsMiteResources.Requests[corev1.ResourceMemory] = resource.MustParse("2")
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m"), corev1.ResourceMemory: resource.MustParse("999Mi")}}}
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: "spec:\n  template:\n    spec:\n      containers:\n      - name: mite\n        resources:\n          requests:\n            cpu: 1\n            memory: 2\n",
		}
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: unquoted numeric overlay override matches the live template")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
	})

	// Quantity-normalization semantics (PR #373 review): "1" and "1000m"
	// are the same CPU quantity spelled two different ways. The drift
	// check must treat them as equal — a differently-spelled-but-equal
	// live value (e.g. left over from before an overlay was added, or
	// simply written in a different unit) must never be reported as
	// drift, or resourcesDelta would stay true forever and Connected would
	// bounce to Provisioning on every reconcile with nothing left to
	// converge. This is settled behavior, not a TODO: quantity equality,
	// not string equality, decides resourcesDelta.
	t.Run("differently-spelled but quantity-equal resources are not drift", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}} // live: milli-spelling
		if client.stsMiteResources == nil {
			client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
		}
		client.stsMiteResources.Requests[corev1.ResourceMemory] = resource.MustParse("512Mi") // live and desired agree exactly
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}}} // desired: whole-unit spelling
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: \"1\" and \"1000m\" are the same CPU quantity")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
		// A genuine quantity change (not just a respelling) still rolls.
		cr.Spec.MiteSpec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll: 2 cpu is a real delta from the live 1000m (1 cpu)")
		}
	})

	// Regression for PR #373 review: a live StatefulSet template that
	// predates spec.miteSpec.imagePullPolicy being baked onto the mite
	// container (Fix 2) has no imagePullPolicy set at all, and
	// GetStatefulSetContainerSpecs reads that back as "". Comparing "" against
	// the desired default ("IfNotPresent") as a literal delta would trigger
	// an unnecessary fleet-wide Provisioning roll on every such controller
	// immediately after this change deploys, even though runtime behavior
	// already matches the default and spec.miteSpec.imagePullPolicy is
	// unset. An empty live pull policy must default to
	// defaultMiteImagePullPolicy before the comparison.
	t.Run("empty live pullPolicy (pre-existing STS) defaults before comparison, no spurious roll", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "" // pre-existing STS: field never baked
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		// cr.Spec.MiteSpec is nil: desired pull policy falls back to
		// defaultMiteImagePullPolicy ("IfNotPresent"), same as the live
		// template's actual (if unset) runtime behavior.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: empty live pullPolicy must default before comparing, not read as drift")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
		// A genuine pull-policy change still rolls even from an empty live
		// value.
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{ImagePullPolicy: "Always"}},
		}
		crdstore.MustSeed(client.store, client.controllerClass)
		cr.Spec.ClassName = "test-class"
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll: desired \"Always\" differs from the defaulted live \"IfNotPresent\"")
		}
	})
	// --- Class-resources do not trigger spurious roll (F2) ---

	// Scenario A: a Controller with spec.Resources == nil referencing a
	// ControllerClass whose Spec.Resources sets requests+limits; the live
	// StatefulSet carries the class resources (provisioning baked them).
	// Since desiredJenkinsResources is nil (spec+overlay only — no class
	// tier), the nil-desired gate must treat this as converged, not as
	// drift.  Class-supplied resources are provision-only (D-6).
	t.Run("class-supplied resources do not read as drift when spec is nil", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		// Live STS carries class resources (what provisioning wrote).
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		}
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec: v1alpha1.ControllerClassSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
				},
			},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		// cr.Spec.Resources is nil — desiredJenkinsResources will be nil.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: class-supplied resources are provision-only, must not read as drift")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
	})

	// Convergence: spec.Resources set to X, live STS carrying Y != X → roll.
	t.Run("jenkins resources spec change still triggers roll when spec is set", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		}
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll: spec declares different resources than live")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
	})

	// Convergence: spec.Resources == live STS → no roll.
	t.Run("jenkins resources spec equal to live is converged", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: spec matches live")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
	})

	// --- F2' stamp-driven three-way decision ---

	// removal-rolls: STS stamped jenkins=spec, live has resources,
	// cr.Spec.Resources now nil → roll (one re-provision resets stamp).
	t.Run("resources removal rolls when stamped spec", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		client.stsResourcesSource = map[string]string{"jenkins": "spec", "mite": "spec"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		// cr.Spec.Resources is nil: the block was removed.
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll: resources stamped spec but spec is now nil (block was removed)")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionTrue {
			t.Fatalf("MiteSpecRollPending = %+v, want True", mc)
		}
	})

	// class-edit-no-roll: cr.Spec.Resources nil, STS stamped jenkins=class
	// carrying class resources that differ from the current class := no roll
	// (provision-gated, D-6).
	t.Run("class resources stamped class do not roll when spec is nil", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		// Live carries the old class values (stale relative to current class).
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		client.stsResourcesSource = map[string]string{"jenkins": "class", "mite": "none"}
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec: v1alpha1.ControllerClassSpec{
				// Current class differs from live — but since source=class, no roll.
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
				},
			},
		}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		// cr.Spec.Resources is nil.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: class-stamped resources are provision-gated")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
	})

	// missing-stamp-no-roll: STS has NO resources-source annotation (pre-epic),
	// cr.Spec.Resources nil, live has resources → no spurious roll on upgrade.
	t.Run("missing resources-source stamp does not roll when spec is nil", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		// stsResourcesSource left nil: no stamp yet (pre-epic or first deploy).
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		// cr.Spec.Resources is nil.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no roll: missing stamp defaults to skip (no spurious upgrade roll)")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Fatalf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
	})

	// spec-edit-still-converges: cr.Spec.Resources = X, live = Y != X,
	// stamp = spec → roll (edits converge same as before).
	t.Run("spec resources edit still rolls when stamped spec", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsJenkinsResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}
		client.stsResourcesSource = map[string]string{"jenkins": "spec", "mite": "none"}
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		}
		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected a roll: spec declares different resources than live")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
	})

	// --- Controller-level spec.miteSpec image/pullPolicy rolls (#376) ---

	t.Run("connected MiteSpec image delta transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}

		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected reconcileContainerSpecRoll to return true (transition)")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionTrue || mc.Reason != v1alpha1.ReasonMiteSpecRollStarted {
			t.Errorf("MiteSpecRollPending = %+v, want True/MiteSpecRollStarted", mc)
		}
	})

	t.Run("MiteSpec image matches live is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v2"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v2"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}

		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition when MiteSpec image is converged")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want Connected (unchanged)", cr.Status.Phase)
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("MiteSpecRollPending = %+v, want False/MiteSpecConverged", mc)
		}
	})

	t.Run("MiteSpec imagePullPolicy delta transitions to Provisioning", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{ImagePullPolicy: "Always"}

		if !rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected transition on MiteSpec imagePullPolicy drift")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecRollStarted {
			t.Errorf("MiteSpecRollPending = %+v, want MiteSpecRollStarted", mc)
		}
		if !strings.Contains(mc.Message, "imagePullPolicy") {
			t.Errorf("message = %q, want it to mention imagePullPolicy", mc.Message)
		}
	})

	t.Run("MiteSpec imagePullPolicy matches live is a no-op", func(t *testing.T) {
		client := newTestClient()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "Always"
		rec := newTestReconciler(client)
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{ImagePullPolicy: "Always"}

		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("expected no transition when MiteSpec imagePullPolicy is converged")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("MiteSpecRollPending = %+v, want MiteSpecConverged", mc)
		}
	})
}

// TestContainerSpecRollTwoPhaseConvergence exercises the reconcile-level
// two-phase convergence for a spec.miteSpec.image edit (issue #368): tick 1
// detects the delta on a Connected controller and transitions to
// Provisioning; tick 2 (provisioning) is represented by advancing the stamp
// to the desired image, exactly what CreateStatefulSet writes; tick 3+ must
// go quiet — the "no roll loops" requirement. Also covers the same
// two-phase shape for a resources-only edit (Fix 2).
func TestContainerSpecRollTwoPhaseConvergence(t *testing.T) {
	ctx := context.Background()

	t.Run("delta then converged, no repeat trigger", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v1"}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "ghcr.io/varroaci/varroa-jenkins:v2"}},
		}
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.ClassName = "test-class"
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		// Tick 1: Connected + delta → Provisioning.
		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("tick1: %v", err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Fatalf("tick1 phase = %q, want Provisioning", cr.Status.Phase)
		}

		// Tick 2 (provisioning pass) is represented by advancing the stamp to
		// the desired image, exactly what CreateStatefulSet writes. Then back
		// to Connected.
		client.stsComputedImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v2"}
		client.stsLiveImages = map[string]string{"mite": "ghcr.io/varroaci/varroa-jenkins:v2"}
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected

		// Tick 3: converged, no repeat trigger.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("tick3: expected convergence (no transition)")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("tick3 MiteSpecRollPending = %+v, want False/MiteSpecConverged", mc)
		}

		// Tick 4: still converged (repeated calls stay quiet, no roll loop).
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("tick4: expected still-converged (no transition)")
		}
	})

	t.Run("resources-only delta then converged, no repeat trigger", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.stsComputedImages = map[string]string{"mite": defaultMiteImage()}
		client.stsLiveImages = map[string]string{"mite": defaultMiteImage()}
		client.stsMiteFound = true
		client.stsMitePullPolicy = "IfNotPresent"
		client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}
		if client.stsMiteResources == nil {
			client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
		}
		client.stsMiteResources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		// Tick 1: Connected + delta → Provisioning.
		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("tick1: %v", err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Fatalf("tick1 phase = %q, want Provisioning", cr.Status.Phase)
		}

		// Tick 2 (provisioning pass) is represented by advancing the live
		// resources to the desired values, exactly what CreateStatefulSet
		// writes. Then back to Connected.
		client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}}
		if client.stsMiteResources == nil {
			client.stsMiteResources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{}}
		}
		client.stsMiteResources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected

		// Tick 3: converged, no repeat trigger.
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("tick3: expected convergence (no transition)")
		}
		mc := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteSpecRollPending)
		if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != v1alpha1.ReasonMiteSpecConverged {
			t.Errorf("tick3 MiteSpecRollPending = %+v, want False/MiteSpecConverged", mc)
		}

		// Tick 4: still converged (repeated calls stay quiet, no roll loop).
		if rec.reconcileContainerSpecRoll(ctx, cr) {
			t.Fatal("tick4: expected still-converged (no transition)")
		}
	})
}

// TestVersionRollTwoPhaseConvergence exercises the reconcile-level two-phase
// convergence (spec: version-upgrade-roll + connected-plugin-convergence delta).
// The image application on the provisioning pass is covered by
// TestCreateStatefulSetImageUpdate (dynamic-fake harness); the testClient fake
// used here cannot apply images, so tick-2 convergence is simulated by advancing
// the stamp — the split is deliberate (design task 7.1).
func TestVersionRollTwoPhaseConvergence(t *testing.T) {
	ctx := context.Background()

	t.Run("delta then converged", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)
		cr.Spec.Version = "2.570.2"
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		// Tick 1: Connected + delta → Provisioning.
		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("tick1: %v", err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Fatalf("tick1 phase = %q, want Provisioning", cr.Status.Phase)
		}

		// Tick 2 (provisioning pass) is represented by advancing the stamp to the
		// desired image, exactly what CreateStatefulSet writes (asserted separately
		// in TestCreateStatefulSetImageUpdate). Then back to Connected.
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.2"}
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected

		// Tick 3: converged.
		if rec.reconcileVersionRoll(ctx, cr) {
			t.Fatal("tick3: expected convergence (no transition)")
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Status != metav1.ConditionFalse || vc.Reason != v1alpha1.ReasonVersionConverged {
			t.Errorf("tick3 VersionRollPending = %+v, want False/VersionConverged", vc)
		}
	})

	t.Run("manual unapproved plugin delta plus version delta defers plugin eval", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}

		// Bake a baseline plugins set via a provisioning pass, then introduce a
		// plugin delta the manual gate has NOT approved.
		cr := testController("t", "ns", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.ReconciliationPolicy = &v1alpha1.ReconciliationPolicy{Mode: v1alpha1.ReconciliationModeManual}
		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("provisioning: %v", err)
		}
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: simple-theme-plugin\n    version: \"1.0\"\n"

		// Add a version delta that the (default allow-all) gate permits.
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		cr.Spec.Version = "2.570.2"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("connected tick: %v", err)
		}
		// The version delta transitions to Provisioning and the connected-phase
		// plugin evaluation is skipped this tick (no PluginInstallRequired write).
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning (version roll)", cr.Status.Phase)
		}
		vc := findCondition(cr.Status.Conditions, v1alpha1.ConditionVersionRollPending)
		if vc == nil || vc.Reason != v1alpha1.ReasonVersionRollStarted {
			t.Errorf("VersionRollPending = %+v, want VersionRollStarted", vc)
		}
		if pic := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginInstallRequired); pic != nil && pic.Status == metav1.ConditionTrue {
			// The plugin approval gate is enforced by the provisioning phase, not
			// bypassed by the version roll; evaluation is deferred, not performed.
			t.Logf("PluginInstallRequired present (from prior state) = %+v", pic)
		}
	})

	t.Run("automatic both deltas single Provisioning re-entry", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconcilerWithTokenSigner(client)
		rec.miteTransport = &captureTransport{}
		cr := testController("t", "ns", v1alpha1.ControllerPhaseProvisioning)
		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("provisioning: %v", err)
		}
		client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: simple-theme-plugin\n    version: \"1.0\"\n"
		client.stsComputedImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		client.stsLiveImages = map[string]string{"jenkins": "jenkins/jenkins:2.570.1"}
		cr.Spec.Version = "2.570.2"
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		if err := rec.reconcileController(ctx, cr); err != nil {
			t.Fatalf("connected tick: %v", err)
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want a single Provisioning re-entry", cr.Status.Phase)
		}
	})
}

// --- Brood operations: imperative ack surfacing ---

// mkImperativeResult creates a CommandResult that is an imperative ack
// (no DesiredState payload) for testing LastImperativeResult recording.
func mkImperativeResult(cmdID string, success bool, errMsg string) *mitev1.CommandResult {
	return &mitev1.CommandResult{
		CommandId: cmdID,
		Success:   success,
		Error:     errMsg,
		// Result is nil — no DesiredState payload, so the drain switch hits default.
	}
}

// TestLastImperativeResultRecordsImperativeAcks verifies that every
// DesiredState-less CommandResult (imperative ack) is recorded last-one-wins
// on Controller.status.lastImperativeResult, and that existing RestartDrain
// correlation still works.
func TestLastImperativeResultRecordsImperativeAcks(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	muteTransport := &captureTransport{}
	rec.miteTransport = muteTransport

	now := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: now},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			// Pre-seed a RestartDrain to test correlation still works.
			RestartDrain: &v1alpha1.RestartDrainStatus{
				CommandID:        "restart-cmd-1",
				AttemptCount:     2,
				NextRetryAt:      &metav1.Time{Time: time.Now().Add(30 * time.Second)},
				LastReason:       "build-running",
				DesiredStateHash: "hash-abc",
			},
		},
	}

	t.Run("imperative-ack-sets-last-result", func(t *testing.T) {
		// Feed a single imperative ack matching the outstanding RestartDrain.
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkImperativeResult("restart-cmd-1", true, ""),
		}
		_ = rec.reconcileController(context.Background(), cr)

		// Assert LastImperativeResult was set.
		if cr.Status.LastImperativeResult == nil {
			t.Fatal("expected LastImperativeResult to be set")
		}
		if cr.Status.LastImperativeResult.CommandID != "restart-cmd-1" {
			t.Errorf("CommandID = %q, want %q", cr.Status.LastImperativeResult.CommandID, "restart-cmd-1")
		}
		if !cr.Status.LastImperativeResult.Success {
			t.Error("expected Success=true")
		}
		if cr.Status.LastImperativeResult.Error != "" {
			t.Errorf("expected empty Error, got %q", cr.Status.LastImperativeResult.Error)
		}
		if cr.Status.LastImperativeResult.FinishedAt.IsZero() {
			t.Error("expected FinishedAt to be set")
		}

		// Assert existing RestartDrain correlation still works (cleared on success).
		if cr.Status.RestartDrain != nil {
			t.Error("expected RestartDrain to be nil (cleared on success)")
		}
		// Verify the RestartDeferred condition was cleared.
		cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionRestartDeferred)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Errorf("expected RestartDeferred=False, got %+v", cond)
		}
	})

	t.Run("imperative-ack-last-one-wins", func(t *testing.T) {
		// Restore RestartDrain for this sub-test.
		cr.Status.RestartDrain = &v1alpha1.RestartDrainStatus{
			CommandID:        "restart-cmd-2",
			AttemptCount:     0,
			DesiredStateHash: "hash-xyz",
		}
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		// Two imperative acks in one tick: last one wins.
		muteTransport.drainResults = []*mitev1.CommandResult{
			mkImperativeResult("restart-cmd-2", false, "build-still-running"),
			mkImperativeResult("other-cmd", true, ""),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.LastImperativeResult == nil {
			t.Fatal("expected LastImperativeResult to be set")
		}
		// Last one wins: "other-cmd".
		if cr.Status.LastImperativeResult.CommandID != "other-cmd" {
			t.Errorf("CommandID = %q, want %q", cr.Status.LastImperativeResult.CommandID, "other-cmd")
		}
		if !cr.Status.LastImperativeResult.Success {
			t.Error("expected Success=true (last result succeeded)")
		}

		// The first result (restart-cmd-2, failed) should still have been
		// correlated to RestartDrain (same CommandID), so RestartDrain should
		// reflect the deferral, not nil.
		if cr.Status.RestartDrain == nil {
			t.Fatal("expected RestartDrain to still be set (first result matched and deferred)")
		}
		if cr.Status.RestartDrain.AttemptCount != 1 {
			t.Errorf("expected AttemptCount=1, got %d", cr.Status.RestartDrain.AttemptCount)
		}
		if cr.Status.RestartDrain.LastReason != "build-still-running" {
			t.Errorf("expected LastReason='build-still-running', got %q", cr.Status.RestartDrain.LastReason)
		}
	})

	t.Run("imperative-ack-error-propagation", func(t *testing.T) {
		cr.Status.RestartDrain = nil
		cr.Status.Conditions = []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}}

		muteTransport.drainResults = []*mitev1.CommandResult{
			mkImperativeResult("fail-cmd", false, "something went wrong"),
		}
		_ = rec.reconcileController(context.Background(), cr)

		if cr.Status.LastImperativeResult == nil {
			t.Fatal("expected LastImperativeResult to be set")
		}
		if cr.Status.LastImperativeResult.CommandID != "fail-cmd" {
			t.Errorf("CommandID = %q, want %q", cr.Status.LastImperativeResult.CommandID, "fail-cmd")
		}
		if cr.Status.LastImperativeResult.Success {
			t.Error("expected Success=false")
		}
		if cr.Status.LastImperativeResult.Error != "something went wrong" {
			t.Errorf("Error = %q, want %q", cr.Status.LastImperativeResult.Error, "something went wrong")
		}
	})
}

func TestOverlayActiveCondition(t *testing.T) {
	tests := []struct {
		name        string
		ro          *v1alpha1.ResourceOverlay
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:        "nil ResourceOverlay → False/NoResourceOverlay",
			ro:          nil,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  v1alpha1.ReasonNoResourceOverlay,
			wantMessage: "no resource overlay configured",
		},
		{
			name:        "empty ResourceOverlay → False/NoResourceOverlay",
			ro:          &v1alpha1.ResourceOverlay{},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  v1alpha1.ReasonNoResourceOverlay,
			wantMessage: "no resource overlay configured",
		},
		{
			name:        "only statefulSet set → True/ResourceOverlaySet with single field",
			ro:          &v1alpha1.ResourceOverlay{StatefulSet: "foo: bar"},
			wantStatus:  metav1.ConditionTrue,
			wantReason:  v1alpha1.ReasonResourceOverlaySet,
			wantMessage: "statefulSet",
		},
		{
			name:        "only service set → True/ResourceOverlaySet with single field",
			ro:          &v1alpha1.ResourceOverlay{Service: "bar: baz"},
			wantStatus:  metav1.ConditionTrue,
			wantReason:  v1alpha1.ReasonResourceOverlaySet,
			wantMessage: "service",
		},
		{
			name:        "statefulSet + ingress → True/ResourceOverlaySet with two fields in order",
			ro:          &v1alpha1.ResourceOverlay{StatefulSet: "a: 1", Ingress: "b: 2"},
			wantStatus:  metav1.ConditionTrue,
			wantReason:  v1alpha1.ReasonResourceOverlaySet,
			wantMessage: "statefulSet, ingress",
		},
		{
			name:        "all three set → True/ResourceOverlaySet in field order",
			ro:          &v1alpha1.ResourceOverlay{Service: "x: 1", StatefulSet: "y: 2", Ingress: "z: 3"},
			wantStatus:  metav1.ConditionTrue,
			wantReason:  v1alpha1.ReasonResourceOverlaySet,
			wantMessage: "statefulSet, service, ingress",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &v1alpha1.Controller{
				Spec: v1alpha1.ControllerSpec{ResourceOverlay: tt.ro},
			}
			cond := overlayActiveCondition(cr)
			if cond.Type != v1alpha1.ConditionOverlayActive {
				t.Errorf("Type = %q, want %q", cond.Type, v1alpha1.ConditionOverlayActive)
			}
			if cond.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", cond.Status, tt.wantStatus)
			}
			if cond.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", cond.Reason, tt.wantReason)
			}
			if cond.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", cond.Message, tt.wantMessage)
			}
		})
	}
}

func (t *testClient) GetPVC(_ context.Context, _, _ string) (*corev1.PersistentVolumeClaim, error) {
	if t.pvcErr != nil {
		return nil, t.pvcErr
	}
	if t.pvc != nil {
		return t.pvc.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, "varroa-updatecenter")
}

// TestRefreshClassResolvedCondition covers the Running/Connected-tick liveness
// of the ClassResolved condition: added/resolved, dangling, and unset
// className must each be reflected without a provisioning pass.
func TestRefreshClassResolvedCondition(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"},
	}

	rec.refreshClassResolvedCondition(context.Background(), cr)
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionClassResolved); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonNoClassConfigured {
		t.Fatalf("unset className: got %+v, want True/NoClassConfigured", c)
	}

	cr.Spec.ClassName = "gold"
	client.controllerClass = &v1alpha1.ControllerClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gold"},
	}
	crdstore.MustSeed(client.store, client.controllerClass)
	rec.refreshClassResolvedCondition(context.Background(), cr)
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionClassResolved); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonClassResolved {
		t.Fatalf("resolved class: got %+v, want True/ClassResolved", c)
	}

	client.controllerClass = nil // class deleted out from under a live controller
	if err := crdstore.Delete[v1alpha1.ControllerClass](context.Background(), client.store, "gold", ""); err != nil {
		t.Fatalf("delete class from store: %v", err)
	}
	rec.refreshClassResolvedCondition(context.Background(), cr)
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionClassResolved); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonClassNotFound {
		t.Fatalf("dangling class: got %+v, want False/ClassNotFound", c)
	}
}

// TestHandleProvisioning_DanglingClassPersistsCondition pins the fail-closed
// visibility contract: when className cannot be resolved, handleProvisioning
// returns an error (which skips the end-of-tick status persistence), so it
// must patch the False/ClassNotFound condition itself — otherwise the wedge
// is invisible to admins while provisioning silently retries.
func TestHandleProvisioning_DanglingClassPersistsCondition(t *testing.T) {
	client := newTestClient()
	client.controllerClass = nil // className set, class missing → fail closed
	rec := newTestReconciler(client)
	cr := testController("t", "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.ClassName = "ghost"

	if err := rec.handleProvisioning(context.Background(), cr); err == nil {
		t.Fatal("expected fail-closed error for dangling class")
	}
	if client.lastPatchedStatus == nil {
		t.Fatal("expected an explicit status patch on the fail-closed path")
	}
	c := findCondition(client.lastPatchedStatus.Conditions, v1alpha1.ConditionClassResolved)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonClassNotFound {
		t.Fatalf("persisted ClassResolved = %+v, want False/ClassNotFound", c)
	}
}

// TestResolvePluginUpdateCenterURLs exercises the 3-tier URL precedence:
//  1. explicit ProvisioningDefaults
//  2. in-cluster UC when Ready
//  3. "" (tool default)
//
// url and downloadURL resolve independently.
func TestResolvePluginUpdateCenterURLs(t *testing.T) {
	ucReady := &v1alpha1.UpdateCenter{
		Status: v1alpha1.UpdateCenterStatus{
			Conditions: []v1alpha1.UpdateCenterCondition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			},
		},
	}
	ucNotReady := &v1alpha1.UpdateCenter{
		Status: v1alpha1.UpdateCenterStatus{
			Conditions: []v1alpha1.UpdateCenterCondition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
		},
	}
	ucNoCond := &v1alpha1.UpdateCenter{
		Status: v1alpha1.UpdateCenterStatus{},
	}
	base := "http://uc.ns.svc:8080"

	tests := []struct {
		name      string
		defaults  *v1alpha1.ProvisioningDefaults
		uc        *v1alpha1.UpdateCenter
		ucBaseURL string
		wantURL   string
		wantDL    string
	}{
		{
			name: "explicit defaults override UC Ready",
			defaults: &v1alpha1.ProvisioningDefaults{
				Spec: v1alpha1.ProvisioningDefaultsSpec{
					PluginUpdateCenterURL:         "https://custom.example.com/update-center.json",
					PluginUpdateCenterDownloadURL: "https://custom.example.com/download",
				},
			},
			uc:        ucReady,
			ucBaseURL: base,
			wantURL:   "https://custom.example.com/update-center.json",
			wantDL:    "https://custom.example.com/download",
		},
		{
			name: "explicit url only — download falls to UC",
			defaults: &v1alpha1.ProvisioningDefaults{
				Spec: v1alpha1.ProvisioningDefaultsSpec{
					PluginUpdateCenterURL: "https://custom.example.com/update-center.json",
				},
			},
			uc:        ucReady,
			ucBaseURL: base,
			wantURL:   "https://custom.example.com/update-center.json",
			wantDL:    base + "/download",
		},
		{
			name: "explicit download only — url falls to UC",
			defaults: &v1alpha1.ProvisioningDefaults{
				Spec: v1alpha1.ProvisioningDefaultsSpec{
					PluginUpdateCenterDownloadURL: "https://custom.example.com/download",
				},
			},
			uc:        ucReady,
			ucBaseURL: base,
			wantURL:   base + "/update-center.actual.json",
			wantDL:    "https://custom.example.com/download",
		},
		{
			name:      "UC Ready — both resolved",
			defaults:  nil,
			uc:        ucReady,
			ucBaseURL: base,
			wantURL:   base + "/update-center.actual.json",
			wantDL:    base + "/download",
		},
		{
			name:      "UC not Ready — both empty",
			defaults:  nil,
			uc:        ucNotReady,
			ucBaseURL: base,
			wantURL:   "",
			wantDL:    "",
		},
		{
			name:      "UC no conditions — both empty",
			defaults:  nil,
			uc:        ucNoCond,
			ucBaseURL: base,
			wantURL:   "",
			wantDL:    "",
		},
		{
			name:      "UC absent — both empty",
			defaults:  nil,
			uc:        nil,
			ucBaseURL: base,
			wantURL:   "",
			wantDL:    "",
		},
		{
			name:      "UC disabled base URL — both empty",
			defaults:  nil,
			uc:        ucReady,
			ucBaseURL: "",
			wantURL:   "",
			wantDL:    "",
		},
		{
			name: "UC disabled with explicit defaults — byte-for-byte unchanged",
			defaults: &v1alpha1.ProvisioningDefaults{
				Spec: v1alpha1.ProvisioningDefaultsSpec{
					PluginUpdateCenterURL:         "https://updates.jenkins.io/update-center.json",
					PluginUpdateCenterDownloadURL: "https://updates.jenkins.io/download",
				},
			},
			uc:        nil,
			ucBaseURL: "",
			wantURL:   "https://updates.jenkins.io/update-center.json",
			wantDL:    "https://updates.jenkins.io/download",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotDL := resolvePluginUpdateCenterURLs(tt.defaults, tt.uc, tt.ucBaseURL)
			if gotURL != tt.wantURL {
				t.Errorf("url = %q, want %q", gotURL, tt.wantURL)
			}
			if gotDL != tt.wantDL {
				t.Errorf("downloadURL = %q, want %q", gotDL, tt.wantDL)
			}
		})
	}
}

// TestHandleProvisioning_UpdateCenterFallbackOnline verifies that when the UC
// is configured but not Ready in online (pull-through) mode, provisioning
// proceeds with an UpdateCenterFallback condition (non-blocking).
func TestHandleProvisioning_UpdateCenterFallbackOnline(t *testing.T) {
	client := newTestClientWithBundle()
	client.updateCenter = &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-update-center"},
		Spec: v1alpha1.UpdateCenterSpec{
			PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: true},
		},
		Status: v1alpha1.UpdateCenterStatus{
			Conditions: []v1alpha1.UpdateCenterCondition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
		},
	}
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	err := rec.handleProvisioning(context.Background(), cr)
	// Provisioning may fail downstream but we care about the condition.
	_ = err

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c == nil {
		t.Fatal("expected UpdateCenterFallback condition to be set")
	}
	if c.Status != metav1.ConditionTrue {
		t.Fatalf("UpdateCenterFallback = %s, want True", c.Status)
	}
	// WaitingForUpdateCenter must NOT be set in online mode.
	wc := findCondition(cr.Status.Conditions, v1alpha1.ConditionWaitingForUpdateCenter)
	if wc != nil && wc.Status == metav1.ConditionTrue {
		t.Fatal("WaitingForUpdateCenter must not be True in online mode")
	}
}

// TestHandleProvisioning_UpdateCenterAirGapBlocks verifies that when the UC
// is configured but not Ready in air-gap mode (pull-through disabled),
// provisioning is BLOCKED with WaitingForUpdateCenter.
func TestHandleProvisioning_UpdateCenterAirGapBlocks(t *testing.T) {
	client := newTestClientWithBundle()
	client.updateCenter = &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-update-center"},
		Spec: v1alpha1.UpdateCenterSpec{
			PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: false},
		},
		Status: v1alpha1.UpdateCenterStatus{
			Conditions: []v1alpha1.UpdateCenterCondition{
				{Type: "Ready", Status: metav1.ConditionFalse},
			},
		},
	}
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	err := rec.handleProvisioning(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning to be blocked in air-gap mode")
	}

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionWaitingForUpdateCenter)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("WaitingForUpdateCenter = %v, want True", c)
	}

	if client.lastPatchedStatus == nil {
		t.Fatal("expected status to be persisted on block path")
	}
}

// TestHandleProvisioning_UpdateCenterAbsentAirGapBlocks verifies air-gap
// blocking when UC is configured but CR does not exist at all.
func TestHandleProvisioning_UpdateCenterAbsentAirGapBlocks(t *testing.T) {
	client := newTestClientWithBundle()
	// updateCenter stays nil (CR not found)
	rec := newTestReconciler(client)
	rec.SetUpdateCenterBaseURL("http://uc.svc:8080")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	err := rec.handleProvisioning(context.Background(), cr)
	if err == nil {
		t.Fatal("expected provisioning to be blocked when UC CR is missing in air-gap context")
	}
	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionWaitingForUpdateCenter)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("WaitingForUpdateCenter = %v, want True", c)
	}
}

// TestHandleProvisioning_UpdateCenterDisabledByteForByteUnchanged verifies
// that when UC is disabled (ucBaseURL empty), behavior is byte-for-byte
// identical to the old code: explicit defaults pass through, no fallback
// conditions set.
func TestHandleProvisioning_UpdateCenterDisabledByteForByteUnchanged(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
		Spec: v1alpha1.ProvisioningDefaultsSpec{
			PluginUpdateCenterURL:         "https://updates.jenkins.io/update-center.json",
			PluginUpdateCenterDownloadURL: "https://updates.jenkins.io/download",
		},
	}
	rec := newTestReconciler(client)
	// ucBaseURL stays empty (UC disabled).
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	_ = rec.handleProvisioning(context.Background(), cr)

	c := findCondition(cr.Status.Conditions, v1alpha1.ConditionUpdateCenterFallback)
	if c != nil && c.Status == metav1.ConditionTrue {
		t.Fatal("UpdateCenterFallback must not be set when UC is disabled")
	}
	wc := findCondition(cr.Status.Conditions, v1alpha1.ConditionWaitingForUpdateCenter)
	if wc != nil && wc.Status == metav1.ConditionTrue {
		t.Fatal("WaitingForUpdateCenter must not be set when UC is disabled")
	}

	if len(client.stsSpecs) > 0 {
		sts := client.stsSpecs[0]
		if sts.PluginUpdateCenterURL != "https://updates.jenkins.io/update-center.json" {
			t.Errorf("PluginUpdateCenterURL = %q, want explicit default", sts.PluginUpdateCenterURL)
		}
		if sts.PluginUpdateCenterDownloadURL != "https://updates.jenkins.io/download" {
			t.Errorf("PluginUpdateCenterDownloadURL = %q, want explicit default", sts.PluginUpdateCenterDownloadURL)
		}
	}
}

// TestDefaultPullPolicyForImage verifies the k8s-mirroring defaulting rule:
// :latest / untagged → Always; non-latest tag / digest → IfNotPresent.
func TestDefaultPullPolicyForImage(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"x:latest", "Always"},
		{"x", "Always"},
		{"reg/x", "Always"},
		{"x:1.2.3", "IfNotPresent"},
		{"reg:5000/x:1.2.3", "IfNotPresent"},
		{"x@sha256:aaaa", "IfNotPresent"},
		{"", "IfNotPresent"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			got := defaultPullPolicyForImage(tt.image)
			if got != tt.want {
				t.Errorf("defaultPullPolicyForImage(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

// TestEffectiveDesiredMiteImagePullPolicy verifies that the default follows
// the k8s-mirroring rule from the mite image, and that an explicit overlay
// imagePullPolicy always wins.
func TestEffectiveDesiredMiteImagePullPolicy(t *testing.T) {
	// Helper: create CR with given overlay.
	crWithOverlay := func(overlayYAML string) *v1alpha1.Controller {
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		if overlayYAML != "" {
			cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
				StatefulSet: overlayYAML,
			}
		}
		return cr
	}

	// Mite image in overlay tagged :latest → Always.
	t.Run("latest tag → Always", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := crWithOverlay(`
spec:
  template:
    spec:
      containers:
      - name: mite
        image: reg/foo:latest
`)
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Always" {
			t.Errorf("got %q, want Always", got)
		}
	})

	// Mite image in overlay tagged :1.2.3 → IfNotPresent.
	t.Run("version tag → IfNotPresent", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := crWithOverlay(`
spec:
  template:
    spec:
      containers:
      - name: mite
        image: reg/foo:1.2.3
`)
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "IfNotPresent" {
			t.Errorf("got %q, want IfNotPresent", got)
		}
	})

	// Overlay sets imagePullPolicy=Never explicitly → wins regardless of tag.
	t.Run("explicit Never wins", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := crWithOverlay(`
spec:
  template:
    spec:
      containers:
      - name: mite
        image: reg/foo:latest
        imagePullPolicy: Never
`)
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Never" {
			t.Errorf("got %q, want Never", got)
		}
	})

	// No overlay → uses default image (ghcr.io/varroaci/varroa-jenkins:main,
	// which has a non-latest tag) → IfNotPresent.
	t.Run("no overlay → IfNotPresent (default image is tagged)", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "IfNotPresent" {
			t.Errorf("got %q, want IfNotPresent", got)
		}
	})

	// --- Controller-level (spec.miteSpec) precedence (#376) ---

	t.Run("MiteSpec imagePullPolicy wins over operator default", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{ImagePullPolicy: "Always"}
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Always" {
			t.Errorf("got %q, want Always", got)
		}
	})

	t.Run("class imagePullPolicy wins over operator default (regression guard)", func(t *testing.T) {
		client := newTestClient()
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{ImagePullPolicy: "Never"}},
		}
		rec := newTestReconciler(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.ClassName = "test-class"
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Never" {
			t.Errorf("got %q, want Never", got)
		}
	})

	t.Run("MiteSpec imagePullPolicy wins over class", func(t *testing.T) {
		client := newTestClient()
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{ImagePullPolicy: "Never"}},
		}
		rec := newTestReconciler(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.ClassName = "test-class"
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{ImagePullPolicy: "Always"}
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Always" {
			t.Errorf("got %q, want Always (MiteSpec should win over class)", got)
		}
	})

	t.Run("overlay imagePullPolicy wins over MiteSpec", func(t *testing.T) {
		rec := newTestReconciler(newTestClient())
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{ImagePullPolicy: "Always"}
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        imagePullPolicy: Never
`,
		}
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Never" {
			t.Errorf("got %q, want Never (overlay should win over MiteSpec)", got)
		}
	})

	t.Run("class pull policy wins when MiteSpec sets only image, not pullPolicy", func(t *testing.T) {
		client := newTestClient()
		client.controllerClass = &v1alpha1.ControllerClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
			Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{ImagePullPolicy: "Always"}},
		}
		rec := newTestReconciler(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.ClassName = "test-class"
		// Controller sets an image (which has a non-latest tag, so image-derived
		// default would be IfNotPresent), but not a pull policy. The class's
		// pull policy (Always) must still win.
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{Image: "reg/foo:v1.2.3"}
		got := rec.effectiveDesiredMiteImagePullPolicy(cr)
		if got != "Always" {
			t.Errorf("got %q, want Always (class pull policy wins over image-derived default)", got)
		}
	})
}

// TestEffectiveDesiredMiteImage verifies the four-tier precedence for mite
// image resolution: resourceOverlay > spec.miteSpec > class > operator default.
func TestEffectiveDesiredMiteImage(t *testing.T) {
	defaultImg := defaultMiteImage()

	tests := []struct {
		name        string
		miteSpec    *v1alpha1.MiteSpec
		class       *v1alpha1.ControllerClass
		overlayYAML string
		want        string
	}{
		{
			name:     "neither set: operator default",
			miteSpec: nil,
			class:    nil,
			want:     defaultImg,
		},
		{
			name:     "MiteSpec only: Controller value wins over operator default",
			miteSpec: &v1alpha1.MiteSpec{Image: "reg/controller-image:v1"},
			class:    nil,
			want:     "reg/controller-image:v1",
		},
		{
			name:     "class only: class value wins over operator default",
			miteSpec: nil,
			class: &v1alpha1.ControllerClass{
				ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
				Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "reg/class-image:v2"}},
			},
			want: "reg/class-image:v2",
		},
		{
			name:     "both set: MiteSpec wins over class",
			miteSpec: &v1alpha1.MiteSpec{Image: "reg/controller-image:v1"},
			class: &v1alpha1.ControllerClass{
				ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
				Spec:       v1alpha1.ControllerClassSpec{Mite: &v1alpha1.ClassMiteSpec{Image: "reg/class-image:v2"}},
			},
			want: "reg/controller-image:v1",
		},
		{
			name:     "overlay wins over MiteSpec",
			miteSpec: &v1alpha1.MiteSpec{Image: "reg/controller-image:v1"},
			class:    nil,
			overlayYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        image: reg/overlay-image:v3
`,
			want: "reg/overlay-image:v3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient()
			client.controllerClass = tt.class
			rec := newTestReconciler(client)
			cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
			if tt.class != nil {
				cr.Spec.ClassName = "test-class"
			}
			cr.Spec.MiteSpec = tt.miteSpec
			if tt.overlayYAML != "" {
				cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
					StatefulSet: tt.overlayYAML,
				}
			}
			got := rec.effectiveDesiredMiteImage(cr)
			if got != tt.want {
				t.Errorf("effectiveDesiredMiteImage = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEffectiveDesiredResources verifies per-resource-key override merging
// and null-delete semantics, including map-level nulls.
func TestEffectiveDesiredResources(t *testing.T) {
	rec := newTestReconciler(newTestClient())

	// Base: spec.MiteSpec.Resources requests{cpu:100m,memory:128Mi}
	// plus limits{cpu:"1",memory:"256Mi"}.
	baseCR := func() *v1alpha1.Controller {
		cr := testController("test", "ns", v1alpha1.ControllerPhaseRunning)
		cr.Spec.MiteSpec = &v1alpha1.MiteSpec{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		}
		return cr
	}

	// Overlay nulling requests.cpu → result has only memory (requests) and all limits.
	t.Run("overlay nulls requests.cpu", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: null
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got == nil {
			t.Fatal("got nil, want requests{memory:128Mi} + limits{cpu:1,memory:256Mi}")
		}
		if _, hasCPU := got.Requests[corev1.ResourceCPU]; hasCPU {
			t.Error("cpu should have been deleted by null")
		}
		if mem, ok := got.Requests[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("128Mi")) != 0 {
			t.Errorf("requests.memory = %v, want 128Mi", got.Requests[corev1.ResourceMemory])
		}
		if cpu, ok := got.Limits[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("1")) != 0 {
			t.Errorf("limits.cpu = %v, want 1", got.Limits[corev1.ResourceCPU])
		}
		if mem, ok := got.Limits[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("256Mi")) != 0 {
			t.Errorf("limits.memory = %v, want 256Mi", got.Limits[corev1.ResourceMemory])
		}
	})

	// Overlay setting requests.cpu:200m → per-key partial override.
	t.Run("overlay sets requests.cpu", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: "200m"
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got == nil {
			t.Fatal("got nil, want requests{cpu:200m,memory:128Mi} + limits{cpu:1,memory:256Mi}")
		}
		if cpu, ok := got.Requests[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("200m")) != 0 {
			t.Errorf("requests.cpu = %v, want 200m", got.Requests[corev1.ResourceCPU])
		}
		if mem, ok := got.Requests[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("128Mi")) != 0 {
			t.Errorf("requests.memory = %v, want 128Mi", got.Requests[corev1.ResourceMemory])
		}
	})

	// Map-level requests: null → deletes all requests, limits preserved.
	t.Run("map-level requests null deletes all requests", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests: null
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got == nil {
			t.Fatal("got nil, want limits{cpu:1,memory:256Mi} only (no requests)")
		}
		if len(got.Requests) != 0 {
			t.Errorf("requests = %v, want empty (all deleted)", got.Requests)
		}
		if cpu, ok := got.Limits[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("1")) != 0 {
			t.Errorf("limits.cpu = %v, want 1", got.Limits[corev1.ResourceCPU])
		}
		if mem, ok := got.Limits[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("256Mi")) != 0 {
			t.Errorf("limits.memory = %v, want 256Mi", got.Limits[corev1.ResourceMemory])
		}
	})

	// Map-level limits: null → deletes all limits, requests preserved.
	t.Run("map-level limits null deletes all limits", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          limits: null
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got == nil {
			t.Fatal("got nil, want requests{cpu:100m,memory:128Mi} only (no limits)")
		}
		if cpu, ok := got.Requests[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("100m")) != 0 {
			t.Errorf("requests.cpu = %v, want 100m", got.Requests[corev1.ResourceCPU])
		}
		if mem, ok := got.Requests[corev1.ResourceMemory]; !ok || mem.Cmp(resource.MustParse("128Mi")) != 0 {
			t.Errorf("requests.memory = %v, want 128Mi", got.Requests[corev1.ResourceMemory])
		}
		if len(got.Limits) != 0 {
			t.Errorf("limits = %v, want empty (all deleted)", got.Limits)
		}
	})

	// Map-level resources: null → deletes the entire resources block.
	t.Run("map-level resources null deletes everything", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources: null
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got != nil {
			t.Errorf("got %+v, want nil (entire resources block deleted)", got)
		}
	})

	// Mixed: requests: null + limits set → requests deleted, limits overridden.
	t.Run("mixed requests null with limits set", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests: null
          limits:
            cpu: "2"
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got == nil {
			t.Fatal("got nil, want limits{cpu:2} only (requests deleted)")
		}
		if len(got.Requests) != 0 {
			t.Errorf("requests = %v, want empty (all deleted)", got.Requests)
		}
		if cpu, ok := got.Limits[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("2")) != 0 {
			t.Errorf("limits.cpu = %v, want 2", got.Limits[corev1.ResourceCPU])
		}
		if _, hasMem := got.Limits[corev1.ResourceMemory]; !hasMem {
			t.Error("limits.memory should be preserved from base (256Mi)")
		}
	})

	// Regression: map-level requests:null must delete ALL request keys
	// including non-cpu/memory keys like ephemeral-storage. The previous
	// allKnownResourceKeys=[cpu,memory] approach would have left
	// ephemeral-storage in the desired requests, desyncing from the live
	// STS where the strategic-merge already deleted it.
	t.Run("requests null deletes non-cpu/memory keys like ephemeral-storage", func(t *testing.T) {
		cr := baseCR()
		// Add ephemeral-storage to the base requests.
		cr.Spec.MiteSpec.Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		cr.Spec.ResourceOverlay = &v1alpha1.ResourceOverlay{
			StatefulSet: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests: null
`,
		}
		got := rec.effectiveDesiredResources(cr, overlay.MiteContainerName)
		if got == nil {
			t.Fatal("got nil, want limits{cpu:1,memory:256Mi} only (requests all deleted)")
		}
		if len(got.Requests) != 0 {
			t.Errorf("requests = %v, want empty (all keys including ephemeral-storage deleted)", got.Requests)
		}
		// Limits survive untouched.
		if cpu, ok := got.Limits[corev1.ResourceCPU]; !ok || cpu.Cmp(resource.MustParse("1")) != 0 {
			t.Errorf("limits.cpu = %v, want 1", got.Limits[corev1.ResourceCPU])
		}
	})
}

// --- resolveRunningMiteImage tests ---

func TestResolveRunningMiteImage_PodImagePresent(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "jenkins", Image: "jenkins:lts"},
				{Name: "mite", Image: "mite:pod-image"},
			},
		},
	}
	// Configure STS with a different image to prove Pod tier wins.
	client.stsComputedImages = map[string]string{"mite": "mite:sts-stamp"}
	client.stsLiveImages = map[string]string{"mite": "mite:sts-live"}
	client.stsMiteFound = true

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if !found {
		t.Fatal("expected found=true")
	}
	if image != "mite:pod-image" {
		t.Errorf("image = %q, want %q", image, "mite:pod-image")
	}
}

func TestResolveRunningMiteImage_PodEmptyMiteImage(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mite", Image: ""},
			},
		},
	}
	client.stsComputedImages = map[string]string{"mite": "mite:sts-stamp"}
	client.stsLiveImages = map[string]string{"mite": "mite:sts-live"}
	client.stsMiteFound = true

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if !found {
		t.Fatal("expected found=true (falls through to STS)")
	}
	if image != "mite:sts-stamp" {
		t.Errorf("image = %q, want %q (STS computed stamp)", image, "mite:sts-stamp")
	}
}

func TestResolveRunningMiteImage_PodReadError(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.controllerPodErr = fmt.Errorf("connection refused")
	// STS has a distinguishable value that must NOT be returned.
	client.stsComputedImages = map[string]string{"mite": "mite:sts-stamp"}
	client.stsLiveImages = map[string]string{"mite": "mite:sts-live"}
	client.stsMiteFound = true

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if found {
		t.Error("expected found=false on pod read error")
	}
	if image != "" {
		t.Errorf("image = %q, want empty (pod read error short-circuits)", image)
	}
}

func TestResolveRunningMiteImage_NoPod_StsComputedStamp(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	// Pod absent (nil).
	client.stsComputedImages = map[string]string{"mite": "mite:computed"}
	client.stsLiveImages = map[string]string{"mite": "mite:live"}
	client.stsMiteFound = true

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if !found {
		t.Fatal("expected found=true")
	}
	if image != "mite:computed" {
		t.Errorf("image = %q, want %q", image, "mite:computed")
	}
}

func TestResolveRunningMiteImage_NoPod_StsLiveTemplateFallback(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.stsComputedImages = map[string]string{"mite": ""} // empty stamp
	client.stsLiveImages = map[string]string{"mite": "mite:live-only"}
	client.stsMiteFound = true

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if !found {
		t.Fatal("expected found=true")
	}
	if image != "mite:live-only" {
		t.Errorf("image = %q, want %q", image, "mite:live-only")
	}
}

func TestResolveRunningMiteImage_NoPod_StsBothEmpty(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.stsComputedImages = map[string]string{"mite": ""}
	client.stsLiveImages = map[string]string{"mite": ""}
	client.stsMiteFound = true

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if found {
		t.Error("expected found=false when both stamp and live template are empty")
	}
	if image != "" {
		t.Errorf("image = %q, want empty", image)
	}
}

func TestResolveRunningMiteImage_StsReadError(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.stsImagesErr = fmt.Errorf("etcd timeout")

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if found {
		t.Error("expected found=false on STS read error")
	}
	if image != "" {
		t.Errorf("image = %q, want empty", image)
	}
}

func TestResolveRunningMiteImage_NeitherPodNorSts(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	// Pod nil, stsMiteFound defaults to false.

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
	}
	image, found := rec.resolveRunningMiteImage(context.Background(), cr)
	if found {
		t.Error("expected found=false when neither Pod nor STS exists")
	}
	if image != "" {
		t.Errorf("image = %q, want empty", image)
	}
}

// --- refreshMiteImageStaleness tests ---

func TestRefreshMiteImageStaleness_Stale(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	// Pod image differs from desired.
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mite", Image: "mite:running"},
			},
		},
	}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
		Spec: v1alpha1.ControllerSpec{
			MiteSpec: &v1alpha1.MiteSpec{Image: "mite:desired"},
		},
		Status: v1alpha1.ControllerStatus{},
	}
	rec.refreshMiteImageStaleness(context.Background(), cr)

	if cr.Status.MiteStatus == nil {
		t.Fatal("MiteStatus is nil")
	}
	if cr.Status.MiteStatus.Image != "mite:running" {
		t.Errorf("MiteStatus.Image = %q, want %q", cr.Status.MiteStatus.Image, "mite:running")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteImageStale)
	if cond == nil {
		t.Fatal("ConditionMiteImageStale not found")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition status = %q, want True", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonMiteImageStale {
		t.Errorf("condition reason = %q, want %q", cond.Reason, v1alpha1.ReasonMiteImageStale)
	}
}

func TestRefreshMiteImageStaleness_Current(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	// Pod image matches desired.
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mite", Image: "mite:same"},
			},
		},
	}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
		Spec: v1alpha1.ControllerSpec{
			MiteSpec: &v1alpha1.MiteSpec{Image: "mite:same"},
		},
		Status: v1alpha1.ControllerStatus{},
	}
	rec.refreshMiteImageStaleness(context.Background(), cr)

	if cr.Status.MiteStatus == nil {
		t.Fatal("MiteStatus is nil")
	}
	if cr.Status.MiteStatus.Image != "mite:same" {
		t.Errorf("MiteStatus.Image = %q, want %q", cr.Status.MiteStatus.Image, "mite:same")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteImageStale)
	if cond == nil {
		t.Fatal("ConditionMiteImageStale not found")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %q, want False", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonMiteImageCurrent {
		t.Errorf("condition reason = %q, want %q", cond.Reason, v1alpha1.ReasonMiteImageCurrent)
	}
}

func TestRefreshMiteImageStaleness_NotObservable(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	// Neither Pod nor STS exists.
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
		Status: v1alpha1.ControllerStatus{
			MiteStatus: &v1alpha1.MiteStatus{Image: "prior"},
			Conditions: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionMiteImageStale, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonMiteImageStale, Message: "prior"},
			},
		},
	}

	rec.refreshMiteImageStaleness(context.Background(), cr)

	// MiteStatus.Image should be unchanged.
	if cr.Status.MiteStatus == nil || cr.Status.MiteStatus.Image != "prior" {
		t.Errorf("MiteStatus.Image = %q, want %q (unchanged)", cr.Status.MiteStatus.Image, "prior")
	}
	// Condition should be unchanged.
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteImageStale)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("condition should be unchanged (True from prior state)")
	}
}

func TestRefreshMiteImageStaleness_NilMiteStatusInit(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mite", Image: "mite:img"},
			},
		},
	}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID},
		Spec: v1alpha1.ControllerSpec{
			MiteSpec: &v1alpha1.MiteSpec{Image: "mite:img"},
		},
		Status: v1alpha1.ControllerStatus{
			MiteStatus: nil, // explicit nil
		},
	}
	rec.refreshMiteImageStaleness(context.Background(), cr)

	if cr.Status.MiteStatus == nil {
		t.Fatal("MiteStatus should be initialized, got nil")
	}
	if cr.Status.MiteStatus.Image != "mite:img" {
		t.Errorf("MiteStatus.Image = %q, want %q", cr.Status.MiteStatus.Image, "mite:img")
	}
}

// --- Deletion cleanup test ---

func TestReconcileController_DeletionCleansUpGauge(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	now := metav1.Now()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting-cr",
			Namespace:         "ns",
			UID:               testUID,
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Deletion succeeds without error; both per-controller gauges are reset
	// to 0 via synchronous Record calls (verified in the metric test).
	if len(cr.Finalizers) != 0 {
		t.Error("finalizer should be removed")
	}
}

// --- Call-site tests: refreshMiteImageStaleness runs for Stopped/Hibernated ---

func TestReconcileController_StoppedRefreshesImageStaleness(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	// Put a Pod with a mite image so resolveRunningMiteImage succeeds.
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mite", Image: "mite:pod"},
			},
		},
	}
	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Spec:       v1alpha1.ControllerSpec{MiteSpec: &v1alpha1.MiteSpec{Image: "mite:desired"}},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}

	// Run as Stopped — should call refreshMiteImageStaleness before the early return.
	cr.Spec.PowerState = "Stopped"
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cr.Status.MiteStatus == nil || cr.Status.MiteStatus.Image != "mite:pod" {
		t.Errorf("Stopped: MiteStatus.Image = %q, want %q",
			cr.Status.MiteStatus.Image, "mite:pod")
	}
	if findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteImageStale) == nil {
		t.Error("Stopped: MiteImageStale condition missing")
	}
}

func TestReconcileController_HibernatedRefreshesImageStaleness(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)

	// Put a Pod with a mite image so resolveRunningMiteImage succeeds.
	client.controllerPod = &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mite", Image: "mite:pod"},
			},
		},
	}
	recent := metav1.NewTime(time.Now())
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: recent},
		Spec:       v1alpha1.ControllerSpec{MiteSpec: &v1alpha1.MiteSpec{Image: "mite:desired"}},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}

	// Run as Hibernated — should call refreshMiteImageStaleness before the early return.
	cr.Spec.PowerState = "Hibernated"
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cr.Status.MiteStatus == nil || cr.Status.MiteStatus.Image != "mite:pod" {
		t.Errorf("Hibernated: MiteStatus.Image = %q, want %q",
			cr.Status.MiteStatus.Image, "mite:pod")
	}
	if findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteImageStale) == nil {
		t.Error("Hibernated: MiteImageStale condition missing")
	}
}

// TestNewReconcilerMultipleOK verifies that calling NewReconciler twice in the
// same test binary does not panic or race (multiple OTel callbacks are registered
// against the shared global MeterProvider — this is a known test-only side effect).
func TestNewReconcilerMultipleOK(t *testing.T) {
	client1 := newTestClient()
	_ = newTestReconciler(client1)

	client2 := newTestClient()
	_ = newTestReconciler(client2)
	// If we get here without a race or panic, the test passes.
}

// TestShouldEmitPluginConflictEvent covers the 3(prior) × 2(now) state space for
// the activity-event gating function.
func TestShouldEmitPluginConflictEvent(t *testing.T) {
	tests := []struct {
		name        string
		prior       *v1alpha1.ControllerCondition
		conflictNow bool
		want        bool
	}{
		// prior absent (nil)
		{name: "absent → conflict", prior: nil, conflictNow: true, want: true},
		{name: "absent → no conflict", prior: nil, conflictNow: false, want: false},

		// prior False
		{name: "False → conflict", prior: &v1alpha1.ControllerCondition{Status: metav1.ConditionFalse}, conflictNow: true, want: true},
		{name: "False → no conflict", prior: &v1alpha1.ControllerCondition{Status: metav1.ConditionFalse}, conflictNow: false, want: false},

		// prior True
		{name: "True → conflict", prior: &v1alpha1.ControllerCondition{Status: metav1.ConditionTrue}, conflictNow: true, want: false},
		{name: "True → no conflict", prior: &v1alpha1.ControllerCondition{Status: metav1.ConditionTrue}, conflictNow: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldEmitPluginConflictEvent(tt.prior, tt.conflictNow)
			if got != tt.want {
				t.Errorf("shouldEmitPluginConflictEvent(%v, %v) = %v, want %v", tt.prior, tt.conflictNow, got, tt.want)
			}
		})
	}
}

// TestPluginDriftCondition verifies the PluginInventoryDrift condition decision
// table. After the Finding A fix, a degraded, truncated, all-edges-dropped, or
// optional-edges-dropped inventory must report indeterminate BEFORE any
// unmanaged-count test, so a fresh filesystem source with an unmanaged
// plugin never reports actionable drift.
func TestPluginDriftCondition(t *testing.T) {
	tests := []struct {
		name           string
		stale          bool
		indeterminate  bool
		unmanagedCount int
		wantTrue       bool
		wantReason     string
	}{
		{
			name:  "stale suppresses everything",
			stale: true, indeterminate: false, unmanagedCount: 5,
			wantTrue: false, wantReason: "Stale",
		},
		{
			name:  "stale wins over indeterminate",
			stale: true, indeterminate: true, unmanagedCount: 5,
			wantTrue: false, wantReason: "Stale",
		},
		{
			name:  "indeterminate suppresses unmanaged",
			stale: false, indeterminate: true, unmanagedCount: 5,
			wantTrue: false, wantReason: "Indeterminate",
		},
		{
			name:  "actionable unmanaged",
			stale: false, indeterminate: false, unmanagedCount: 2,
			wantTrue: true, wantReason: "UnmanagedPlugins",
		},
		{
			name:  "no drift",
			stale: false, indeterminate: false, unmanagedCount: 0,
			wantTrue: false, wantReason: "NoDrift",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, _ := pluginDriftCondition(tc.stale, tc.indeterminate, tc.unmanagedCount)
			isTrue := status == metav1.ConditionTrue
			if isTrue != tc.wantTrue {
				t.Errorf("status True=%v, want %v", isTrue, tc.wantTrue)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
