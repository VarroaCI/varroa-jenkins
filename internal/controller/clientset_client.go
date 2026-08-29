package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

var (
	jenkinsGVR               = schema.GroupVersionResource{Group: "jenkins.io", Version: "v1alpha2", Resource: "jenkins"}
	controllerGVR            = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "controllers"}
	varroaRoleGVR            = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "varroaroles"}
	varroaRoleBindingGVR     = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "varroarolebindings"}
	jenkinsRoleGVR           = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "jenkinsroles"}
	jenkinsRoleBindingGVR    = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "jenkinsrolebindings"}
	podTemplateGVR           = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "podtemplates"}
	catalogSourceGVR         = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "catalogsources"}
	catalogItemGVR           = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "catalogitems"}
	composedBundleGVR        = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "composedbundles"}
	broodOperationGVR        = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "broodoperations"}
	provisioningDefaultsGVR  = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "provisioningdefaults"}
	userGVR                  = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "users"}
	groupGVR                 = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "groups"}
	teamGVR                  = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "teams"}
	jenkinsVersionProfileGVR = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "jenkinsversionprofiles"}
	controllerClassGVR       = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "controllerclasses"}
	updateCentersGVR         = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "updatecenters"}
)

// operatorUserAgent is the explicit field-manager identity the operator
// declares on its Kubernetes client configuration. client-go would otherwise
// derive the name from the binary basename (/app/varroa-operator), so renaming
// the binary would silently re-partition field ownership. See
// controller-write-scoping "The Operator Declares Its Field-Manager Identity".
const operatorUserAgent = "varroa-operator"

// computedImagesAnnotation stamps the operator-computed container images on
// the StatefulSet object metadata so out-of-band image edits are detectable.
const computedImagesAnnotation = "varroa.dev/computed-images"

// resourcesSourceAnnotation stamps which tier supplied each container's
// resource block at provisioning time — "spec", "class", or "none" — keyed
// by container name ("jenkins", "mite").  reconcileContainerSpecRoll reads
// this stamp to distinguish "desired is nil because spec block was removed
// → must roll" from "desired is nil because resources were never user-set →
// skip (provision-gated, D-6)".
const resourcesSourceAnnotation = "varroa.dev/resources-source"

// pluginsInitScript runs jenkins-plugin-cli with a bounded retry.
//
// jenkins-plugin-cli aborts the entire run on the first plugin it cannot download, and
// exits non-zero, which fails the init container and restarts the pod. Any plugin the
// in-cluster update center cannot serve is fetched from the public internet instead, so
// a single transient upstream failure — a truncated response, a mirror hiccup — discards
// every plugin already downloaded in that run and costs a full pod restart.
//
// Retrying in place absorbs that. The tool is idempotent (plugins already present in the
// download directory are not refetched), so each attempt resumes rather than restarting
// the work, and the backoff keeps a genuinely unreachable upstream from spinning. A real
// failure still exits non-zero after the last attempt, so a broken plugin set surfaces
// as a failed init container exactly as before, just later.
const pluginsInitScript = `set -e
attempt=1
max_attempts=5
while true; do
  if jenkins-plugin-cli --plugin-file /var/run/varroa/plugins/plugins.txt \
      --plugin-download-directory /var/jenkins_home/plugins --latest false --verbose; then
    exit 0
  fi
  if [ "$attempt" -ge "$max_attempts" ]; then
    echo "plugins-init: giving up after $max_attempts attempts" >&2
    exit 1
  fi
  delay=$((attempt * 10))
  echo "plugins-init: attempt $attempt failed, retrying in ${delay}s" >&2
  attempt=$((attempt + 1))
  sleep "$delay"
done`

type probeKind int

const (
	probeStartup probeKind = iota
	probeReadiness
	probeLiveness
)

type probeDefaults struct {
	initialDelaySeconds int32
	periodSeconds       int32
	timeoutSeconds      int32
	failureThreshold    int32
	successThreshold    int32
}

var probeDefaultTable = [...]probeDefaults{
	probeStartup: {
		initialDelaySeconds: 10,
		periodSeconds:       10,
		timeoutSeconds:      5,
		failureThreshold:    30,
		successThreshold:    1,
	},
	probeReadiness: {
		initialDelaySeconds: 0,
		periodSeconds:       10,
		timeoutSeconds:      5,
		failureThreshold:    3,
		successThreshold:    1,
	},
	probeLiveness: {
		initialDelaySeconds: 0,
		periodSeconds:       10,
		timeoutSeconds:      5,
		failureThreshold:    6,
		successThreshold:    1,
	},
}

func probeSpecFor(probes *v1alpha1.ProbesSpec, kind probeKind) *v1alpha1.ProbeSpec {
	if probes == nil {
		return nil
	}
	switch kind {
	case probeStartup:
		return probes.Startup
	case probeReadiness:
		return probes.Readiness
	case probeLiveness:
		return probes.Liveness
	default:
		return nil
	}
}

func renderProbe(kind probeKind, spec *v1alpha1.ProbeSpec, pathPrefix string) map[string]interface{} {
	if spec != nil && spec.Disabled {
		return nil
	}
	defaults := probeDefaultTable[kind]
	rendered := map[string]interface{}{
		"httpGet": map[string]interface{}{
			"path":   pathPrefix + "/login",
			"port":   "http",
			"scheme": "HTTP",
		},
		"periodSeconds":    int64(defaults.periodSeconds),
		"timeoutSeconds":   int64(defaults.timeoutSeconds),
		"failureThreshold": int64(defaults.failureThreshold),
		"successThreshold": int64(defaults.successThreshold),
	}
	initialDelay := defaults.initialDelaySeconds
	if spec != nil {
		if spec.InitialDelaySeconds != nil {
			initialDelay = *spec.InitialDelaySeconds
		}
		if spec.PeriodSeconds != nil {
			rendered["periodSeconds"] = int64(*spec.PeriodSeconds)
		}
		if spec.TimeoutSeconds != nil {
			rendered["timeoutSeconds"] = int64(*spec.TimeoutSeconds)
		}
		if spec.FailureThreshold != nil {
			rendered["failureThreshold"] = int64(*spec.FailureThreshold)
		}
		if spec.SuccessThreshold != nil {
			rendered["successThreshold"] = int64(*spec.SuccessThreshold)
		}
	}
	if initialDelay != 0 {
		rendered["initialDelaySeconds"] = int64(initialDelay)
	}
	return rendered
}

// ClientsetClient implements ResourceClient using client-go against the Kubernetes API.
type ClientsetClient struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	restCfg   *rest.Config
}

// NewClientsetClient creates a new ClientsetClient.
// Tries in-cluster config first (for pod deployments with proper RBAC),
// then falls back to kubeconfig (for local development).
func NewClientsetClient() (*ClientsetClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", "")
		if err != nil {
			return nil, fmt.Errorf("in-cluster config and kubeconfig both unavailable: %w", err)
		}
	}
	config.UserAgent = operatorUserAgent
	TuneClientRates(config)

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	dc, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	return &ClientsetClient{clientset: cs, dynamic: dc, restCfg: config}, nil
}

// NewClientsetClientWithKubeconfig creates a new ClientsetClient using an
// explicit kubeconfig path (for integration tests against external clusters).
func NewClientsetClientWithKubeconfig(kubeconfig string) (*ClientsetClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig from %s: %w", kubeconfig, err)
	}
	config.UserAgent = operatorUserAgent
	TuneClientRates(config)

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	dc, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	return &ClientsetClient{clientset: cs, dynamic: dc, restCfg: config}, nil
}

// RESTConfig returns the underlying rest.Config used to connect to the cluster.
func (c *ClientsetClient) RESTConfig() *rest.Config { return c.restCfg }

// TuneClientRates raises the client-side request ceiling off client-go's
// defaults of QPS 5 / Burst 10.
//
// Those defaults are a poor fit here. client-go builds one token bucket per
// REST client, and every CRD read and write in the operator goes through
// crdstore onto this config's single dynamic client — so all of them share one
// 5 QPS bucket, alongside whatever the manager built from the same config.
// Bulk passes starve everything behind them: the update-center catalog arm
// alone issues a few hundred CRD calls per sync, which is enough to let a slow
// source block ComposedBundle reconciliation for minutes.
//
// controller-runtime's own GetConfig sets QPS to -1, disabling client-side
// limiting entirely and deferring to API Priority and Fairness. We keep a
// finite ceiling instead: APF is not guaranteed to be configured on every
// cluster Varroa runs against, and a finite bucket still puts a runaway
// reconcile loop under backpressure on the client rather than at the API
// server. An explicitly configured QPS is never overridden.
func TuneClientRates(config *rest.Config) {
	if config == nil || config.QPS != 0 {
		return
	}
	config.QPS = 50
	config.Burst = 100
}

// Clientset returns the underlying kubernetes clientset interface.
func (c *ClientsetClient) Clientset() kubernetes.Interface { return c.clientset }

// DynamicClient returns the underlying dynamic Kubernetes client.
func (c *ClientsetClient) DynamicClient() dynamic.Interface {
	return c.dynamic
}

// ServerVersion returns the Kubernetes apiserver version (GitVersion).
func (c *ClientsetClient) ServerVersion(_ context.Context) (string, error) {
	v, err := c.clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// IsStatefulSetReady checks whether a StatefulSet has at least one ready replica.
func (c *ClientsetClient) IsStatefulSetReady(ctx context.Context, name, namespace string) (bool, error) {
	sts, err := c.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return sts.Status.ReadyReplicas >= 1, nil
}

// ScaleStatefulSet sets a StatefulSet's replica count via a merge patch.
// It is idempotent: a no-op when the count already matches, and not an error
// when the StatefulSet does not exist (e.g. a controller stopped before it was
// ever provisioned).
func (c *ClientsetClient) ScaleStatefulSet(ctx context.Context, name, namespace string, replicas int32) error {
	sts, err := c.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == replicas {
		return nil
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err = c.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// GetStatefulSetPluginsChecksum reads the plugins-checksum annotation from the
// live StatefulSet's pod template, or returns "" if the StatefulSet does not exist.
func (c *ClientsetClient) GetStatefulSetPluginsChecksum(ctx context.Context, name, namespace string) (string, error) {
	sts, err := c.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if sts.Spec.Template.Annotations != nil {
		return sts.Spec.Template.Annotations["varroa.dev/plugins-checksum"], nil
	}
	return "", nil
}

// GetStatefulSetImages returns the varroa.dev/computed-images stamp (nil when
// absent/unparseable) and the live container images (containers + initContainers,
// by name) of the controller StatefulSet. A missing StatefulSet returns
// (nil, nil, nil) — callers treat nil live as "nothing to roll".
func (c *ClientsetClient) GetStatefulSetImages(ctx context.Context, name, namespace string) (map[string]string, map[string]string, error) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	sts, err := c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return parseComputedImagesAnnotation(sts), stsImagesByName(sts), nil
}

// GetStatefulSetContainerSpecs returns the mite container's full independently-
// editable spec surface and the Jenkins container's live resources — the
// varroa.dev/computed-images stamp's mite entry, the live mite image, the
// live mite and Jenkins resource requirements (requests+limits), the
// mite image pull policy, and the varroa.dev/resources-source stamp
// (per-container "spec"/"class"/"none" map) — in a single StatefulSet
// read. The resources-source stamp distinguishes "desired is nil because
// spec block was removed → must roll" from "desired is nil because
// resources were never user-set → skip (provision-gated, D-6)". A missing
// StatefulSet returns all values zero, found=false, err=nil — callers treat
// found=false as "nothing to roll".
func (c *ClientsetClient) GetStatefulSetContainerSpecs(ctx context.Context, name, namespace string) (
	computedMiteImage, liveMiteImage string,
	miteResources, jenkinsResources *corev1.ResourceRequirements,
	mitePullPolicy string,
	resourcesSource map[string]string,
	found bool, err error,
) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	sts, getErr := c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return "", "", nil, nil, "", nil, false, nil
		}
		return "", "", nil, nil, "", nil, false, getErr
	}
	if computed := parseComputedImagesAnnotation(sts); computed != nil {
		computedMiteImage = computed[overlay.MiteContainerName]
	}
	liveMiteImage = stsImagesByName(sts)[overlay.MiteContainerName]
	miteResources = stsContainerResources(sts, overlay.MiteContainerName)
	jenkinsResources = stsContainerResources(sts, overlay.JenkinsContainerName)
	mitePullPolicy = stsPullPoliciesByName(sts)[overlay.MiteContainerName]
	resourcesSource = parseResourcesSourceAnnotation(sts)
	return computedMiteImage, liveMiteImage, miteResources, jenkinsResources, mitePullPolicy, resourcesSource, true, nil
}

// CreatePVC creates a PersistentVolumeClaim.
func (c *ClientsetClient) CreatePVC(ctx context.Context, name, namespace, storageClass string, sizeGiB int) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", sizeGiB)),
				},
			},
			StorageClassName: &storageClass,
		},
	}
	_, err := c.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		slog.Warn("resource already exists during create, treating as idempotent retry",
			"kind", "PersistentVolumeClaim", "name", name, "namespace", namespace)
		return nil
	}
	return err
}

// CreateService creates or updates a ClusterIP Service, optionally applying a
// strategic-merge overlayYAML first. The Service carries both the Jenkins HTTP
// port and the fixed inbound agent port: the jenkins/jenkins image pins
// the TCP agent listener at 50000 (JENKINS_SLAVE_AGENT_PORT image env) and the
// StatefulSet declares the matching containerPort, so kubernetes-plugin agents
// in TCP inbound mode dial <svc>:50000. Multi-port Services require every port
// to be named.
func (c *ClientsetClient) CreateService(ctx context.Context, name, namespace string, port int32, overlayYAML string) error {
	appName := strings.TrimSuffix(name, "-svc")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       port,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(8080),
			}, {
				Name:       "agent",
				Port:       50000,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(50000),
			}},
			Selector: map[string]string{"app": appName},
		},
	}

	// Apply overlay via unstructured round-trip.
	if overlayYAML != "" {
		obj, err := toUnstructured(svc)
		if err != nil {
			return fmt.Errorf("convert service to unstructured: %w", err)
		}
		merged, err := overlay.Merge(obj, []byte(overlayYAML), corev1.Service{})
		if err != nil {
			return fmt.Errorf("apply service overlay: %w", err)
		}
		data, err := json.Marshal(merged.Object)
		if err != nil {
			return fmt.Errorf("marshal merged service: %w", err)
		}
		if err := json.Unmarshal(data, svc); err != nil {
			return fmt.Errorf("unmarshal merged service: %w", err)
		}
	}

	services := c.clientset.CoreV1().Services(namespace)
	_, err := services.Create(ctx, svc, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}

	// Already exists: update the live object so spec changes converge
	// (mirroring CreateIngress). Mutate the fetched object to preserve
	// resourceVersion, ownerReferences, labels, and the immutable ClusterIP
	// family fields the API server allocated.
	existing, getErr := services.Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get existing service for update: %w", getErr)
	}
	// Skip the write when the owned fields already match: this path runs on
	// every Running/Connected tick fleet-wide, and a converged Service must
	// not generate steady-state update traffic.
	annotationsConverged := true
	for k, v := range svc.Annotations {
		if existing.Annotations[k] != v {
			annotationsConverged = false
			break
		}
	}
	if annotationsConverged &&
		existing.Spec.Type == svc.Spec.Type &&
		reflect.DeepEqual(existing.Spec.Ports, svc.Spec.Ports) &&
		reflect.DeepEqual(existing.Spec.Selector, svc.Spec.Selector) {
		return nil
	}

	desired := svc.Spec
	desired.ClusterIP = existing.Spec.ClusterIP
	desired.ClusterIPs = existing.Spec.ClusterIPs
	desired.IPFamilies = existing.Spec.IPFamilies
	desired.IPFamilyPolicy = existing.Spec.IPFamilyPolicy
	existing.Spec = desired
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range svc.Annotations {
		existing.Annotations[k] = v
	}
	if _, err := services.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	return nil
}

// CreateServiceAccount creates a ServiceAccount.
func (c *ClientsetClient) CreateServiceAccount(ctx context.Context, name, namespace string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	_, err := c.clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		slog.Warn("resource already exists during create, treating as idempotent retry",
			"kind", "ServiceAccount", "name", name, "namespace", namespace)
		return nil
	}
	return err
}

// CreateAgentRBAC creates a Role and RoleBinding granting the agent
// ServiceAccount permission to manage pods in its namespace.
func (c *ClientsetClient) CreateAgentRBAC(ctx context.Context, name, namespace string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods", "pods/log", "pods/exec"},
			Verbs:     []string{"get", "list", "watch", "create", "delete", "patch", "update"},
		}},
	}
	_, err := c.clientset.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create role %s: %w", name, err)
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}},
	}
	_, err = c.clientset.RbacV1().RoleBindings(namespace).Create(ctx, rb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create rolebinding %s: %w", name, err)
	}
	return nil
}

// CreateJenkinsCR creates a Jenkins custom resource via the dynamic client.
func (c *ClientsetClient) CreateJenkinsCR(ctx context.Context, name, namespace, configMapName, imageVersion string, plugins []string, backupEnabled bool) error {
	controllerName := strings.TrimSuffix(name, "-jenkins")
	cr := &v1alpha1.Controller{
		Spec: v1alpha1.ControllerSpec{Version: imageVersion},
	}
	cr.Name = controllerName
	cr.Namespace = namespace
	if backupEnabled {
		cr.Spec.BackupSpec = &v1alpha1.BackupSpec{Enabled: true}
	}
	if len(plugins) > 0 {
		cr.Spec.PluginSpec = &v1alpha1.PluginSpec{}
		for _, p := range plugins {
			parts := strings.SplitN(p, ":", 2)
			entry := v1alpha1.PluginEntry{ArtifactId: parts[0]}
			if len(parts) > 1 {
				entry.Version = parts[1]
			}
			cr.Spec.PluginSpec.Entries = append(cr.Spec.PluginSpec.Entries, entry)
		}
	}
	jc, err := GenerateJenkinsCR(cr, configMapName)
	if err != nil {
		return fmt.Errorf("generate jenkins CR: %w", err)
	}
	// Use the caller-supplied name (already UID-based) rather than the name
	// derived from the synthetic CR, which has no UID.
	jc.Metadata.Name = name
	obj, err := toUnstructured(jc)
	if err != nil {
		return fmt.Errorf("convert jenkins CR to unstructured: %w", err)
	}
	_, err = c.dynamic.Resource(jenkinsGVR).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		slog.Warn("resource already exists during create, treating as idempotent retry",
			"kind", "Jenkins", "name", name, "namespace", namespace)
		return nil
	}
	return err
}

// CreateIngress reconciles a networking.k8s.io/v1 Ingress to the desired spec,
// optionally applying a strategic-merge overlayYAML first. It creates the
// Ingress when absent and updates it when present, so post-provisioning changes
// (TLS secret, annotations, host) converge instead of going stale. On
// update the live object is mutated in place, preserving its resourceVersion,
// ownerReferences, and labels; desired annotations are merged onto any added by
// external controllers (cert-manager, ingress-nginx).
func (c *ClientsetClient) CreateIngress(ctx context.Context, name, namespace, host, pathPrefix, tlsSecret, ingressClass string, annotations map[string]string, overlayYAML string) error {
	controllerName := strings.TrimSuffix(name, "-ingress")
	pathType := networkingv1.PathTypePrefix
	path := "/"
	if pathPrefix != "" {
		path = pathPrefix
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     path,
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: controllerName + "-svc",
									Port: networkingv1.ServiceBackendPort{Number: 8080},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if tlsSecret != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{host},
			SecretName: tlsSecret,
		}}
	}

	// Apply overlay via unstructured round-trip.
	if overlayYAML != "" {
		obj, err := toUnstructured(ing)
		if err != nil {
			return fmt.Errorf("convert ingress to unstructured: %w", err)
		}
		merged, err := overlay.Merge(obj, []byte(overlayYAML), &networkingv1.Ingress{})
		if err != nil {
			return fmt.Errorf("apply ingress overlay: %w", err)
		}
		data, err := json.Marshal(merged.Object)
		if err != nil {
			return fmt.Errorf("marshal merged ingress: %w", err)
		}
		if err := json.Unmarshal(data, ing); err != nil {
			return fmt.Errorf("unmarshal merged ingress: %w", err)
		}
	}

	ingresses := c.clientset.NetworkingV1().Ingresses(namespace)
	_, err := ingresses.Create(ctx, ing, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}

	// Already exists: update the live object so spec changes converge. Mutate
	// the fetched object to preserve resourceVersion, ownerReferences, labels,
	// and any annotations set by external controllers.
	existing, getErr := ingresses.Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get existing ingress for update: %w", getErr)
	}
	existing.Spec = ing.Spec
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range ing.Annotations {
		existing.Annotations[k] = v
	}
	if _, err := ingresses.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ingress: %w", err)
	}
	return nil
}

// CreateSecret creates an opaque Secret with the given labels.
func (c *ClientsetClient) CreateSecret(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       data,
	}
	_, err := c.clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		slog.Warn("resource already exists during create, treating as idempotent retry",
			"kind", "Secret", "name", name, "namespace", namespace)
		return nil
	}
	return err
}

// ListSecrets returns the Data of Secrets matching the label selector.
func (c *ClientsetClient) ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error) {
	secrets, err := c.clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	var result []map[string][]byte
	for i := range secrets.Items {
		data := secrets.Items[i].Data
		if data == nil {
			data = map[string][]byte{}
		}
		// Include the Secret name as a synthetic key so callers can identify
		// which key each record belongs to.
		data["_name"] = []byte(secrets.Items[i].Name)
		result = append(result, data)
	}
	return result, nil
}

// GetSecret reads a Secret and returns its Data field.
func (c *ClientsetClient) GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error) {
	secret, err := c.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return secret.Data, nil
}

// GetSecretAnnotations reads a Secret and returns its Annotations field.
func (c *ClientsetClient) GetSecretAnnotations(ctx context.Context, name, namespace string) (map[string]string, error) {
	secret, err := c.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return secret.Annotations, nil
}

// CreateOrUpdateSecret creates a Secret, or updates it if it already exists.
// This is used for bootstrap token rotation — the operator regenerates tokens
// on restart and must write them to existing Secrets.
func (c *ClientsetClient) CreateOrUpdateSecret(ctx context.Context, name, namespace string, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
	_, err := c.clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, getErr := c.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get existing secret for update: %w", getErr)
	}
	secret.ResourceVersion = existing.ResourceVersion
	_, err = c.clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

// CreateSecretExclusive creates an opaque Secret with the given labels and,
// unlike CreateSecret, surfaces an AlreadyExists error instead of treating it
// as an idempotent no-op. API-key generation relies on this to detect prefix
// collisions and retry with a fresh prefix.
func (c *ClientsetClient) CreateSecretExclusive(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       data,
	}
	_, err := c.clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	return err
}

// PatchSecretData applies a strategic-merge patch that updates only the given
// data keys, leaving all other data, labels, and metadata intact. Used for the
// throttled API-key lastUsed write so it does not clobber the key's labels.
func (c *ClientsetClient) PatchSecretData(ctx context.Context, name, namespace string, data map[string][]byte) error {
	// json.Marshal base64-encodes []byte values, matching the Secret wire format.
	patch, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("marshal secret patch: %w", err)
	}
	_, err = c.clientset.CoreV1().Secrets(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// CopyImagePullSecret copies a docker-registry-type Secret from one namespace
// to another, preserving Type and Data. An equality guard skips the write when
// the destination is already converged, so repeated calls (which run on every
// Team's every 30s tick) do not bump resourceVersion unnecessarily.
func (c *ClientsetClient) CopyImagePullSecret(ctx context.Context, srcNamespace, dstNamespace, name string) error {
	if srcNamespace == dstNamespace {
		return nil
	}
	src, err := c.clientset.CoreV1().Secrets(srcNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // admin hasn't seeded the source secret yet
		}
		return err
	}
	secrets := c.clientset.CoreV1().Secrets(dstNamespace)
	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		// Skip the write when already converged (same rationale as CreateService
		// above): this runs on every Team's every 30s tick, and must not bump
		// resourceVersion on a namespace that's already in sync.
		if existing.Type == src.Type && reflect.DeepEqual(existing.Data, src.Data) {
			return nil
		}
		existing.Type = src.Type
		existing.Data = src.Data
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dstNamespace},
		Type:       src.Type,
		Data:       src.Data,
	}
	_, err = secrets.Create(ctx, dst, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil // create/read race — next tick converges via Get-then-compare
	}
	return err
}

// DeleteSecret deletes a Secret.
func (c *ClientsetClient) DeleteSecret(ctx context.Context, name, namespace string) error {
	err := c.clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// buildStatefulSet constructs the desired StatefulSet object for a spec.
// Split from CreateStatefulSet so tests can assert the rendered pod template
// (container env, prefixes) without a live API roundtrip.
func buildStatefulSet(spec StatefulSetSpec) *unstructured.Unstructured {
	replicas := int32(1)
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}
	sts := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "StatefulSet",
			"metadata": map[string]interface{}{
				"name":      spec.Name,
				"namespace": spec.Namespace,
				"labels": map[string]interface{}{
					"app": spec.Name,
				},
			},
			"spec": map[string]interface{}{
				"serviceName": spec.Name + "-svc",
				"replicas":    replicas,
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app": spec.Name,
					},
				},
				"template": map[string]interface{}{
					"metadata": func() map[string]interface{} {
						m := map[string]interface{}{
							"labels": map[string]interface{}{
								"app":                          spec.Name,
								"app.kubernetes.io/managed-by": "varroa-operator",
							},
						}
						if spec.PluginsChecksum != "" {
							m["annotations"] = map[string]interface{}{
								"varroa.dev/plugins-checksum": spec.PluginsChecksum,
							}
						}
						return m
					}(),
					"spec": map[string]interface{}{
						"serviceAccountName": spec.ServiceAccountName,
						"securityContext": map[string]interface{}{
							"fsGroup":        int64(1000),
							"runAsNonRoot":   true,
							"runAsUser":      int64(1000),
							"runAsGroup":     int64(1000),
							"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
						},
						"initContainers": func() []map[string]interface{} {
							pluginsInit := map[string]interface{}{
								"name":    "plugins-init",
								"image":   spec.JenkinsImage,
								"command": []string{"sh", "-c"},
								"args":    []string{pluginsInitScript},
								"securityContext": map[string]interface{}{
									"runAsUser":                int64(1000),
									"allowPrivilegeEscalation": false,
									"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
								},
								"volumeMounts": []map[string]interface{}{
									{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
									{"name": "plugins", "mountPath": "/var/run/varroa/plugins"},
								},
							}
							if spec.PluginUpdateCenterURL != "" || spec.PluginUpdateCenterDownloadURL != "" {
								var env []map[string]interface{}
								if spec.PluginUpdateCenterURL != "" {
									env = append(env, map[string]interface{}{"name": "JENKINS_UC", "value": spec.PluginUpdateCenterURL})
								}
								if spec.PluginUpdateCenterDownloadURL != "" {
									env = append(env, map[string]interface{}{"name": "JENKINS_UC_DOWNLOAD_URL", "value": spec.PluginUpdateCenterDownloadURL})
								}
								pluginsInit["env"] = env
							}
							initContainers := []map[string]interface{}{pluginsInit, {
								"name":            "init-groovy",
								"image":           spec.MiteImage,
								"imagePullPolicy": spec.MiteImagePullPolicy,
								"command":         []string{"sh", "-c"},
								"args":            []string{"mkdir -p /var/jenkins_home/init.groovy.d /var/jenkins_home/plugins && cp /var/run/varroa/init/init.groovy /var/jenkins_home/init.groovy.d/init.groovy && cp /var/run/varroa/init/varroa-mite-auth.groovy /var/jenkins_home/init.groovy.d/varroa-mite-auth.groovy && cp /opt/varroa/varroa-mite-auth.hpi /var/jenkins_home/plugins/varroa-mite-auth.jpi"},
								"securityContext": map[string]interface{}{
									"runAsUser":                int64(1000),
									"allowPrivilegeEscalation": false,
									"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
								},
								"volumeMounts": []map[string]interface{}{
									{"name": "init-scripts", "mountPath": "/var/run/varroa/init"},
									{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
								},
							}}
							if spec.CascConfigMap != "" {
								initContainers = append(initContainers, map[string]interface{}{
									"name":            "casc-seed",
									"image":           spec.MiteImage,
									"imagePullPolicy": spec.MiteImagePullPolicy,
									"command":         []string{"sh", "-c"},
									"args":            []string{"cp /var/run/varroa/casc/* /var/jenkins_home/casc/"},
									"securityContext": map[string]interface{}{
										"runAsUser": int64(1000), "allowPrivilegeEscalation": false, "capabilities": map[string]interface{}{"drop": []interface{}{"ALL"}},
									},
									"volumeMounts": []map[string]interface{}{
										{"name": "casc-config", "mountPath": "/var/run/varroa/casc"},
										{"name": "casc-bundle", "mountPath": "/var/jenkins_home/casc"},
									},
								})
							}
							return initContainers
						}(),
						"containers": []map[string]interface{}{
							func() map[string]interface{} {
								container := map[string]interface{}{
									"name": overlay.JenkinsContainerName,
									"securityContext": map[string]interface{}{
										"allowPrivilegeEscalation": false,
										"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
									},
									"image": spec.JenkinsImage,
									"ports": []map[string]interface{}{
										{"name": "http", "containerPort": 8080},
										{"name": "agent", "containerPort": 50000},
									},
									"volumeMounts": []map[string]interface{}{
										{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
										{"name": "init-scripts", "mountPath": "/var/run/varroa/init"},
										{"name": "bootstrap", "mountPath": "/var/run/varroa/bootstrap"},
										{"name": "casc-bundle", "mountPath": "/var/jenkins_home/casc"},
										{"name": "varroa-run", "mountPath": "/var/run/varroa/run"},
									},
									"lifecycle": map[string]interface{}{
										"preStop": map[string]interface{}{
											"exec": map[string]interface{}{
												"command": []string{"/bin/sh", "-c",
													fmt.Sprintf("deadline=$(( $(date +%%s) + %d )); "+
														"while [ ! -f /var/run/varroa/run/drain.done ] && [ $(date +%%s) -lt $deadline ]; "+
														"do sleep 1; done", spec.DrainTimeoutSec+15)},
											},
										},
									},
									"env": func() []map[string]interface{} {
										javaOptsValue := "-Djenkins.install.runSetupWizard=false -Djenkins.security.SystemReadPermission=true"
										if spec.PodOverrides != nil && spec.PodOverrides.JvmOpts != "" {
											javaOptsValue += " " + spec.PodOverrides.JvmOpts
										}
										jenkinsEnv := []map[string]interface{}{
											// SystemReadPermission is opt-in: without it the SYSTEM_READ permission
											// is disabled and the varroa:system-mite SystemRead grant is inert, so the
											// mite's drift-baseline /configuration-as-code/export 403s.
											{"name": "JAVA_OPTS", "value": javaOptsValue},
											{"name": "VARROA_OIDC_ISSUER", "value": spec.OIDCIssuer},
											{"name": "VARROA_LOGIN_URL", "value": spec.VarroaLoginURL},
											{"name": "VARROA_OIDC_USER_CLAIM", "value": spec.OIDCUserClaim},
											{"name": "VARROA_OIDC_GROUP_CLAIM", "value": spec.OIDCGroupClaim},
											{"name": "VARROA_MITE_PUBKEY_PEM", "value": spec.MitePubKeyPEM},
											{"name": "VARROA_MITE_PUBKEY_KID", "value": spec.MitePubKeyKID},
											{"name": "VARROA_MITE_AUD", "value": spec.Namespace + "/" + spec.ControllerName},
											{"name": "VARROA_APIKEY_VERIFY_URL", "value": spec.ApikeyVerifyURL},
											{"name": "VARROA_CA_PEM", "value": spec.CAPEM},
											{"name": "CASC_JENKINS_CONFIG", "value": "/var/jenkins_home/casc"},
										}
										if spec.VarroaBaseURL != "" {
											bannerURL := spec.VarroaBaseURL + "?controller=" + spec.ControllerName +
												"&namespace=" + spec.Namespace +
												"&back=" + spec.VarroaBaseURL + "/controllers/" + spec.ClusterName + "/" + spec.Namespace + "/" + spec.ControllerName
											jenkinsEnv = append(jenkinsEnv, map[string]interface{}{
												"name": "VARROA_BANNER_URL", "value": bannerURL,
											})
										}
										if spec.PathPrefix != "" {
											jenkinsEnv = append(jenkinsEnv, map[string]interface{}{
												"name": "JENKINS_OPTS", "value": "--prefix=" + spec.PathPrefix,
											})
										}
										if spec.HibernationIgnoreRegex != "" {
											jenkinsEnv = append(jenkinsEnv, map[string]interface{}{
												"name": "VARROA_HIBERNATION_IGNORE_REGEX", "value": spec.HibernationIgnoreRegex,
											})
										}
										return jenkinsEnv
									}(),
									"resources": resourceRequirementsMap(spec.Resources),
								}
								if probe := renderProbe(probeStartup, probeSpecFor(spec.Probes, probeStartup), spec.PathPrefix); probe != nil {
									container["startupProbe"] = probe
								}
								if probe := renderProbe(probeReadiness, probeSpecFor(spec.Probes, probeReadiness), spec.PathPrefix); probe != nil {
									container["readinessProbe"] = probe
								}
								if probe := renderProbe(probeLiveness, probeSpecFor(spec.Probes, probeLiveness), spec.PathPrefix); probe != nil {
									container["livenessProbe"] = probe
								}
								return container
							}(),
							{
								"name": overlay.MiteContainerName,
								"securityContext": map[string]interface{}{
									"allowPrivilegeEscalation": false,
									"readOnlyRootFilesystem":   true,
									"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
								},
								"image":           spec.MiteImage,
								"imagePullPolicy": spec.MiteImagePullPolicy,
								"resources":       resourceRequirementsMap(spec.MiteResources),
								"command":         []string{"/app/varroa-mite"},
								"env": []map[string]interface{}{
									{"name": "VARROA_ENDPOINT", "value": spec.VarroaEndpoint},
									{"name": "JENKINS_URL", "value": "http://localhost:8080" + spec.PathPrefix},
									{"name": "CONTROLLER_NAME", "value": spec.ControllerName},
									{"name": "NAMESPACE", "value": spec.Namespace},
									{"name": "VARROA_CA_PEM", "value": spec.CAPEM},
								},
								"volumeMounts": []map[string]interface{}{
									{"name": "bootstrap", "mountPath": "/var/run/varroa/bootstrap"},
									{"name": "jenkins-home", "mountPath": "/var/jenkins_home"},
									{"name": "casc-bundle", "mountPath": "/var/jenkins_home/casc"},
									{"name": "varroa-run", "mountPath": "/var/run/varroa/run"},
								},
							},
						},
						"volumes": func() []map[string]interface{} {
							vols := []map[string]interface{}{
								{
									"name": "init-scripts",
									"configMap": map[string]interface{}{
										"name": spec.InitConfigMap,
									},
								},
								{
									"name": "bootstrap",
									"secret": map[string]interface{}{
										"secretName": spec.BootstrapSecret,
									},
								},
								{
									"name": "plugins",
									"configMap": map[string]interface{}{
										"name": spec.PluginsConfigMap,
									},
								},
								{
									"name":     "casc-bundle",
									"emptyDir": map[string]interface{}{},
								},
								{
									"name":     "varroa-run",
									"emptyDir": map[string]interface{}{},
								},
							}
							if spec.CascConfigMap != "" {
								vols = append(vols, map[string]interface{}{
									"name": "casc-config",
									"configMap": map[string]interface{}{
										"name": spec.CascConfigMap,
									},
								})
							}
							return vols
						}(),
					},
				},
				"volumeClaimTemplates": []map[string]interface{}{
					{
						"metadata": map[string]interface{}{
							"name": "jenkins-home",
						},
						"spec": map[string]interface{}{
							"accessModes": []string{"ReadWriteOnce"},
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"storage": spec.StorageSize,
								},
							},
						},
					},
				},
			},
		},
	}
	if spec.StorageClass != "" {
		templates := sts.Object["spec"].(map[string]interface{})["volumeClaimTemplates"].([]map[string]interface{})
		templates[0]["spec"].(map[string]interface{})["storageClassName"] = spec.StorageClass
	}
	if len(spec.ImagePullSecrets) > 0 {
		podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
		secrets := make([]map[string]interface{}, 0, len(spec.ImagePullSecrets))
		for _, s := range spec.ImagePullSecrets {
			secrets = append(secrets, map[string]interface{}{"name": s})
		}
		podSpec["imagePullSecrets"] = secrets
	}
	if spec.TerminationGracePeriodSec > 0 {
		podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
		podSpec["terminationGracePeriodSeconds"] = spec.TerminationGracePeriodSec
	}
	return sts
}

// ---------------------------------------------------------------------------
// Image-stamp helpers
// ---------------------------------------------------------------------------

// stsImagesByName walks spec.template.spec.containers and initContainers and
// returns {containerName: image} for entries with both fields. A flat map is
// safe: pod validation requires container names to be unique across both lists.
func stsImagesByName(sts *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	for _, path := range [][]string{{"spec", "template", "spec", "containers"}, {"spec", "template", "spec", "initContainers"}} {
		list, _, _ := unstructured.NestedSlice(sts.Object, path...)
		for _, c := range list {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := cm["name"].(string)
			img, _ := cm["image"].(string)
			if name != "" && img != "" {
				out[name] = img
			}
		}
	}
	return out
}

// stsPullPoliciesByName is the same walk returning {containerName: imagePullPolicy}.
func stsPullPoliciesByName(sts *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	for _, path := range [][]string{{"spec", "template", "spec", "containers"}, {"spec", "template", "spec", "initContainers"}} {
		list, _, _ := unstructured.NestedSlice(sts.Object, path...)
		for _, c := range list {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := cm["name"].(string)
			policy, _ := cm["imagePullPolicy"].(string)
			if name != "" && policy != "" {
				out[name] = policy
			}
		}
	}
	return out
}

// resourceRequirementsMap renders a *corev1.ResourceRequirements into the
// map baked onto a container's "resources" key. Both "requests" and "limits"
// sub-maps are rendered when non-empty. A nil rr or one with both sub-maps
// empty yields an empty map (map[string]interface{}{}), matching the
// pre-existing zero-value behavior so an unset resources field never reads as
// drift against history.
func resourceRequirementsMap(rr *corev1.ResourceRequirements) map[string]interface{} {
	out := map[string]interface{}{}
	if rr == nil {
		return out
	}
	if m := quantityListMap(rr.Requests); len(m) > 0 {
		out["requests"] = m
	}
	if m := quantityListMap(rr.Limits); len(m) > 0 {
		out["limits"] = m
	}
	return out
}

// quantityListMap renders every entry of a corev1.ResourceList into its
// string form for embedding in the unstructured container map. Dropping keys
// here (e.g. ephemeral-storage, hugepages, extended resources) livelocks the
// controller: the drift comparator sees the desired key missing from the live
// StatefulSet on every Running tick and rolls forever.
func quantityListMap(list corev1.ResourceList) map[string]interface{} {
	m := map[string]interface{}{}
	for name, qty := range list {
		m[string(name)] = qty.String()
	}
	return m
}

// stsContainerResources reads the live container's resources.requests and
// resources.limits from a StatefulSet (either the unstructured object just
// built by buildStatefulSet, pre-Create/Update, or one fetched live from the
// API). Both containers and initContainers are searched. Returns nil when
// the container was not found, the container has no resources key, or every
// cpu/memory entry under requests/limits is missing or unparseable.
func stsContainerResources(sts *unstructured.Unstructured, containerName string) *corev1.ResourceRequirements {
	for _, path := range [][]string{{"spec", "template", "spec", "containers"}, {"spec", "template", "spec", "initContainers"}} {
		list, _, _ := unstructured.NestedSlice(sts.Object, path...)
		for _, c := range list {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _ := cm["name"].(string); name != containerName {
				continue
			}
			return parseResourcesFromContainerMap(cm)
		}
	}
	return nil
}

// parseResourcesFromContainerMap reads resources.requests.* and
// resources.limits.* from an unstructured container map, iterating ALL keys
// actually present in the live map (not a hardcoded cpu/memory list — any
// resource key legal in corev1.ResourceRequirements, including
// ephemeral-storage, hugepages-*, and extended/GPU resources, must survive
// the parse so they participate in the drift comparison). Each value is
// parsed into a resource.Quantity; returns nil (not an empty non-nil
// struct) when nothing was found. Both missing AND unparseable entries are
// omitted from the resulting ResourceList — do NOT special-case unparseable
// values as "equal" or "skip the comparison." Omission is a deliberate
// encoding: when a key is omitted from the live map, resourceListsEqual
// sees that key present on the desired side but absent on the live side,
// which trips its okA != okB branch and reports a delta — exactly
// reproducing the old resourceListsEqual's "return false" on parse failure
// (invariant I4: parse failure on either side is always a delta, never
// guessed equal).
func parseResourcesFromContainerMap(cm map[string]interface{}) *corev1.ResourceRequirements {
	var requests, limits corev1.ResourceList
	if resMap, ok, _ := unstructured.NestedMap(cm, "resources", "requests"); ok {
		for k, v := range resMap {
			str, ok := v.(string)
			if !ok || str == "" {
				continue
			}
			q, err := resource.ParseQuantity(str)
			if err != nil {
				continue
			}
			if requests == nil {
				requests = corev1.ResourceList{}
			}
			requests[corev1.ResourceName(k)] = q
		}
	}
	if resMap, ok, _ := unstructured.NestedMap(cm, "resources", "limits"); ok {
		for k, v := range resMap {
			str, ok := v.(string)
			if !ok || str == "" {
				continue
			}
			q, err := resource.ParseQuantity(str)
			if err != nil {
				continue
			}
			if limits == nil {
				limits = corev1.ResourceList{}
			}
			limits[corev1.ResourceName(k)] = q
		}
	}
	if requests == nil && limits == nil {
		return nil
	}
	return &corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

// parseComputedImagesAnnotation reads metadata.annotations[computedImagesAnnotation]
// and json.Unmarshals it; returns nil on absent key or unmarshal error (treat-as-absent adoption).
func parseComputedImagesAnnotation(sts *unstructured.Unstructured) map[string]string {
	anns, _, _ := unstructured.NestedStringMap(sts.Object, "metadata", "annotations")
	raw, ok := anns[computedImagesAnnotation]
	if !ok || raw == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// parseResourcesSourceAnnotation reads metadata.annotations[resourcesSourceAnnotation]
// and json.Unmarshals it; returns nil on absent/unparseable (treat-as-missing,
// which reconcileContainerSpecRoll handles the same as "pre-epic"/first-tick).
func parseResourcesSourceAnnotation(sts *unstructured.Unstructured) map[string]string {
	anns, _, _ := unstructured.NestedStringMap(sts.Object, "metadata", "annotations")
	raw, ok := anns[resourcesSourceAnnotation]
	if !ok || raw == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// CreateStatefulSet creates a StatefulSet via the dynamic client.
//
// Ordering:
//  1. Build the base StatefulSet via buildStatefulSet (includes mite sidecar,
//     OIDC env, init containers, volumes).
//  2. Apply compiled PodOverrides patch (typed struct → strategic-merge YAML).
//  3. Apply raw ResourceOverlay.StatefulSet YAML (overlay overrides podOverrides).
//  4. Existing Create / AlreadyExists path with ownership-stamped image updates
//     (varroa.dev/computed-images): a live image is preserved only when it
//     differs from the previous stamp (an out-of-band edit was made directly on
//     the StatefulSet) AND the operator's own newly-computed value for that
//     container is unchanged from the previous stamp (no new spec/overlay
//     intent). If the operator's desired value changed, the new intent always
//     wins over an old out-of-band edit — otherwise a one-time manual hotfix
//     (e.g. a direct mite-image STS patch) permanently wedges every future
//     spec-driven roll for that container, because the "preserved" branch keeps
//     re-adopting the stale live image forever — a manual mite-image patch must
//     not survive a subsequent spec.miteSpec.image edit. The
//     annotation records the operator's own computed desired-value baseline
//     for this reconcile — written once, before the preservation loop runs —
//     and is never re-derived from what the template ends up holding after
//     preservation decisions are applied. The preservation predicate above
//     depends on the stamp keeping that meaning across ticks: comparing this
//     tick's newly-computed value against the previous stamp only detects
//     "did the operator's own desired computation change" if the stamp is
//     also a desired value, not a possibly-preserved live one. Re-stamping
//     from the post-preservation template instead does not work: it would make
//     the stamp equal the preserved value on the very next tick, so an
//     unchanged desired value would no longer match the stamp and the
//     preserved override would get silently stomped back to desired one
//     reconcile later. Ground truth
//     for "what's actually applied" is available separately via the live
//     container map (GetStatefulSetImages' second return), used only for
//     informational messaging, never for this decision.
//
// UpdateStatefulSetOIDCEnv re-asserts VARROA_OIDC_* env vars LATER on the live
// object, so a user overlay editing OIDC env is warned and then silently
// overwritten — consistent with warn-but-allow.
func (c *ClientsetClient) CreateStatefulSet(ctx context.Context, spec StatefulSetSpec) error {
	sts := buildStatefulSet(spec)

	// Apply compiled PodOverrides (typed → strategic-merge patch).
	if spec.PodOverrides != nil {
		poPatch, err := overlay.CompilePodOverrides(spec.PodOverrides, overlay.JenkinsContainerName)
		if err != nil {
			return fmt.Errorf("compile podOverrides: %w", err)
		}
		sts, err = overlay.Merge(sts, poPatch, appsv1.StatefulSet{})
		if err != nil {
			return fmt.Errorf("apply podOverrides: %w", err)
		}
	}

	// Apply raw ResourceOverlay (statefulSet YAML → strategic-merge patch).
	if spec.ResourceOverlay != nil && spec.ResourceOverlay.StatefulSet != "" {
		var err error
		sts, err = overlay.Merge(sts, []byte(spec.ResourceOverlay.StatefulSet), appsv1.StatefulSet{})
		if err != nil {
			return fmt.Errorf("apply statefulSet overlay: %w", err)
		}
	}

	// Normalize sts.Object to JSON-compatible types. buildStatefulSet assembles
	// container/init lists as []map[string]interface{}, which unstructured
	// helpers (NestedSlice) cannot read and the API deep-copy rejects; an overlay
	// merge round-trips through JSON, but a controller with neither podOverrides
	// nor a resourceOverlay would otherwise reach here un-normalized. The
	// round-trip is idempotent for already-merged objects.
	{
		raw, mErr := json.Marshal(sts.Object)
		if mErr != nil {
			return fmt.Errorf("normalize statefulset object: %w", mErr)
		}
		var norm map[string]interface{}
		if uErr := json.Unmarshal(raw, &norm); uErr != nil {
			return fmt.Errorf("normalize statefulset object: %w", uErr)
		}
		sts.Object = norm
	}

	// Stamp the operator-computed images on the object metadata AFTER all
	// overlay merges, so the operator's value is authoritative even if a user
	// overlay wrote the same annotation key. Object-metadata (not pod-template)
	// placement means a stamp-only change never restarts pods.
	stampNew := stsImagesByName(sts)
	stampJSON, err := json.Marshal(stampNew)
	if err != nil {
		return fmt.Errorf("marshal computed-images stamp: %w", err)
	}
	anns, _, _ := unstructured.NestedStringMap(sts.Object, "metadata", "annotations")
	if anns == nil {
		anns = map[string]string{}
	}
	anns[computedImagesAnnotation] = string(stampJSON)
	// Stamp the resource source so the drift comparator can distinguish
	// "spec block was removed → roll" from "never user-set → skip".
	if spec.ResourcesSource != nil {
		srcJSON, srcErr := json.Marshal(spec.ResourcesSource)
		if srcErr != nil {
			return fmt.Errorf("marshal resources-source stamp: %w", srcErr)
		}
		anns[resourcesSourceAnnotation] = string(srcJSON)
	}
	if err := unstructured.SetNestedStringMap(sts.Object, anns, "metadata", "annotations"); err != nil {
		return fmt.Errorf("set computed-images annotation: %w", err)
	}

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	_, err = c.dynamic.Resource(gvr).Namespace(spec.Namespace).Create(ctx, sts, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Update existing StatefulSet with ownership-stamped image updates
	// (varroa.dev/computed-images): a live container image + pull policy is
	// preserved ONLY when (1) it differs from the previous stamp entry (an
	// out-of-band edit made directly on the StatefulSet) AND (2) the operator's
	// own newly-computed value for that container is unchanged from the
	// previous stamp — i.e. no new spec/overlay intent for this container since
	// the last apply. A changed desired value always overrides a stale
	// out-of-band edit. An absent/unparseable stamp adopts the computed images.
	// Containers are matched by name across both the containers and
	// initContainers lists.
	//
	// Deliberately NOT re-derived from the post-preservation template: the
	// annotation written above (stampNew, before we knew create-vs-update)
	// represents the operator's own desired-value baseline for this
	// reconcile, and must keep that meaning across ticks for the (2) check
	// above to work — comparing this tick's want against last tick's prev
	// only detects "did the operator's own computation change" if prev is
	// also a desired value, not a possibly-preserved live one. Re-stamping
	// from the applied template does not work: it would turn prev into
	// "whatever got applied" on the very next tick, which would no longer
	// equal this tick's want even when desired never moved, so the preserved
	// out-of-band value would get stomped back to the unchanged desired image
	// on the following reconcile — breaking the exact "persist the hotfix
	// while desired hasn't moved" guarantee (1) exists for. The live map
	// already carries ground truth for callers
	// that want it (GetStatefulSetImages' second return, GetStatefulSetMiteResources)
	// — reconcileContainerSpecRoll/reconcileVersionRoll use it only for the
	// informational "preserved (out-of-band override)" note, not to decide
	// whether to roll.
	existing, getErr := c.dynamic.Resource(gvr).Namespace(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get existing statefulset for update: %w", getErr)
	}
	stampOld := parseComputedImagesAnnotation(existing) // nil => adopt
	liveImages := stsImagesByName(existing)
	livePullPolicies := stsPullPoliciesByName(existing)
	for _, path := range [][]string{{"spec", "template", "spec", "containers"}, {"spec", "template", "spec", "initContainers"}} {
		list, _, _ := unstructured.NestedSlice(sts.Object, path...)
		if len(list) == 0 {
			continue
		}
		for _, c := range list {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := cm["name"].(string)
			if name == "" {
				continue
			}
			live, hasLive := liveImages[name]
			prev, hasPrev := stampOld[name]
			want, hasWant := stampNew[name]
			if hasLive && hasPrev && hasWant && live != prev && want == prev {
				cm["image"] = live
				// The mite container's imagePullPolicy is deliberately NOT
				// preserved here alongside its image:
				// spec.miteSpec.imagePullPolicy has its own independent
				// drift check (reconcileContainerSpecRoll via
				// effectiveDesiredMiteImagePullPolicy) with no
				// out-of-band-preservation semantic requested for it (same
				// as resources). Piggybacking pull-policy preservation onto
				// the image predicate would mean that as long as a controller
				// had a preserved out-of-band mite image override, a
				// genuine spec-driven mite pull-policy change would get
				// silently reverted back to the stale live value on every
				// Provisioning pass — an unconvergeable Connected-
				// >Provisioning loop. Every other
				// container (jenkins, plugins-init, init-groovy, ...) has
				// no independent pull-policy drift check anywhere in the
				// reconciler, so preserving their out-of-band pull-policy
				// edits alongside the image remains correct and harmless —
				// nothing will ever try to converge them back.
				if name != overlay.MiteContainerName {
					if p, ok := livePullPolicies[name]; ok {
						cm["imagePullPolicy"] = p
					}
				}
			}
		}
		if err := unstructured.SetNestedSlice(sts.Object, list, path...); err != nil {
			return fmt.Errorf("set %s for update: %w", path[len(path)-1], err)
		}
	}

	// StatefulSet spec fields outside {replicas, ordinals, template,
	// updateStrategy, revisionHistoryLimit, persistentVolumeClaimRetentionPolicy,
	// minReadySeconds} are immutable — any rendered difference (a pre-epic CR
	// whose old storage value was pruned and now renders the default,
	// spec.persistence edited after creation, or a resourceOverlay touching
	// serviceName/selector/podManagementPolicy) makes the API server reject
	// the whole update, wedging Provisioning forever. Preserve the live values
	// verbatim: these fields are creation-time-only; changing them on a live
	// controller requires teardown/recreate (PVC expansion is outside the
	// operator's scope).
	for _, field := range []string{"volumeClaimTemplates", "serviceName", "selector", "podManagementPolicy"} {
		live, found, _ := unstructured.NestedFieldNoCopy(existing.Object, "spec", field)
		if !found {
			unstructured.RemoveNestedField(sts.Object, "spec", field)
			continue
		}
		if err := unstructured.SetNestedField(sts.Object, runtime.DeepCopyJSONValue(live), "spec", field); err != nil {
			return fmt.Errorf("preserve immutable field %s for update: %w", field, err)
		}
	}

	sts.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(gvr).Namespace(spec.Namespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

// UpdateStatefulSetOIDCEnv patches the VARROA_OIDC_ISSUER and VARROA_LOGIN_URL
// env vars on an existing StatefulSet's Jenkins container without rebuilding
// the full StatefulSet spec. Note: changing env vars updates the pod template,
// so the StatefulSet controller will perform a rolling update to apply them.
func (c *ClientsetClient) UpdateStatefulSetOIDCEnv(ctx context.Context, name, namespace, oidcIssuer, loginURL, oidcUserClaim, oidcGroupClaim, pubKeyPEM, pubKeyKID, aud, apikeyVerifyURL, caPEM string) error {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	sts, err := c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get statefulset: %w", err)
	}

	containers, found, err := unstructured.NestedSlice(sts.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return fmt.Errorf("get containers: found=%v err=%v", found, err)
	}

	// Find the Jenkins container by name.
	var jenkins map[string]interface{}
	for _, c := range containers {
		cMap, _ := c.(map[string]interface{})
		if name, _, _ := unstructured.NestedString(cMap, "name"); name == "jenkins" {
			jenkins = cMap
			break
		}
	}
	if jenkins == nil {
		return fmt.Errorf("jenkins container not found in statefulset")
	}

	needUpdate := false
	envs, found, _ := unstructured.NestedSlice(jenkins, "env")
	if !found {
		envs = make([]interface{}, 0)
	}

	// Upsert OIDC env vars.
	issuerFound, loginFound, userClaimFound, groupClaimFound, pubKeyFound, kidFound, audFound := false, false, false, false, false, false, false
	apikeyURLFound, caPEMFound := false, false
	for i, e := range envs {
		env, _ := e.(map[string]interface{})
		name, _, _ := unstructured.NestedString(env, "name")
		switch name {
		case "VARROA_OIDC_ISSUER":
			issuerFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != oidcIssuer {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), oidcIssuer, "value")
				needUpdate = true
			}
		case "VARROA_LOGIN_URL":
			loginFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != loginURL {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), loginURL, "value")
				needUpdate = true
			}
		case "VARROA_OIDC_USER_CLAIM":
			userClaimFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != oidcUserClaim {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), oidcUserClaim, "value")
				needUpdate = true
			}
		case "VARROA_OIDC_GROUP_CLAIM":
			groupClaimFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != oidcGroupClaim {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), oidcGroupClaim, "value")
				needUpdate = true
			}
		case "VARROA_MITE_PUBKEY_PEM":
			pubKeyFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != pubKeyPEM {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), pubKeyPEM, "value")
				needUpdate = true
			}
		case "VARROA_MITE_PUBKEY_KID":
			kidFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != pubKeyKID {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), pubKeyKID, "value")
				needUpdate = true
			}
		case "VARROA_MITE_AUD":
			audFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != aud {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), aud, "value")
				needUpdate = true
			}
		case "VARROA_APIKEY_VERIFY_URL":
			apikeyURLFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != apikeyVerifyURL {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), apikeyVerifyURL, "value")
				needUpdate = true
			}
		case "VARROA_CA_PEM":
			caPEMFound = true
			if old, _, _ := unstructured.NestedString(env, "value"); old != caPEM {
				_ = unstructured.SetNestedField(envs[i].(map[string]interface{}), caPEM, "value")
				needUpdate = true
			}
		}
	}
	if !issuerFound {
		envs = append(envs, map[string]interface{}{"name": "VARROA_OIDC_ISSUER", "value": oidcIssuer})
		needUpdate = true
	}
	if !loginFound {
		envs = append(envs, map[string]interface{}{"name": "VARROA_LOGIN_URL", "value": loginURL})
		needUpdate = true
	}
	if !userClaimFound && oidcUserClaim != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_OIDC_USER_CLAIM", "value": oidcUserClaim})
		needUpdate = true
	}
	if !groupClaimFound && oidcGroupClaim != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_OIDC_GROUP_CLAIM", "value": oidcGroupClaim})
		needUpdate = true
	}
	if !pubKeyFound && pubKeyPEM != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_MITE_PUBKEY_PEM", "value": pubKeyPEM})
		needUpdate = true
	}
	if !kidFound && pubKeyKID != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_MITE_PUBKEY_KID", "value": pubKeyKID})
		needUpdate = true
	}
	if !audFound && aud != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_MITE_AUD", "value": aud})
		needUpdate = true
	}
	if !apikeyURLFound && apikeyVerifyURL != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_APIKEY_VERIFY_URL", "value": apikeyVerifyURL})
		needUpdate = true
	}
	if !caPEMFound && caPEM != "" {
		envs = append(envs, map[string]interface{}{"name": "VARROA_CA_PEM", "value": caPEM})
		needUpdate = true
	}

	if !needUpdate {
		return nil
	}

	_ = unstructured.SetNestedSlice(jenkins, envs, "env")
	_ = unstructured.SetNestedSlice(sts.Object, containers, "spec", "template", "spec", "containers")

	_, err = c.dynamic.Resource(gvr).Namespace(namespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

// EnsureStatefulSetPodLabel adds a pod-template label to an existing StatefulSet.
func (c *ClientsetClient) EnsureStatefulSetPodLabel(ctx context.Context, namespace, stsName, key, value string) (bool, error) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	sts, err := c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, stsName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get statefulset: %w", err)
	}

	labels, found, err := unstructured.NestedStringMap(sts.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return false, fmt.Errorf("get pod template labels: %w", err)
	}
	if found && labels[key] == value {
		return false, nil
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[key] = value
	if err := unstructured.SetNestedStringMap(sts.Object, labels, "spec", "template", "metadata", "labels"); err != nil {
		return false, fmt.Errorf("set pod template labels: %w", err)
	}

	if _, err := c.dynamic.Resource(gvr).Namespace(namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("update statefulset: %w", err)
	}
	return true, nil
}

// CreateOrUpdateConfigMap creates a ConfigMap, or updates it if it already exists.
func (c *ClientsetClient) CreateOrUpdateConfigMap(ctx context.Context, name, namespace string, data map[string]string, owners ...metav1.OwnerReference) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: owners},
		Data:       data,
	}
	_, err := c.clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, getErr := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get existing configmap for update: %w", getErr)
	}
	cm.ResourceVersion = existing.ResourceVersion
	_, err = c.clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// GetConfigMap reads a ConfigMap and returns its Data field.
func (c *ClientsetClient) GetConfigMap(ctx context.Context, name, namespace string) (map[string]string, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	return cm.Data, nil
}

// GetLiveResource fetches a live Kubernetes resource by its GVR, name, and
// namespace. Returns (nil, nil) when the resource does not exist. This is
// used by the BFF preview endpoint to fetch the current live object.
func (c *ClientsetClient) GetLiveResource(ctx context.Context, gvr schema.GroupVersionResource, name, namespace string) (*unstructured.Unstructured, error) {
	obj, err := c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get live resource %s/%s: %w", namespace, name, err)
	}
	return obj, nil
}

// DeleteResource deletes a Kubernetes resource by kind name.
func (c *ClientsetClient) DeleteResource(ctx context.Context, kind, name, namespace string) error {
	gvr, ok := kindToGVR(kind)
	if !ok {
		return fmt.Errorf("unknown resource kind: %s", kind)
	}
	err := c.dynamic.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// GetJenkinsCRStatus returns the status phase of a Jenkins CR.
func (c *ClientsetClient) GetJenkinsCRStatus(ctx context.Context, name, namespace string) (string, error) {
	obj, err := c.dynamic.Resource(jenkinsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	status, ok := obj.Object["status"].(map[string]interface{})
	if !ok {
		return "Pending", nil
	}
	phase, ok := status["phase"].(string)
	if !ok || phase == "" {
		return "Pending", nil
	}
	return phase, nil
}

// ListControllerCRDs lists all Controller CRDs via the dynamic client.
func (c *ClientsetClient) ListControllerCRDs(ctx context.Context, namespace string) ([]*v1alpha1.Controller, error) {
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = c.dynamic.Resource(controllerGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = c.dynamic.Resource(controllerGVR).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list controllers: %w", err)
	}
	result := make([]*v1alpha1.Controller, 0, len(list.Items))
	for _, item := range list.Items {
		cr := &v1alpha1.Controller{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, cr); err != nil {
			return nil, fmt.Errorf("convert controller %s: %w", item.GetName(), err)
		}
		// FromUnstructured does not properly deserialize metav1.Time fields.
		if dt := item.GetDeletionTimestamp(); dt != nil {
			cr.DeletionTimestamp = dt
		}
		result = append(result, cr)
	}
	return result, nil
}

// ErrNullInList is returned by ApplyControllerSpecSSA when a spec patch
// contains a JSON null inside a list. Lists are never descended into by
// completion, and a null list element has no defined meaning, so the patch is
// rejected rather than guessed at. Callers map it to a 400 (BFF) or
// CodeInvalid (brood); no apply is attempted.
var ErrNullInList = errors.New("a null element inside a list is not allowed in a spec patch")

// translateNulls walks a spec patch recursively, deleting every key whose
// value is JSON null and recording its dot-separated path in removals. A map
// that becomes empty after its null children are removed is KEPT as an empty
// map — {hibernation:{activityIgnoreRegex:null}} must still assert
// hibernation while releasing activityIgnoreRegex. Lists are not descended
// into for removal (a list-internal null has no defined removal path), so any
// null reachable through a list is invalid and rejected with ErrNullInList.
func translateNulls(m map[string]any) (map[string]any, map[string]bool, error) {
	cleaned := make(map[string]any, len(m))
	removals := make(map[string]bool)
	for k, v := range m {
		switch val := v.(type) {
		case nil:
			removals[k] = true
		case map[string]any:
			sub, subRemovals, err := translateNulls(val)
			if err != nil {
				return nil, nil, err
			}
			for sk := range subRemovals {
				removals[k+"."+sk] = true
			}
			cleaned[k] = sub
		case []any:
			if listContainsNull(val) {
				return nil, nil, ErrNullInList
			}
			cleaned[k] = val
		default:
			cleaned[k] = v
		}
	}
	return cleaned, removals, nil
}

// listContainsNull reports whether a null is reachable anywhere within a list
// subtree (a null element, a null inside a list entry's map, or a nested list).
// Lists are never translated — a null inside a list would otherwise reach SSA
// and be coerced to a zero value — so it is rejected instead.
func listContainsNull(v any) bool {
	switch val := v.(type) {
	case nil:
		return true
	case []any:
		for _, e := range val {
			if listContainsNull(e) {
				return true
			}
		}
	case map[string]any:
		for _, mv := range val {
			if listContainsNull(mv) {
				return true
			}
		}
	}
	return false
}

// completionTree describes the leaves of metadata.managedFields the applying
// manager owns, shaped to be walked alongside the current spec during backfill.
// The traversal is driven entirely by the fieldsV1 key shape — never a schema
// lookup — so a new CRD field needs no change here.
type completionTree struct {
	// copyWhole marks an opaque owned leaf: a scalar, an atomic map, an atomic
	// list, or an i:-indexed list. Backfill must copy the ENTIRE current value
	// at this path; it must never assert an empty map, which would wipe a
	// populated atomic map.
	copyWhole bool
	// setValues, when non-nil, lists the scalar values owned in a set-type
	// list (v: keys); backfill copies only those current elements.
	setValues []string
	// children maps an f: spec key to its owned subtree.
	children map[string]*completionTree
	// entries maps a merge-key tuple's canonical JSON (k: keys) to the owned
	// subtree of that associative-list entry.
	entries map[string]*completionTree
}

// ownedLeaves selects the applying manager's Apply entry from the current
// object's metadata.managedFields and builds its f:spec completion tree. It
// returns nil when no such entry exists (the manager owns nothing under spec).
//
// The entry must match all three of manager, operation == "Apply", and
// apiVersion == the object's current apiVersion. An Update entry under the
// same manager name is a DISTINCT ownership record whose leaves are already
// protected; reading it would inflate the Apply set. Entries are recorded
// per-version; a mismatched apiVersion describes a different schema.
func ownedLeaves(cur *unstructured.Unstructured, fieldManager string) *completionTree {
	mf, found, err := unstructured.NestedSlice(cur.Object, "metadata", "managedFields")
	if err != nil || !found {
		return nil
	}
	apiVersion := cur.GetAPIVersion()
	var entry map[string]any
	for _, raw := range mf {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["manager"] != fieldManager || m["operation"] != "Apply" || m["apiVersion"] != apiVersion {
			continue
		}
		entry = m
		break
	}
	if entry == nil {
		return nil
	}
	fv, found, err := unstructured.NestedMap(entry, "fieldsV1")
	if err != nil || !found {
		return nil
	}
	specNode, found, err := unstructured.NestedMap(fv, "f:spec")
	if err != nil || !found {
		return nil
	}
	return buildTree(specNode)
}

// buildTree walks one fieldsV1 node into a completionTree. Node shapes are
// keyed by their fieldset element prefix (measured through
// structured-merge-diff, the library the apiserver itself uses):
//
//	{}                          -> copyWhole (opaque owned leaf)
//	f:<key>                     -> child subtree; "." marks membership of the
//	                               container itself and is NOT a child
//	k:<canonical merge-key JSON>-> associative-list entry subtree
//	v:<value>                   -> set list; only the listed scalars are owned
//	i:<n>                       -> index-keyed list; the whole list is owned
//
// A bare {} node cannot distinguish an atomic map (owned wholly) from a
// granular map owned membership-only, and both are reachable, so it resolves
// toward copyWhole: preserving too much only costs co-ownership, while
// asserting {} would wipe a populated atomic map.
func buildTree(node map[string]any) *completionTree {
	t := &completionTree{}
	if len(node) == 0 {
		t.copyWhole = true
		return t
	}
	var setValues []string
	indexKeyed := false
	for key, val := range node {
		switch {
		case key == ".":
			// Membership of the container itself; not a child.
		case strings.HasPrefix(key, "f:"):
			if t.children == nil {
				t.children = make(map[string]*completionTree)
			}
			name := strings.TrimPrefix(key, "f:")
			if child, ok := val.(map[string]any); ok {
				t.children[name] = buildTree(child)
			} else {
				t.children[name] = &completionTree{copyWhole: true}
			}
		case strings.HasPrefix(key, "k:"):
			if t.entries == nil {
				t.entries = make(map[string]*completionTree)
			}
			mergeKey := strings.TrimPrefix(key, "k:")
			if child, ok := val.(map[string]any); ok {
				t.entries[mergeKey] = buildTree(child)
			} else {
				t.entries[mergeKey] = &completionTree{}
			}
		case strings.HasPrefix(key, "v:"):
			setValues = append(setValues, strings.TrimPrefix(key, "v:"))
		case strings.HasPrefix(key, "i:"):
			indexKeyed = true
		}
	}
	switch {
	case indexKeyed:
		// Per-index ownership cannot be expressed in an apply config: copy the
		// whole list.
		t.copyWhole = true
	case len(setValues) > 0:
		t.setValues = setValues
	}
	return t
}

// backfill deep-merges the owned current values under the cleaned patch: the
// patch always wins per-leaf for maps, a path in removals is never backfilled
// at any depth, and where the patch supplies a list at a path that list is
// used verbatim (owned entries are not re-injected — otherwise removing one
// entry from an associative list would be impossible). patch is freshly
// produced by translateNulls and is mutated in place.
func backfill(patch, cur map[string]any, owned *completionTree, removals map[string]bool, path string) map[string]any {
	if owned == nil {
		return patch
	}
	result := patch
	for key, node := range owned.children {
		childPath := path + key
		if removals[childPath] {
			continue
		}
		curVal, curHas := cur[key]
		patchVal, patchHas := result[key]
		switch {
		case node.copyWhole:
			// Opaque owned leaf: backfill the ENTIRE current value if the patch
			// does not set it. Copy, never assert {} — that would wipe a
			// populated atomic map.
			if !patchHas && curHas {
				result[key] = curVal
			}
		case node.setValues != nil:
			// Set list: copy only the listed current scalar values.
			if !patchHas && curHas {
				if l, ok := curVal.([]any); ok {
					if filtered := filterSetValues(l, node.setValues); filtered != nil {
						result[key] = filtered
					}
				}
			}
		case len(node.entries) > 0:
			// Associative list: backfill only the owned entries, recursing into
			// each entry's owned sub-fields. A patch-supplied list is used
			// verbatim (5.12).
			if !patchHas && curHas {
				if entries := backfillAssocList(curVal, node, removals, childPath); entries != nil {
					result[key] = entries
				}
			}
		case len(node.children) > 0:
			if patchHas {
				if p, ok := patchVal.(map[string]any); ok {
					if c, ok := curVal.(map[string]any); ok {
						result[key] = backfill(p, c, node, removals, childPath+".")
					}
				}
			} else if curHas {
				// Recurse with an empty patch. Drop the container if nothing was
				// backfilled — asserting an empty container for a subtree whose
				// owned children are all absent (e.g. a stale managedFields
				// window after another manager force-removed them) would claim
				// membership the patch never requested.
				if c, ok := curVal.(map[string]any); ok {
					if merged := backfill(map[string]any{}, c, node, removals, childPath+"."); len(merged) > 0 {
						result[key] = merged
					}
				}
			}
		default:
			// Empty owned node (membership-only): nothing to backfill.
		}
	}
	return result
}

// backfillAssocList re-asserts only the owned entries of an associative list
// (x-kubernetes-list-type: map). A k: key is the canonical JSON of the
// merge-key tuple (e.g. k:{"name":"x"}); the current entry whose key fields
// all match is found, and only that entry's owned sub-fields are recursed into
// — never the entry whole, or sub-fields another manager owns would be claimed.
// A partial list-map apply merges by key rather than replacing the list, so a
// subset neither deletes nor claims unowned entries.
func backfillAssocList(curVal any, owned *completionTree, removals map[string]bool, path string) []any {
	curList, ok := curVal.([]any)
	if !ok {
		return nil
	}
	// Deterministic output: Go map iteration is randomized, so walking
	// owned.entries directly would emit the associative-list entries in a
	// different order on every run, making the applied payload unstable. A
	// plain lexical sort of the canonical merge-key JSON is stable and
	// sufficient — the apiserver merges list-map entries by key, so the
	// relative order in the payload is immaterial to the apply result.
	keys := make([]string, 0, len(owned.entries))
	for mergeKeyJSON := range owned.entries {
		keys = append(keys, mergeKeyJSON)
	}
	sort.Strings(keys)

	var out []any
	for _, mergeKeyJSON := range keys {
		entryNode := owned.entries[mergeKeyJSON]
		var keyTuple map[string]any
		if err := json.Unmarshal([]byte(mergeKeyJSON), &keyTuple); err != nil {
			continue
		}
		for _, raw := range curList {
			curEntry, ok := raw.(map[string]any)
			if !ok || !entryMatchesKey(curEntry, keyTuple) {
				continue
			}
			entry := backfillAssocEntry(curEntry, entryNode, removals, path)
			// The merge-key fields are required to identify the entry in a
			// list-map apply; include any not already covered by owned children.
			for kk, vv := range keyTuple {
				if _, ok := entry[kk]; !ok {
					entry[kk] = vv
				}
			}
			out = append(out, entry)
			break
		}
	}
	return out
}

// backfillAssocEntry builds the backfilled map for one owned list entry by
// recursing into the entry node's owned f: children.
func backfillAssocEntry(curEntry map[string]any, entryNode *completionTree, removals map[string]bool, path string) map[string]any {
	entry := map[string]any{}
	for field, node := range entryNode.children {
		childPath := path + "." + field
		if removals[childPath] {
			continue
		}
		val, has := curEntry[field]
		if !has {
			continue
		}
		switch {
		case node.copyWhole:
			entry[field] = val
		case node.setValues != nil:
			if l, ok := val.([]any); ok {
				if filtered := filterSetValues(l, node.setValues); filtered != nil {
					entry[field] = filtered
				}
			}
		case len(node.entries) > 0:
			if l, ok := val.([]any); ok {
				if entries := backfillAssocList(l, node, removals, childPath); entries != nil {
					entry[field] = entries
				}
			}
		case len(node.children) > 0:
			if c, ok := val.(map[string]any); ok {
				entry[field] = backfill(map[string]any{}, c, node, removals, childPath+".")
			}
		}
	}
	return entry
}

// entryMatchesKey reports whether a current list entry carries the given
// merge-key tuple. Values are compared by canonical JSON so int/float64
// decoding differences do not cause false mismatches.
func entryMatchesKey(entry, keyTuple map[string]any) bool {
	for k, v := range keyTuple {
		ev, has := entry[k]
		if !has || !jsonEqualValues(ev, v) {
			return false
		}
	}
	return true
}

func jsonEqualValues(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}

// filterSetValues copies only the current set-list elements whose canonical
// JSON matches one of the listed v: values.
func filterSetValues(list []any, values []string) []any {
	wanted := make(map[string]bool, len(values))
	for _, v := range values {
		wanted[v] = true
	}
	var out []any
	for _, elem := range list {
		b, err := json.Marshal(elem)
		if err != nil {
			continue
		}
		if wanted[string(b)] {
			out = append(out, elem)
		}
	}
	return out
}

// ApplyControllerSpecSSA applies a sparse Controller spec via Kubernetes
// server-side apply, completing the patch with the leaves THIS manager already
// owns (read from metadata.managedFields) before applying. Server-side apply
// releases any field the manager owns but does not re-send, so a sparse patch
// would otherwise silently delete fields a different surface set; completion
// makes omission mean "leave as-is". Fields the patch explicitly nulls are
// removed from both the patch and the completion — a null is a removal marker,
// never a value. The current object is read unstructured (never a typed
// round-trip, which would reintroduce zero-valued non-omitempty fields and
// claim ownership), and no metadata.resourceVersion is set on the apply. On a
// field-manager conflict, returns an apierrors.StatusError whose details carry
// the conflicting fields (parseable via SSAConflicts).
//
// The returned unapplied list names every requested removal (explicit null)
// that did not take effect. Presence is tested against the APPLIED object's
// unstructured spec (res.Object) — not the marshalled typed Controller — so a
// field another manager retained at its zero value (e.g. hibernation.enabled:
// false, which omitempty would drop from the typed JSON) is still reported.
// Computing it here makes the local-cluster and Brood-routed paths identical
// by construction: both call this method, and both receive the same
// []bus.UnappliedRemoval.
func (c *ClientsetClient) ApplyControllerSpecSSA(
	ctx context.Context, namespace, name string, specPatch map[string]any,
	fieldManager string, force bool,
) (*v1alpha1.Controller, []bus.UnappliedRemoval, error) {
	// Null-translation runs BEFORE completion: completing first would backfill
	// a removal from the current object and silently undo it — a bundle detach
	// ({composedBundleRef: null}) would then do nothing.
	cleaned, removals, err := translateNulls(specPatch)
	if err != nil {
		return nil, nil, err
	}
	completed := cleaned

	// Read the current object unstructured and backfill every owned leaf the
	// patch neither sets nor removes. A missing object has nothing to backfill
	// (apply the cleaned patch as-is, matching create semantics); any OTHER
	// read failure must not proceed with a bare patch, which would release
	// every owned leaf.
	cur, err := c.dynamic.Resource(controllerGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("get controller for spec completion: %w", err)
		}
	} else if owned := ownedLeaves(cur, fieldManager); owned != nil {
		if curSpec, ok, nestedErr := unstructured.NestedMap(cur.Object, "spec"); nestedErr == nil && ok {
			completed = backfill(cleaned, curSpec, owned, removals, "")
		}
	}

	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "Controller",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": completed,
	}}
	data, err := json.Marshal(u)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal partial controller: %w", err)
	}
	res, err := c.dynamic.Resource(controllerGVR).Namespace(namespace).Patch(
		ctx, name, types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force},
	)
	if err != nil {
		return nil, nil, err
	}

	// Report which of the requested removals are still present on the applied
	// object. Tested against the unstructured spec (res.Object), never the
	// marshalled typed controller below — omitempty would drop a retained
	// zero-valued field (hibernation.enabled=false) and falsely report the
	// removal as taken effect.
	appliedSpec, _, _ := unstructured.NestedMap(res.Object, "spec")
	unapplied := UnappliedRemovalsFromSpec(appliedSpec, removalPathsFromSet(removals))

	var out v1alpha1.Controller
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(res.Object, &out); err != nil {
		return nil, nil, fmt.Errorf("convert applied controller: %w", err)
	}
	return &out, unapplied, nil
}

// PatchControllerStatus patches the status subresource of a Controller CRD.
func (c *ClientsetClient) PatchControllerStatus(ctx context.Context, name, namespace string, status *v1alpha1.ControllerStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	conditions := status.Conditions
	if conditions == nil {
		conditions = []v1alpha1.ControllerCondition{}
	}
	statusPatch := map[string]interface{}{
		"phase":      status.Phase,
		"conditions": conditions,
	}
	// hibernated/hibernatedAt are deliberately NOT carried here. This
	// end-of-reconcile status write is blind, and a JSON merge patch retains
	// omitted keys — a false value could never clear the flag, and a stale
	// true read before a concurrent wake would re-assert it and un-wake the
	// controller. They are written only by SetHibernated, the status CAS.
	// Force overlayWarnings so that an empty slice clears prior warnings
	// (omitempty would prevent the merge patch from clearing them).
	overlayWarnings := status.OverlayWarnings
	if overlayWarnings == nil {
		overlayWarnings = []v1alpha1.OverlayWarning{}
	}
	statusPatch["overlayWarnings"] = overlayWarnings
	if status.MiteStatus != nil {
		statusPatch["miteStatus"] = status.MiteStatus
	}
	if status.ConfigHash != "" {
		statusPatch["configHash"] = status.ConfigHash
	}
	if status.RBACHash != "" {
		statusPatch["rbacHash"] = status.RBACHash
	}
	if status.DesiredStateHash != "" {
		statusPatch["desiredStateHash"] = status.DesiredStateHash
	}
	if status.FirstConnectedAt != nil {
		statusPatch["firstConnectedAt"] = status.FirstConnectedAt
	}
	if status.WakeToken != "" {
		statusPatch["wakeToken"] = status.WakeToken
	}
	if status.LastActivityAt != nil {
		statusPatch["lastActivityAt"] = status.LastActivityAt
	}
	if status.LastReconciledAt != nil {
		statusPatch["lastReconciledAt"] = status.LastReconciledAt
	}
	statusPatch["pendingRestart"] = status.PendingRestart
	statusPatch["pendingPluginRoll"] = status.PendingPluginRoll
	statusPatch["approvedPluginRollChecksum"] = status.ApprovedPluginRollChecksum
	if status.PendingItemDeletions != nil {
		statusPatch["pendingItemDeletions"] = status.PendingItemDeletions
	}
	statusPatch["restartDrain"] = status.RestartDrain
	statusPatch["liveDrift"] = status.LiveDrift
	if status.LastDesiredPushAt != nil {
		statusPatch["lastDesiredPushAt"] = status.LastDesiredPushAt
	}
	if status.AppliedBundleHash != "" {
		statusPatch["appliedBundleHash"] = status.AppliedBundleHash
	}
	statusPatch["rollout"] = status.Rollout
	statusPatch["lastReconcileError"] = status.LastReconcileError
	statusPatch["lastReconcileErrorAt"] = status.LastReconcileErrorAt
	if status.LastApplyResult != nil {
		statusPatch["lastApplyResult"] = status.LastApplyResult
	}
	if status.ApplyHistory != nil {
		statusPatch["applyHistory"] = status.ApplyHistory
	}
	// PluginInventory is always written whole to avoid the merge-patch
	// omitempty trap — an omitted key is retained, not cleared.
	statusPatch["pluginInventory"] = status.PluginInventory
	patch := map[string]interface{}{
		"status": statusPatch,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	_, err = c.dynamic.Resource(controllerGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// SetHibernated is the single writer for status.hibernated and
// status.hibernatedAt. It reads the live object and returns (false, nil) when
// the flag already matches want. Otherwise it sets the two hibernation fields
// on the freshly read object and Updates the STATUS SUBRESOURCE, carrying the
// fetched resourceVersion so a concurrent write conflicts rather than being
// clobbered.
//
// The status subresource Update is a full PUT of status, so the write starts
// from the freshly read object and mutates only those two fields — building
// from anything else would wipe phase and conditions. Both directions retry a
// bounded number of times on conflict: the clear (wake) callers are one-shot
// and would otherwise drop the wake, and the set direction is reached by
// HibernateController — also a one-shot request-reply action — which would
// surface a conflict as a spurious 500. Auto-hibernate additionally requeues
// naturally inside the reconcile.
func (c *ClientsetClient) SetHibernated(ctx context.Context, name, namespace string, want bool) (bool, error) {
	var changed bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		changed, err = c.setHibernatedOnce(ctx, name, namespace, want)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// setHibernatedOnce performs a single read-compare-write of the hibernation
// flag on the status subresource. See SetHibernated for the contract.
func (c *ClientsetClient) setHibernatedOnce(ctx context.Context, name, namespace string, want bool) (bool, error) {
	u, err := c.dynamic.Resource(controllerGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	cur, _, err := unstructured.NestedBool(u.Object, "status", "hibernated")
	if err != nil {
		return false, fmt.Errorf("read status.hibernated: %w", err)
	}
	if cur == want {
		return false, nil
	}
	if err := unstructured.SetNestedField(u.Object, want, "status", "hibernated"); err != nil {
		return false, fmt.Errorf("set status.hibernated: %w", err)
	}
	if want {
		if err := unstructured.SetNestedField(u.Object, metav1.Now().UTC().Format(time.RFC3339), "status", "hibernatedAt"); err != nil {
			return false, fmt.Errorf("set status.hibernatedAt: %w", err)
		}
	} else {
		unstructured.RemoveNestedField(u.Object, "status", "hibernatedAt")
	}
	if _, err := c.dynamic.Resource(controllerGVR).Namespace(namespace).Update(ctx, u, metav1.UpdateOptions{}, "status"); err != nil {
		return false, err
	}
	return true, nil
}

// PatchControllerAnnotations merge-patches metadata.annotations on a Controller.
// A nil *string value deletes the key (JSON merge patch null semantics).
func (c *ClientsetClient) PatchControllerAnnotations(ctx context.Context, name, namespace string, ann map[string]*string) error {
	annMap := make(map[string]interface{}, len(ann))
	for k, v := range ann {
		if v == nil {
			annMap[k] = nil
		} else {
			annMap[k] = *v
		}
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annMap,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal annotations patch: %w", err)
	}
	_, err = c.dynamic.Resource(controllerGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	return err
}

// PatchControllerFinalizers patches the metadata.finalizers of a Controller CRD.
func (c *ClientsetClient) PatchControllerFinalizers(ctx context.Context, name, namespace string, finalizers []string) error {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": finalizers,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal finalizers patch: %w", err)
	}
	_, err = c.dynamic.Resource(controllerGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	return err
}

// kindToGVR maps DeleteResource kind strings to GVRs.
func kindToGVR(kind string) (schema.GroupVersionResource, bool) {
	switch kind {
	case "Service":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}, true
	case "ConfigMap":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, true
	case "Ingress":
		return schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, true
	case "StatefulSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, true
	case "PersistentVolumeClaim":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}, true
	default:
		return schema.GroupVersionResource{}, false
	}
}

// GetControllerCRD fetches a single Controller CRD.
func (c *ClientsetClient) GetControllerCRD(ctx context.Context, name, namespace string) (*v1alpha1.Controller, error) {
	obj, err := c.dynamic.Resource(controllerGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cr := &v1alpha1.Controller{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cr); err != nil {
		return nil, fmt.Errorf("convert controller: %w", err)
	}
	return cr, nil
}

// DeleteControllerCRD deletes a Controller CRD.
func (c *ClientsetClient) DeleteControllerCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(controllerGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// VarroaRole CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// ListVarroaRoleCRDs lists all VarroaRole CRDs.
func (c *ClientsetClient) ListVarroaRoleCRDs(ctx context.Context) ([]*v1alpha1.VarroaRole, error) {
	list, err := c.dynamic.Resource(varroaRoleGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list varroaroles: %w", err)
	}
	var roles []*v1alpha1.VarroaRole
	for i := range list.Items {
		role := &v1alpha1.VarroaRole{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, role); err != nil {
			return nil, fmt.Errorf("convert varroarole: %w", err)
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// GetVarroaRoleCRD retrieves a single VarroaRole CRD by name.
func (c *ClientsetClient) GetVarroaRoleCRD(ctx context.Context, name string) (*v1alpha1.VarroaRole, error) {
	u, err := c.dynamic.Resource(varroaRoleGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	role := &v1alpha1.VarroaRole{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, role); err != nil {
		return nil, fmt.Errorf("convert varroarole: %w", err)
	}
	return role, nil
}

// ApplyVarroaRoleCRD creates or updates a VarroaRole CRD.
func (c *ClientsetClient) ApplyVarroaRoleCRD(ctx context.Context, cr *v1alpha1.VarroaRole) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert varroarole to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.VarroaRoleGVK)

	existing, err := c.dynamic.Resource(varroaRoleGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.dynamic.Resource(varroaRoleGVR).Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get existing varroarole: %w", err)
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(varroaRoleGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteVarroaRoleCRD deletes a VarroaRole CRD by name.
func (c *ClientsetClient) DeleteVarroaRoleCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(varroaRoleGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// VarroaRoleBinding CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// ListVarroaRoleBindingCRDs lists all VarroaRoleBinding CRDs.
func (c *ClientsetClient) ListVarroaRoleBindingCRDs(ctx context.Context) ([]*v1alpha1.VarroaRoleBinding, error) {
	list, err := c.dynamic.Resource(varroaRoleBindingGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list varroarolebindings: %w", err)
	}
	var bindings []*v1alpha1.VarroaRoleBinding
	for i := range list.Items {
		binding := &v1alpha1.VarroaRoleBinding{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, binding); err != nil {
			return nil, fmt.Errorf("convert varroarolebinding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// GetVarroaRoleBindingCRD retrieves a single VarroaRoleBinding CRD by name.
func (c *ClientsetClient) GetVarroaRoleBindingCRD(ctx context.Context, name string) (*v1alpha1.VarroaRoleBinding, error) {
	u, err := c.dynamic.Resource(varroaRoleBindingGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	binding := &v1alpha1.VarroaRoleBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, binding); err != nil {
		return nil, fmt.Errorf("convert varroarolebinding: %w", err)
	}
	return binding, nil
}

// ApplyVarroaRoleBindingCRD creates or updates a VarroaRoleBinding CRD.
func (c *ClientsetClient) ApplyVarroaRoleBindingCRD(ctx context.Context, cr *v1alpha1.VarroaRoleBinding) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert varroarolebinding to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.VarroaRoleBindingGVK)

	existing, err := c.dynamic.Resource(varroaRoleBindingGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.dynamic.Resource(varroaRoleBindingGVR).Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get existing varroarolebinding: %w", err)
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(varroaRoleBindingGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteVarroaRoleBindingCRD deletes a VarroaRoleBinding CRD by name.
func (c *ClientsetClient) DeleteVarroaRoleBindingCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(varroaRoleBindingGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// JenkinsRole CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// GetJenkinsRoleCRD retrieves a single JenkinsRole CRD by name.
func (c *ClientsetClient) GetJenkinsRoleCRD(ctx context.Context, name string) (*v1alpha1.JenkinsRole, error) {
	u, err := c.dynamic.Resource(jenkinsRoleGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get jenkinsrole %s: %w", name, err)
	}
	jr := &v1alpha1.JenkinsRole{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, jr); err != nil {
		return nil, fmt.Errorf("convert jenkinsrole: %w", err)
	}
	return jr, nil
}

// ApplyJenkinsRoleCRD creates or updates a JenkinsRole CRD.
func (c *ClientsetClient) ApplyJenkinsRoleCRD(ctx context.Context, cr *v1alpha1.JenkinsRole) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsrole to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsRoleGVK)

	existing, err := c.dynamic.Resource(jenkinsRoleGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.dynamic.Resource(jenkinsRoleGVR).Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get existing jenkinsrole: %w", err)
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(jenkinsRoleGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// CreateJenkinsRoleCRD creates a JenkinsRole CRD (create-only, cluster-scoped).
// Returns AlreadyExists when the CR already exists.
func (c *ClientsetClient) CreateJenkinsRoleCRD(ctx context.Context, cr *v1alpha1.JenkinsRole) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsrole to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsRoleGVK)
	_, err = c.dynamic.Resource(jenkinsRoleGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// UpdateJenkinsRoleCRD updates a JenkinsRole CRD (cluster-scoped).
// When the passed CR carries a non-empty resourceVersion, it is passed to
// Update() verbatim (so a stale RV surfaces as a k8s Conflict).
// When RV is empty, the method fetches the existing CR and applies the
// caller's spec/labels/annotations onto it (last-write-wins semantics).
func (c *ClientsetClient) UpdateJenkinsRoleCRD(ctx context.Context, cr *v1alpha1.JenkinsRole) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsrole to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsRoleGVK)

	if cr.ResourceVersion != "" {
		u.SetResourceVersion(cr.ResourceVersion)
		_, err = c.dynamic.Resource(jenkinsRoleGVR).Update(ctx, u, metav1.UpdateOptions{})
		return err
	}

	// RV-less: get-then-set (last-write-wins).
	existing, getErr := c.dynamic.Resource(jenkinsRoleGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(jenkinsRoleGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteJenkinsRoleCRD deletes a JenkinsRole CRD by name.
func (c *ClientsetClient) DeleteJenkinsRoleCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(jenkinsRoleGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ListJenkinsRoleCRDs lists all JenkinsRole CRDs.
func (c *ClientsetClient) ListJenkinsRoleCRDs(ctx context.Context) ([]*v1alpha1.JenkinsRole, error) {
	list, err := c.dynamic.Resource(jenkinsRoleGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list jenkinsroles: %w", err)
	}
	var roles []*v1alpha1.JenkinsRole
	for i := range list.Items {
		role := &v1alpha1.JenkinsRole{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, role); err != nil {
			return nil, fmt.Errorf("convert jenkinsrole: %w", err)
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// ---------------------------------------------------------------------------
// JenkinsRoleBinding CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// ListJenkinsRoleBindingCRDs lists all JenkinsRoleBinding CRDs.
func (c *ClientsetClient) ListJenkinsRoleBindingCRDs(ctx context.Context) ([]*v1alpha1.JenkinsRoleBinding, error) {
	list, err := c.dynamic.Resource(jenkinsRoleBindingGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list jenkinsrolebindings: %w", err)
	}
	var bindings []*v1alpha1.JenkinsRoleBinding
	for i := range list.Items {
		binding := &v1alpha1.JenkinsRoleBinding{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, binding); err != nil {
			return nil, fmt.Errorf("convert jenkinsrolebinding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// GetJenkinsRoleBindingCRD retrieves a single JenkinsRoleBinding CRD by name.
func (c *ClientsetClient) GetJenkinsRoleBindingCRD(ctx context.Context, name string) (*v1alpha1.JenkinsRoleBinding, error) {
	u, err := c.dynamic.Resource(jenkinsRoleBindingGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	binding := &v1alpha1.JenkinsRoleBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, binding); err != nil {
		return nil, fmt.Errorf("convert jenkinsrolebinding: %w", err)
	}
	return binding, nil
}

// ApplyJenkinsRoleBindingCRD creates or updates a JenkinsRoleBinding CRD.
func (c *ClientsetClient) ApplyJenkinsRoleBindingCRD(ctx context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsrolebinding to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsRoleBindingGVK)

	existing, err := c.dynamic.Resource(jenkinsRoleBindingGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.dynamic.Resource(jenkinsRoleBindingGVR).Create(ctx, u, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return fmt.Errorf("get existing jenkinsrolebinding: %w", err)
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(jenkinsRoleBindingGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// CreateJenkinsRoleBindingCRD creates a JenkinsRoleBinding CRD (create-only, cluster-scoped).
// Returns AlreadyExists when the CR already exists.
func (c *ClientsetClient) CreateJenkinsRoleBindingCRD(ctx context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsrolebinding to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsRoleBindingGVK)
	_, err = c.dynamic.Resource(jenkinsRoleBindingGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// UpdateJenkinsRoleBindingCRD updates a JenkinsRoleBinding CRD (cluster-scoped).
// When the passed CR carries a non-empty resourceVersion, it is passed to
// Update() verbatim (so a stale RV surfaces as a k8s Conflict).
// When RV is empty, the method fetches the existing CR and applies the
// caller's spec/labels/annotations onto it (last-write-wins semantics).
func (c *ClientsetClient) UpdateJenkinsRoleBindingCRD(ctx context.Context, cr *v1alpha1.JenkinsRoleBinding) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsrolebinding to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsRoleBindingGVK)

	if cr.ResourceVersion != "" {
		u.SetResourceVersion(cr.ResourceVersion)
		_, err = c.dynamic.Resource(jenkinsRoleBindingGVR).Update(ctx, u, metav1.UpdateOptions{})
		return err
	}

	// RV-less: get-then-set (last-write-wins).
	existing, getErr := c.dynamic.Resource(jenkinsRoleBindingGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(jenkinsRoleBindingGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteJenkinsRoleBindingCRD deletes a JenkinsRoleBinding CRD by name.
func (c *ClientsetClient) DeleteJenkinsRoleBindingCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(jenkinsRoleBindingGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// PodTemplate CRD operations
// ---------------------------------------------------------------------------

// ListPodTemplateCRDs lists all PodTemplate CRDs in a namespace.
func (c *ClientsetClient) ListPodTemplateCRDs(ctx context.Context, namespace string) ([]*v1alpha1.PodTemplate, error) {
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = c.dynamic.Resource(podTemplateGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = c.dynamic.Resource(podTemplateGVR).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list podtemplates: %w", err)
	}
	result := make([]*v1alpha1.PodTemplate, 0, len(list.Items))
	for _, item := range list.Items {
		pt := &v1alpha1.PodTemplate{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, pt); err != nil {
			return nil, fmt.Errorf("convert podtemplate %s: %w", item.GetName(), err)
		}
		result = append(result, pt)
	}
	return result, nil
}

// GetPodTemplateCRD retrieves a single PodTemplate CRD by name.
func (c *ClientsetClient) GetPodTemplateCRD(ctx context.Context, name, namespace string) (*v1alpha1.PodTemplate, error) {
	obj, err := c.dynamic.Resource(podTemplateGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pt := &v1alpha1.PodTemplate{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, pt); err != nil {
		return nil, fmt.Errorf("convert podtemplate: %w", err)
	}
	return pt, nil
}

// ApplyPodTemplateCRD creates or updates a PodTemplate CRD.
func (c *ClientsetClient) ApplyPodTemplateCRD(ctx context.Context, cr *v1alpha1.PodTemplate) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert podtemplate to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("PodTemplate")
	_, err = c.dynamic.Resource(podTemplateGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(podTemplateGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing podtemplate for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(podTemplateGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// DeletePodTemplateCRD deletes a PodTemplate CRD by name.
func (c *ClientsetClient) DeletePodTemplateCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(podTemplateGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// CatalogSource CRD operations (namespaced)
// ---------------------------------------------------------------------------

// ListCatalogSourceCRDs lists all CatalogSource CRDs in a namespace.
func (c *ClientsetClient) ListCatalogSourceCRDs(ctx context.Context, namespace string) ([]*v1alpha1.CatalogSource, error) {
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = c.dynamic.Resource(catalogSourceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = c.dynamic.Resource(catalogSourceGVR).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list catalogsources: %w", err)
	}
	result := make([]*v1alpha1.CatalogSource, 0, len(list.Items))
	for _, item := range list.Items {
		cr := &v1alpha1.CatalogSource{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, cr); err != nil {
			return nil, fmt.Errorf("convert catalogsource %s: %w", item.GetName(), err)
		}
		result = append(result, cr)
	}
	return result, nil
}

// GetCatalogSourceCRD retrieves a single CatalogSource CRD by name and namespace.
func (c *ClientsetClient) GetCatalogSourceCRD(ctx context.Context, name, namespace string) (*v1alpha1.CatalogSource, error) {
	obj, err := c.dynamic.Resource(catalogSourceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cr := &v1alpha1.CatalogSource{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cr); err != nil {
		return nil, fmt.Errorf("convert catalogsource: %w", err)
	}
	return cr, nil
}

// ApplyCatalogSourceCRD creates or updates a CatalogSource CRD.
func (c *ClientsetClient) ApplyCatalogSourceCRD(ctx context.Context, cr *v1alpha1.CatalogSource) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert catalogsource to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("CatalogSource")
	u.SetResourceVersion("")
	_, err = c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing catalogsource for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// CreateCatalogSourceCRD creates a CatalogSource CRD (create-only).
// Returns AlreadyExists when the CR already exists.
func (c *ClientsetClient) CreateCatalogSourceCRD(ctx context.Context, cr *v1alpha1.CatalogSource) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert catalogsource to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("CatalogSource")
	_, err = c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// UpdateCatalogSourceCRD updates a CatalogSource CRD.
// When the passed CR carries a non-empty resourceVersion, it is passed to
// Update() verbatim (so a stale RV surfaces as a k8s Conflict).
// When RV is empty, the method fetches the existing CR and applies the
// caller's spec/labels/annotations onto it (last-write-wins semantics).
func (c *ClientsetClient) UpdateCatalogSourceCRD(ctx context.Context, cr *v1alpha1.CatalogSource) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert catalogsource to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("CatalogSource")

	if cr.ResourceVersion != "" {
		u.SetResourceVersion(cr.ResourceVersion)
		_, err = c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
		return err
	}

	// RV-less: get-then-set (last-write-wins).
	existing, getErr := c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(catalogSourceGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteCatalogSourceCRD deletes a CatalogSource CRD by name and namespace.
func (c *ClientsetClient) DeleteCatalogSourceCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(catalogSourceGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchCatalogSourceStatus patches the status subresource of a CatalogSource CRD.
func (c *ClientsetClient) PatchCatalogSourceStatus(ctx context.Context, name, namespace string, status *v1alpha1.CatalogSourceStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	// Convert to a map so we can force-include the clearable fields below.
	statusMap := map[string]interface{}{}
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	if err := json.Unmarshal(statusBytes, &statusMap); err != nil {
		return fmt.Errorf("unmarshal status: %w", err)
	}
	// message and itemCount carry `omitempty`, so when the current status no
	// longer has them the fields are dropped from the JSON merge patch —
	// leaving a stale prior value in etcd (e.g. a git-fetch error or a
	// nonzero item count persisting after a later successful sync). Set them
	// explicitly so the merge patch clears them.
	statusMap["message"] = status.Message
	statusMap["itemCount"] = status.ItemCount

	patch := map[string]interface{}{
		"status": statusMap,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	_, err = c.dynamic.Resource(catalogSourceGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// ---------------------------------------------------------------------------
// CatalogItem CRD operations (namespaced)
// ---------------------------------------------------------------------------

// ListCatalogItemCRDs lists all CatalogItem CRDs in a namespace, optionally filtered by label selector.
func (c *ClientsetClient) ListCatalogItemCRDs(ctx context.Context, namespace, selector string) ([]*v1alpha1.CatalogItem, error) {
	opts := metav1.ListOptions{}
	if selector != "" {
		opts.LabelSelector = selector
	}
	list, err := c.dynamic.Resource(catalogItemGVR).Namespace(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list catalogitems: %w", err)
	}
	result := make([]*v1alpha1.CatalogItem, 0, len(list.Items))
	for _, item := range list.Items {
		ci := &v1alpha1.CatalogItem{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, ci); err != nil {
			return nil, fmt.Errorf("convert catalogitem %s: %w", item.GetName(), err)
		}
		result = append(result, ci)
	}
	return result, nil
}

// GetCatalogItemCRD retrieves a single CatalogItem CRD by name and namespace.
func (c *ClientsetClient) GetCatalogItemCRD(ctx context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	obj, err := c.dynamic.Resource(catalogItemGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	ci := &v1alpha1.CatalogItem{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, ci); err != nil {
		return nil, fmt.Errorf("convert catalogitem: %w", err)
	}
	return ci, nil
}

// ApplyCatalogItemCRD creates or updates a CatalogItem CRD.
func (c *ClientsetClient) ApplyCatalogItemCRD(ctx context.Context, cr *v1alpha1.CatalogItem) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert catalogitem to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("CatalogItem")
	u.SetResourceVersion("")
	_, err = c.dynamic.Resource(catalogItemGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(catalogItemGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing catalogitem for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(catalogItemGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// DeleteCatalogItemCRD deletes a CatalogItem CRD by name and namespace.
func (c *ClientsetClient) DeleteCatalogItemCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(catalogItemGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchCatalogItemStatus patches the status subresource of a CatalogItem CRD.
func (c *ClientsetClient) PatchCatalogItemStatus(ctx context.Context, name, namespace string, status *v1alpha1.CatalogItemStatus) error {
	statusObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		return fmt.Errorf("convert catalogitem status to unstructured: %w", err)
	}
	// content carries `omitempty`, so when an item no longer stores content
	// (e.g. it became invalid) the key is dropped from the JSON merge patch
	// and the stale prior content survives in etcd. Set it explicitly so the
	// merge patch clears it (same pattern as PatchComposedBundleStatus).
	statusObj["content"] = status.Content
	statusMap := map[string]any{"status": statusObj}
	patchBytes, err := json.Marshal(statusMap)
	if err != nil {
		return fmt.Errorf("marshal catalogitem status patch: %w", err)
	}
	_, err = c.dynamic.Resource(catalogItemGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// ---------------------------------------------------------------------------
// ComposedBundle CRD operations (namespaced)
// ---------------------------------------------------------------------------

// ListComposedBundleCRDs lists all ComposedBundle CRDs in a namespace.
func (c *ClientsetClient) ListComposedBundleCRDs(ctx context.Context, namespace string) ([]*v1alpha1.ComposedBundle, error) {
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = c.dynamic.Resource(composedBundleGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = c.dynamic.Resource(composedBundleGVR).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list composedbundles: %w", err)
	}
	result := make([]*v1alpha1.ComposedBundle, 0, len(list.Items))
	for _, item := range list.Items {
		cb := &v1alpha1.ComposedBundle{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, cb); err != nil {
			return nil, fmt.Errorf("convert composedbundle %s: %w", item.GetName(), err)
		}
		result = append(result, cb)
	}
	return result, nil
}

// GetComposedBundleCRD retrieves a single ComposedBundle CRD by name and namespace.
func (c *ClientsetClient) GetComposedBundleCRD(ctx context.Context, name, namespace string) (*v1alpha1.ComposedBundle, error) {
	obj, err := c.dynamic.Resource(composedBundleGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cb := &v1alpha1.ComposedBundle{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cb); err != nil {
		return nil, fmt.Errorf("convert composedbundle: %w", err)
	}
	return cb, nil
}

// ApplyComposedBundleCRD creates or updates a ComposedBundle CRD.
func (c *ClientsetClient) ApplyComposedBundleCRD(ctx context.Context, cr *v1alpha1.ComposedBundle) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert composedbundle to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("ComposedBundle")
	u.SetResourceVersion("")
	_, err = c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing composedbundle for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// CreateComposedBundleCRD creates a ComposedBundle CRD (create-only).
// Returns an AlreadyExists error (wrapped) when the CR already exists.
// Used for the bus config CRUD path where the caller wants to observe
// the conflict rather than silently fall through to update.
func (c *ClientsetClient) CreateComposedBundleCRD(ctx context.Context, cr *v1alpha1.ComposedBundle) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert composedbundle to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("ComposedBundle")
	_, err = c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// UpdateComposedBundleCRD updates a ComposedBundle CRD.
// When the passed CR carries a non-empty resourceVersion, it is passed to
// Update() verbatim (so a stale RV surfaces as a k8s Conflict).
// When RV is empty, the method fetches the existing CR and applies the
// caller's spec/labels/annotations onto it (last-write-wins semantics).
func (c *ClientsetClient) UpdateComposedBundleCRD(ctx context.Context, cr *v1alpha1.ComposedBundle) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert composedbundle to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("ComposedBundle")

	if cr.ResourceVersion != "" {
		u.SetResourceVersion(cr.ResourceVersion)
		_, err = c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
		return err
	}

	// RV-less: get-then-set (last-write-wins).
	existing, getErr := c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(composedBundleGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteComposedBundleCRD deletes a ComposedBundle CRD by name and namespace.
func (c *ClientsetClient) DeleteComposedBundleCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(composedBundleGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchComposedBundleStatus patches the status subresource of a ComposedBundle CRD.
func (c *ClientsetClient) PatchComposedBundleStatus(ctx context.Context, name, namespace string, status *v1alpha1.ComposedBundleStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	// Convert to a map so we can force-include the clearable fields below.
	statusMap := map[string]interface{}{}
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	if err := json.Unmarshal(statusBytes, &statusMap); err != nil {
		return fmt.Errorf("unmarshal status: %w", err)
	}
	// errors/warnings/message carry `omitempty`, so when the current status no
	// longer has them they are dropped from the JSON merge patch — leaving a
	// stale prior value in etcd (e.g. a compose error persisting after a later
	// successful recompose). Set them explicitly so the merge patch clears them.
	statusMap["errors"] = status.Errors
	statusMap["warnings"] = status.Warnings
	statusMap["message"] = status.Message

	patch := map[string]interface{}{
		"status": statusMap,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	_, err = c.dynamic.Resource(composedBundleGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// ---------------------------------------------------------------------------
// BroodOperation CRD operations (namespaced)
// ---------------------------------------------------------------------------

// ListBroodOperationCRDs retrieves all BroodOperation CRDs, optionally filtered
// by namespace.
func (c *ClientsetClient) ListBroodOperationCRDs(ctx context.Context, namespace string) ([]*v1alpha1.BroodOperation, error) {
	var list *unstructured.UnstructuredList
	var err error
	if namespace != "" {
		list, err = c.dynamic.Resource(broodOperationGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		list, err = c.dynamic.Resource(broodOperationGVR).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("list broodoperations: %w", err)
	}
	result := make([]*v1alpha1.BroodOperation, 0, len(list.Items))
	for _, item := range list.Items {
		bo := &v1alpha1.BroodOperation{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, bo); err != nil {
			return nil, fmt.Errorf("convert broodoperation %s: %w", item.GetName(), err)
		}
		result = append(result, bo)
	}
	return result, nil
}

// GetBroodOperationCRD retrieves a single BroodOperation CRD by name and namespace.
func (c *ClientsetClient) GetBroodOperationCRD(ctx context.Context, name, namespace string) (*v1alpha1.BroodOperation, error) {
	obj, err := c.dynamic.Resource(broodOperationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	bo := &v1alpha1.BroodOperation{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, bo); err != nil {
		return nil, fmt.Errorf("convert broodoperation: %w", err)
	}
	return bo, nil
}

// ApplyBroodOperationCRD creates or updates a BroodOperation CRD.
func (c *ClientsetClient) ApplyBroodOperationCRD(ctx context.Context, cr *v1alpha1.BroodOperation) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert broodoperation to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("BroodOperation")
	u.SetResourceVersion("")

	if cr.Name != "" {
		// Named CR - try update first, fall back to create.
		existing, getErr := c.dynamic.Resource(broodOperationGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr == nil {
			u.SetResourceVersion(existing.GetResourceVersion())
			_, err = c.dynamic.Resource(broodOperationGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
			return err
		}
	}

	created, err := c.dynamic.Resource(broodOperationGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(broodOperationGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing broodoperation for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		created, err = c.dynamic.Resource(broodOperationGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	if err != nil {
		return err
	}
	// Write the server object back so generateName-derived names (and
	// resourceVersion) reach the caller — the BFF returns the created CR and
	// stamps status by name.
	return runtime.DefaultUnstructuredConverter.FromUnstructured(created.Object, cr)
}

// DeleteBroodOperationCRD deletes a BroodOperation CRD by name and namespace.
func (c *ClientsetClient) DeleteBroodOperationCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(broodOperationGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchBroodOperationStatus patches the status subresource of a BroodOperation CRD.
func (c *ClientsetClient) PatchBroodOperationStatus(ctx context.Context, name, namespace string, status *v1alpha1.BroodOperationStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	statusMap := map[string]interface{}{}
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	if err := json.Unmarshal(statusBytes, &statusMap); err != nil {
		return fmt.Errorf("unmarshal status: %w", err)
	}
	patch := map[string]interface{}{
		"status": statusMap,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	_, err = c.dynamic.Resource(broodOperationGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// ---------------------------------------------------------------------------
// ProvisioningDefaults CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// GetProvisioningDefaultsCRD retrieves a cluster-scoped ProvisioningDefaults CRD by name.
func (c *ClientsetClient) GetProvisioningDefaultsCRD(ctx context.Context, name string) (*v1alpha1.ProvisioningDefaults, error) {
	obj, err := c.dynamic.Resource(provisioningDefaultsGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pd := &v1alpha1.ProvisioningDefaults{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, pd); err != nil {
		return nil, fmt.Errorf("convert provisioningdefaults: %w", err)
	}
	return pd, nil
}

// ApplyProvisioningDefaultsCRD creates or updates a cluster-scoped ProvisioningDefaults CRD.
func (c *ClientsetClient) ApplyProvisioningDefaultsCRD(ctx context.Context, cr *v1alpha1.ProvisioningDefaults) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert provisioningdefaults to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("ProvisioningDefaults")
	_, err = c.dynamic.Resource(provisioningDefaultsGVR).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(provisioningDefaultsGVR).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing provisioningdefaults for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(provisioningDefaultsGVR).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// ---------------------------------------------------------------------------
// JenkinsVersionProfile CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// ListJenkinsVersionProfileCRDs lists all JenkinsVersionProfile CRDs (cluster-scoped).
func (c *ClientsetClient) ListJenkinsVersionProfileCRDs(ctx context.Context) ([]*v1alpha1.JenkinsVersionProfile, error) {
	list, err := c.dynamic.Resource(jenkinsVersionProfileGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list jenkinsversionprofiles: %w", err)
	}
	result := make([]*v1alpha1.JenkinsVersionProfile, 0, len(list.Items))
	for _, item := range list.Items {
		jvp := &v1alpha1.JenkinsVersionProfile{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, jvp); err != nil {
			return nil, fmt.Errorf("convert jenkinsversionprofile %s: %w", item.GetName(), err)
		}
		result = append(result, jvp)
	}
	return result, nil
}

// GetJenkinsVersionProfileCRD retrieves a single JenkinsVersionProfile CRD by name (cluster-scoped).
func (c *ClientsetClient) GetJenkinsVersionProfileCRD(ctx context.Context, name string) (*v1alpha1.JenkinsVersionProfile, error) {
	obj, err := c.dynamic.Resource(jenkinsVersionProfileGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	jvp := &v1alpha1.JenkinsVersionProfile{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, jvp); err != nil {
		return nil, fmt.Errorf("convert jenkinsversionprofile: %w", err)
	}
	return jvp, nil
}

// CreateJenkinsVersionProfileCRD creates a JenkinsVersionProfile CRD (create-only, cluster-scoped).
// Returns AlreadyExists when the CR already exists.
func (c *ClientsetClient) CreateJenkinsVersionProfileCRD(ctx context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsversionprofile to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsVersionProfileGVK)
	_, err = c.dynamic.Resource(jenkinsVersionProfileGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// UpdateJenkinsVersionProfileCRD updates a JenkinsVersionProfile CRD (cluster-scoped).
// A non-empty resourceVersion is passed through so stale writes surface as k8s conflicts.
func (c *ClientsetClient) UpdateJenkinsVersionProfileCRD(ctx context.Context, cr *v1alpha1.JenkinsVersionProfile) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert jenkinsversionprofile to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(v1alpha1.JenkinsVersionProfileGVK)

	if cr.ResourceVersion != "" {
		u.SetResourceVersion(cr.ResourceVersion)
		_, err = c.dynamic.Resource(jenkinsVersionProfileGVR).Update(ctx, u, metav1.UpdateOptions{})
		return err
	}

	existing, getErr := c.dynamic.Resource(jenkinsVersionProfileGVR).Get(ctx, cr.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	u.SetResourceVersion(existing.GetResourceVersion())
	_, err = c.dynamic.Resource(jenkinsVersionProfileGVR).Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// DeleteJenkinsVersionProfileCRD deletes a JenkinsVersionProfile CRD by name.
func (c *ClientsetClient) DeleteJenkinsVersionProfileCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(jenkinsVersionProfileGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// ControllerClass CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// ListControllerClassCRDs lists all ControllerClass CRDs (cluster-scoped).
func (c *ClientsetClient) ListControllerClassCRDs(ctx context.Context) ([]*v1alpha1.ControllerClass, error) {
	list, err := c.dynamic.Resource(controllerClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list controllerclasses: %w", err)
	}
	result := make([]*v1alpha1.ControllerClass, 0, len(list.Items))
	for _, item := range list.Items {
		cc := &v1alpha1.ControllerClass{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, cc); err != nil {
			return nil, fmt.Errorf("convert controllerclass %s: %w", item.GetName(), err)
		}
		result = append(result, cc)
	}
	return result, nil
}

// GetControllerClassCRD retrieves a single ControllerClass CRD by name (cluster-scoped).
func (c *ClientsetClient) GetControllerClassCRD(ctx context.Context, name string) (*v1alpha1.ControllerClass, error) {
	obj, err := c.dynamic.Resource(controllerClassGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cc := &v1alpha1.ControllerClass{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cc); err != nil {
		return nil, fmt.Errorf("convert controllerclass: %w", err)
	}
	return cc, nil
}

// CreateOrUpdateConfigMapWithOwner creates or updates a ConfigMap with the given
// owner reference set on its metadata.
func (c *ClientsetClient) CreateOrUpdateConfigMapWithOwner(ctx context.Context, name, namespace string, data map[string]string, owner metav1.OwnerReference) error {
	return c.CreateOrUpdateConfigMap(ctx, name, namespace, data, owner)
}

// PatchJenkinsVersionProfileStatus patches the status subresource of a
// JenkinsVersionProfile CRD (cluster-scoped).
func (c *ClientsetClient) PatchJenkinsVersionProfileStatus(ctx context.Context, name string, status *v1alpha1.JenkinsVersionProfileStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	statusMap := map[string]interface{}{
		"status": status,
	}
	patchBytes, err := json.Marshal(statusMap)
	if err != nil {
		return fmt.Errorf("marshal jenkinsversionprofile status patch: %w", err)
	}
	_, err = c.dynamic.Resource(jenkinsVersionProfileGVR).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// ---------------------------------------------------------------------------
// UpdateCenter CRD operations (cluster-scoped)
// ---------------------------------------------------------------------------

// PatchUpdateCenterStatus patches the status subresource of an UpdateCenter CRD
// (cluster-scoped). All status fields are sent unconditionally every call so the
// reconciler can clear fields.
func (c *ClientsetClient) PatchUpdateCenterStatus(ctx context.Context, name string, status *v1alpha1.UpdateCenterStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	statusMap := map[string]interface{}{
		"status": map[string]interface{}{
			"phase":                   status.Phase,
			"conditions":              status.Conditions,
			"gaps":                    status.Gaps,
			"pluginCount":             status.PluginCount,
			"storeBytes":              status.StoreBytes,
			"seedImportedDigests":     status.SeedImportedDigests,
			"lastSyncTime":            status.LastSyncTime,
			"resolvedMetadataSources": status.ResolvedMetadataSources,
		},
	}
	patchBytes, err := json.Marshal(statusMap)
	if err != nil {
		return fmt.Errorf("marshal updatecenter status patch: %w", err)
	}
	_, err = c.dynamic.Resource(updateCentersGVR).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// GetUpdateCenter retrieves a single UpdateCenter CRD by name (cluster-scoped).
func (c *ClientsetClient) GetUpdateCenter(ctx context.Context, name string) (*v1alpha1.UpdateCenter, error) {
	obj, err := c.dynamic.Resource(updateCentersGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	uc := &v1alpha1.UpdateCenter{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, uc); err != nil {
		return nil, fmt.Errorf("convert updatecenter: %w", err)
	}
	return uc, nil
}

// GetPVC retrieves a PersistentVolumeClaim by namespace and name.
func (c *ClientsetClient) GetPVC(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	pvc, err := c.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pvc, nil
}

// ---------------------------------------------------------------------------
// User CRD operations
// ---------------------------------------------------------------------------

// GetUserCRD fetches a single User CRD by name and namespace.
func (c *ClientsetClient) GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error) {
	obj, err := c.dynamic.Resource(userGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	user := &v1alpha1.User{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, user); err != nil {
		return nil, fmt.Errorf("convert user: %w", err)
	}
	return user, nil
}

// ApplyUserCRD creates or updates a User CRD.
func (c *ClientsetClient) ApplyUserCRD(ctx context.Context, cr *v1alpha1.User) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert user to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("User")
	// Clear fields that are forbidden on Create (the caller may have
	// fetched a live object, which carries resourceVersion etc.).
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetCreationTimestamp(metav1.Time{})
	u.SetGeneration(0)
	u.SetManagedFields(nil)
	_, err = c.dynamic.Resource(userGVR).Namespace(cr.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(userGVR).Namespace(cr.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing user for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(userGVR).Namespace(cr.Namespace).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// PatchUserStatus patches the status subresource of a User CRD.
func (c *ClientsetClient) PatchUserStatus(ctx context.Context, name, namespace string, status *v1alpha1.UserStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	patch := map[string]interface{}{
		"status": status,
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal user status patch: %w", err)
	}
	_, err = c.dynamic.Resource(userGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// ClearUserPassword clears spec.password on a User CRD.
func (c *ClientsetClient) ClearUserPassword(ctx context.Context, name, namespace string) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"password": "",
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal clear password patch: %w", err)
	}
	_, err = c.dynamic.Resource(userGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	return err
}

// ListUserCRDs returns all User CRDs in the given namespace.
func (c *ClientsetClient) ListUserCRDs(ctx context.Context, namespace string) ([]*v1alpha1.User, error) {
	list, err := c.dynamic.Resource(userGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var users []*v1alpha1.User
	for _, item := range list.Items {
		u := &v1alpha1.User{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, u); err != nil {
			return nil, fmt.Errorf("convert user: %w", err)
		}
		users = append(users, u)
	}
	return users, nil
}

// DeleteUserCRD deletes a User CRD. Idempotent: not-found is not an error.
func (c *ClientsetClient) DeleteUserCRD(ctx context.Context, name, namespace string) error {
	err := c.dynamic.Resource(userGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Group CRD operations
// ---------------------------------------------------------------------------

// ListGroupCRDs returns all Group CRDs (cluster-scoped).
func (c *ClientsetClient) ListGroupCRDs(ctx context.Context) ([]*v1alpha1.Group, error) {
	list, err := c.dynamic.Resource(groupGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var groups []*v1alpha1.Group
	for _, item := range list.Items {
		g := &v1alpha1.Group{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, g); err != nil {
			return nil, fmt.Errorf("convert group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// GetGroupCRD fetches a single Group CRD by name (cluster-scoped).
func (c *ClientsetClient) GetGroupCRD(ctx context.Context, name string) (*v1alpha1.Group, error) {
	obj, err := c.dynamic.Resource(groupGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	g := &v1alpha1.Group{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, g); err != nil {
		return nil, fmt.Errorf("convert group: %w", err)
	}
	return g, nil
}

// ApplyGroupCRD creates or updates a Group CRD (cluster-scoped).
func (c *ClientsetClient) ApplyGroupCRD(ctx context.Context, cr *v1alpha1.Group) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert group to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("Group")
	_, err = c.dynamic.Resource(groupGVR).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(groupGVR).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing group for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(groupGVR).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// DeleteGroupCRD deletes a Group CRD. Idempotent: not-found is not an error.
func (c *ClientsetClient) DeleteGroupCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(groupGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ListTeamCRDs lists all Team CRDs (cluster-scoped).
func (c *ClientsetClient) ListTeamCRDs(ctx context.Context) ([]*v1alpha1.Team, error) {
	list, err := c.dynamic.Resource(teamGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var teams []*v1alpha1.Team
	for _, item := range list.Items {
		t := &v1alpha1.Team{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, t); err != nil {
			return nil, fmt.Errorf("convert team: %w", err)
		}
		teams = append(teams, t)
	}
	return teams, nil
}

// GetTeamCRD fetches a single Team CRD by name (cluster-scoped).
func (c *ClientsetClient) GetTeamCRD(ctx context.Context, name string) (*v1alpha1.Team, error) {
	obj, err := c.dynamic.Resource(teamGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	t := &v1alpha1.Team{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, t); err != nil {
		return nil, fmt.Errorf("convert team: %w", err)
	}
	return t, nil
}

// ApplyTeamCRD creates or updates a Team CRD (cluster-scoped).
func (c *ClientsetClient) ApplyTeamCRD(ctx context.Context, cr *v1alpha1.Team) error {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		return fmt.Errorf("convert team to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("varroa.dev/v1alpha1")
	u.SetKind("Team")
	_, err = c.dynamic.Resource(teamGVR).Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.dynamic.Resource(teamGVR).Get(ctx, cr.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing team for update: %w", getErr)
		}
		u.SetResourceVersion(existing.GetResourceVersion())
		_, err = c.dynamic.Resource(teamGVR).Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// DeleteTeamCRD deletes a Team CRD. Idempotent: not-found is not an error.
func (c *ClientsetClient) DeleteTeamCRD(ctx context.Context, name string) error {
	err := c.dynamic.Resource(teamGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// PatchTeamStatus patches the status subresource of a Team CRD (cluster-scoped).
func (c *ClientsetClient) PatchTeamStatus(ctx context.Context, name string, status *v1alpha1.TeamStatus) error {
	if status == nil {
		return fmt.Errorf("status must not be nil")
	}
	statusMap := map[string]interface{}{
		"status": status,
	}
	patchBytes, err := json.Marshal(statusMap)
	if err != nil {
		return fmt.Errorf("marshal team status patch: %w", err)
	}
	_, err = c.dynamic.Resource(teamGVR).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
	)
	return err
}

// DeleteControllerPod deletes the Jenkins pod for a controller (hard restart).
// The StatefulSet recreates it. Idempotent: a missing pod is not an error.
func (c *ClientsetClient) DeleteControllerPod(ctx context.Context, namespace, name string) error {
	// The StatefulSet (and therefore its pod) is named "<name>-<uid8>-0", not
	// the bare CR name, so we must resolve the CR to build the real pod name
	// via PodName. Deleting "<name>-0" would miss every UID-named controller.
	cr, err := c.GetControllerCRD(ctx, name, namespace)
	if err != nil {
		return err
	}
	pod := PodName(cr, 0)
	if derr := c.clientset.CoreV1().Pods(namespace).Delete(ctx, pod, metav1.DeleteOptions{}); derr != nil {
		if apierrors.IsNotFound(derr) {
			return nil
		}
		return derr
	}
	return nil
}

// GetControllerPod returns the Jenkins pod for a controller, or nil when it
// does not exist.
func (c *ClientsetClient) GetControllerPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	cr, err := c.GetControllerCRD(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	podName := PodName(cr, 0)
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pod, nil
}

// StreamPodLogs streams stdout+stderr from a container in the named pod.
func (c *ClientsetClient) StreamPodLogs(ctx context.Context, namespace, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Container: container,
		Follow:    follow,
		TailLines: &tailLines,
	}
	return c.clientset.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
}

// ListResourceQuotas lists ResourceQuota objects in the given namespace.
func (c *ClientsetClient) ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error) {
	list, err := c.clientset.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListIngressHosts returns a map from hostname to a slice of "namespace/name"
// ingress identifiers that claim it.
func (c *ClientsetClient) ListIngressHosts(ctx context.Context) (map[string][]string, error) {
	list, err := c.clientset.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	hosts := make(map[string][]string)
	for _, ing := range list.Items {
		owner := ing.Namespace + "/" + ing.Name
		for _, rule := range ing.Spec.Rules {
			if rule.Host != "" {
				hosts[rule.Host] = append(hosts[rule.Host], owner)
			}
		}
	}
	return hosts, nil
}

// toUnstructured converts a typed struct to an unstructured object via JSON round-trip.
func toUnstructured(v interface{}) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// ---------------------------------------------------------------------------
// Namespace methods (tenancy.NamespaceClient interface)
// ---------------------------------------------------------------------------

// GetNamespace returns the namespace, or a k8s IsNotFound error if it does not exist.
func (c *ClientsetClient) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	return c.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
}

// CreateNamespace creates the namespace with the given labels.
func (c *ClientsetClient) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	_, err := c.clientset.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
	return err
}

// PatchNamespaceLabels merges labels onto an existing namespace via a JSON merge patch.
func (c *ClientsetClient) PatchNamespaceLabels(ctx context.Context, name string, labels map[string]string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	_, err = c.clientset.CoreV1().Namespaces().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Compile-time assertion that ClientsetClient implements tenancy.NamespaceClient.
var _ tenancy.NamespaceClient = (*ClientsetClient)(nil)
