package mcp

import (
	"context"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

type stubClient struct{}

func (s *stubClient) CreateService(ctx context.Context, name, namespace string, port int32, overlayYAML string) error {
	return nil
}
func (s *stubClient) CreateServiceAccount(ctx context.Context, name, namespace string) error {
	return nil
}
func (s *stubClient) CreateAgentRBAC(ctx context.Context, name, namespace string) error { return nil }
func (s *stubClient) CreateStatefulSet(ctx context.Context, spec controller.StatefulSetSpec) error {
	return nil
}
func (s *stubClient) UpdateStatefulSetOIDCEnv(ctx context.Context, name, namespace, oidcIssuer, loginURL, oidcUserClaim, oidcGroupClaim, pubKeyPEM, pubKeyKID, aud, apikeyVerifyURL, caPEM string) error {
	return nil
}
func (s *stubClient) EnsureStatefulSetPodLabel(ctx context.Context, namespace, stsName, key, value string) (bool, error) {
	return false, nil
}
func (s *stubClient) IsStatefulSetReady(ctx context.Context, name, namespace string) (bool, error) {
	return false, nil
}
func (s *stubClient) ScaleStatefulSet(ctx context.Context, name, namespace string, replicas int32) error {
	return nil
}
func (s *stubClient) CreateSecret(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error {
	return nil
}
func (s *stubClient) CreateSecretExclusive(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error {
	return nil
}
func (s *stubClient) CreateOrUpdateSecret(ctx context.Context, name, namespace string, data map[string][]byte) error {
	return nil
}
func (s *stubClient) PatchSecretData(ctx context.Context, name, namespace string, data map[string][]byte) error {
	return nil
}
func (s *stubClient) GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error) {
	return nil, nil
}
func (s *stubClient) GetSecretAnnotations(ctx context.Context, name, namespace string) (map[string]string, error) {
	return nil, nil
}
func (s *stubClient) ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error) {
	return nil, nil
}
func (s *stubClient) CopyImagePullSecret(ctx context.Context, srcNamespace, dstNamespace, name string) error {
	return nil
}
func (s *stubClient) CreateIngress(ctx context.Context, name, namespace, host, pathPrefix, tlsSecret, ingressClass string, annotations map[string]string, overlayYAML string) error {
	return nil
}
func (s *stubClient) CreateOrUpdateConfigMap(ctx context.Context, name, namespace string, data map[string]string, owners ...metav1.OwnerReference) error {
	return nil
}
func (s *stubClient) GetConfigMap(ctx context.Context, name, namespace string) (map[string]string, error) {
	return nil, nil
}
func (s *stubClient) RemoveConfigMapLabel(ctx context.Context, name, namespace, labelKey string) error {
	return nil
}
func (s *stubClient) UpdateConfigMapData(ctx context.Context, name, namespace string, data map[string]string) error {
	return nil
}
func (s *stubClient) DeleteResource(ctx context.Context, kind, name, namespace string) error {
	return nil
}
func (s *stubClient) DeleteSecret(ctx context.Context, name, namespace string) error { return nil }
func (s *stubClient) ApplyControllerSpecSSA(_ context.Context, _, _ string, _ map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	return nil, nil, nil
}
func (s *stubClient) ApplyControllerSpecSSAIfExists(_ context.Context, _, _ string, _ map[string]any, _ string, _ bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	return nil, nil, nil
}
func (s *stubClient) SetHibernated(_ context.Context, _, _ string, _ bool) (bool, error) {
	return false, nil
}
func (s *stubClient) PatchControllerStatus(ctx context.Context, name, namespace string, status *v1alpha1.ControllerStatus) error {
	return nil
}
func (s *stubClient) ClearUserPassword(ctx context.Context, name, namespace string) error { return nil }
func (s *stubClient) StreamPodLogs(ctx context.Context, namespace, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (s *stubClient) DeleteControllerPod(ctx context.Context, namespace, name string) error {
	return nil
}
func (s *stubClient) ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error) {
	return nil, nil
}
func (s *stubClient) ListIngressHosts(ctx context.Context) (map[string][]string, error) {
	return nil, nil
}
func (s *stubClient) GetStatefulSetPluginsChecksum(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (s *stubClient) GetStatefulSetImages(_ context.Context, _, _ string) (map[string]string, map[string]string, error) {
	return nil, nil, nil
}
func (s *stubClient) GetStatefulSetContainerSpecs(_ context.Context, _, _ string) (string, string, *corev1.ResourceRequirements, *corev1.ResourceRequirements, string, map[string]string, bool, error) {
	return "", "", nil, nil, "", nil, false, nil
}
func (s *stubClient) GetControllerPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	return nil, nil
}

func (s *stubClient) GetNamespace(_ context.Context, name string) (*corev1.Namespace, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func (s *stubClient) GetLiveResource(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (s *stubClient) CreateOrUpdateConfigMapWithOwner(_ context.Context, _, _ string, _ map[string]string, _ metav1.OwnerReference) error {
	return nil
}

func (s *stubClient) CreateOrUpdateOwnedConfigMap(_ context.Context, _, _ string, _ map[string]string, _ map[string]string) error {
	return nil
}

func (s *stubClient) EnsureWakeEndpointSlice(_ context.Context, _, _ string, _ []string, _ int32) error {
	return nil
}

func (s *stubClient) DeleteWakeEndpointSlice(_ context.Context, _, _ string) error {
	return nil
}

func (s *stubClient) ListOperatorPodIPs(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s *stubClient) GetPVC(_ context.Context, _, _ string) (*corev1.PersistentVolumeClaim, error) {
	return nil, nil
}
