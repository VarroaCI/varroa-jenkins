package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/mite"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
	"github.com/varroaci/varroa-jenkins/internal/plugininv"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

const (
	provisioningTimeout         = 5 * time.Minute
	pluginRollTimeout           = 10 * time.Minute
	finalizerName               = "varroa.dev/finalizer"
	annotationAllowAdminLockout = "varroa.dev/allow-admin-lockout"
	annotationWakeRequested     = "varroa.dev/wake-requested"    // RFC3339Nano stamp; never consumed
	annotationForceReprovision  = "varroa.dev/force-reprovision" // RFC3339Nano stamp; consumed by the owner

	// defaultMaxConcurrentReconciles limits how many controllers can be in Reconcile() at once.
	defaultMaxConcurrentReconciles = 8

	// minReconcileInterval is the floor for per-controller interval configuration.
	minReconcileInterval = 10 * time.Second

	// defaultReconcileInterval is used when no policy or ProvisioningDefaults are configured.
	defaultReconcileInterval = 30 * time.Second

	// restartHeadroomSec is the budget added to DrainTimeoutSeconds for
	// SafeRestart+WaitForJenkins so drain never starves the restart wait.
	restartHeadroomSec int64 = 600
)

func (r *Reconciler) reconcileFleetPodLabel(ctx context.Context, cr *v1alpha1.Controller) {
	patched, err := r.client.EnsureStatefulSetPodLabel(ctx, cr.Namespace, controllerPrefix(cr), "app.kubernetes.io/managed-by", "varroa-operator")
	if err != nil {
		r.Logger.Error("failed to label controller pods for fleet netpol", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		return
	}
	if patched {
		r.Logger.Info("labeled controller pods for fleet netpol; rolling", "controller", cr.Namespace+"/"+cr.Name)
	}
}

var (
	reconcileDuration       metric.Int64Histogram
	reconcileErrors         metric.Int64Counter
	reconcileBlockedGauge   metric.Int64Gauge
	jcascResolveDuration    metric.Float64Histogram
	pluginLockConflictGauge metric.Int64Gauge
	miteImageStaleGauge     metric.Int64Gauge
)

func init() { bindControllerMetrics() }

// bindControllerMetrics (re)binds the package-level instruments against the
// current global MeterProvider. It runs once at package init; tests re-invoke
// it after swapping in a test MeterProvider so they exercise the real package
// instruments (and the real recording call sites) rather than a throwaway
// local gauge — validating the actual metric name/description/attribute wiring
// that production code emits.
func bindControllerMetrics() {
	m := otel.Meter("varroa-operator")
	reconcileDuration, _ = m.Int64Histogram("varroa.reconcile.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Reconciler tick duration"),
	)
	reconcileErrors, _ = m.Int64Counter("varroa.reconcile.errors",
		metric.WithDescription("Reconciler error count"),
	)
	reconcileBlockedGauge, _ = m.Int64Gauge("varroa.controller.reconcile.blocked",
		metric.WithDescription("1 if the controller's reconcile loop is currently blocked by an unresolved error, 0 otherwise"),
	)
	jcascResolveDuration, _ = m.Float64Histogram("varroa.jcasc.resolve.duration",
		metric.WithUnit("s"),
	)
	pluginLockConflictGauge, _ = m.Int64Gauge("varroa.controller.plugin_lock_conflict",
		metric.WithDescription("1 if ConditionPluginConflict is currently True for the controller, 0 otherwise (mirrors ConditionPluginConflict)"),
	)
	miteImageStaleGauge, _ = m.Int64Gauge("varroa.controller.mite_image_stale",
		metric.WithDescription("1 if the controller's running mite image differs from the operator-desired image, 0 if current"),
	)
}

// miteTokenEntry caches a signed JWT for a mite together with its expiry.
type miteTokenEntry struct {
	token string
	exp   time.Time
}

// Reconciler reconciles Controller CRs.
type Reconciler struct {
	Resolver          *bundle.Resolver
	Composer          *bundle.Composer
	rbacGenerator     *rbac.Generator
	client            ResourceClient
	store             crdstore.Backend
	miteTransport     transport.Transport
	tokenSigner       *mite.TokenSigner
	miteTokenSigner   *mite.MiteTokenSigner
	miteTokenMu       sync.Mutex
	miteTokens        map[string]miteTokenEntry // key "ns/name"; cached signed JWT
	lastMiteEpoch     map[string]int64          // key "ns/name"; for reconnect detect
	varroaEndpoint    string
	varroaRedirectURL string
	clusterName       string
	disconnectedTicks map[string]int
	Logger            *slog.Logger
	caPEM             string
	ucBaseURL         string // VARROA_UPDATE_CENTER_URL, read once at boot

	reconcileEvents   chan event.GenericEvent // controller-runtime source.Channel: on-demand reconcile enqueue (wired in SetupWithManager)
	activityPublisher *activity.Publisher
	seenControllers   map[string]bool // key: "namespace/name", prevents duplicate created events
	wakePort          int32
	wakeEnabled       bool
	wakePodMu         sync.Mutex
	wakePodIPs        []string
	wakePodFetchedAt  time.Time
	wakeSliceMu       sync.Mutex
	wakeSliceState    map[types.NamespacedName]bool // true only after confirmed absence

	// per-controller fields
	maxConcurrentReconciles int                            // max concurrent Controller reconciles
	provisioningDefaults    *v1alpha1.ProvisioningDefaults // cached defaults, refreshed each fetch
	provisioningDefaultsMu  sync.Mutex                     // guards provisioningDefaults
	operatorNamespace       string                         // namespace where the operator runs; for reading profile contentRef CMs
	// versionRollGate decides whether a detected version/image delta may roll now.
	// Default allows everything; change B (guard-version-upgrade-path) installs a
	// real gate at construction. Contract: design.md section 4 of fix-version-driven-upgrade.
	versionRollGate func(ctx context.Context, cr *v1alpha1.Controller, currentImage, targetImage string) (allow bool, reason, message string)
	tenancy         *tenancy.Classifier // namespace classifier for the operator gate; nil ⇒ gate skipped

	// shardRing and shardSet provide shard-based ownership gating for
	// active/active controller reconciliation. Nil ⇒ owns all (pre-wire default).
	shardRing *sharding.Ring
	shardSet  *sharding.ShardSet
}

// SetVarroaEndpoint sets the gRPC endpoint that mite sidecars use to connect back to Varroa.
func (r *Reconciler) SetVarroaEndpoint(endpoint string) {
	r.varroaEndpoint = endpoint
}

// SetClusterName sets the dashboard cluster path segment for controller links.
func (r *Reconciler) SetClusterName(name string) {
	r.clusterName = name
}

// SetVarroaRedirectURL sets the OIDC redirect URL for login URL derivation.
func (r *Reconciler) SetVarroaRedirectURL(url string) {
	r.varroaRedirectURL = url
}

// SetMiteTokenSigner sets the RS256 token signer for mite Jenkins auth.
func (r *Reconciler) SetMiteTokenSigner(s *mite.MiteTokenSigner) {
	r.miteTokenSigner = s
}

// SetCAPEM sets the CA PEM string for apikey verify endpoint TLS.
func (r *Reconciler) SetCAPEM(pem string) {
	r.caPEM = pem
}

// SetUpdateCenterBaseURL sets the in-cluster UC service base URL for
// resolving init-container plugin update center URLs.
func (r *Reconciler) SetUpdateCenterBaseURL(baseURL string) {
	r.ucBaseURL = strings.TrimRight(baseURL, "/")
}

// resolvePluginUpdateCenterURLs resolves the plugin update center URLs for the
// init container using the 3-tier precedence:
//  1. explicit ProvisioningDefaults.pluginUpdateCenterURL/DownloadURL (user override)
//  2. in-cluster UC URL when the UpdateCenter CR is Ready
//  3. "" (tool default upstream)
//
// url and downloadURL resolve independently.
func resolvePluginUpdateCenterURLs(defaults *v1alpha1.ProvisioningDefaults, uc *v1alpha1.UpdateCenter, ucBaseURL string) (url, downloadURL string) {
	ucReady := uc != nil && ucConditionTrue(uc.Status.Conditions, "Ready") && ucBaseURL != ""

	// URL (metadata endpoint)
	if defaults != nil && defaults.Spec.PluginUpdateCenterURL != "" {
		url = defaults.Spec.PluginUpdateCenterURL
	} else if ucReady {
		url = ucBaseURL + "/update-center.actual.json"
	}
	// else: url stays "" (tool default upstream)

	// Download URL
	if defaults != nil && defaults.Spec.PluginUpdateCenterDownloadURL != "" {
		downloadURL = defaults.Spec.PluginUpdateCenterDownloadURL
	} else if ucReady {
		// The in-cluster UC serves blobs under /download/plugins/<name>/<version>/<file>
		// (see internal/updatecenter.Server routes); jenkins-plugin-cli appends
		// <name>/<version>/<file> to this base, so the /plugins segment must be included.
		downloadURL = ucBaseURL + "/download/plugins"
	}
	// else: downloadURL stays ""

	return url, downloadURL
}

// ucConditionTrue returns true if the condition with the given type exists
// and has Status=True.
func ucConditionTrue(conditions []v1alpha1.UpdateCenterCondition, condType string) bool {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func (r *Reconciler) mitePubKeyPEM() string {
	if r.miteTokenSigner == nil {
		return ""
	}
	return r.miteTokenSigner.PublicKeyPEM()
}

func (r *Reconciler) mitePubKeyKID() string {
	if r.miteTokenSigner == nil {
		return ""
	}
	return r.miteTokenSigner.KID()
}

// apikeyVerifyURL returns the URL for the apikey verify endpoint. Uses env
// VARROA_APIKEY_VERIFY_URL if set, otherwise derives from the gateway endpoint.
func (r *Reconciler) apikeyVerifyURL() string {
	if v := os.Getenv("VARROA_APIKEY_VERIFY_URL"); v != "" {
		return v
	}
	if r.varroaEndpoint == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(r.varroaEndpoint)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("https://%s:9092/v1/verify-apikey", host)
}

// SetActivityPublisher sets the activity publisher for emitting operator events.
func (r *Reconciler) SetActivityPublisher(pub *activity.Publisher) {
	r.activityPublisher = pub
}

// getComposedBundle looks up a ComposedBundle by name in exactly the given namespace.
// There is deliberately no cross-namespace fallback (see bundle-namespace-scoping):
// a bundle in another namespace must be referenced via spec.composedBundleRef.namespace.
func (r *Reconciler) getComposedBundle(ctx context.Context, name, ns string) (*v1alpha1.ComposedBundle, error) {
	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, r.store, name, ns)
	if err != nil {
		return nil, fmt.Errorf("composed bundle %q not found in namespace %q (set spec.composedBundleRef.namespace if it lives elsewhere): %w", name, ns, err)
	}
	return cb, nil
}

// resolvedBundleEmpty reports whether a composition produced no content in any
// section. A bundle with only plugins, items, or rbac (and no jenkins.yaml) is
// still valid, so emptiness is judged across all sections, not jenkins alone.
func resolvedBundleEmpty(rb *bundle.MaterializedBundle) bool {
	return rb == nil ||
		(rb.JenkinsYAML == "" && rb.PluginsYAML == "" && rb.ItemsYAML == "" && rb.RbacYAML == "")
}

// bundleIdentity uniquely names a ComposedBundle for cross-namespace sibling matching.
type bundleIdentity struct{ Name, Namespace string }

// effectiveBundleRef resolves the ComposedBundle a Controller uses.
//
// A nil spec.composedBundleRef is not an error: it falls back by convention to
// the built-in starter bundle the operator seeds into its own namespace, so
// `kubectl apply` of a bare Controller provisions a working Jenkins. The
// fallback is a convention rather than a configurable default deliberately —
// ProvisioningDefaults is optional (its fetch error is discarded), so a field
// there could not be relied on at this point in the reconcile.
//
// This is the single place the fallback is decided inside the reconciler, and it
// delegates to v1alpha1.EffectiveBundleRef so that the BFF and brood-operation
// filtering give the same answer. A caller that dereferences
// spec.composedBundleRef directly reintroduces both the nil panic this replaced
// and the "zero-config controllers have no bundle" bug.
func (r *Reconciler) effectiveBundleRef(cr *v1alpha1.Controller) bundleIdentity {
	name, ns := v1alpha1.EffectiveBundleRef(cr, r.getOperatorNamespace())
	return bundleIdentity{Name: name, Namespace: ns}
}

// resolveBundleForController loads the materialized bundle content for a
// controller from the referenced ComposedBundle's ConfigMap, injects per-controller
// variables, and runs the completeness check. It returns an error if:
// - The ComposedBundle does not exist
// - The bundle is not Ready (controller should wait in Provisioning)
// - The content ConfigMap is missing
// - Unresolved ${var} placeholders remain after injection
//
// It also returns the bundle's ResolvedHash and resolved identity so callers can
// use them as cross-sibling gate keys without an extra API read.
func (r *Reconciler) resolveBundleForController(ctx context.Context, cr *v1alpha1.Controller) (*bundle.MaterializedBundle, string, bundleIdentity, error) {
	ident := r.effectiveBundleRef(cr)
	cb, err := r.getComposedBundle(ctx, ident.Name, ident.Namespace)
	if err != nil {
		return nil, "", bundleIdentity{}, fmt.Errorf("get composed bundle %s: %w", ident.Name, err)
	}

	// Wait if bundle is not Ready or Drifted. Drifted bundles have valid
	// content (drift is a warning); only Invalid/Pending should block.
	if cb.Status.Phase != v1alpha1.ComposedBundleReady &&
		cb.Status.Phase != v1alpha1.ComposedBundleDrifted {
		// Surface the bundle phase and any errors on the controller status.
		msg := fmt.Sprintf("waiting for composed bundle %s to be Ready (current phase: %s)", cb.Name, cb.Status.Phase)
		if len(cb.Status.Errors) > 0 {
			msg += fmt.Sprintf("; bundle errors: %s", strings.Join(cb.Status.Errors, ", "))
		}
		return nil, "", bundleIdentity{}, fmt.Errorf("%s", msg)
	}

	// Read the materialized content from the ConfigMap.
	if cb.Status.ContentRef == "" {
		return nil, "", bundleIdentity{}, fmt.Errorf("composed bundle %s has no contentRef", cb.Name)
	}

	data, err := r.client.GetConfigMap(ctx, cb.Status.ContentRef, cb.Namespace)
	if err != nil {
		return nil, "", bundleIdentity{}, fmt.Errorf("read content configmap %s: %w", cb.Status.ContentRef, err)
	}

	// Build MaterializedBundle from ConfigMap data.
	mat := &bundle.MaterializedBundle{
		JenkinsYAML:    data["jenkins.yaml"],
		PluginsYAML:    data["plugins.yaml"],
		ItemsYAML:      data["items.yaml"],
		RbacYAML:       data["rbac.yaml"],
		RawPluginsYAML: data["plugins.yaml"],
	}

	if resolvedBundleEmpty(mat) {
		return nil, "", bundleIdentity{}, fmt.Errorf("materialized bundle content is empty")
	}

	// Restore bundle-defined variables from the ConfigMap.
	vars := make(bundle.Variables)
	if varYAML := data["variables.yaml"]; varYAML != "" {
		for _, line := range strings.Split(varYAML, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				vars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	// Inject varroa_controller_* variables (highest priority).
	// The endpoint must point at the UID-named Service (controllerPrefix(cr)+"-svc",
	// created in handleProvisioning), NOT the bare CR name — agents connect to this
	// URL and "<name>-svc" resolves to a stale/non-existent Service (no endpoints →
	// connection refused).
	vars["varroa_controller_name"] = cr.Name
	vars["varroa_controller_namespace"] = cr.Namespace
	baseEndpoint := fmt.Sprintf("http://%s-svc.%s.svc.cluster.local:8080", controllerPrefix(cr), cr.Namespace)
	vars["varroa_controller_endpoint"] = baseEndpoint
	vars["varroa_frontend_url"] = varroaBaseURL(r.varroaRedirectURL)

	// Inject mode-aware routing vars.
	extURL := ""
	pathPref := ""
	host := ""
	if cr.Spec.IngressSpec != nil {
		host = cr.Spec.IngressSpec.Host
	}
	if host == "" {
		defaults, _ := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
		if defaults != nil && defaults.Spec.RootDomain != "" {
			isSubdomain := cr.Spec.IngressSpec == nil || cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModeSubdomain
			if isSubdomain {
				host = cr.Name + "." + defaults.Spec.RootDomain
			}
		}
	}
	if host != "" {
		if cr.Spec.IngressSpec != nil && cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModePath {
			pp := v1alpha1.PathPrefix(cr.Namespace, cr.Name)
			extURL = "https://" + host + pp
			pathPref = pp
			vars["varroa_controller_endpoint"] = baseEndpoint + pp
		} else {
			extURL = "https://" + host
		}
	}
	vars["varroa_controller_external_url"] = extURL
	vars["varroa_controller_path_prefix"] = pathPref

	if r.Resolver != nil {
		if r.Resolver.OIDCIssuer() != "" {
			vars["varroa_oidc_issuer"] = r.Resolver.OIDCIssuer()
			vars["varroa_oidc_client_id"] = r.Resolver.OIDCClientID()
		}
		loginURL := r.Resolver.LoginURL(r.varroaRedirectURL)
		if loginURL != "" {
			vars["varroa_login_url"] = loginURL
		}
	}

	// Merge version-profile JCasC overlay before ResolveVars so that
	// ${varroa_controller_*} injected variables resolve in the overlay too.
	if profile, _ := r.resolveProfileForCr(cr); profile != nil && profile.Spec.JCasC != nil && strings.TrimSpace(profile.Spec.JCasC.Content) != "" {
		if merged, mErr := bundle.MergeJenkinsYAML(mat.JenkinsYAML, profile.Spec.JCasC.Content); mErr == nil {
			mat.JenkinsYAML = merged
		} else if r.Logger != nil {
			r.Logger.Warn("version-profile JCasC overlay merge failed; using base bundle", "profile", profile.Name, "error", mErr)
		}
	}

	// Resolve variables in all content.
	mat.Variables = vars
	{
		resolveStart := time.Now()
		mat.JenkinsYAML = bundle.ResolveVars(mat.JenkinsYAML, vars)
		mat.PluginsYAML = bundle.ResolveVars(mat.PluginsYAML, vars)
		mat.ItemsYAML = bundle.ResolveVars(mat.ItemsYAML, vars)
		mat.RbacYAML = bundle.ResolveVars(mat.RbacYAML, vars)
		duration := time.Since(resolveStart).Seconds()
		result := "success"
		unresolved := findUnresolvedVars(true, mat.JenkinsYAML, mat.RbacYAML)
		for _, v := range findUnresolvedVars(false, mat.PluginsYAML, mat.ItemsYAML) {
			if !slices.Contains(unresolved, v) {
				unresolved = append(unresolved, v)
			}
		}
		if len(unresolved) > 0 {
			result = "error"
		}
		jcascResolveDuration.Record(ctx, duration, metric.WithAttributes(
			attribute.String("result", result),
		))
		if len(unresolved) > 0 {
			return nil, "", bundleIdentity{}, fmt.Errorf("unresolved variables after injection: %s", strings.Join(unresolved, ", "))
		}
	}

	// Inject location URL whenever the controller has a resolvable external URL (both modes).
	if vars["varroa_controller_external_url"] != "" {
		out, overrode, lerr := bundle.InjectLocationURL(mat.JenkinsYAML, vars["varroa_controller_external_url"]+"/")
		if lerr != nil {
			return nil, "", bundleIdentity{}, fmt.Errorf("inject location url: %w", lerr)
		}
		if overrode {
			r.Logger.Info("overrode bundle-provided location url", "controller", cr.Name, "namespace", cr.Namespace)
		}
		mat.JenkinsYAML = out
	}

	return mat, cb.Status.ResolvedHash, ident, nil
}

// findUnresolvedVars scans content for remaining ${var} placeholders.
// JCasC literal escapes (^${var}) are skipped: Jenkins strips the caret and
// the value is not Varroa's to resolve. JCasC secret-source references
// (${readFile:...}, ${base64:...}) are also skipped: they must reach Jenkins
// byte-for-byte so configuration-as-code's own SecretSourceResolver can
// resolve them at Jenkins startup.
func findUnresolvedVars(allowJcascSecretSources bool, contents ...string) []string {
	var found []string
	seen := make(map[string]bool)
	for _, c := range contents {
		for {
			start := strings.Index(c, "${")
			if start < 0 {
				break
			}
			rel := strings.Index(c[start:], "}")
			if rel < 0 {
				break
			}
			end := start + rel
			escaped := start > 0 && c[start-1] == '^'
			varName := c[start+2 : end]
			if !escaped && (!allowJcascSecretSources || !bundle.IsJCascSecretSourceRef(varName)) && !seen[varName] {
				found = append(found, varName)
				seen[varName] = true
			}
			c = c[end+1:]
		}
	}
	return found
}

// StatefulSetSpec carries the spec for a Jenkins + mite StatefulSet.
// Resources and MiteResources use corev1.ResourceRequirements (requests and
// limits). StorageSize and StorageClass are flat strings consumed directly
// by buildStatefulSet's PVC template. PersistenceSpec (Size/StorageClass)
// lives on the CR's spec.persistence and is wired in at the provisioning
// call site, not stored here as a nested type.
type StatefulSetSpec struct {
	Name            string
	Namespace       string
	ControllerName  string
	ClusterName     string
	JenkinsImage    string
	MiteImage       string
	BootstrapSecret string
	InitConfigMap   string
	CascConfigMap   string
	// CascContentHash is a deterministic hash of the CASC ConfigMap payload
	// (see cascContentHash), stamped as a pod-template annotation so a
	// content change rolls the pod even when the running container never
	// re-reads the ConfigMap on its own.
	CascContentHash               string
	PluginsConfigMap              string
	StorageSize                   string
	StorageClass                  string
	Resources                     *corev1.ResourceRequirements
	VarroaEndpoint                string
	ImagePullSecrets              []string
	ServiceAccountName            string
	OIDCIssuer                    string
	VarroaLoginURL                string
	OIDCUserClaim                 string
	OIDCGroupClaim                string
	MitePubKeyPEM                 string
	MitePubKeyKID                 string
	MiteImagePullPolicy           string
	MiteResources                 *corev1.ResourceRequirements
	ApikeyVerifyURL               string
	CAPEM                         string
	PathPrefix                    string
	Replicas                      *int32
	PluginsChecksum               string
	TerminationGracePeriodSec     int64
	DrainTimeoutSec               int64
	PluginUpdateCenterURL         string
	PluginUpdateCenterDownloadURL string
	VarroaBaseURL                 string

	// PodOverrides supplies typed, pod-scoped customizations merged onto the
	// Jenkins StatefulSet via strategic-merge patch.
	// HibernationIgnoreRegex is the activityIgnoreRegex from the controller's
	// hibernation spec. Set as VARROA_HIBERNATION_IGNORE_REGEX on the Jenkins
	// container so the plugin's HttpActivityFilter can exclude matching paths.
	HibernationIgnoreRegex string

	PodOverrides *v1alpha1.PodOverrides `json:"podOverrides,omitempty"`

	Probes *v1alpha1.ProbesSpec `json:"probes,omitempty"`

	// ResourceOverlay supplies raw strategic-merge-patch YAML for the
	// StatefulSet. Applied after PodOverrides (overlay overrides compile).
	ResourceOverlay *v1alpha1.ResourceOverlay `json:"resourceOverlay,omitempty"`

	// ResourcesSource records which tier supplied each container's
	// resources at provisioning time ("spec", "class", or "none"), keyed
	// by container name ("jenkins", "mite"). Stamped as a
	// varroa.dev/resources-source annotation on the StatefulSet so the
	// drift comparator can distinguish "spec block removed → roll" from
	// "never user-set → skip".
	ResourcesSource map[string]string `json:"resourcesSource,omitempty"`
}

// ResourceClient abstracts Kubernetes resource operations.
type ResourceClient interface {
	CreateService(ctx context.Context, name, namespace string, port int32, overlayYAML string) error
	CreateServiceAccount(ctx context.Context, name, namespace string) error
	CreateAgentRBAC(ctx context.Context, name, namespace string) error
	CreateStatefulSet(ctx context.Context, spec StatefulSetSpec) error
	UpdateStatefulSetOIDCEnv(ctx context.Context, name, namespace, oidcIssuer, loginURL, oidcUserClaim, oidcGroupClaim, pubKeyPEM, pubKeyKID, aud, apikeyVerifyURL, caPEM string) error
	EnsureStatefulSetPodLabel(ctx context.Context, namespace, stsName, key, value string) (bool, error)
	IsStatefulSetReady(ctx context.Context, name, namespace string) (bool, error)
	// GetStatefulSetPluginsChecksum returns the plugins-checksum annotation from the
	// live StatefulSet's pod template, or "" if the StatefulSet does not exist.
	GetStatefulSetPluginsChecksum(ctx context.Context, name, namespace string) (string, error)
	// GetStatefulSetImages returns the varroa.dev/computed-images stamp (nil when
	// absent/unparseable) and the live container images (containers + initContainers,
	// by name) of the controller StatefulSet. A missing StatefulSet returns
	// (nil, nil, nil) — callers treat nil live as "nothing to roll".
	GetStatefulSetImages(ctx context.Context, name, namespace string) (computed, live map[string]string, err error)
	// GetStatefulSetContainerSpecs returns the mite container's full independently-
	// editable spec surface — computed-images stamp entry, live image, live
	// resource requirements (requests+limits for both mite and Jenkins),
	// live image pull policy, and the varroa.dev/resources-source stamp —
	// from a single StatefulSet read, so reconcileContainerSpecRoll doesn't
	// need a separate GetStatefulSetImages call on top of this one (avoids a
	// redundant API read per controller per reconcile tick).
	// found=false when the StatefulSet does not exist yet — callers treat
	// that as "nothing to roll".
	GetStatefulSetContainerSpecs(ctx context.Context, name, namespace string) (
		computedMiteImage, liveMiteImage string,
		miteResources, jenkinsResources *corev1.ResourceRequirements,
		mitePullPolicy string,
		resourcesSource map[string]string,
		found bool, err error,
	)
	// ScaleStatefulSet sets the StatefulSet's replica count (power on/off).
	// Idempotent: a no-op when the count already matches, and not an error
	// when the StatefulSet does not exist.
	ScaleStatefulSet(ctx context.Context, name, namespace string, replicas int32) error
	CreateSecret(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error
	CreateSecretExclusive(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error
	CreateOrUpdateSecret(ctx context.Context, name, namespace string, data map[string][]byte) error
	PatchSecretData(ctx context.Context, name, namespace string, data map[string][]byte) error
	GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error)
	// GetSecretAnnotations returns the Secret's own annotations (not its Data),
	// used to enforce host-scoped credential use (varroa.dev/allowed-hosts)
	// for basic-auth git credentials.
	GetSecretAnnotations(ctx context.Context, name, namespace string) (map[string]string, error)
	ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error)
	// CopyImagePullSecret copies a Secret with its Type and Data preserved
	// from srcNamespace into dstNamespace. The destination is only written when
	// it differs from the source (equality guard), so repeated calls are
	// idempotent and do not bump resourceVersion on every tick. A missing source
	// Secret is silently ignored (the admin hasn't seeded it yet).
	CopyImagePullSecret(ctx context.Context, srcNamespace, dstNamespace, name string) error
	CreateIngress(ctx context.Context, name, namespace, host, pathPrefix, tlsSecret, ingressClass string, annotations map[string]string, overlayYAML string) error
	CreateOrUpdateConfigMap(ctx context.Context, name, namespace string, data map[string]string, owners ...metav1.OwnerReference) error
	GetConfigMap(ctx context.Context, name, namespace string) (map[string]string, error)
	// RemoveConfigMapLabel deletes labelKey from a ConfigMap's labels, leaving
	// its data untouched. A no-op success when the ConfigMap does not exist or
	// does not carry the label — used by promotion to strip the
	// version-profile-seed ownership label ahead of a content overwrite,
	// closing the race with the periodic seed reconciler.
	RemoveConfigMapLabel(ctx context.Context, name, namespace, labelKey string) error
	// UpdateConfigMapData replaces a ConfigMap's Data via a read-modify-write,
	// leaving Labels, Annotations, and OwnerReferences untouched — unlike
	// CreateOrUpdateConfigMap, whose Update path replaces the whole object and
	// would strip Helm's ownership metadata. Used by promotion to overwrite
	// the Helm-owned "<profile>-pluginset" ConfigMap's plugin lock content
	// without disturbing that metadata.
	UpdateConfigMapData(ctx context.Context, name, namespace string, data map[string]string) error
	DeleteResource(ctx context.Context, kind, name, namespace string) error
	DeleteSecret(ctx context.Context, name, namespace string) error
	EnsureWakeEndpointSlice(ctx context.Context, namespace, serviceName string, podIPs []string, port int32) error
	DeleteWakeEndpointSlice(ctx context.Context, namespace, serviceName string) error
	ListOperatorPodIPs(ctx context.Context, namespace string) ([]string, error)

	// GetLiveResource fetches a live Kubernetes resource by GVR, name, and
	// namespace. Returns nil, nil when the resource does not exist.
	GetLiveResource(ctx context.Context, gvr schema.GroupVersionResource, name, namespace string) (*unstructured.Unstructured, error)

	// ApplyControllerSpecSSA applies a sparse Controller spec via Kubernetes
	// server-side apply, completing the patch with the leaves this manager
	// already owns (read from metadata.managedFields) before applying. No typed
	// round-trip. The returned []bus.UnappliedRemoval names each requested
	// removal (explicit null) that did not take effect, tested against the
	// applied object's unstructured spec. On a field-manager conflict, returns
	// an error whose status causes are parseable via SSAConflicts.
	ApplyControllerSpecSSA(ctx context.Context, namespace, name string, specPatch map[string]any, fieldManager string, force bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error)
	// ApplyControllerSpecSSAIfExists is the existence-guarded sibling of
	// ApplyControllerSpecSSA: it performs its own GET, fails with a
	// recognizable apierrors NotFound rather than creating the object when
	// absent, and stamps that GET's resourceVersion into the apply so a
	// concurrent change between the GET and the apply surfaces as a
	// recognizable apierrors Conflict, and a concurrent delete fails the
	// apply rather than resurrecting the object (the stamped resourceVersion
	// is invalid on the create path the server takes for a missing object).
	// Existing ApplyControllerSpecSSA call sites are unaffected.
	ApplyControllerSpecSSAIfExists(ctx context.Context, namespace, name string, specPatch map[string]any, fieldManager string, force bool) (*v1alpha1.Controller, []bus.UnappliedRemoval, error)
	// SetHibernated is the single writer for status.hibernated and
	// status.hibernatedAt. It reads the live object, returns (false, nil)
	// when the flag already matches want, and otherwise writes the STATUS
	// SUBRESOURCE via Update carrying the fetched resourceVersion so a
	// concurrent write conflicts instead of being clobbered.
	SetHibernated(ctx context.Context, name, namespace string, want bool) (bool, error)
	PatchControllerStatus(ctx context.Context, name, namespace string, status *v1alpha1.ControllerStatus) error

	// CreateOrUpdateConfigMapWithOwner creates or updates a ConfigMap, setting
	// the given owner reference on the ConfigMap's metadata.
	CreateOrUpdateConfigMapWithOwner(ctx context.Context, name, namespace string, data map[string]string, owner metav1.OwnerReference) error

	// CreateOrUpdateOwnedConfigMap creates a ConfigMap, or updates one only
	// when a live object of that name already carries every entry in labels.
	// Get; not found -> Create with labels; found and labels absent or
	// mismatched -> ErrConfigMapNotOwned without writing; found and owned ->
	// resourceVersion-scoped Update; on Conflict, one bounded retry that
	// re-Gets and re-checks ownership rather than blindly retrying the write.
	CreateOrUpdateOwnedConfigMap(ctx context.Context, name, namespace string, data map[string]string, labels map[string]string) error

	ClearUserPassword(ctx context.Context, name, namespace string) error

	// StreamPodLogs streams stdout+stderr from a container in the given pod.
	// The caller must close the returned ReadCloser when done.
	StreamPodLogs(ctx context.Context, namespace, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error)

	// DeleteControllerPod deletes the Jenkins pod for a controller (hard restart).
	// The StatefulSet recreates it. Idempotent: a missing pod is not an error.
	DeleteControllerPod(ctx context.Context, namespace, name string) error

	// GetControllerPod returns the Jenkins pod for a controller. Returns nil
	// without error when the pod does not exist.
	GetControllerPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)

	// ListResourceQuotas lists ResourceQuota objects in the given namespace.
	ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error)

	// ListIngressHosts returns a map from hostname to a slice of "namespace/name"
	// ingress identifiers that claim it.
	ListIngressHosts(ctx context.Context) (map[string][]string, error)

	// GetNamespace returns the namespace, or a k8s IsNotFound error if it does not exist.
	GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error)

	// GetPVC retrieves a PersistentVolumeClaim by namespace and name.
	// Returns a typed not-found error when the resource does not exist.
	GetPVC(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error)
}

// allowAllVersionRolls is the default version-roll gate. Change B
// (guard-version-upgrade-path) replaces it at Reconciler construction.
func allowAllVersionRolls(context.Context, *v1alpha1.Controller, string, string) (bool, string, string) {
	return true, "", ""
}

// NewReconciler creates a new Reconciler.
func NewReconciler(r *bundle.Resolver, client ResourceClient, store crdstore.Backend, miteTransport transport.Transport, signer *mite.TokenSigner, rbGen *rbac.Generator, composer *bundle.Composer) *Reconciler {
	rec := &Reconciler{
		Resolver:                r,
		Composer:                composer,
		rbacGenerator:           rbGen,
		client:                  client,
		store:                   store,
		miteTransport:           miteTransport,
		tokenSigner:             signer,
		disconnectedTicks:       make(map[string]int),
		lastMiteEpoch:           make(map[string]int64),
		miteTokens:              make(map[string]miteTokenEntry),
		reconcileEvents:         make(chan event.GenericEvent, 128),
		seenControllers:         make(map[string]bool),
		wakeSliceState:          make(map[types.NamespacedName]bool),
		maxConcurrentReconciles: defaultMaxConcurrentReconciles,
		versionRollGate:         allowAllVersionRolls,
		clusterName:             "core",
	}
	// Replaces the default allow-all gate with the composed core↔plugin
	// compat + upgradePolicy gate. Assigned post-construction because it is a
	// method value bound to rec.
	rec.versionRollGate = rec.upgradePolicyVersionRollGate

	return rec
}

// SetWakeServerPort configures the wake listener and its EndpointSlice port.
func (r *Reconciler) SetWakeServerPort(port int32, enabled bool) {
	r.wakePort = port
	r.wakeEnabled = enabled
}

func (r *Reconciler) wakeSliceKey(cr *v1alpha1.Controller) types.NamespacedName {
	return types.NamespacedName{Namespace: cr.Namespace, Name: controllerPrefix(cr) + "-svc"}
}

func (r *Reconciler) wakeSliceKnownAbsent(cr *v1alpha1.Controller) bool {
	r.wakeSliceMu.Lock()
	defer r.wakeSliceMu.Unlock()
	return r.wakeSliceState[r.wakeSliceKey(cr)]
}

func (r *Reconciler) markWakeSlicePresent(cr *v1alpha1.Controller) {
	r.wakeSliceMu.Lock()
	delete(r.wakeSliceState, r.wakeSliceKey(cr))
	r.wakeSliceMu.Unlock()
}

func (r *Reconciler) deleteWakeSlice(ctx context.Context, cr *v1alpha1.Controller, logger *slog.Logger) {
	if r.wakeSliceKnownAbsent(cr) {
		return
	}
	if err := r.client.DeleteWakeEndpointSlice(ctx, cr.Namespace, controllerPrefix(cr)+"-svc"); err != nil {
		logger.Warn("delete wake EndpointSlice", "error", err)
		return
	}
	r.wakeSliceMu.Lock()
	r.wakeSliceState[r.wakeSliceKey(cr)] = true
	r.wakeSliceMu.Unlock()
}

func (r *Reconciler) operatorPodIPs(ctx context.Context) ([]string, error) {
	r.wakePodMu.Lock()
	if time.Since(r.wakePodFetchedAt) < 30*time.Second {
		ips := append([]string(nil), r.wakePodIPs...)
		r.wakePodMu.Unlock()
		return ips, nil
	}
	r.wakePodMu.Unlock()
	ips, err := r.client.ListOperatorPodIPs(ctx, r.getOperatorNamespace())
	if err != nil {
		// Back off on failure so a persistent error (e.g. missing RBAC) does not
		// re-list every ~1s hibernated tick, and keep serving the last-known-good
		// IPs through a transient blip rather than dropping the wake slice.
		r.wakePodMu.Lock()
		last := append([]string(nil), r.wakePodIPs...)
		r.wakePodFetchedAt = time.Now()
		r.wakePodMu.Unlock()
		if len(last) > 0 {
			return last, nil
		}
		return nil, err
	}
	if len(ips) == 0 {
		if podIP := os.Getenv("POD_IP"); podIP != "" {
			ips = []string{podIP}
		}
	}
	r.wakePodMu.Lock()
	r.wakePodIPs = append([]string(nil), ips...)
	r.wakePodFetchedAt = time.Now()
	r.wakePodMu.Unlock()
	return ips, nil
}

// TriggerReconcile enqueues an on-demand reconcile for the given controller.
func (r *Reconciler) TriggerReconcile(cluster, name, namespace string) {
	_ = cluster // operator always acts on its own cluster
	r.wakeController(namespace + "/" + name)
}

// SetProvisioningDefaults caches the cluster-wide ProvisioningDefaults for
// resolution of default reconciliation policies.
func (r *Reconciler) SetProvisioningDefaults(defaults *v1alpha1.ProvisioningDefaults) {
	r.provisioningDefaultsMu.Lock()
	r.provisioningDefaults = defaults
	r.provisioningDefaultsMu.Unlock()
}

// RootDomain returns the root domain from the cached ProvisioningDefaults.
func (r *Reconciler) RootDomain() string {
	r.provisioningDefaultsMu.Lock()
	defer r.provisioningDefaultsMu.Unlock()
	if r.provisioningDefaults == nil {
		return ""
	}
	return r.provisioningDefaults.Spec.RootDomain
}

// SetMaxConcurrentReconciles bounds how many Controller CRs reconcile
// concurrently. n <= 0 falls back to defaultMaxConcurrentReconciles.
func (r *Reconciler) SetMaxConcurrentReconciles(n int) {
	if n <= 0 {
		n = defaultMaxConcurrentReconciles
	}
	r.maxConcurrentReconciles = n
}

// SetOperatorNamespace sets the operator's own namespace for reading profile
// contentRef ConfigMaps.
func (r *Reconciler) SetOperatorNamespace(ns string) {
	r.operatorNamespace = ns
}

// SetTenancy sets the namespace classifier for the operator gate.
// Nil (unset) skips the gate.
func (r *Reconciler) SetTenancy(c *tenancy.Classifier) {
	r.tenancy = c
}

// SetShardOwnership wires the shard ring and held set for ownership gating.
// Nil values (or never called) mean the reconciler owns every controller,
// which is the pre-wire default and the test/unsharded behavior.
func (r *Reconciler) SetShardOwnership(ring *sharding.Ring, held *sharding.ShardSet) {
	r.shardRing = ring
	r.shardSet = held
}

// ownsController returns true if this replica owns the controller identified
// by namespace/name. When sharding is not wired (nil ring or set), owns all.
func (r *Reconciler) ownsController(ns, name string) bool {
	if r.shardRing == nil || r.shardSet == nil {
		return true
	}
	return r.shardSet.OwnsController(r.shardRing, ns, name)
}

// EnqueueShards re-enqueues every Controller whose key hashes into one of the
// given shards. Called by the ShardManager after acquiring shards. The list is
// retried: a controller missed here has no scheduled requeue on this replica
// and would strand until an external event (ResyncOwned is the safety net).
func (r *Reconciler) EnqueueShards(shards []int) {
	if r.shardRing == nil {
		return // sharding not wired
	}
	in := make(map[int]bool, len(shards))
	for _, s := range shards {
		in[s] = true
	}
	r.enqueueMatching(func(ns, name string) bool {
		return in[r.shardRing.ShardFor(ns+"/"+name)]
	}, "enqueue shards")
}

// ResyncOwned re-enqueues every Controller owned by this replica's shard set.
// Periodic safety net for shard handoffs that raced a failed list or a full
// event channel; runs on every replica.
func (r *Reconciler) ResyncOwned() {
	if r.shardRing == nil || r.shardSet == nil {
		return
	}
	r.enqueueMatching(func(ns, name string) bool {
		return r.shardSet.OwnsController(r.shardRing, ns, name)
	}, "resync owned")
}

// enqueueMatching lists all controllers (with retry) and enqueues those the
// predicate selects, blocking briefly rather than dropping when the event
// channel is full.
func (r *Reconciler) enqueueMatching(match func(ns, name string) bool, op string) {
	var crs []*v1alpha1.Controller
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var err error
		crs, err = crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
		cancel()
		if err == nil {
			break
		}
		if attempt >= 3 {
			r.Logger.Error(op+": list failed, giving up until next resync", "error", err, "attempts", attempt)
			return
		}
		r.Logger.Warn(op+": list failed, retrying", "error", err, "attempt", attempt)
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	for _, cr := range crs {
		if match(cr.Namespace, cr.Name) {
			r.enqueueReconcileBlocking(cr.Namespace + "/" + cr.Name)
		}
	}
}

// getOperatorNamespace returns the operator namespace, falling back to the
// OPERATOR_NAMESPACE env var, then the deprecated WATCH_NAMESPACE alias, then
// "varroa-system".
func (r *Reconciler) getOperatorNamespace() string {
	if r.operatorNamespace != "" {
		return r.operatorNamespace
	}
	if ns := os.Getenv("OPERATOR_NAMESPACE"); ns != "" {
		return ns
	}
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		return ns
	}
	return "varroa-system"
}

// ResolveOperatorNamespace returns the operator's own-resource namespace. Order:
// OPERATOR_NAMESPACE, then WATCH_NAMESPACE (deprecated alias — logs a one-time warning),
// then "varroa-system". It sets ONLY where the operator/BFF read/write their own resources;
// it scopes NO informer or reconciliation.
func ResolveOperatorNamespace(logger *slog.Logger) string {
	if v := os.Getenv("OPERATOR_NAMESPACE"); v != "" {
		return v
	}
	if v := os.Getenv("WATCH_NAMESPACE"); v != "" {
		if logger != nil {
			logger.Warn("WATCH_NAMESPACE is deprecated; use OPERATOR_NAMESPACE")
		}
		return v
	}
	return "varroa-system"
}

// resolveProfileForCr lists JenkinsVersionProfile CRDs and resolves the
// profile matching the controller's spec.version using the Resolution ladder.
func (r *Reconciler) resolveProfileForCr(cr *v1alpha1.Controller) (*v1alpha1.JenkinsVersionProfile, MatchKind) {
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](context.Background(), r.store, "", "")
	if err != nil {
		return nil, MatchBaseline
	}
	return ResolveProfile(cr.Spec.Version, profiles)
}

// resolveClassForCr resolves cr.Spec.ClassName to a ControllerClass.
// Returns (nil, true) when ClassName is unset — not an error, the class
// layer is simply skipped. Returns (nil, false) when ClassName is set but
// the object can't be fetched (not found OR any other read error) — the
// caller MUST fail closed and MUST NOT silently proceed as if unclassed.
func (r *Reconciler) resolveClassForCr(ctx context.Context, cr *v1alpha1.Controller) (*v1alpha1.ControllerClass, bool) {
	if cr.Spec.ClassName == "" {
		return nil, true
	}
	class, err := crdstore.Get[v1alpha1.ControllerClass](ctx, r.store, cr.Spec.ClassName, "")
	if err != nil || class == nil {
		return nil, false
	}
	return class, true
}

// persistStatusDiagnostics best-effort persists cr.Status ahead of an error
// return. Reconcile deliberately skips status persistence on failed passes
// (half-mutated state must not be committed wholesale), so error paths that
// record intentional, admin-facing diagnostics — blocking conditions, the
// Failed phase — must call this explicitly before returning their error, or
// the diagnostic keeps its last persisted value while the reconciler silently
// retries.
func (r *Reconciler) persistStatusDiagnostics(ctx context.Context, cr *v1alpha1.Controller) {
	if err := r.client.PatchControllerStatus(ctx, cr.Name, cr.Namespace, &cr.Status); err != nil {
		r.Logger.Warn("persist status diagnostics", "controller", cr.Namespace+"/"+cr.Name, "error", err)
	}
}

const reconcileBlockedMessageCap = 2048

// markReconcileBlocked sets ConditionReconcileBlocked=True with the given
// reason/message, stamps LastReconcileError/LastReconcileErrorAt, records
// the gauge, and persists via persistStatusDiagnostics. Called immediately
// before each of the 20 in-scope sites' existing `return`, so the returned
// error is unchanged — Reconcile's rate-limited-requeue behavior is
// unaffected.
func (r *Reconciler) markReconcileBlocked(ctx context.Context, cr *v1alpha1.Controller, reason, message string) {
	if len(message) > reconcileBlockedMessageCap {
		message = message[:reconcileBlockedMessageCap]
		// The cap is a byte length; a byte-boundary cut can split a multi-byte
		// UTF-8 rune. Drop the trailing partial rune so the value we persist
		// into the Kubernetes status stays valid UTF-8 (at most utf8.UTFMax-1
		// bytes are trimmed, only on the rare over-cap path).
		for len(message) > 0 && !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	now := metav1Now()
	// LastTransitionTime is deliberately left zero-valued here: setCondition's
	// stamp logic only advances it when the condition's Status actually flips,
	// matching the standard Kubernetes condition contract this codebase already
	// follows elsewhere. LastReconcileErrorAt is the separate, always-updated
	// "when did we last see this" timestamp.
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionReconcileBlocked,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	cr.Status.LastReconcileError = message
	cr.Status.LastReconcileErrorAt = &now
	reconcileBlockedGauge.Record(ctx, 1, metric.WithAttributes(
		attribute.String("namespace", cr.Namespace),
		attribute.String("controller", cr.Name),
	))
	r.persistStatusDiagnostics(ctx, cr)
}

// refreshClassResolvedCondition re-evaluates the ClassResolved condition from
// the current spec.className. handleProvisioning maintains the condition on
// the provisioning path; this covers Running/Connected ticks, where the spec
// requires it to stay live — a className added, changed, or whose class was
// deleted after provisioning must be reflected without waiting for the next
// provisioning pass. Mutates cr.Status.Conditions only; the tick's normal
// status persistence carries it.
func (r *Reconciler) refreshClassResolvedCondition(ctx context.Context, cr *v1alpha1.Controller) {
	class, ok := r.resolveClassForCr(ctx, cr)
	cond := v1alpha1.ControllerCondition{Type: v1alpha1.ConditionClassResolved}
	switch {
	case !ok:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonClassNotFound
		cond.Message = fmt.Sprintf("ControllerClass %q not found", cr.Spec.ClassName)
	case class != nil:
		cond.Status = metav1.ConditionTrue
		cond.Reason = v1alpha1.ReasonClassResolved
		cond.Message = fmt.Sprintf("ControllerClass %q resolved", class.Name)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = v1alpha1.ReasonNoClassConfigured
		cond.Message = "no className configured"
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, cond)
}

// isZeroClassSpec reports whether a ControllerClassSpec sets nothing at
// all. Uses reflect.DeepEqual, not ==, because ControllerClassSpec
// contains map/slice fields and is not a comparable type.
func isZeroClassSpec(spec v1alpha1.ControllerClassSpec) bool {
	return reflect.DeepEqual(spec, v1alpha1.ControllerClassSpec{})
}

// mergeClassPodDefaults returns a PodOverrides where every field the
// controller's own spec.podOverrides did NOT set falls back to the
// resolved ControllerClass's equivalent field. Controller spec always
// wins per-field except NodeSelector/PodLabels/PodAnnotations (key-level
// merged) and JvmOpts (space-joined, additive). Only called from
// handleProvisioning — these fields are provisioning-time-only, not
// continuously reconciled for an already-Connected Controller.
func mergeClassPodDefaults(class *v1alpha1.ControllerClass, po *v1alpha1.PodOverrides) *v1alpha1.PodOverrides {
	if class == nil || isZeroClassSpec(class.Spec) {
		return po
	}
	eff := po.DeepCopy()
	if eff == nil {
		eff = &v1alpha1.PodOverrides{}
	}
	eff.NodeSelector = v1alpha1.MergeStringMaps(class.Spec.NodeSelector, eff.NodeSelector)
	if eff.Affinity == nil {
		eff.Affinity = class.Spec.Affinity
	}
	if eff.SecurityContext == nil {
		eff.SecurityContext = class.Spec.SecurityContext
	}
	if len(eff.Tolerations) == 0 {
		eff.Tolerations = class.Spec.Tolerations
	}
	eff.PodLabels = v1alpha1.MergeStringMaps(class.Spec.PodLabels, eff.PodLabels)
	eff.PodAnnotations = v1alpha1.MergeStringMaps(class.Spec.PodAnnotations, eff.PodAnnotations)
	eff.JvmOpts = strings.TrimSpace(class.Spec.JvmOpts + " " + eff.JvmOpts)
	return eff
}

// resolveCoreSet returns the resolved plugin set for the controller's version.
// When a profile is matched and PluginSetReady, it reads the materialized
// ConfigMap. Otherwise it falls back to the embedded baseline.
func (r *Reconciler) resolveCoreSet(ctx context.Context, cr *v1alpha1.Controller, profile *v1alpha1.JenkinsVersionProfile, logger *slog.Logger) []pluginlock.PluginEntry {
	if profile != nil && profileIsPluginSetReady(profile) && profile.Status.ContentRef != "" {
		cmData, err := r.client.GetConfigMap(ctx, profile.Status.ContentRef, r.getOperatorNamespace())
		if err == nil {
			if pluginsYAML, ok := cmData["plugins.yaml"]; ok && pluginsYAML != "" {
				var lockSet struct {
					Plugins []pluginlock.PluginEntry `yaml:"plugins"`
				}
				if err := yaml.Unmarshal([]byte(pluginsYAML), &lockSet); err == nil && len(lockSet.Plugins) > 0 {
					return lockSet.Plugins
				}
			}
		}
		if logger != nil {
			logger.Warn("failed to read profile contentRef, falling back to embedded baseline",
				"profile", profile.Name, "contentRef", profile.Status.ContentRef, "error", err)
		}
	}
	coreSet, _ := pluginlock.Resolve(cr.Spec.Version)
	return coreSet
}

// coreSetForCr resolves a controller's pinned plugin core set via its
// JenkinsVersionProfile (exact→line→embedded baseline). Used by BOTH provisioning
// and the connected-phase drift checks so the "desired" set compared against the
// baked plugins.txt is identical to what provisioning installed; otherwise a
// profile whose pinned set differs from the embedded baseline would report
// permanent spurious plugin drift.
func (r *Reconciler) coreSetForCr(ctx context.Context, cr *v1alpha1.Controller, logger *slog.Logger) []pluginlock.PluginEntry {
	profile, _ := r.resolveProfileForCr(cr)
	return r.resolveCoreSet(ctx, cr, profile, logger)
}

// profileIsPluginSetReady checks whether the profile has a PluginSetReady
// condition with Status==ConditionTrue.
func profileIsPluginSetReady(p *v1alpha1.JenkinsVersionProfile) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "PluginSetReady" && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// effectivePolicy resolves the effective ReconciliationPolicy for a controller.
// Per-controller spec > ProvisioningDefaults > hardcoded defaults.
// Fields are merged individually: setting only mode does not lose default timing values.
func (r *Reconciler) effectivePolicy(cr *v1alpha1.Controller) v1alpha1.ReconciliationPolicy {
	pol := v1alpha1.ReconciliationPolicy{
		Mode:                v1alpha1.ReconciliationModeAutomatic,
		MaxDeferSeconds:     1800,
		DrainTimeoutSeconds: 900,
		Interval:            &metav1.Duration{Duration: defaultReconcileInterval},
	}

	r.provisioningDefaultsMu.Lock()
	defs := r.provisioningDefaults
	r.provisioningDefaultsMu.Unlock()
	if defs != nil && defs.Spec.DefaultReconciliationPolicy != nil {
		dp := defs.Spec.DefaultReconciliationPolicy
		if dp.Mode != "" {
			pol.Mode = dp.Mode
		}
		if dp.MaxDeferSeconds > 0 {
			pol.MaxDeferSeconds = dp.MaxDeferSeconds
		}
		if dp.DrainTimeoutSeconds > 0 {
			pol.DrainTimeoutSeconds = dp.DrainTimeoutSeconds
		}
		if dp.Interval != nil {
			pol.Interval = dp.Interval
		}
	}

	if cr.Spec.ReconciliationPolicy != nil {
		cp := cr.Spec.ReconciliationPolicy
		if cp.Mode != "" {
			pol.Mode = cp.Mode
		}
		if cp.MaxDeferSeconds > 0 {
			pol.MaxDeferSeconds = cp.MaxDeferSeconds
		}
		if cp.DrainTimeoutSeconds > 0 {
			pol.DrainTimeoutSeconds = cp.DrainTimeoutSeconds
		}
		if cp.Interval != nil {
			pol.Interval = cp.Interval
		}
	}

	return pol
}

// effectiveInterval returns the reconciled interval for a controller, clamped
// to a sensible minimum.
func (r *Reconciler) effectiveInterval(cr *v1alpha1.Controller) time.Duration {
	policy := r.effectivePolicy(cr)
	if policy.Interval != nil && policy.Interval.Duration >= minReconcileInterval {
		return policy.Interval.Duration
	}
	if policy.Interval != nil && policy.Interval.Duration < minReconcileInterval && policy.Interval.Duration > 0 {
		logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
		logger.Warn("interval below minimum, clamping", "interval", policy.Interval.Duration, "min", minReconcileInterval)
	}
	return defaultReconcileInterval
}

// backoff returns the retry delay for a drain-timeout restart deferral.
// schedule: min(60s * 2^(attempt-1), 1800s) → 60s, 2m, 4m, 8m, 16m, 30m cap.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(60) * time.Second
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > 30*time.Minute {
			return 30 * time.Minute
		}
	}
	return d
}

// wakeController enqueues a reconcile for the controller at key "namespace/name".
// If this replica owns the controller (or sharding is not wired), it enqueues
// locally with no API write. Otherwise it stamps the wake-requested annotation
// so the owning replica picks it up on the next periodic tick or annotation watch.
func (r *Reconciler) wakeController(key string) {
	ns, name, ok := strings.Cut(key, "/")
	if !ok {
		return
	}
	if r.ownsController(ns, name) {
		r.enqueueReconcile(key)
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := crdstore.PatchAnnotations[v1alpha1.Controller](context.Background(), r.store, name, ns,
		map[string]*string{annotationWakeRequested: &ts}); err != nil {
		r.Logger.Warn("wake routing patch failed; owner will pick up on next periodic tick",
			"controller", key, "error", err)
	}
}

// enqueueReconcile pushes a controller-runtime reconcile event for the
// "namespace/name" key. Non-blocking: if the buffer is full the event is
// dropped, since the phase-based periodic requeue will still reconcile.
func (r *Reconciler) enqueueReconcile(key string) {
	if r.reconcileEvents == nil {
		return
	}
	namespace, name, ok := strings.Cut(key, "/")
	if !ok {
		return
	}
	ev := event.GenericEvent{Object: &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}}
	select {
	case r.reconcileEvents <- ev:
	default:
		r.Logger.Warn("reconcile event channel full, dropping on-demand trigger",
			"controller", key)
	}
}

// enqueueReconcileBlocking is enqueueReconcile for callers that must not lose
// the event (shard acquisition/resync, where no requeue is scheduled yet on
// this replica). Blocks up to 5s before dropping with an error log.
func (r *Reconciler) enqueueReconcileBlocking(key string) {
	if r.reconcileEvents == nil {
		return
	}
	namespace, name, ok := strings.Cut(key, "/")
	if !ok {
		return
	}
	ev := event.GenericEvent{Object: &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}}
	select {
	case r.reconcileEvents <- ev:
	case <-time.After(5 * time.Second):
		r.Logger.Error("reconcile event channel full for 5s, dropping shard enqueue (resync will retry)",
			"controller", key)
	}
}

// WakeController sends a non-blocking wake-up to a per-controller goroutine
// by namespace and name. This is the exported version used by API handlers.
func (r *Reconciler) WakeController(cluster, namespace, name string) {
	_ = cluster // operator always acts on its own cluster
	r.wakeController(namespace + "/" + name)
}

// WakeHibernatedController clears status.hibernated via the CAS helper, then
// wakes its reconcile goroutine so provisioning resumes promptly. It is a no-op
// for a controller that is not hibernated (a Stopped controller is never woken
// by traffic). Driven by wake commands the BFF publishes on wake.<cluster>.>.
func (r *Reconciler) WakeHibernatedController(ctx context.Context, namespace, name string) {
	changed, err := r.client.SetHibernated(ctx, name, namespace, false)
	if err != nil {
		r.Logger.Warn("wake hibernation clear failed", "controller", namespace+"/"+name, "error", err)
		return
	}
	if changed {
		r.Logger.Info("woke hibernated controller", "controller", namespace+"/"+name)
		r.wakeController(namespace + "/" + name)
	}
}

// ErrControllerStopped is returned by the authenticated hibernate and wake
// actions when the target controller has spec.powerState == "Stopped". Hard
// power beats hibernation, so neither action may run against a Stopped
// controller; the caller maps this to a conflict response.
var ErrControllerStopped = errors.New("controller is stopped (spec.powerState=Stopped)")

// HibernateController parks a controller on demand via the status
// compare-and-swap helper, independent of spec.hibernation.enabled, then
// nudges the owning replica so the hibernated branch scales the workload
// down promptly. It refuses a Stopped controller: hard power beats
// hibernation, and a Stopped controller must not be left with a stale
// hibernation flag.
func (r *Reconciler) HibernateController(ctx context.Context, namespace, name string) error {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, namespace)
	if err != nil {
		return fmt.Errorf("get controller: %w", err)
	}
	if cr.Spec.PowerState == "Stopped" {
		return ErrControllerStopped
	}
	if _, err := r.client.SetHibernated(ctx, name, namespace, true); err != nil {
		return fmt.Errorf("set hibernated: %w", err)
	}
	r.Logger.Info("hibernated controller", "controller", namespace+"/"+name)
	r.wakeController(namespace + "/" + name)
	return nil
}

// WakeControllerAction clears a controller's hibernation via the status
// compare-and-swap helper and nudges the owning replica so provisioning
// resumes promptly. It is the authenticated request-reply counterpart to
// WakeHibernatedController (the fire-and-forget traffic wake): it refuses a
// Stopped controller and returns errors to the caller instead of logging them.
func (r *Reconciler) WakeControllerAction(ctx context.Context, namespace, name string) error {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, namespace)
	if err != nil {
		return fmt.Errorf("get controller: %w", err)
	}
	if cr.Spec.PowerState == "Stopped" {
		return ErrControllerStopped
	}
	if _, err := r.client.SetHibernated(ctx, name, namespace, false); err != nil {
		return fmt.Errorf("clear hibernated: %w", err)
	}
	r.Logger.Info("woke hibernated controller", "controller", namespace+"/"+name)
	r.wakeController(namespace + "/" + name)
	return nil
}

// Hibernate is the ReconcilerAPI entry point for the authenticated hibernate
// action. The operator always acts on its own cluster, so the cluster argument
// is ignored — it exists to satisfy the interface shared with the BFF's
// NATSReconcilerProxy.
func (r *Reconciler) Hibernate(ctx context.Context, cluster, namespace, name string) error {
	_ = cluster
	return r.HibernateController(ctx, namespace, name)
}

// Wake is the ReconcilerAPI entry point for the authenticated wake action.
// The operator always acts on its own cluster, so the cluster argument is
// ignored — it exists to satisfy the interface shared with the BFF's
// NATSReconcilerProxy.
func (r *Reconciler) Wake(ctx context.Context, cluster, namespace, name string) error {
	_ = cluster
	return r.WakeControllerAction(ctx, namespace, name)
}

// Reprovision forces the next reconcile of the controller to re-push the full
// desired state to the mite, bypassing the convergence short-circuit. It always
// stamps the force-reprovision annotation (the annotation update IS the wake);
// the owning replica's watch fires on it.
func (r *Reconciler) Reprovision(cluster, namespace, name string) {
	_ = cluster // operator always acts on its own cluster
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := crdstore.PatchAnnotations[v1alpha1.Controller](context.Background(), r.store, name, namespace,
		map[string]*string{annotationForceReprovision: &ts}); err != nil {
		r.Logger.Warn("reprovision annotation patch failed", "controller", namespace+"/"+name, "error", err)
	}
}

// ApproveRestart applies a pending configuration change. action must be
// "reload" (JCasC reload), "restart" (drain-aware safe restart),
// "force-restart" (immediate restart), "approve" (release manual-drift push with idle gating),
// or "force" (release manual-drift push immediately).
func (r *Reconciler) ApproveRestart(ctx context.Context, cluster, namespace, name, action string) error {
	_ = cluster // operator always acts on its own cluster
	key := namespace + "/" + name
	logger := r.Logger.With("controller", key)
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, namespace)
	if err != nil {
		return fmt.Errorf("get controller: %w", err)
	}

	pol := r.effectivePolicy(cr)
	drainSec := int64(pol.DrainTimeoutSeconds)

	// force-restart: immediate restart, no drain, clear any backoff state.
	if action == "force-restart" {
		cmdID := fmt.Sprintf("%d", time.Now().UnixNano())
		if err := r.client.DeleteControllerPod(ctx, namespace, name); err != nil {
			return fmt.Errorf("force-restart via pod delete: %w", err)
		}
		cr.Status.RestartDrain = nil
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionRestartDeferred, Status: metav1.ConditionFalse, Reason: "ForceRestarted",
		})
		logger.Info("force-restart issued", "commandId", cmdID)
		if err := r.client.PatchControllerStatus(ctx, name, namespace, &cr.Status); err != nil {
			logger.Warn("failed to patch status after force-restart", "error", err)
		}
		r.emitOperatorEvent("restart.approved", cr, "force restart approved by user", "")
		r.wakeController(key)
		return nil
	}

	// Plugin-roll branch: when PendingPluginRoll is set, record the one-shot
	// approval checksum so the next reconcile bumps the annotation.
	if action == "plugin-roll" && cr.Status.PendingPluginRoll != nil {
		cr.Status.ApprovedPluginRollChecksum = cr.Status.PendingPluginRoll.TargetChecksum
		cr.Status.PendingPluginRoll = nil
		logger.Info("plugin-roll approved, will roll on next reconcile",
			"targetChecksum", cr.Status.ApprovedPluginRollChecksum)
		if err := r.client.PatchControllerStatus(ctx, name, namespace, &cr.Status); err != nil {
			return fmt.Errorf("persist plugin-roll approval: %w", err)
		}
		r.emitOperatorEvent("pluginRoll.approved", cr, "plugin roll approved by user", "")
		r.wakeController(key)
		return nil
	}

	if cr.Status.PendingRestart == nil {
		return fmt.Errorf("no pending restart for %s", key)
	}

	// Re-fetch bundle content from the materialized ConfigMap and resolve vars.
	resolvedBundle, _, _, err := r.resolveBundleForController(ctx, cr)
	if err != nil {
		return fmt.Errorf("resolve bundle: %w", err)
	}

	desired := r.buildDesiredStateCommand(cr, resolvedBundle)

	// Set the Reload flag on the DesiredStateCommand for "reload" action
	// before sending so the mite receives it.
	if action == "reload" {
		desired.Reload = true
	}

	desiredHash := computeDesiredStateHash(desired)

	// "restart" action: release the manual-mode push and issue a drain-aware
	// safe restart (re-honors idle gating).
	switch action {
	case "restart":
		desired.ApplyWhen = "idle"
		desired.MaxDeferSec = int64(pol.MaxDeferSeconds)
		desired.DrainTimeoutSec = int64(pol.DrainTimeoutSeconds)
	case "approve":
		desired.ApplyWhen = "idle"
		desired.MaxDeferSec = int64(pol.MaxDeferSeconds)
		desired.DrainTimeoutSec = int64(pol.DrainTimeoutSeconds)
	case "force":
		desired.ApplyWhen = "automatic"
		desired.MaxDeferSec = int64(pol.MaxDeferSeconds)
		desired.DrainTimeoutSec = int64(pol.DrainTimeoutSeconds)
	default: // reload
		// reload: push with Reload=true (existing behavior, no idle gating).
	}

	if err := r.miteTransport.Send(ctx, namespace, name, &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_DesiredState{
			DesiredState: desired,
		},
	}); err != nil {
		return fmt.Errorf("send desired state: %w", err)
	}

	if action == "restart" {
		cr.Status.DesiredStateHash = desiredHash
		if err := r.issueSafeRestart(ctx, cr, drainSec); err != nil {
			return fmt.Errorf("issue safe-restart: %w", err)
		}
		logger.Info("restart requested, desired state pushed with idle gating + safe-restart")
	} else {
		logger.Info("manual config action completed", "action", action)
	}

	// Clear the pending restart flag.
	cr.Status.PendingRestart = nil
	cr.Status.DesiredStateHash = desiredHash

	if err := r.client.PatchControllerStatus(ctx, name, namespace, &cr.Status); err != nil {
		logger.Warn("failed to clear pending restart", "action", action, "error", err)
	}

	r.emitOperatorEvent("restart.approved", cr, fmt.Sprintf("%s approved by user", action), "")

	r.wakeController(key)

	return nil
}

// issueSafeRestart generates a command ID, sends a SAFE_RESTART imperative
// with a deadline covering drain+restart, and sets RestartDrain for backoff
// tracking. This is the single owner of CommandID+cooldown bookkeeping.
func (r *Reconciler) issueSafeRestart(ctx context.Context, cr *v1alpha1.Controller, drainTimeoutSec int64) error {
	now := time.Now()
	cmdID := fmt.Sprintf("%d", now.UnixNano())
	if err := r.client.DeleteControllerPod(ctx, cr.Namespace, cr.Name); err != nil {
		return fmt.Errorf("restart via pod delete: %w", err)
	}

	attempt := 0
	if cr.Status.RestartDrain != nil {
		attempt = cr.Status.RestartDrain.AttemptCount
	}
	cr.Status.RestartDrain = &v1alpha1.RestartDrainStatus{
		DesiredStateHash: cr.Status.DesiredStateHash,
		CommandID:        cmdID,
		AttemptCount:     attempt,
		NextRetryAt:      &metav1.Time{Time: now.Add(time.Duration(drainTimeoutSec+restartHeadroomSec) * time.Second)},
	}
	return nil
}

// ApproveDeletion authorizes deleting a previously-deferred item even though a
// build may be running, issuing a targeted delete to the mite.
func (r *Reconciler) ApproveDeletion(ctx context.Context, cluster, namespace, name, path string) error {
	_ = cluster // operator always acts on its own cluster
	key := namespace + "/" + name
	logger := r.Logger.With("controller", key)
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, name, namespace)
	if err != nil {
		return fmt.Errorf("get controller: %w", err)
	}
	idx := -1
	for i, d := range cr.Status.PendingItemDeletions {
		if d.Path == path {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no pending deletion for %q on %s", path, key)
	}

	cmdID := fmt.Sprintf("%d", time.Now().UnixNano())
	if err := r.miteTransport.SendImperative(ctx, namespace, name, &mitev1.ImperativeCommand{
		CommandId: cmdID,
		Type:      mitev1.CommandTypeDeleteItem,
		Target:    path,
	}); err != nil {
		return fmt.Errorf("send delete-item: %w", err)
	}

	cr.Status.PendingItemDeletions = append(cr.Status.PendingItemDeletions[:idx], cr.Status.PendingItemDeletions[idx+1:]...)
	if len(cr.Status.PendingItemDeletions) == 0 {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionDeletionPending,
			Status: metav1.ConditionFalse,
			Reason: v1alpha1.ReasonNoDeletionPending,
		})
	}
	if err := r.client.PatchControllerStatus(ctx, name, namespace, &cr.Status); err != nil {
		logger.Warn("failed to clear pending deletion", "error", err)
	}
	r.emitOperatorEvent("deletion.approved", cr, "item deletion approved by user", path)
	r.wakeController(key)
	return nil
}

// computeChangedSections compares old and new component hashes and returns
// the names of sections that changed.
func computeChangedSections(snapshot *mitev1.StateSnapshot, desired *mitev1.DesiredStateCommand) []string {
	if snapshot == nil {
		var sections []string
		if desired.JcascYaml != "" {
			sections = append(sections, "config")
		}
		if desired.RbacYaml != "" {
			sections = append(sections, "rbac")
		}
		if desired.ItemsYaml != "" {
			sections = append(sections, "items")
		}
		if len(sections) == 0 {
			sections = append(sections, "unknown")
		}
		return sections
	}
	var changes []string
	newJcasc := sha256Hex([]byte(desired.JcascYaml))
	newRbac := sha256Hex([]byte(desired.RbacYaml))
	newItems := sha256Hex([]byte(desired.ItemsYaml))

	if newJcasc != "" && newJcasc != snapshot.ConfigHash {
		changes = append(changes, "config")
	}
	if newRbac != "" && newRbac != snapshot.RbacHash {
		changes = append(changes, "rbac")
	}
	if newItems != "" && newItems != snapshot.ItemsHash {
		changes = append(changes, "items")
	}
	if len(changes) == 0 {
		changes = append(changes, "unknown")
	}
	return changes
}

// reconcileController processes a Controller CR through the phase state machine.
func (r *Reconciler) reconcileController(ctx context.Context, cr *v1alpha1.Controller) (reconcileErr error) {
	start := time.Now()
	oldPhase := cr.Status.Phase
	tracer := otel.Tracer("varroa-operator")
	ctx, span := tracer.Start(ctx, "reconcile",
		trace.WithAttributes(
			attribute.String("controller", cr.Name),
			attribute.String("namespace", cr.Namespace),
			attribute.String("phase", string(cr.Status.Phase)),
		),
	)
	defer span.End()

	defer func() {
		duration := time.Since(start)
		result := "success"
		if reconcileErr != nil {
			result = "error"
			reconcileErrors.Add(ctx, 1, metric.WithAttributes(
				attribute.String("phase", string(cr.Status.Phase)),
			))
		} else if oldPhase != v1alpha1.ControllerPhaseFailed {
			if existing := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked); existing != nil && existing.Status == metav1.ConditionTrue {
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionReconcileBlocked,
					Status:  metav1.ConditionFalse,
					Reason:  "ReconcileSucceeded",
					Message: "",
				})
				cr.Status.LastReconcileError = ""
				cr.Status.LastReconcileErrorAt = nil
				reconcileBlockedGauge.Record(ctx, 0, metric.WithAttributes(
					attribute.String("namespace", cr.Namespace),
					attribute.String("controller", cr.Name),
				))
			}
		}
		reconcileDuration.Record(ctx, duration.Milliseconds(), metric.WithAttributes(
			attribute.String("phase", string(cr.Status.Phase)),
			attribute.String("result", result),
		))
	}()

	logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
	logger.Info("reconciling controller")

	// If the CR is being deleted, run finalization and remove the finalizer.
	if !cr.DeletionTimestamp.IsZero() {
		logger.Info("deletion detected, running finalization")
		hadFinalizer := containsString(cr.Finalizers, finalizerName)
		if err := r.Finalize(ctx, cr); err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedFinalizeFailed, err.Error())
			return fmt.Errorf("finalize: %w", err)
		}
		// Remove the mite's persisted desired state so it does not outlive the CR.
		if err := r.miteTransport.ClearDesired(ctx, cr.Namespace, cr.Name); err != nil {
			logger.Error("clear desired state failed", "error", err)
		}

		// Clean up token record.
		r.miteTokenMu.Lock()
		delete(r.miteTokens, cr.Namespace+"/"+cr.Name)
		delete(r.lastMiteEpoch, cr.Namespace+"/"+cr.Name)
		r.miteTokenMu.Unlock()

		// Reset both per-controller gauges to 0 on deletion so series do not
		// stick at 1 if the controller is deleted while in a conflict/stale state.
		// These are synchronous OTel gauges: the label series is not physically
		// removed, it resets to 0 and lingers there until the operator restarts —
		// so consumers must alert on == 1, not on series presence (see the C2/C4
		// specs and docs/operations/observability.md).
		pluginLockConflictGauge.Record(ctx, 0, metric.WithAttributes(
			attribute.String("namespace", cr.Namespace),
			attribute.String("controller", cr.Name),
		))
		miteImageStaleGauge.Record(ctx, 0, metric.WithAttributes(
			attribute.String("namespace", cr.Namespace),
			attribute.String("controller", cr.Name),
		))

		cr.Finalizers = removeString(cr.Finalizers, finalizerName)
		if hadFinalizer {
			r.emitOperatorEvent("controller.deleted", cr, "controller deleted", "")
		}
		return nil
	}

	r.refreshMiteImageStaleness(ctx, cr)

	var err error
	// Powered-off: scale the StatefulSet to 0 and report Stopped. This must
	// actually scale the workload down (the replica count is otherwise only
	// applied in handleProvisioning, which the Stopped phase never reaches),
	// so powering off a running controller really stops Jenkins. ScaleStatefulSet
	// is idempotent and a no-op when the StatefulSet is missing or already at 0.
	if cr.Spec.PowerState == "Stopped" {
		if serr := r.client.ScaleStatefulSet(ctx, controllerPrefix(cr), cr.Namespace, 0); serr != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedScaleDownFailed, fmt.Sprintf("scale down statefulset (phase %s, powerState Stopped): %v", cr.Status.Phase, serr))
			return fmt.Errorf("scale down statefulset: %w", serr)
		}
		// Hard power beats hibernation: clear any stale hibernation flag so a
		// later power-on cannot resurrect it. Must go through SetHibernated —
		// an in-memory assignment is silently dropped, because the flag is
		// excluded from the end-of-reconcile PatchControllerStatus.
		if cr.Status.Hibernated {
			if _, herr := r.client.SetHibernated(ctx, cr.Name, cr.Namespace, false); herr != nil {
				logger.Warn("clear hibernation flag on stop", "error", herr)
			}
		}
		clearMiteConnection(cr)
		if oldPhase != v1alpha1.ControllerPhaseStopped {
			cr.Status.Phase = v1alpha1.ControllerPhaseStopped
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "PoweredOff",
				Message: "controller is stopped (spec.powerState=Stopped)",
			})
		}
		r.deleteWakeSlice(ctx, cr, logger)
		return nil
	}
	// Hibernated: scale the StatefulSet to 0 and report Hibernated. Mirrors the
	// Stopped branch above except the reason and phase names differ. Gated on
	// status.hibernated — hard power (Stopped) beats hibernation.
	if cr.Status.Hibernated && cr.Spec.PowerState != "Stopped" {
		if serr := r.client.ScaleStatefulSet(ctx, controllerPrefix(cr), cr.Namespace, 0); serr != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedScaleDownFailed, fmt.Sprintf("scale down statefulset for hibernation (phase %s, status.hibernated): %v", cr.Status.Phase, serr))
			return fmt.Errorf("scale down statefulset for hibernation: %w", serr)
		}
		// Mint wake token if missing (D5).
		if cr.Status.WakeToken == "" {
			newToken, mintErr := mintWakeToken(cr.Spec.Hibernation, cr.Status.Hibernated, cr.Status.WakeToken)
			if mintErr != nil {
				logger.Warn("failed to mint wake token", "error", mintErr)
			} else if newToken != "" {
				cr.Status.WakeToken = newToken
			}
		}
		clearMiteConnection(cr)
		if oldPhase != v1alpha1.ControllerPhaseHibernated {
			cr.Status.Phase = v1alpha1.ControllerPhaseHibernated
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.ReasonHibernated,
				Message: "controller is hibernated (status.hibernated=true)",
			})
		}
		if r.wakeEnabled {
			ips, ipErr := r.operatorPodIPs(ctx)
			if ipErr != nil {
				logger.Warn("list operator pod IPs for wake EndpointSlice", "error", ipErr)
			} else if len(ips) == 0 {
				logger.Warn("no ready operator pod IPs for wake EndpointSlice")
			} else if sliceErr := r.client.EnsureWakeEndpointSlice(ctx, cr.Namespace, controllerPrefix(cr)+"-svc", ips, r.wakePort); sliceErr != nil {
				logger.Warn("ensure wake EndpointSlice", "error", sliceErr)
			} else {
				r.markWakeSlicePresent(cr)
			}
		} else {
			r.deleteWakeSlice(ctx, cr, logger)
		}
		return nil
	}
	// Powered back on: transition out of Stopped to Pending. handlePending
	// resets ProvisioningStartedAt, so re-provisioning (which scales the
	// StatefulSet back to 1) does not immediately hit the provisioning timeout.
	if oldPhase == v1alpha1.ControllerPhaseStopped {
		cr.Status.Phase = v1alpha1.ControllerPhasePending
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "PoweredOn",
			Message: "controller is powering on",
		})
		return nil
	}
	// Leaving Hibernated: transition to Pending (re-provision path) and clear
	// the HibernationCronTriggersSkipped condition.
	if oldPhase == v1alpha1.ControllerPhaseHibernated && !cr.Status.Hibernated {
		cr.Status.Phase = v1alpha1.ControllerPhasePending
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Waking",
			Message: "controller is waking from hibernation",
		})
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionHibernationCronTriggersSkipped,
			Status: metav1.ConditionFalse,
			Reason: "Waking",
		})
		return nil
	}

	// reconcileSharedHostMismatch for all active phases: set Degraded/True
	// when a path-mode controller's Host differs from the dashboard host;
	// clear the condition when the mismatch is resolved.
	r.reconcileSharedHostMismatch(cr)

	// Wake-token mint per D5: generate a 128-bit token when hibernation
	// is enabled (or the controller is already hibernated) and no token exists.
	// Persist immediately — the active-phase handlers below do not patch status,
	// so an in-memory-only assignment would be re-minted (and discarded) every
	// tick and never reach the wake surface.
	if cr.Status.WakeToken == "" {
		newToken, mintErr := mintWakeToken(cr.Spec.Hibernation, cr.Status.Hibernated, cr.Status.WakeToken)
		if mintErr != nil {
			logger.Warn("failed to mint wake token", "error", mintErr)
		} else if newToken != "" {
			cr.Status.WakeToken = newToken
			if perr := r.client.PatchControllerStatus(ctx, cr.Name, cr.Namespace, &cr.Status); perr != nil {
				logger.Warn("failed to persist wake token", "error", perr)
			}
		}
	}

	// Keep the Service and Ingress converged after provisioning.
	// handleProvisioning creates them initially; once the controller is past
	// provisioning, reconcile updates to ports, TLS, annotations, and host on
	// every tick. Non-fatal: an update failure must not wedge an otherwise-
	// healthy controller.
	rolled := false
	switch oldPhase {
	case v1alpha1.ControllerPhaseRunning, v1alpha1.ControllerPhaseConnected:
		r.refreshClassResolvedCondition(ctx, cr)
		if err := r.reconcileService(ctx, cr); err != nil {
			r.Logger.Warn("reconcile service", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		if err := r.reconcileIngress(ctx, cr); err != nil {
			r.Logger.Warn("reconcile ingress", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		rolled = r.reconcileVersionRoll(ctx, cr)
		if !rolled {
			rolled = r.reconcileContainerSpecRoll(ctx, cr)
		}
		r.deleteWakeSlice(ctx, cr, logger)
	}

	if !rolled {
		switch oldPhase {
		case "":
			err = r.handlePending(ctx, cr)
			// Emit controller.created only once per CR.
			key := cr.Namespace + "/" + cr.Name
			if err == nil && !r.seenControllers[key] {
				r.seenControllers[key] = true
				r.emitOperatorEvent("controller.created", cr, "controller created", "")
			}
		case v1alpha1.ControllerPhasePending:
			err = r.handlePending(ctx, cr)
		case v1alpha1.ControllerPhaseProvisioning:
			err = r.handleProvisioning(ctx, cr)
		case v1alpha1.ControllerPhaseRunning:
			err = r.handleRunning(ctx, cr)
		case v1alpha1.ControllerPhaseConnected:
			err = r.handleConnected(ctx, cr)
		case v1alpha1.ControllerPhaseStopped:
			// Idle: StatefulSet is at 0 replicas. No action needed.
			return nil
		case v1alpha1.ControllerPhaseHibernated:
			// Parked: StatefulSet is at 0 replicas. No action needed.
			return nil
		case v1alpha1.ControllerPhaseFailed:
			r.handleFailed(ctx, cr)
		default:
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedUnknownPhase, fmt.Sprintf("unknown phase: %s", cr.Status.Phase))
			return fmt.Errorf("unknown phase: %s", cr.Status.Phase)
		}
	}

	if err != nil {
		return err
	}

	// Sync RBAC bindings from rbacSpec on every non-deleting pass.
	if syncErr := r.syncRBACBindings(ctx, cr); syncErr != nil {
		r.Logger.Warn("sync RBAC bindings failed", "error", syncErr)
	}

	// Sync the OverlayActive condition on every non-deleting pass.
	cr.Status.Conditions = setCondition(cr.Status.Conditions, overlayActiveCondition(cr))

	// Emit phase transition events if the phase changed.
	if cr.Status.Phase != oldPhase {
		r.emitPhaseEvent(cr, oldPhase)
	}

	return nil
}

func (r *Reconciler) handlePending(ctx context.Context, cr *v1alpha1.Controller) error {
	tracer := otel.Tracer("varroa-operator")
	ctx, span := tracer.Start(ctx, "reconcile.handlePending")
	defer span.End()

	// Add finalizer (persisted by the operator loop via PatchControllerFinalizers).
	if !containsString(cr.Finalizers, finalizerName) {
		cr.Finalizers = append(cr.Finalizers, finalizerName)
	}

	// Tenancy gate: if the namespace is not managed (scoped mode), hold in Pending
	// with ConditionDegraded / ReasonTargetNamespaceUnmanaged.
	if r.tenancy != nil {
		if state, err := r.tenancy.Classify(ctx, cr.Namespace); err == nil {
			if state == tenancy.NamespaceUnmanaged {
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
					Reason:             v1alpha1.ReasonTargetNamespaceUnmanaged,
					Message:            "namespace " + cr.Namespace + " is not in managedNamespaces; add it and helm upgrade",
					LastTransitionTime: metav1Now(),
				})
				return nil // hold in Pending; requeue on normal tick
			}
			// Ready: reason-scoped clear so we never clobber a Degraded set by another subsystem.
			if d := findCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded); d != nil &&
				d.Status == metav1.ConditionTrue && d.Reason == v1alpha1.ReasonTargetNamespaceUnmanaged {
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionFalse,
					Reason: v1alpha1.ReasonTargetNamespaceReady, LastTransitionTime: metav1Now(),
				})
			}
		}
	}

	// Verify the effective ComposedBundle exists. Any phase (including Invalid
	// and Pending) is accepted — the controller will wait in Provisioning if the
	// bundle is not Ready.
	//
	// A nil spec.composedBundleRef resolves to the built-in starter bundle
	// rather than failing, so the only way to land here is a bundle that is
	// genuinely absent: a named one that does not exist, or — if the operator
	// has not finished seeding on a fresh install — the starter itself, which
	// the next tick resolves.
	bundleRef := r.effectiveBundleRef(cr)
	if _, err := r.getComposedBundle(ctx, bundleRef.Name, bundleRef.Namespace); err != nil {
		msg := fmt.Sprintf("composed bundle %s: %v", bundleRef.Name, err)
		if cr.Spec.ComposedBundleRef == nil {
			msg = fmt.Sprintf("built-in starter bundle %s/%s is not available yet: %v",
				bundleRef.Namespace, bundleRef.Name, err)
		}
		condition := v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionBundleFailed,
			Status:  metav1.ConditionTrue,
			Reason:  "BundleResolutionFailed",
			Message: msg,
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, condition)
		cr.Status.Phase = v1alpha1.ControllerPhaseFailed
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBundleUnreadable, msg)
		return err
	}

	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionBundleResolved,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1Now(),
		Reason:             "BundleReferenceValid",
	})

	// Transition to Provisioning
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	now := metav1Now()
	cr.Status.ProvisioningStartedAt = &now
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ProvisioningStarted",
	})
	return nil
}

// controllerPrefix returns a unique naming prefix for all Kubernetes resources
// owned by this Controller. It incorporates the first 8 characters of the CR's
// UID, guaranteeing names are unique even across delete/recreate cycles.
//
// Kubernetes resource names are limited to 253 characters. The longest name
// derived from this prefix is the PVC "jenkins-home-{prefix}-0" (15 extra
// characters), so the prefix is capped at 238 characters. Some child resources
// (notably Services) require DNS-1035 names that start with a letter, while the
// Controller CR name may start with a digit, so generated prefixes are made
// DNS-1035-safe without changing the Controller's own name.
func controllerPrefix(cr *v1alpha1.Controller) string {
	uid := string(cr.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	const maxPrefixLen = 253 - 15 // 238: reserved for "-jenkins-home-X"
	name := cr.Name
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "c-" + name
	}
	if len(name)+1+len(uid) > maxPrefixLen {
		name = name[:maxPrefixLen-len(uid)-1]
	}
	return name + "-" + uid
}

// PodName returns the StatefulSet pod name for the given controller and
// ordinal. Pods are named "<controllerPrefix>-<ordinal>" — i.e.
// "<name>-<uid8>-0" — not the bare CR name. Callers outside this package (e.g.
// the BFF log streamer) must use this to address a controller's pod; using
// "<name>-0" misses every UID-named controller.
func PodName(cr *v1alpha1.Controller, ordinal int) string {
	return fmt.Sprintf("%s-%d", controllerPrefix(cr), ordinal)
}

// managedPluginLines returns the ordered, deduplicated managed plugin lines for
// plugins.txt. Core lockfile entries come first (pinned); non-core entries from
// PluginSpec (if non-empty) or bundle plugins.yaml follow, with duplicates of
// core entries dropped. Each line is "artifactId[:version]".
// coreSet is the resolved plugin set from a JenkinsVersionProfile (or the
// embedded baseline when no profile matched).
func managedPluginLines(cr *v1alpha1.Controller, resolved *bundle.MaterializedBundle, coreSet []pluginlock.PluginEntry) []string {
	coreIDs := make(map[string]bool, len(coreSet))
	var lines []string
	for _, e := range coreSet {
		coreIDs[e.ArtifactID] = true
		if e.Version != "" {
			lines = append(lines, e.ArtifactID+":"+e.Version)
		} else {
			lines = append(lines, e.ArtifactID)
		}
	}

	nonCore := nonCorePluginEntries(cr, resolved)

	for _, e := range nonCore {
		if e.ArtifactId == "" {
			continue
		}
		if coreIDs[e.ArtifactId] {
			continue
		}
		if e.Version != "" {
			lines = append(lines, e.ArtifactId+":"+e.Version)
		} else {
			lines = append(lines, e.ArtifactId)
		}
		// Track to deduplicate later non-core entries with the same artifact ID.
		coreIDs[e.ArtifactId] = true
	}
	return lines
}

func nonCorePluginEntries(cr *v1alpha1.Controller, resolved *bundle.MaterializedBundle) []v1alpha1.PluginEntry {
	if cr.Spec.PluginSpec != nil && len(cr.Spec.PluginSpec.Entries) > 0 {
		return append([]v1alpha1.PluginEntry(nil), cr.Spec.PluginSpec.Entries...)
	}
	if resolved != nil && resolved.PluginsYAML != "" {
		entries, err := parsePluginEntries(resolved.PluginsYAML)
		if err == nil {
			return entries
		}
	}
	return nil
}

func pluginVersionConflict(cr *v1alpha1.Controller, resolved *bundle.MaterializedBundle, coreSet []pluginlock.PluginEntry) string {
	locked := make(map[string]string, len(coreSet))
	for _, e := range coreSet {
		if e.ArtifactID != "" && !isUnpinnedPluginVersion(e.Version) {
			locked[e.ArtifactID] = e.Version
		}
	}
	for _, e := range nonCorePluginEntries(cr, resolved) {
		if e.ArtifactId == "" || isUnpinnedPluginVersion(e.Version) {
			continue
		}
		if lockedVersion, ok := locked[e.ArtifactId]; ok && lockedVersion != e.Version {
			return fmt.Sprintf("plugin %s requested at %s conflicts with profile lock %s", e.ArtifactId, e.Version, lockedVersion)
		}
	}
	return ""
}

func isUnpinnedPluginVersion(version string) bool {
	v := strings.TrimSpace(version)
	return v == "" || strings.EqualFold(v, "latest")
}

// pluginChangeDiff computes the human-readable diff between new and old
// managed plugin lines, formatted as "+id:ver" (added/changed) or "-id:ver" (removed).
func pluginChangeDiff(newLines, oldLines []string) []string {
	if oldLines == nil {
		return nil
	}
	oldSet := make(map[string]bool, len(oldLines))
	for _, l := range oldLines {
		oldSet[l] = true
	}
	newSet := make(map[string]bool, len(newLines))
	for _, l := range newLines {
		newSet[l] = true
	}
	var changes []string
	for _, l := range newLines {
		if !oldSet[l] {
			changes = append(changes, "+"+l)
		}
	}
	for _, l := range oldLines {
		if !newSet[l] {
			changes = append(changes, "-"+l)
		}
	}
	sort.Strings(changes)
	return changes
}

// resourceOverlayService returns the ResourceOverlay.Service YAML string for a
// Controller CR, or "" if none is set.
func resourceOverlayService(cr *v1alpha1.Controller) string {
	if cr.Spec.ResourceOverlay != nil {
		return cr.Spec.ResourceOverlay.Service
	}
	return ""
}

// resourceOverlayIngress returns the ResourceOverlay.Ingress YAML string for a
// Controller CR, or "" if none is set.
func resourceOverlayIngress(cr *v1alpha1.Controller) string {
	if cr.Spec.ResourceOverlay != nil {
		return cr.Spec.ResourceOverlay.Ingress
	}
	return ""
}

// tokenNeedsRemint reports whether a stored bootstrap token must be re-minted.
// A token is rotated when it is missing, not a current-format (v2) unexpired
// token, or no longer validates under the CURRENT signer. The last case catches
// a token that is structurally valid and unexpired but was signed under a
// previous CA/HMAC key — e.g. after a hive cluster's control plane was
// reinstalled and regenerated the internal CA. Without the signature check such
// a token passes the format gate and is never rotated, so the gateway rejects
// every reconnect with "invalid or expired bootstrap token".
// ensureBootstrapToken guarantees an unexpired, current-format bootstrap token
// exists in <pre>-bootstrap. Re-mint only happens when there is no token, the
// stored token is not a current-format (v2), unexpired token, or it no longer
// validates under the current signer — e.g. after a signer/format upgrade,
// expiry, or a control-plane reinstall that regenerated the internal CA
// (leaving an unexpired token signed under the OLD HMAC key that the gateway
// now rejects). A token that still validates is left untouched so reconnecting
// mites are not invalidated before Kubernetes propagates the Secret to mounted
// volumes (up to ~60 s lag).
//
// Called from every pre-Connected phase tick (Provisioning AND Running), not
// just Provisioning: the token TTL (15 min) can elapse while the pod is still
// pulling plugins in init, and by the time the mite first registers the phase
// has already advanced to Running. Without a remint there, the controller
// wedges on Unauthenticated registrations until the disconnect-recovery path
// thrashes it back through Pending — minutes of avoidable downtime. A
// connected mite never reads the bootstrap token (it holds an mTLS cert), so
// reminting outside Connected is always safe.
func (r *Reconciler) ensureBootstrapToken(ctx context.Context, cr *v1alpha1.Controller, pre string) error {
	secretName := pre + "-bootstrap"
	existing, _ := r.client.GetSecret(ctx, secretName, cr.Namespace)
	if !tokenNeedsRemint(r.tokenSigner, string(existing["token"]), cr.Name, cr.Namespace) {
		return nil
	}
	token, err := r.tokenSigner.GenerateToken(cr.Name, cr.Namespace, 15*time.Minute)
	if err != nil {
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBootstrapTokenFailed, err.Error())
		return fmt.Errorf("generate token: %w", err)
	}
	if err := r.client.CreateOrUpdateSecret(ctx, secretName, cr.Namespace, map[string][]byte{
		"token": []byte(token),
	}); err != nil {
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedBootstrapTokenFailed, err.Error())
		return fmt.Errorf("create bootstrap secret: %w", err)
	}
	return nil
}

func tokenNeedsRemint(signer *mite.TokenSigner, token, controllerName, namespace string) bool {
	if len(token) == 0 || !mite.IsCurrentTokenFormat(token) {
		return true
	}
	_, err := signer.ValidateToken(token, controllerName, namespace)
	return err != nil
}

func (r *Reconciler) handleProvisioning(ctx context.Context, cr *v1alpha1.Controller) error {
	tracer := otel.Tracer("varroa-operator")
	ctx, span := tracer.Start(ctx, "reconcile.handleProvisioning")
	defer span.End()

	// Resolve ControllerClass (if configured) before any provisioning work.
	// Fail closed: a dangling className blocks StatefulSet/ConfigMap creation.
	class, classOK := r.resolveClassForCr(ctx, cr)
	if !classOK {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionClassResolved,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.ReasonClassNotFound,
			Message: fmt.Sprintf("ControllerClass %q not found", cr.Spec.ClassName),
		})
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedClassResolutionFailed, fmt.Sprintf("ControllerClass %q not found", cr.Spec.ClassName))
		return fmt.Errorf("controller class %q not found", cr.Spec.ClassName)
	}
	if class != nil {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionClassResolved,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonClassResolved,
			Message: fmt.Sprintf("ControllerClass %q resolved", class.Name),
		})
	} else {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionClassResolved,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonNoClassConfigured,
			Message: "no className configured",
		})
	}

	pre := controllerPrefix(cr)

	// Create/update the Service. Shared with the post-provisioning reconcile
	// path so the desired-service derivation lives in one place.
	{
		_, span := tracer.Start(ctx, "provision.createService")
		err := r.reconcileService(ctx, cr)
		span.End()
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedServiceReconcileFailed, err.Error())
			return fmt.Errorf("reconcile service: %w", err)
		}
	}

	// Create ServiceAccount for StatefulSet pods.
	// Create agent ServiceAccount and RBAC. Fail provisioning if the operator
	// lacks permissions — silent RBAC gaps make agent pods impossible to debug.
	saName := pre + "-agent"
	if err := r.client.CreateServiceAccount(ctx, saName, cr.Namespace); err != nil {
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedAgentRBACFailed, err.Error())
		return fmt.Errorf("create agent serviceaccount %q: %w", saName, err)
	}
	if err := r.client.CreateAgentRBAC(ctx, saName, cr.Namespace); err != nil {
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedAgentRBACFailed, err.Error())
		return fmt.Errorf("create agent RBAC %q: %w", saName, err)
	}

	// Only create the bootstrap token if one does not already exist. Rotating
	// on every tick invalidates reconnecting mite sidecars before Kubernetes
	// propagates the updated Secret to mounted volumes (up to ~60 s lag).
	secretName := pre + "-bootstrap"
	if err := r.ensureBootstrapToken(ctx, cr, pre); err != nil {
		return err
	}

	// Create plugins ConfigMap
	pluginsCMName := pre + "-plugins"
	var pluginsTxt string
	// Read existing ConfigMap for diff computation.
	oldCMData, _ := r.client.GetConfigMap(ctx, pluginsCMName, cr.Namespace)
	oldLines := strings.Split(oldCMData["plugins.txt"], "\n")
	if len(oldLines) == 1 && oldLines[0] == "" {
		oldLines = nil
	}

	resolved, _, bundleIdent, resolveErr := r.resolveBundleForController(ctx, cr)
	// Every Controller now has an effective bundle — a named one or the built-in
	// starter — so a resolve failure is always worth blocking on. The former
	// `&& ComposedBundleRef != nil` exemption existed only for the no-bundle
	// case, which no longer occurs.
	if resolveErr != nil {
		// The bundle's plugins.yaml drives plugins.txt, which is baked into the
		// plugins-init container at pod creation. handleProvisioning runs ONLY in
		// the Provisioning phase, so a core-only plugins.txt written here while the
		// ComposedBundle is still materializing never self-heals once the controller
		// connects — the bundle-declared plugins (e.g. workflow-aggregator) would be
		// silently dropped. Block provisioning until the bundle is resolved instead
		// of baking an incomplete plugin set; the next tick re-provisions with the
		// full set. (Matches the documented intent that a controller waits in
		// Provisioning until its bundle is Ready.)
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionBundleResolved,
			Status:  metav1.ConditionFalse,
			Reason:  "BundleNotMaterialized",
			Message: fmt.Sprintf("waiting for composed bundle to materialize before provisioning plugins: %v", resolveErr),
		})
		r.persistStatusDiagnostics(ctx, cr)
		return fmt.Errorf("provisioning blocked until bundle materializes: %w", resolveErr)
	}
	// Resolve the plugin set from JenkinsVersionProfile (or embedded baseline).
	profile, matchKind := r.resolveProfileForCr(cr)
	// Bake-time compat gate (guard-version-upgrade-path §7): a provably-unsafe
	// core (older than the effective plugin-set source, or unparseable with no
	// vouching profile) would crash-loop plugins-init. Block BEFORE the
	// plugins.txt write and all StatefulSet work later in this function, so a
	// deny never rolls anything. The condition is (re)written every pass.
	compat := EvaluateCoreCompat(cr.Spec.Version, profile, matchKind, ProfilePluginSetReady(profile), pluginlock.Baseline())
	r.setVersionUpgradeBlocked(cr, compat)
	if compat.Verdict != CompatOK {
		r.persistStatusDiagnostics(ctx, cr)
		return fmt.Errorf("version upgrade blocked: %s", compat.Message)
	}
	coreSet := r.resolveCoreSet(ctx, cr, profile, r.Logger)
	// PluginPinConflict runs before the blocking pluginVersionConflict gate below
	// so it still fires on the same reconcile that gate blocks on — placing it
	// after would skip this independent signal exactly when it's most relevant.
	if report, pinErr := bundle.CheckPluginPins(resolved.RawPluginsYAML, coreSet); pinErr == nil {
		if len(report.Conflicts) > 0 {
			r.surfacePluginPinConflict(ctx, cr, bundleIdent, pluginPinConflictMessage(report))
		} else {
			r.clearPluginPinConflict(ctx, cr, bundleIdent)
		}
	}
	if conflict := pluginVersionConflict(cr, resolved, coreSet); conflict != "" {
		r.surfacePluginConflict(ctx, cr, conflict)
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedPluginConflict, conflict)
		return fmt.Errorf("plugin version conflict: %s", conflict)
	}
	r.clearPluginConflict(ctx, cr)

	lines := managedPluginLines(cr, resolved, coreSet)
	pluginsTxt = strings.Join(lines, "\n")
	{
		_, span := tracer.Start(ctx, "provision.createConfigMap")
		pluginsData := map[string]string{
			"plugins.txt": pluginsTxt,
		}
		err := r.client.CreateOrUpdateConfigMap(ctx, pluginsCMName, cr.Namespace, pluginsData)
		span.End()
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedPluginsConfigMapFailed, err.Error())
			return fmt.Errorf("create plugins configmap: %w", err)
		}
	}
	// Set PluginLockMissing condition based on profile resolution.
	if matchKind == MatchBaseline && profile == nil {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionPluginLockMissing, Status: metav1.ConditionTrue,
			Reason:  "NoLockForCore",
			Message: fmt.Sprintf("no version profile for %q; using embedded baseline %q", cr.Spec.Version, pluginlock.Baseline()),
		})
	} else {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionPluginLockMissing, Status: metav1.ConditionFalse,
			Reason:  "LockMatched",
			Message: fmt.Sprintf("version profile matched for %q", cr.Spec.Version),
		})
	}

	// Create init.groovy.d ConfigMap
	initCMName := pre + "-init"
	initScript := `// Varroa setup is in varroa-mite-auth.groovy.`

	authScript := `
import java.util.logging.Logger

def log = Logger.getLogger('varroa-mite-auth')
// Realm and authorization are applied declaratively by JCasC at boot via
// CASC_JENKINS_CONFIG. This script intentionally no longer imperatively
// installs the security realm or rebuilds the authorization strategy.
log.info('Varroa init: realm and RBAC are managed declaratively by JCasC')
`

	{
		_, span := tracer.Start(ctx, "provision.createConfigMap")
		err := r.client.CreateOrUpdateConfigMap(ctx, initCMName, cr.Namespace, map[string]string{
			"init.groovy":             initScript,
			"varroa-mite-auth.groovy": authScript,
		})
		span.End()
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedInitConfigMapFailed, err.Error())
			return fmt.Errorf("create init configmap: %w", err)
		}
	}

	// Create per-controller CASC ConfigMap (next-boot seed).
	cascCMName := cascConfigMapName(pre)
	realmDoc := generateRealmDocument()
	cascData := map[string]string{
		"realm.yaml": realmDoc,
		"config.yaml": func() string {
			// Strip authorizationStrategy from the resolved JenkinsYAML;
			// Varroa owns authz and delivers it separately as rbac.yaml.
			if resolved != nil {
				return injectProjectNamingStrategy(
					stripAuthorizationStrategy(resolved.JenkinsYAML))
			}
			return ""
		}(),
	}
	rbacYAML, humanAdmin, rbacErr := r.rbacGenerator.GenerateWithAdminCheck(cr)
	if rbacErr != nil {
		r.Logger.Warn("rbac generation failed during provisioning; omitting authz from CASC bundle", "error", rbacErr)
	} else if !humanAdmin && !adminLockoutOverridden(cr) {
		r.Logger.Info("rbac lockout guard withholding authz from CASC bundle during provisioning")
	} else {
		cascData["rbac.yaml"] = rbacYAML
	}
	cascHash := cascContentHash(cascData)
	{
		_, span := tracer.Start(ctx, "provision.createConfigMap")
		err := r.client.CreateOrUpdateConfigMap(ctx, cascCMName, cr.Namespace, cascData)
		span.End()
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedCascConfigMapFailed, err.Error())
			return fmt.Errorf("create casc configmap: %w", err)
		}
	}

	// Create StatefulSet. Read cluster-wide defaults once — the desired-state
	// builder and the update-center gate below both consume them.
	var defaults *v1alpha1.ProvisioningDefaults
	if d, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, ""); err == nil && d != nil {
		defaults = d
	}

	pol := r.effectivePolicy(cr)

	// Compute gated plugins-checksum (design §3); the decision logic is
	// table-tested in desiredstate_test.go.
	desiredChecksum := sha256Hex([]byte(pluginsTxt))
	appliedChecksum, _ := r.client.GetStatefulSetPluginsChecksum(ctx, pre, cr.Namespace)
	roll := computePluginRollGate(desiredChecksum, appliedChecksum,
		cr.Status.ApprovedPluginRollChecksum, pol.Mode,
		cr.Annotations[annotationForceReprovision] != "")
	if roll.ClearApproved {
		cr.Status.ApprovedPluginRollChecksum = ""
	}
	stsPluginsChecksum := roll.STSChecksum

	// Task 6.1: Raise/clear PluginRollPending condition.
	if roll.RaisePending {
		// Manual/idle — defer the roll and surface pending condition.
		now := metav1Now()
		changes := pluginChangeDiff(lines, oldLines)
		if cr.Status.PendingPluginRoll == nil || cr.Status.PendingPluginRoll.TargetChecksum != desiredChecksum {
			cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{
				TargetChecksum: desiredChecksum,
				Since:          now,
				Changes:        changes,
			}
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionPluginRollPending,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonPluginRollPending,
			Message: fmt.Sprintf("plugin roll pending approval (checksum: %s)", desiredChecksum[:8]),
		})
	} else if roll.ResolvePending {
		cr.Status.PendingPluginRoll = nil
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionPluginRollPending,
			Status: metav1.ConditionFalse,
			Reason: v1alpha1.ReasonPluginRollApproved,
		})
	}

	stsSpec := r.buildStatefulSetSpec(cr, class, defaults, profile, stsBuildInputs{
		Name:             pre,
		BootstrapSecret:  secretName,
		InitConfigMap:    initCMName,
		CascConfigMap:    cascCMName,
		CascContentHash:  cascHash,
		PluginsConfigMap: pluginsCMName,
		PluginsChecksum:  stsPluginsChecksum,
		Policy:           pol,
	})
	// Resolve plugin update center URLs with 3-tier precedence.
	// Fetch the UpdateCenter CR best-effort for UC readiness gating.
	var uc *v1alpha1.UpdateCenter
	if r.ucBaseURL != "" {
		if u, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, r.store, "varroa-update-center", ""); err == nil {
			uc = u
		} else if !apierrors.IsNotFound(err) {
			r.Logger.Warn("failed to fetch UpdateCenter during provisioning; treating as absent",
				"controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		// Not-found or other error: uc stays nil (UC treated as absent).
	}
	ucGate := resolveUpdateCenterGate(defaults, uc, r.ucBaseURL)
	stsSpec.PluginUpdateCenterURL = ucGate.URL
	stsSpec.PluginUpdateCenterDownloadURL = ucGate.DownloadURL

	// §5.2 fallback semantics — the decision is table-tested in
	// desiredstate_test.go; this switch only applies it.
	switch ucGate.Outcome {
	case ucGateFallbackOnline:
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionUpdateCenterFallback,
			Status:  metav1.ConditionTrue,
			Reason:  "UpdateCenterUnavailable",
			Message: "update center configured but unreachable; init container will use tool defaults",
		})
		r.Logger.Info("update center fallback — online mode, provisioning continues",
			"controller", cr.Namespace+"/"+cr.Name)
	case ucGateBlockAirgap:
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionWaitingForUpdateCenter,
			Status:  metav1.ConditionTrue,
			Reason:  "UpdateCenterUnavailable",
			Message: ucBlockAirgapMessage(uc),
		})
		r.persistStatusDiagnostics(ctx, cr)
		return fmt.Errorf("update center not ready in air-gap mode")
	case ucGateClear:
		// UC ready or not configured: clear any stale fallback/waiting conditions.
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionUpdateCenterFallback,
			Status: metav1.ConditionFalse,
			Reason: "UpdateCenterAvailable",
		})
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionWaitingForUpdateCenter,
			Status: metav1.ConditionFalse,
			Reason: "UpdateCenterAvailable",
		})
	case ucGateNoop:
		// Explicit defaults override both URL tiers: leave conditions untouched.
	}
	{
		_, span := tracer.Start(ctx, "provision.createStatefulSet")
		err := r.client.CreateStatefulSet(ctx, stsSpec)
		span.End()
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedStatefulSetCreateFailed, err.Error())
			return fmt.Errorf("create statefulset: %w", err)
		}
	}

	// Create/update the Ingress. Shared with the post-provisioning reconcile path
	// so the desired-ingress derivation lives in one place.
	{
		_, span := tracer.Start(ctx, "provision.createIngress")
		err := r.reconcileIngress(ctx, cr)
		span.End()
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedIngressCreateFailed, err.Error())
			return fmt.Errorf("create ingress: %w", err)
		}
	}

	// Shared-host mismatch is reconciled globally in reconcileController.

	// Compute overlay warnings for status surface (warn-but-allow).
	{
		ws, err := overlay.ScanOverrides(cr.Spec.PodOverrides, cr.Spec.ResourceOverlay)
		if err != nil {
			r.Logger.Warn("scan overrides failed", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		} else {
			cr.Status.OverlayWarnings = make([]v1alpha1.OverlayWarning, 0, len(ws))
			for _, w := range ws {
				cr.Status.OverlayWarnings = append(cr.Status.OverlayWarnings, v1alpha1.OverlayWarning{
					Resource: w.Resource,
					Path:     w.Path,
					Message:  w.Message,
				})
			}
		}
	}

	// Enforce provisioning timeout against ProvisioningStartedAt, not
	// CreationTimestamp. CreationTimestamp is the age of the CR object and
	// would immediately fail any controller that re-enters this phase after
	// an operator restart (all live CRs are older than 5 minutes).
	if cr.Status.ProvisioningStartedAt == nil {
		now := metav1Now()
		cr.Status.ProvisioningStartedAt = &now
	}
	if time.Since(cr.Status.ProvisioningStartedAt.Time) > provisioningTimeout {
		cr.Status.Phase = v1alpha1.ControllerPhaseFailed
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionFailed,
			Status:  metav1.ConditionTrue,
			Reason:  "ProvisioningTimeout",
			Message: fmt.Sprintf("provisioning exceeded %v", provisioningTimeout),
		})
		r.deleteWakeSlice(ctx, cr, r.Logger.With("controller", cr.Namespace+"/"+cr.Name))

		r.persistStatusDiagnostics(ctx, cr)
		return fmt.Errorf("provisioning timeout exceeded")
	}

	// Surface plugins-init failures during initial provisioning. Without this, a
	// bad plugin pin leaves the controller in Provisioning with no visible cause.
	r.reconcileProvisioningPluginInitFailure(ctx, cr)

	// Only transition to Running when the StatefulSet is actually ready.
	ready, err := r.client.IsStatefulSetReady(ctx, pre, cr.Namespace)
	if err != nil {
		// reconcileProvisioningPluginInitFailure may have just recorded a
		// PluginRollFailed diagnostic; don't let this error return drop it.
		r.persistStatusDiagnostics(ctx, cr)
		return fmt.Errorf("check statefulset readiness: %w", err)
	}
	if !ready {
		return nil // stay in Provisioning
	}

	r.deleteWakeSlice(ctx, cr, r.Logger.With("controller", cr.Namespace+"/"+cr.Name))
	cr.Status.Phase = v1alpha1.ControllerPhaseRunning
	return nil
}

const (
	// staleMiteThreshold is how long after LastSeen we consider a mite
	// definitively gone, even if the registry has stale data.
	staleMiteThreshold = 2 * time.Minute
)

// reconcileStreamDegradedCondition mirrors the gateway's bus→stream bridge
// health onto the Controller as ConditionMiteStreamDegraded.
//
// The gateway retries a failed KV watch forever, so this condition is
// self-healing — but while it is True, desired
// state is not reaching the mite even though the controller is Connected,
// Ready and reports no apply failure. Without it the only evidence of a
// starved mite is a single controller-scoped gateway log line.
func (r *Reconciler) reconcileStreamDegradedCondition(cr *v1alpha1.Controller, logger *slog.Logger) {
	if r.miteTransport == nil {
		return
	}
	reason, degraded := r.miteTransport.StreamDegraded(cr.Namespace, cr.Name)
	if degraded {
		if reason == "" {
			reason = "gateway bus watch is not established"
		}
		logger.Warn("mite bus bridge degraded; gateway traffic is not reaching the mite", "reason", reason)
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionMiteStreamDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  "BusWatchFailed",
			Message: reason,
		})
		return
	}
	// Only write the False condition once it has actually been True, so healthy
	// controllers do not carry a permanent no-op condition.
	if existing := findCondition(cr.Status.Conditions, v1alpha1.ConditionMiteStreamDegraded); existing == nil {
		return
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionMiteStreamDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  "BusWatchHealthy",
		Message: "gateway is bridging desired state to the mite",
	})
}

func (r *Reconciler) handleConnected(ctx context.Context, cr *v1alpha1.Controller) error {
	tracer := otel.Tracer("varroa-operator")
	ctx, span := tracer.Start(ctx, "reconcile.handleConnected")
	defer span.End()

	key := cr.Namespace + "/" + cr.Name
	logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
	// Keep StatefulSet OIDC env vars in sync with current operator config.
	// UpdateStatefulSetOIDCEnv reads the live StatefulSet and only patches
	// if values differ, so calling it every tick is safe and idempotent.
	currentIssuer := r.Resolver.OIDCIssuer()
	currentLoginURL := r.Resolver.LoginURL(r.varroaRedirectURL)
	if err := r.client.UpdateStatefulSetOIDCEnv(ctx, controllerPrefix(cr), cr.Namespace, currentIssuer, currentLoginURL, r.Resolver.OIDCUserClaim(), r.Resolver.OIDCGroupClaim(), r.mitePubKeyPEM(), r.mitePubKeyKID(), cr.Namespace+"/"+cr.Name, r.apikeyVerifyURL(), r.caPEM); err != nil {
		logger.Error("failed to sync StatefulSet OIDC env", "error", err)
	}
	r.reconcileFleetPodLabel(ctx, cr)

	// Backoff retry: if a restart drain timed out, re-issue at NextRetryAt.
	if cr.Status.RestartDrain != nil && cr.Status.RestartDrain.NextRetryAt != nil && time.Now().After(cr.Status.RestartDrain.NextRetryAt.Time) {
		pol := r.effectivePolicy(cr)
		drainSec := int64(pol.DrainTimeoutSeconds)
		if err := r.issueSafeRestart(ctx, cr, drainSec); err != nil {
			logger.Error("failed to re-issue safe-restart on backoff", "error", err)
		} else {
			logger.Info("re-issued safe-restart on backoff schedule", "attempt", cr.Status.RestartDrain.AttemptCount+1)
		}
	}

	// Gather the transport observation; derivation is table-tested in
	// desiredstate_test.go.
	version, lastHeartbeat, certExpiry, transportConnected := r.miteTransport.Info(cr.Namespace, cr.Name)
	snapshot := r.miteTransport.Snapshot(cr.Namespace, cr.Name)
	health := deriveMiteHealth(miteObservation{
		TransportConnected: transportConnected,
		Version:            version,
		LastHeartbeat:      lastHeartbeat,
		CertExpiry:         certExpiry,
		Health:             r.miteTransport.Health(cr.Namespace, cr.Name),
		Snapshot:           snapshot,
	}, cr.Status.LiveDrift, metav1Now())
	if health.StaleOverride {
		logger.Warn("stale heartbeat, forcing disconnect", "age", time.Since(lastHeartbeat), "threshold", staleMiteThreshold)
	}

	if !health.Connected {
		key = cr.Namespace + "/" + cr.Name
		r.disconnectedTicks[key]++
		// MiteStreamDegraded means "connected, but the bridge is starving it",
		// so it stops being meaningful the moment the mite is gone — including
		// during the reconnect grace window below. Nothing else clears it
		// outside the Connected phase, so drop it here rather than leaving a
		// stale True through the whole Pending/provisioning cycle.
		cr.Status.Conditions = removeConditionByType(cr.Status.Conditions,
			v1alpha1.ConditionMiteStreamDegraded)
		if r.disconnectedTicks[key] < 3 {
			// Grace period: mite may be reconnecting after operator restart.
			// Stay in Connected for a few ticks to allow reconnection.
			return nil
		}

		// Clear the token record so a fresh one is issued on reconnect.
		// The mite's in-memory token is lost when it restarts.
		r.miteTokenMu.Lock()
		delete(r.miteTokens, key)
		r.miteTokenMu.Unlock()
		// Reset the disconnected-tick counter so a later re-provision attempt
		// starts with a fresh grace window (see handleRunning) rather than
		// immediately re-forcing Pending.
		delete(r.disconnectedTicks, key)
		cr.Status.Phase = v1alpha1.ControllerPhasePending
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "MiteDisconnected",
			Message: "mite sidecar disconnected",
		})
		return nil
	}
	// Reset disconnected counter when mite is connected.
	delete(r.disconnectedTicks, cr.Namespace+"/"+cr.Name)

	// A mite can hold a healthy gRPC stream while the gateway's bus→stream
	// bridge is broken, in which case no desired state reaches it and every
	// other signal still reads as healthy. Surface that explicitly.
	// Only meaningful once the mite is known connected.
	r.reconcileStreamDegradedCondition(cr, logger)

	// Apply the derived mite health: MiteStatus fields, LiveDrift, and the
	// ApplyDeferred/LiveDrift/Ready conditions, in order.
	if cr.Status.MiteStatus == nil {
		cr.Status.MiteStatus = &v1alpha1.MiteStatus{}
	}
	hs := health.MiteStatus
	cr.Status.MiteStatus.Connected = hs.Connected
	cr.Status.MiteStatus.Version = hs.Version
	cr.Status.MiteStatus.LastSeen = hs.LastSeen
	cr.Status.MiteStatus.CertExpiry = hs.CertExpiry
	if hs.LastHealthCheck != nil { // snapshot was present at observation time
		cr.Status.MiteStatus.JenkinsVersion = hs.JenkinsVersion
		cr.Status.MiteStatus.JenkinsHealth = hs.JenkinsHealth
		cr.Status.MiteStatus.LastHealthCheck = hs.LastHealthCheck
	}
	if health.LiveDrift != nil {
		cr.Status.LiveDrift = health.LiveDrift
	}
	for _, c := range health.Conditions {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, c)
	}

	// Drain pending results
	results := r.miteTransport.DrainResults(cr.Namespace, cr.Name)
	for _, res := range results {
		switch r := res.Result.(type) {
		case *mitev1.CommandResult_DesiredState:
			if r.DesiredState != nil {
				if r.DesiredState.ConfigSuccess {
					cr.Status.ConfigHash = r.DesiredState.AppliedHash
				}
				if r.DesiredState.RbacSuccess {
					cr.Status.RBACHash = r.DesiredState.AppliedHash
				}
				// DeletionPending: surface deferred item deletions from the mite.
				if len(r.DesiredState.DeferredItemDeletions) > 0 {
					pending := make([]v1alpha1.PendingDeletion, 0, len(r.DesiredState.DeferredItemDeletions))
					now := metav1Now()
					for _, d := range r.DesiredState.DeferredItemDeletions {
						pending = append(pending, v1alpha1.PendingDeletion{Path: d.Path, Reason: d.Reason, DetectedAt: now})
					}
					cr.Status.PendingItemDeletions = pending
					cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
						Type:    v1alpha1.ConditionDeletionPending,
						Status:  metav1.ConditionTrue,
						Reason:  v1alpha1.ReasonItemDeletionPending,
						Message: fmt.Sprintf("%d item deletion(s) deferred: a build is in progress", len(pending)),
					})
				} else {
					cr.Status.PendingItemDeletions = nil
					cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
						Type:   v1alpha1.ConditionDeletionPending,
						Status: metav1.ConditionFalse,
						Reason: v1alpha1.ReasonNoDeletionPending,
					})
				}
				// Build ApplyResult from the mite's per-section outcomes — records
				// per-section errors structurally on status (config, rbac, plugins, items).
				d := r.DesiredState
				// The mite no longer runs a plugins section (managed plugins converge
				// via the init-container pod roll), so the plugins section is
				// always reported OK and never penalizes Succeeded.
				pluginsOK := true
				rbacOK := d.ConfigSuccess
				ar := v1alpha1.ApplyResult{
					Hash:      d.AppliedHash,
					Timestamp: metav1.Now(),
					Succeeded: d.ConfigSuccess && rbacOK && pluginsOK && d.ItemsSuccess,
					Sections: []v1alpha1.ApplySectionResult{
						{Name: "config", OK: d.ConfigSuccess, Error: truncate(d.ConfigError, 1024)},
						{Name: "rbac", OK: rbacOK, Error: ""},
						// plugins section always OK since the mite never runs it;
						// plugins converge via the init-container pod roll.
						{Name: "plugins", OK: pluginsOK, Error: truncate(d.PluginsError, 1024)},
						{Name: "items", OK: d.ItemsSuccess, Error: truncate(d.ItemsError, 1024)},
					},
				}
				if sameOutcome(cr.Status.LastApplyResult, ar) {
					cr.Status.LastApplyResult.Timestamp = ar.Timestamp
					if len(cr.Status.ApplyHistory) > 0 {
						cr.Status.ApplyHistory[0].Timestamp = ar.Timestamp
					}
					continue
				}
				cr.Status.LastApplyResult = &ar
				cr.Status.ApplyHistory = append([]v1alpha1.ApplyResult{ar}, cr.Status.ApplyHistory...)
				if len(cr.Status.ApplyHistory) > 10 {
					cr.Status.ApplyHistory = cr.Status.ApplyHistory[:10]
				}
			}
		default:
			// Every CommandResult without a DesiredState payload is an imperative
			// ack (SAFE_RESTART, DELETE_ITEM, etc.). Always record it last-one-wins
			// on LastImperativeResult for brood operation completion predicates.
			cr.Status.LastImperativeResult = &v1alpha1.ImperativeResult{
				CommandID:  res.CommandId,
				Success:    res.Success,
				Error:      res.Error,
				FinishedAt: metav1.Now(),
			}
			// Also correlate imperative results to the outstanding SAFE_RESTART
			// by command_id. A result without a matching RestartDrain.CommandID
			// (e.g. a DELETE_ITEM result) is ignored by this arm.
			if cr.Status.RestartDrain != nil && res.CommandId == cr.Status.RestartDrain.CommandID {
				now := metav1Now()
				if res.Success {
					cr.Status.RestartDrain = nil
					cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
						Type:   v1alpha1.ConditionRestartDeferred,
						Status: metav1.ConditionFalse,
						Reason: "RestartSucceeded",
					})
				} else {
					reason := res.Error
					if res.Deferred && res.DeferReason != "" {
						reason = res.DeferReason
					}
					attempt := cr.Status.RestartDrain.AttemptCount + 1
					cr.Status.RestartDrain.AttemptCount = attempt
					cr.Status.RestartDrain.NextRetryAt = &metav1.Time{Time: now.Add(backoff(attempt))}
					cr.Status.RestartDrain.LastReason = reason
					cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
						Type:    v1alpha1.ConditionRestartDeferred,
						Status:  metav1.ConditionTrue,
						Reason:  "RestartDrainDeferred",
						Message: fmt.Sprintf("attempt %d: %s (retry at %s)", attempt, reason, cr.Status.RestartDrain.NextRetryAt.Format(time.RFC3339)),
					})
				}
			}
		}
	}

	// Set ConfigApplyFailed condition from LastApplyResult (derived every tick,
	// including empty-drain ticks, so a stale True persists until a successful apply).
	{
		configFailed := false
		configErr := ""
		if cr.Status.LastApplyResult != nil {
			for _, s := range cr.Status.LastApplyResult.Sections {
				if s.Name == "config" {
					if !s.OK {
						configFailed = true
						configErr = s.Error
					}
					break
				}
			}
		}
		if configFailed {
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionConfigApplyFailed,
				Status:  metav1.ConditionTrue,
				Reason:  v1alpha1.ReasonJCascApplyFailed,
				Message: configErr,
			})
		} else {
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:   v1alpha1.ConditionConfigApplyFailed,
				Status: metav1.ConditionFalse,
				Reason: v1alpha1.ReasonConfigApplied,
			})
		}
	}

	// Read current snapshot
	snapshot = r.miteTransport.Snapshot(cr.Namespace, cr.Name)

	// Resolve bundle content from the materialized ConfigMap.
	var (
		resolvedBundle   *bundle.MaterializedBundle
		targetBundleHash string
		bundleIdent      bundleIdentity
		bundleErr        error
	)
	func() {
		_, span := tracer.Start(ctx, "bundle.resolve")
		defer span.End()
		resolvedBundle, targetBundleHash, bundleIdent, bundleErr = r.resolveBundleForController(ctx, cr)
		if bundleErr == nil && resolvedBundle != nil {
			span.SetAttributes(
				attribute.Int("variables_count", len(resolvedBundle.Variables)),
			)
		}
	}()
	if bundleErr != nil {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionBundleFailed,
			Status:  metav1.ConditionTrue,
			Reason:  "BundleResolutionFailed",
			Message: bundleErr.Error(),
		})
		// Non-fatal in Connected: surface the error on status but don't
		// crash the reconcile loop. The controller keeps serving last-good.
		logger.Warn("bundle resolution failed in connected phase", "error", bundleErr)
		return nil //nolint:nilerr // non-fatal in Connected phase
	}

	// Evaluate PluginInstallRequired: compare resolved managed plugin set against
	// the baked plugins.txt ConfigMap. Skip on unusable baseline (ConfigMap read error
	// or empty baked set); the BundleFailed condition already covers the bundle-err
	// early-return case.
	coreSet := r.coreSetForCr(ctx, cr, logger)
	desiredLines := managedPluginLines(cr, resolvedBundle, coreSet)
	{
		// Detection-only plugin lock conflict check (C4): surface the condition,
		// metric, and edge event but do NOT disrupt a running Connected controller.
		// The provisioning path (handleProvisioning) blocks on this same check.
		if conflict := pluginVersionConflict(cr, resolvedBundle, coreSet); conflict != "" {
			r.surfacePluginConflict(ctx, cr, conflict)
		} else {
			r.clearPluginConflict(ctx, cr)
		}

		// PluginPinConflict (C1): same detection-only treatment as above, but an
		// independent condition/event pair — see surfacePluginPinConflict.
		if report, pinErr := bundle.CheckPluginPins(resolvedBundle.RawPluginsYAML, coreSet); pinErr == nil {
			if len(report.Conflicts) > 0 {
				r.surfacePluginPinConflict(ctx, cr, bundleIdent, pluginPinConflictMessage(report))
			} else {
				r.clearPluginPinConflict(ctx, cr, bundleIdent)
			}
		}

		desiredChecksum := sha256Hex([]byte(strings.Join(desiredLines, "\n")))
		pluginsCMName := controllerPrefix(cr) + "-plugins"
		bakedData, cmErr := r.client.GetConfigMap(ctx, pluginsCMName, cr.Namespace)
		bakedLines := strings.Split(bakedData["plugins.txt"], "\n")
		if len(bakedLines) == 1 && bakedLines[0] == "" {
			bakedLines = nil
		}
		if cmErr != nil || len(bakedLines) == 0 {
			logger.Debug("skipping PluginInstallRequired evaluation, no usable baked baseline",
				"cmErr", cmErr, "bakedLinesLen", len(bakedLines))
		} else {
			bakedChecksum := sha256Hex([]byte(strings.Join(bakedLines, "\n")))
			mode := r.effectivePolicy(cr).Mode
			automatic := mode == v1alpha1.ReconciliationModeAutomatic
			approved := desiredChecksum == cr.Status.ApprovedPluginRollChecksum

			switch {
			case desiredChecksum == bakedChecksum:
				// Converged — the only true "installed" state.
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:   v1alpha1.ConditionPluginInstallRequired,
					Status: metav1.ConditionFalse,
					Reason: v1alpha1.ReasonPluginsInstalled,
				})
				cr.Status.PendingPluginRoll = nil // drop any stale pending roll

			case automatic || approved:
				// A roll is warranted — hand off to handleProvisioning via a phase
				// transition. PluginInstallRequired stays True until the rolled pod
				// re-bakes a matching <controller>-plugins set.
				diff := pluginChangeDiff(desiredLines, bakedLines)
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionPluginInstallRequired,
					Status:  metav1.ConditionTrue,
					Reason:  v1alpha1.ReasonPluginInstallRequired,
					Message: strings.Join(diff, ", "),
				})
				now := metav1Now()
				cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
				cr.Status.ProvisioningStartedAt = &now
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:               v1alpha1.ConditionProvisioning,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
					Reason:             "ProvisioningStarted",
					Message:            "re-provisioning to install plugin-set change",
				})
				return nil // skip the rest of handleConnected this tick; next tick rolls

			default:
				// Manual, unapproved — stay Connected, surface an approvable pending roll.
				diff := pluginChangeDiff(desiredLines, bakedLines)
				message := strings.Join(diff, ", ") + " — approve the pending plugin roll (the `plugin-roll` action) or set reconciliationPolicy.mode: automatic"
				cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
					Type:    v1alpha1.ConditionPluginInstallRequired,
					Status:  metav1.ConditionTrue,
					Reason:  v1alpha1.ReasonPluginInstallRequired,
					Message: message,
				})
				if cr.Status.PendingPluginRoll == nil ||
					cr.Status.PendingPluginRoll.TargetChecksum != desiredChecksum {
					now := metav1Now()
					cr.Status.PendingPluginRoll = &v1alpha1.PendingPluginRoll{
						TargetChecksum: desiredChecksum,
						Since:          now,
						Changes:        diff,
					}
				}
			}
		}
	}

	// ---- Plugin inventory classification ----
	// Runs every tick in Connected phase. Reads the raw inventory from the
	// transport, classifies it against the declared set and bootstrap root,
	// projects the bounded summary onto status, and persists the full result
	// to the read model (invc/ KV key).
	r.classifyPluginInventory(ctx, cr, coreSet, desiredLines)

	// Build desired state
	desired, desiredHash := func() (*mitev1.DesiredStateCommand, string) {
		_, span := tracer.Start(ctx, "desiredState.build")
		defer span.End()
		d := r.buildDesiredStateCommand(cr, resolvedBundle)
		h := computeDesiredStateHash(d)
		d.DesiredStateHash = h
		span.SetAttributes(attribute.String("hash", h))
		return d, h
	}()
	if cr.Status.LastApplyResult != nil &&
		cr.Status.LastApplyResult.Succeeded &&
		cr.Status.LastApplyResult.Hash == desiredHash {
		cr.Status.AppliedBundleHash = targetBundleHash
	}

	// Set FirstConnectedAt on first handshake and LastReconciledAt on every tick.
	now := metav1Now()
	if cr.Status.FirstConnectedAt == nil {
		cr.Status.FirstConnectedAt = &now
	}
	cr.Status.LastReconciledAt = &now

	// Detect (re)connection via epoch change and force a push so the
	// mite always gets a fresh token. Also clear the cached token so it
	// is re-minted for the new process.
	forcePush := false
	if epoch, ok := r.miteTransport.ConnEpoch(cr.Namespace, cr.Name); ok && epoch != r.lastMiteEpoch[key] {
		r.lastMiteEpoch[key] = epoch
		r.miteTokenMu.Lock()
		delete(r.miteTokens, key)
		r.miteTokenMu.Unlock()
		forcePush = true
	}

	// An explicit reprovision request forces a re-push regardless of the
	// convergence window. Reconcile consumed the annotation one-shot before
	// dispatching here; the in-memory copy carries the request for this pass.
	reprovisionRequested := false
	if cr.Annotations[annotationForceReprovision] != "" {
		forcePush = true
		desired.Reload = true
		reprovisionRequested = true
	}

	configH := sha256Hex([]byte(desired.JcascYaml))
	rbacH := sha256Hex([]byte(desired.RbacYaml))
	itemsH := sha256Hex([]byte(desired.ItemsYaml))

	// Convergence short-circuit: skip the push if the desired state is
	// unchanged AND we pushed recently (within the 5-min drift-correction
	// window). Also verify the mite's actual applied RBAC matches — if the
	// snapshot reports a different rbacHash the mite hasn't confirmed the
	// push and we must retry regardless of the operator-side hash tracking.
	// Always push on a reconnect (forcePush) so the fresh token reaches the mite.
	rbacInSync := snapshot == nil || desired.RbacYaml == "" || snapshot.RbacHash == rbacH
	if !forcePush &&
		cr.Status.DesiredStateHash == desiredHash &&
		rbacInSync &&
		cr.Status.LastDesiredPushAt != nil &&
		now.Sub(cr.Status.LastDesiredPushAt.Time) < 5*time.Minute {
		return nil
	}
	snapH := "nil"
	if snapshot != nil {
		snapH = snapshot.ActualStateHash
	}
	short := func(s string) string {
		if len(s) < 16 {
			return s
		}
		return s[:16]
	}
	logger.Debug("hash comparison", "snap", short(snapH), "des", short(desiredHash),
		"config", short(configH), "rbac", short(rbacH),
		"items", short(itemsH))

	// Rollout pause gate: when the ComposedBundle is annotated
	// varroa.dev/rollout-paused=true, hold all not-on-target controllers
	// including wave 0. Pause wins over per-controller override. First-provision
	// (empty AppliedBundleHash) and explicit reprovision still proceed.
	// Evaluated before the manual-mode and wave-gate checks so pause supersedes
	// all other gating behavior.
	if targetBundleHash != "" && bundleErr == nil {
		cb, cbErr := r.getComposedBundle(ctx, bundleIdent.Name, bundleIdent.Namespace)
		paused := cbErr == nil && cb != nil &&
			strings.EqualFold(strings.TrimSpace(cb.Annotations["varroa.dev/rollout-paused"]), "true")
		isFirstProvision := cr.Status.AppliedBundleHash == ""
		alreadyOnTarget := cr.Status.AppliedBundleHash == targetBundleHash
		if paused && !isFirstProvision && !alreadyOnTarget && !reprovisionRequested {
			cr.Status.Rollout = &v1alpha1.RolloutStatus{
				TargetBundleHash: targetBundleHash,
				Paused:           true,
			}
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionRolloutPaused,
				Status:  metav1.ConditionTrue,
				Reason:  v1alpha1.ReasonRolloutPaused,
				Message: fmt.Sprintf("rollout paused via annotation on %s/%s", bundleIdent.Namespace, bundleIdent.Name),
			})
			if cr.Status.DesiredStateHash == desiredHash {
				cr.Status.DesiredStateHash = "" // clear stale desired hash so manual mode re-flags on resume
			}
			logger.Info("rollout paused by bundle annotation", "bundle", bundleIdent.Namespace+"/"+bundleIdent.Name)
			return nil
		}
		// On resume (annotation absent or not "true"): clear paused-only state.
		if cr.Status.Rollout != nil && cr.Status.Rollout.Paused && !paused {
			cr.Status.Rollout.Paused = false
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:   v1alpha1.ConditionRolloutPaused,
				Status: metav1.ConditionFalse,
				Reason: v1alpha1.ReasonWaveCleared,
			})
		}
	}

	// Check reconciliation mode.
	policy := r.effectivePolicy(cr)
	if policy.Mode == v1alpha1.ReconciliationModeManual {
		// If the desired state hash hasn't changed since the last push
		// (ApproveRestart sets DesiredStateHash), the mite is still applying
		// the new configuration. Don't re-set PendingRestart yet — wait for
		// the snapshot to catch up. Only re-flag if the mite reported a
		// failure for this hash.
		if cr.Status.DesiredStateHash == desiredHash {
			if hasResultFailureForHash(results, desired, desiredHash) {
				cr.Status.PendingRestart = &v1alpha1.PendingRestart{
					DetectedAt:       now,
					DesiredStateHash: desiredHash,
					Changes:          computeChangedSections(snapshot, desired),
				}
				logger.Info("manual mode, mite reported failure for pending hash, re-flagging")
			}
			return nil
		}
		// Manual mode: set PendingRestart instead of auto-pushing.
		cr.Status.PendingRestart = &v1alpha1.PendingRestart{
			DetectedAt:       now,
			DesiredStateHash: desiredHash,
			Changes:          computeChangedSections(snapshot, desired),
		}
		logger.Info("manual mode, config drift detected, pending approval")
		return nil
	}

	// Wave rollout gate: for controllers with rolloutWave > 0, block the push
	// until every earlier-wave Connected sibling is healthy on targetBundleHash.
	wave := 0
	if cr.Spec.ReconciliationPolicy != nil {
		wave = cr.Spec.ReconciliationPolicy.RolloutWave
	}
	gateEvaluated := false
	hadBlocked := cr.Status.Rollout != nil
	overrideActive := strings.EqualFold(strings.TrimSpace(cr.Annotations[v1alpha1.AnnotationRolloutOverride]), "true")
	gateWouldApply := wave > 0 &&
		cr.Status.AppliedBundleHash != "" &&
		cr.Status.AppliedBundleHash != targetBundleHash &&
		!reprovisionRequested
	if overrideActive && gateWouldApply {
		logger.Info("rollout-override honored, bypassing wave gate")
	}
	if gateWouldApply && !overrideActive {
		gateEvaluated = true
		cleared, waitingOn, err := r.waveGateCleared(ctx, cr, bundleIdent, wave, targetBundleHash)
		if err != nil {
			r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedWaveGateCheckFailed, err.Error())
			return err
		}
		if !cleared {
			now := metav1Now()
			since := &now
			if cr.Status.Rollout != nil && cr.Status.Rollout.Blocked &&
				cr.Status.Rollout.TargetBundleHash == targetBundleHash && cr.Status.Rollout.BlockedSince != nil {
				since = cr.Status.Rollout.BlockedSince
			}
			if len(waitingOn) > 10 {
				waitingOn = waitingOn[:10]
			}
			cr.Status.Rollout = &v1alpha1.RolloutStatus{
				TargetBundleHash: targetBundleHash,
				Blocked:          true,
				WaitingOn:        waitingOn,
				BlockedSince:     since,
			}
			cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
				Type:    v1alpha1.ConditionRolloutBlocked,
				Status:  metav1.ConditionTrue,
				Reason:  v1alpha1.ReasonBlockedByWave,
				Message: fmt.Sprintf("waiting on %d earlier-wave controller(s): %s", len(waitingOn), strings.Join(waitingOn, ", ")),
			})
			return nil
		}
		cr.Status.Rollout = nil
	} else {
		// Clear any stale rollout state if we're no longer gated (wave 0,
		// override active, reprovision, already on target, etc.).
		cr.Status.Rollout = nil
	}
	// Emit RolloutBlocked=False only when the gate just cleared or a previous
	// block is being resolved by a bypass. Unconfigured (wave-0) controllers
	// never get the condition unless they had a prior block.
	if (gateEvaluated && cr.Status.Rollout == nil) || (hadBlocked && cr.Status.Rollout == nil) {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionRolloutBlocked,
			Status: metav1.ConditionFalse,
			Reason: v1alpha1.ReasonWaveCleared,
		})
	}

	// Automatic mode: push desired state to mite.
	// Populate policy fields: idle mode sets ApplyWhen; drain timeout is always set.
	if policy.Mode == v1alpha1.ReconciliationModeIdle {
		desired.ApplyWhen = "idle"
	}
	desired.MaxDeferSec = int64(policy.MaxDeferSeconds)
	desired.DrainTimeoutSec = int64(policy.DrainTimeoutSeconds)

	// Post-boot updates use the reload path (write-then-reload).
	desired.Reload = true

	// Update the per-controller CASC ConfigMap with the current desired
	// state so the next pod (re)start seeds the latest bundle.  The authz
	// value here is the same one carried by desired.RbacYaml (generated
	// once in buildDesiredStateCommand — no divergence).
	cascCM := cascConfigMapName(controllerPrefix(cr))
	cascData := map[string]string{
		"realm.yaml":  generateRealmDocument(),
		"config.yaml": injectProjectNamingStrategy(stripAuthorizationStrategy(resolvedBundle.JenkinsYAML)),
	}
	if desired.RbacYaml != "" {
		cascData["rbac.yaml"] = desired.RbacYaml
	}
	if err := r.client.CreateOrUpdateConfigMap(ctx, cascCM, cr.Namespace, cascData); err != nil {
		logger.Warn("failed to update CASC ConfigMap", "error", err)
		// Non-fatal: the mite will still converge via the desired-state
		// command.  The ConfigMap is the next-boot seed, not the live
		// source.
	}

	cr.Status.DesiredStateHash = desiredHash

	if err := func() error {
		_, span := tracer.Start(ctx, "desiredState.push")
		defer span.End()
		span.SetAttributes(attribute.String("hash", desiredHash))
		return r.miteTransport.Send(ctx, cr.Namespace, cr.Name, &mitev1.OperatorMessage{
			Message: &mitev1.OperatorMessage_DesiredState{
				DesiredState: desired,
			},
		})
	}(); err != nil {
		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedDesiredStatePushFailed, err.Error())
		return fmt.Errorf("send desired state: %w", err)
	}
	cr.Status.LastDesiredPushAt = &now

	r.checkPluginRollFailed(ctx, cr, resolvedBundle, logger)

	// --- Hibernation policy (4.3) ---
	if cr.Spec.Hibernation != nil && cr.Spec.Hibernation.Enabled && cr.Status.Phase == v1alpha1.ControllerPhaseConnected {
		gauges, receivedAt, ok := r.miteTransport.IdleGauges(cr.Namespace, cr.Name)
		if ok {
			shouldHib, lastActivity := shouldHibernate(cr, gauges, receivedAt, now.Time)

			// Keep status.lastActivityAt current every tick (rate-limited to >60s
			// advances to avoid needless status churn), regardless of whether we
			// hibernate — the UI/troubleshooting reads it. The end-of-reconcile
			// PatchControllerStatus in operator.go persists it.
			if !lastActivity.IsZero() &&
				(cr.Status.LastActivityAt == nil || lastActivity.Sub(cr.Status.LastActivityAt.Time) > 60*time.Second) {
				cr.Status.LastActivityAt = &metav1.Time{Time: lastActivity}
			}

			if shouldHib {
				// Persist status.hibernated via the CAS helper, then wake the
				// follow-up reconcile so it observes the flag and scales the
				// StatefulSet down. Persist FIRST: enqueueReconcile is a
				// non-blocking send drained after this reconcile returns, so
				// waking first could let the follow-up read a stale false and
				// skip the scale-down.
				changed, terr := r.client.SetHibernated(ctx, cr.Name, cr.Namespace, true)
				if terr != nil {
					r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedHibernateTransitionFailed, terr.Error())
					return fmt.Errorf("hibernate setHibernated: %w", terr)
				}
				if changed {
					if r.Logger != nil {
						r.Logger.Info("hibernating idle controller", "controller", cr.Name, "namespace", cr.Namespace, "lastActivityAt", lastActivity)
					}

					// Set HibernationCronTriggersSkipped when timer triggers exist.
					if gauges.TimerTriggerJobs > 0 {
						cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
							Type:    v1alpha1.ConditionHibernationCronTriggersSkipped,
							Status:  metav1.ConditionTrue,
							Reason:  v1alpha1.ReasonCronTriggersSkipped,
							Message: fmt.Sprintf("hibernation skipped %d TimerTrigger job(s)", gauges.TimerTriggerJobs),
						})
					}
				}
				r.wakeController(cr.Namespace + "/" + cr.Name)
			}
		}
	}

	return nil
}

func (r *Reconciler) checkPluginRollFailed(ctx context.Context, cr *v1alpha1.Controller, resolved *bundle.MaterializedBundle, logger *slog.Logger) {
	pre := controllerPrefix(cr)
	appliedChecksum, err := r.client.GetStatefulSetPluginsChecksum(ctx, pre, cr.Namespace)
	if err != nil {
		return
	}
	// Resolve bundle if not provided (e.g. called from handleRunning).
	if resolved == nil {
		var resolveErr error
		resolved, _, _, resolveErr = r.resolveBundleForController(ctx, cr)
		if resolveErr != nil {
			resolved = nil
		}
	}
	coreSet := r.coreSetForCr(ctx, cr, logger)
	lines := managedPluginLines(cr, resolved, coreSet)
	desiredChecksum := sha256Hex([]byte(strings.Join(lines, "\n")))

	// No roll annotation means never provisioned — nothing to check.
	if appliedChecksum == "" {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionPluginRollFailed,
			Status: metav1.ConditionFalse,
			Reason: "RollComplete",
		})
		return
	}

	// A roll is in flight or recently completed — inspect the pod.
	pod, err := r.client.GetControllerPod(ctx, cr.Namespace, cr.Name)
	if err != nil {
		logger.Warn("failed to get controller pod for roll-failed check", "error", err)
		return
	}

	// If no pod OR the pod is Ready and checksums match, the roll completed.
	if pod == nil || (appliedChecksum == desiredChecksum && isPodReady(pod)) {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionPluginRollFailed,
			Status: metav1.ConditionFalse,
			Reason: "RollComplete",
		})
		return
	}

	if reason, failed := pluginInitFailureMessage(pod); failed {
		msg := "plugin roll failed"
		if reason != "" {
			msg = reason
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionPluginRollFailed,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonPluginRollFailed,
			Message: msg,
		})
	}
}

func (r *Reconciler) reconcileProvisioningPluginInitFailure(ctx context.Context, cr *v1alpha1.Controller) {
	pod, err := r.client.GetControllerPod(ctx, cr.Namespace, cr.Name)
	if err != nil || pod == nil {
		return
	}
	if reason, failed := pluginInitFailureMessage(pod); failed {
		msg := "plugins-init failed during provisioning"
		if reason != "" {
			msg = reason
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionPluginRollFailed,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ReasonPluginRollFailed,
			Message: msg,
		})
		return
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginRollFailed,
		Status: metav1.ConditionFalse,
		Reason: "PluginInitPending",
	})
}

func pluginInitFailureMessage(pod *corev1.Pod) (string, bool) {
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name != "plugins-init" {
			continue
		}
		if t := s.State.Terminated; t != nil && t.ExitCode != 0 {
			reason := t.Message
			if reason == "" {
				reason = t.Reason
			}
			return reason, true
		}
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull":
				if w.Message == "" {
					return w.Reason, true
				}
				return w.Reason + ": " + w.Message, true
			}
		}
		break
	}
	return "", false
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *Reconciler) handleRunning(ctx context.Context, cr *v1alpha1.Controller) error {
	tracer := otel.Tracer("varroa-operator")
	_, span := tracer.Start(ctx, "reconcile.handleRunning")
	defer span.End()

	// Refresh the bootstrap token BEFORE the timeout guard: the mite may be
	// failing to register precisely because its 15-minute token expired while
	// the pod was still in init (long plugin pulls routinely outlive it), and
	// a tick that fails the controller without reminting leaves even the
	// mite's post-Failed retries presenting a dead token. A connected mite
	// never reads this token, so the refresh is always safe; a still-valid
	// token is never rotated. Warn-only on failure — the next tick retries.
	if _, _, _, connectedNow := r.miteTransport.Info(cr.Namespace, cr.Name); !connectedNow {
		if err := r.ensureBootstrapToken(ctx, cr, controllerPrefix(cr)); err != nil {
			r.Logger.Warn("bootstrap token remint failed",
				"controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
	}

	// Same timeout guard as handleProvisioning — use ProvisioningStartedAt
	// so an operator restart doesn't immediately fail long-lived controllers.
	if cr.Status.ProvisioningStartedAt != nil && time.Since(cr.Status.ProvisioningStartedAt.Time) > provisioningTimeout {
		cr.Status.Phase = v1alpha1.ControllerPhaseFailed
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionFailed,
			Status:  metav1.ConditionTrue,
			Reason:  "ProvisioningTimeout",
			Message: "timed out waiting for mite connection",
		})

		r.markReconcileBlocked(ctx, cr, v1alpha1.ReasonReconcileBlockedMiteConnectTimeout, "timed out waiting for mite connection")
		return fmt.Errorf("provisioning timed out waiting for mite connection")
	}

	// Check if the mite sidecar has connected. Before that, check for
	// failed plugin roll (pod stuck in init).
	logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
	r.reconcileFleetPodLabel(ctx, cr)
	r.checkPluginRollFailed(ctx, cr, nil, logger)

	version, lastHeartbeat, certExpiry, connected := r.miteTransport.Info(cr.Namespace, cr.Name)
	key := cr.Namespace + "/" + cr.Name
	if !connected {
		// (Bootstrap token freshness is handled at the top of this function,
		// before the timeout guard.)
		// Prolonged-disconnect recovery. A controller observed in Running whose
		// mite never (re)connects would otherwise only escape via the 5-minute
		// provisioningTimeout → Failed. Mirror handleConnected: after a short
		// grace period, reset to Pending so the Pending→Provisioning pass runs
		// the FULL provisioning reconcile (CreateStatefulSet). That path re-renders
		// the mite container's VARROA_CA_PEM env and rolls the pod on AlreadyExists
		// — the recovery route after a control-plane reinstall regenerates the CA
		// (UpdateStatefulSetOIDCEnv would not, as it only patches the Jenkins
		// container).
		r.disconnectedTicks[key]++
		if r.disconnectedTicks[key] < 3 {
			// Grace period: mite may be (re)connecting after an operator or
			// control-plane restart.
			return nil
		}

		// Clear the token record so a fresh one is issued on reconnect. The
		// mite's in-memory token is lost when it restarts.
		r.miteTokenMu.Lock()
		delete(r.miteTokens, key)
		r.miteTokenMu.Unlock()
		// Reset the disconnected-tick counter so the next Provisioning→Running
		// attempt gets a fresh grace window instead of immediately re-forcing
		// Pending (which would thrash Pending↔Provisioning↔Running).
		delete(r.disconnectedTicks, key)
		cr.Status.Phase = v1alpha1.ControllerPhasePending
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "MiteDisconnected",
			Message: "mite sidecar disconnected",
		})
		return nil
	}
	// Reset disconnected counter when mite is connected.
	delete(r.disconnectedTicks, key)

	// Mite is connected — transition to Connected and populate MiteStatus.
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	cr.Status.MiteStatus = &v1alpha1.MiteStatus{
		Connected:  true,
		Version:    version,
		LastSeen:   &metav1.Time{Time: lastHeartbeat},
		CertExpiry: &metav1.Time{Time: certExpiry},
	}
	snapshot := r.miteTransport.Snapshot(cr.Namespace, cr.Name)
	if snapshot != nil {
		cr.Status.MiteStatus.JenkinsVersion = snapshot.JenkinsVersion
		cr.Status.MiteStatus.JenkinsHealth = snapshot.JenkinsHealth
		cr.Status.MiteStatus.LastHealthCheck = &metav1.Time{Time: lastHeartbeat}
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "MiteConnected",
		Message: "mite sidecar connected and streaming",
	})
	return nil
}

func (r *Reconciler) handleFailed(_ context.Context, cr *v1alpha1.Controller) {
	logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
	// Reset to Pending to retry provisioning. The mite may have reconnected
	// through a different gateway, or the operator may have restarted.
	cr.Status.Phase = v1alpha1.ControllerPhasePending
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionFailed,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1Now(),
		Reason:             "Retrying",
		Message:            "retrying provisioning after failure",
	})
	logger.Info("retrying provisioning after failed phase")
}

// Finalize cleans up resources when a Controller is deleted.
func (r *Reconciler) Finalize(ctx context.Context, cr *v1alpha1.Controller) error {
	logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
	logger.Info("finalizing controller")

	// Delete JenkinsRoleBindings owned by this controller.
	if err := r.deleteControllerBindings(ctx, cr); err != nil {
		logger.Warn("failed to delete controller bindings", "error", err)
	}

	pre := controllerPrefix(cr)

	// Delete auto-generated inline ComposedBundle ({name}-bundle) if it exists.
	if cr.Spec.ComposedBundleRef != nil && cr.Spec.ComposedBundleRef.Name == cr.Name+"-bundle" {
		_ = crdstore.Delete[v1alpha1.ComposedBundle](ctx, r.store, cr.Spec.ComposedBundleRef.Name, cr.Namespace)
	}

	// Delete StatefulSet
	if err := r.client.DeleteResource(ctx, "StatefulSet", pre, cr.Namespace); err != nil {
		return err
	}
	// Delete ConfigMaps
	for _, suffix := range []string{"-init", "-plugins"} {
		_ = r.client.DeleteResource(ctx, "ConfigMap", pre+suffix, cr.Namespace)
	}
	// Delete bootstrap Secret
	_ = r.client.DeleteSecret(ctx, pre+"-bootstrap", cr.Namespace)
	// Delete Service
	r.deleteWakeSlice(ctx, cr, logger)
	_ = r.client.DeleteResource(ctx, "Service", pre+"-svc", cr.Namespace)
	// Delete PVCs (StatefulSet volumeClaimTemplates are not auto-deleted)
	_ = r.client.DeleteResource(ctx, "PersistentVolumeClaim", "jenkins-home-"+pre+"-0", cr.Namespace)
	// Delete Ingress
	_ = r.client.DeleteResource(ctx, "Ingress", pre+"-ingress", cr.Namespace)
	return nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

// overlayActiveCondition builds the OverlayActive condition for a Controller
// based on whether spec.resourceOverlay has any sub-field set.
func overlayActiveCondition(cr *v1alpha1.Controller) v1alpha1.ControllerCondition {
	ro := cr.Spec.ResourceOverlay
	var set []string
	if ro != nil {
		if ro.StatefulSet != "" {
			set = append(set, "statefulSet")
		}
		if ro.Service != "" {
			set = append(set, "service")
		}
		if ro.Ingress != "" {
			set = append(set, "ingress")
		}
	}
	if len(set) == 0 {
		return v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionOverlayActive,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.ReasonNoResourceOverlay,
			Message: "no resource overlay configured",
		}
	}
	return v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionOverlayActive,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonResourceOverlaySet,
		Message: strings.Join(set, ", "),
	}
}

// setCondition upserts a condition by Type — only one entry per ConditionType.
// removeCondition removes conditions with the given type and reason.
func removeCondition(conditions []v1alpha1.ControllerCondition, ctype v1alpha1.ControllerConditionType, reason string) []v1alpha1.ControllerCondition {
	var result []v1alpha1.ControllerCondition
	for _, c := range conditions {
		if c.Type == ctype && c.Reason == reason {
			continue
		}
		result = append(result, c)
	}
	return result
}

// removeConditionByType removes every condition with the given type, whatever
// its reason. Use when a condition stops being meaningful at all (rather than
// changing state) — removeCondition matches on reason too, so it silently
// no-ops when the live reason differs from the one you pass.
func removeConditionByType(conditions []v1alpha1.ControllerCondition, ctype v1alpha1.ControllerConditionType) []v1alpha1.ControllerCondition {
	var result []v1alpha1.ControllerCondition
	for _, c := range conditions {
		if c.Type == ctype {
			continue
		}
		result = append(result, c)
	}
	return result
}

// setCondition upserts a condition by Type — only one entry per ConditionType.
func setCondition(conditions []v1alpha1.ControllerCondition, c v1alpha1.ControllerCondition) []v1alpha1.ControllerCondition {
	// Stamp LastTransitionTime with standard k8s-condition semantics: preserve it
	// while the Status is unchanged, and advance it only on an actual transition.
	// This gives, e.g., the Ready condition a reliable "last entered this state"
	// timestamp (the hibernation connect-time floor relies on it). Conditions are
	// persisted every reconcile (operator.go), so `existing` carries the stored
	// time. An explicit non-zero LastTransitionTime from the caller wins.
	stamp := func(cond v1alpha1.ControllerCondition, prev *v1alpha1.ControllerCondition) v1alpha1.ControllerCondition {
		if !cond.LastTransitionTime.IsZero() {
			return cond
		}
		if prev != nil && prev.Status == cond.Status && !prev.LastTransitionTime.IsZero() {
			cond.LastTransitionTime = prev.LastTransitionTime
		} else {
			cond.LastTransitionTime = metav1.Now()
		}
		return cond
	}
	for i, existing := range conditions {
		if existing.Type == c.Type {
			conditions[i] = stamp(c, &existing)
			return conditions
		}
	}
	return append(conditions, stamp(c, nil))
}

// findCondition returns a pointer to the condition with the given type, or nil.
func findCondition(conds []v1alpha1.ControllerCondition, t v1alpha1.ControllerConditionType) *v1alpha1.ControllerCondition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// shouldEmitPluginConflictEvent reports whether a plugin-lock-conflict activity
// event should be emitted for this pass: only on the False/absent -> True edge.
func shouldEmitPluginConflictEvent(prior *v1alpha1.ControllerCondition, conflictNow bool) bool {
	if !conflictNow {
		return false
	}
	return prior == nil || prior.Status != metav1.ConditionTrue
}

// surfacePluginConflict sets ConditionPluginConflict=True, records the gauge,
// and emits a pluginConflict.detected activity event on the False/absent→True
// edge. It does NOT markReconcileBlocked or return an error — the caller
// decides whether the conflict should block the current phase.
func (r *Reconciler) surfacePluginConflict(ctx context.Context, cr *v1alpha1.Controller, conflict string) {
	// Read prior state BEFORE setCondition — findCondition returns a pointer
	// into the slice, and setCondition overwrites the same memory location.
	prior := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginConflict)
	shouldEmit := shouldEmitPluginConflictEvent(prior, true)

	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPluginConflict,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonPluginConflict,
		Message: conflict,
	})
	pluginLockConflictGauge.Record(ctx, 1, metric.WithAttributes(
		attribute.String("namespace", cr.Namespace),
		attribute.String("controller", cr.Name),
	))
	if shouldEmit {
		if r.activityPublisher != nil {
			r.activityPublisher.Publish(activity.Event{
				Type:       "pluginConflict.detected",
				Source:     "operator",
				Controller: cr.Name,
				Namespace:  cr.Namespace,
				Message:    conflict,
				Reason:     v1alpha1.ReasonPluginConflict,
				Severity:   "warning",
			})
		}
	}
}

// clearPluginConflict clears ConditionPluginConflict (sets False/NoConflict)
// and resets the gauge to 0.
func (r *Reconciler) clearPluginConflict(ctx context.Context, cr *v1alpha1.Controller) {
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginConflict,
		Status: metav1.ConditionFalse,
		Reason: "NoConflict",
	})
	pluginLockConflictGauge.Record(ctx, 0, metric.WithAttributes(
		attribute.String("namespace", cr.Namespace),
		attribute.String("controller", cr.Name),
	))
}

// pluginPinConflictMessage formats a PinPreflightReport's conflicts for
// ConditionPluginPinConflict's message, naming each conflicting artifact.
func pluginPinConflictMessage(report bundle.PinPreflightReport) string {
	parts := make([]string, 0, len(report.Conflicts))
	for _, c := range report.Conflicts {
		parts = append(parts, fmt.Sprintf("%s (bundle pins %s, set has %s)", c.ArtifactID, c.BundleVersion, c.SetVersion))
	}
	return "plugin pin conflict: " + strings.Join(parts, ", ")
}

// surfacePluginPinConflict sets ConditionPluginPinConflict=True on cr, patches
// the referenced ComposedBundle's own PluginPinConflict condition, and emits a
// pluginPinConflict.detected activity event on the False/absent→True edge.
// This is a distinct signal from ConditionPluginConflict — it does NOT
// markReconcileBlocked and never blocks the calling phase.
func (r *Reconciler) surfacePluginPinConflict(ctx context.Context, cr *v1alpha1.Controller, bundleIdent bundleIdentity, message string) {
	// Read prior state BEFORE setCondition — findCondition returns a pointer
	// into the slice, and setCondition overwrites the same memory location.
	prior := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginPinConflict)
	shouldEmit := shouldEmitPluginConflictEvent(prior, true)

	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPluginPinConflict,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonPluginPinConflict,
		Message: message,
	})
	r.patchBundlePluginPinConflict(ctx, cr, bundleIdent, true, message)
	if shouldEmit && r.activityPublisher != nil {
		r.activityPublisher.Publish(activity.Event{
			Type:       "pluginPinConflict.detected",
			Source:     "operator",
			Controller: cr.Name,
			Namespace:  cr.Namespace,
			Message:    message,
			Reason:     v1alpha1.ReasonPluginPinConflict,
			Severity:   "warning",
		})
	}
}

// clearPluginPinConflict clears ConditionPluginPinConflict (False/NoConflict)
// on cr and on the referenced ComposedBundle.
func (r *Reconciler) clearPluginPinConflict(ctx context.Context, cr *v1alpha1.Controller, bundleIdent bundleIdentity) {
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:   v1alpha1.ConditionPluginPinConflict,
		Status: metav1.ConditionFalse,
		Reason: "NoConflict",
	})
	r.patchBundlePluginPinConflict(ctx, cr, bundleIdent, false, "")
}

// patchBundlePluginPinConflict patches the referenced ComposedBundle's own
// PluginPinConflict condition with a Conditions-only status merge, bypassing
// CatalogReconciler.buildStatus/patchStatus entirely. Multiple controllers
// reconciling the same ComposedBundle leave last-writer-wins semantics on this
// condition — it is not an aggregate across referencing controllers, so the
// message names which controller last set it.
func (r *Reconciler) patchBundlePluginPinConflict(ctx context.Context, cr *v1alpha1.Controller, bundleIdent bundleIdentity, conflictNow bool, message string) {
	cb, err := r.getComposedBundle(ctx, bundleIdent.Name, bundleIdent.Namespace)
	if err != nil {
		return
	}
	status, reason, msg := metav1.ConditionFalse, "NoConflict", ""
	if conflictNow {
		status = metav1.ConditionTrue
		reason = v1alpha1.ReasonPluginPinConflict
		msg = fmt.Sprintf("%s (controller %s/%s)", message, cr.Namespace, cr.Name)
	}
	conditions := setTemplateCatalogCondition(cb.Status.Conditions, v1alpha1.TemplateCatalogCondition{
		Type:    string(v1alpha1.ConditionPluginPinConflict),
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	if err := crdstore.PatchStatus[v1alpha1.ComposedBundle](ctx, r.store, cb.Name, cb.Namespace, &v1alpha1.ComposedBundleStatus{
		Conditions: conditions,
	}); err != nil {
		r.Logger.Warn("failed to patch composed bundle PluginPinConflict condition", "name", cb.Name, "namespace", cb.Namespace, "error", err)
	}
}

// setTemplateCatalogCondition upserts a condition by Type in a
// []TemplateCatalogCondition slice — the ComposedBundle-status analogue of
// setCondition, needed because TemplateCatalogCondition.Type is a plain string
// rather than ControllerConditionType.
func setTemplateCatalogCondition(conditions []v1alpha1.TemplateCatalogCondition, c v1alpha1.TemplateCatalogCondition) []v1alpha1.TemplateCatalogCondition {
	for i, existing := range conditions {
		if existing.Type == c.Type {
			if existing.Status == c.Status && !existing.LastTransitionTime.IsZero() {
				c.LastTransitionTime = existing.LastTransitionTime
			} else {
				c.LastTransitionTime = metav1Now()
			}
			conditions[i] = c
			return conditions
		}
	}
	c.LastTransitionTime = metav1Now()
	return append(conditions, c)
}

func metav1Now() metav1.Time {
	return metav1.Time{Time: time.Now()}
}

//nolint:unparam
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func sameOutcome(prev *v1alpha1.ApplyResult, next v1alpha1.ApplyResult) bool {
	if prev == nil {
		return false
	}
	if prev.Hash != next.Hash || len(prev.Sections) != len(next.Sections) {
		return false
	}
	for i := range prev.Sections {
		if prev.Sections[i] != next.Sections[i] {
			return false
		}
	}
	return true
}

// reconcileService derives the desired Service for a controller and creates or
// updates it. Shared by handleProvisioning (initial create) and the
// post-provisioning reconcile path so port changes converge instead of going
// stale — a Service missing the inbound agent port must converge to add it.
func (r *Reconciler) reconcileService(ctx context.Context, cr *v1alpha1.Controller) error {
	return r.client.CreateService(ctx, controllerPrefix(cr)+"-svc", cr.Namespace, 8080, resourceOverlayService(cr))
}

// reconcileIngress derives the desired Ingress for a controller and creates or
// updates it. It is the single source of truth for ingress derivation,
// shared by handleProvisioning (initial create) and the post-provisioning
// reconcile path so changes to TLS, annotations, or host converge instead of
// going stale. Returns nil when no host resolves (nothing to reconcile).
func (r *Reconciler) reconcileIngress(ctx context.Context, cr *v1alpha1.Controller) error {
	var defaults *v1alpha1.ProvisioningDefaults
	if d, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, ""); err == nil && d != nil {
		defaults = d
	}

	rootDomain := ""
	if defaults != nil {
		rootDomain = defaults.Spec.RootDomain
	}
	resolvedHost := v1alpha1.ResolveHost(cr, rootDomain)
	if resolvedHost == "" {
		// No spec.ingressSpec.host and no ProvisioningDefaults.rootDomain: there
		// is nothing to put in an Ingress rule. That is a supported way to run —
		// evaluation and CI use kubectl port-forward — so this is not an error.
		// It is surfaced as a condition because returning nil silently made a
		// missing rootDomain look exactly like a broken ingress controller.
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:               v1alpha1.ConditionNoExternalURL,
			Status:             metav1.ConditionTrue,
			Reason:             "NoHostResolved",
			Message:            "no ingress host resolved; reach this controller with `kubectl port-forward` or set ProvisioningDefaults.rootDomain (or spec.ingressSpec.host)",
			LastTransitionTime: metav1Now(),
		})
		return nil
	}
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionNoExternalURL); c != nil &&
		c.Status == metav1.ConditionTrue {
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:               v1alpha1.ConditionNoExternalURL,
			Status:             metav1.ConditionFalse,
			Reason:             "HostResolved",
			Message:            "ingress host " + resolvedHost,
			LastTransitionTime: metav1Now(),
		})
	}

	// Path-mode routing shares the dashboard host and terminates TLS upstream,
	// so the per-controller Ingress carries a path prefix and no TLS block.
	pathPrefix := ""
	tlsSecretName := ""
	if cr.Spec.IngressSpec != nil {
		tlsSecretName = cr.Spec.IngressSpec.TLSSecretName
		if cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModePath {
			pathPrefix = v1alpha1.PathPrefix(cr.Namespace, cr.Name)
			tlsSecretName = ""
		}
	}

	ingressClass := "nginx"
	var ingressAnnotations map[string]string
	if defaults != nil {
		if defaults.Spec.IngressClassName != "" {
			ingressClass = defaults.Spec.IngressClassName
		}
		ingressAnnotations = defaults.Spec.IngressAnnotations
	}
	// Class tier: insert between ProvisioningDefaults and Controller spec.
	// Gracefully degrade when the class is not found (treat as empty).
	if class, ok := r.resolveClassForCr(ctx, cr); ok && class != nil {
		if class.Spec.IngressClassName != "" {
			ingressClass = class.Spec.IngressClassName
		}
		ingressAnnotations = v1alpha1.MergeStringMaps(ingressAnnotations, class.Spec.IngressAnnotations)
	}
	if cr.Spec.IngressSpec != nil {
		if cr.Spec.IngressSpec.IngressClassName != "" {
			ingressClass = cr.Spec.IngressSpec.IngressClassName
		}
		ingressAnnotations = v1alpha1.MergeIngressAnnotations(ingressAnnotations, cr.Spec.IngressSpec.Annotations)
	}

	ingressName := controllerPrefix(cr) + "-ingress"
	return r.client.CreateIngress(ctx, ingressName, cr.Namespace, resolvedHost, pathPrefix, tlsSecretName, ingressClass, ingressAnnotations, resourceOverlayIngress(cr))
}

// reconcileVersionRoll compares the effective desired jenkins image against
// the applied (stamped) image and, on a gate-allowed delta, transitions a
// Running/Connected controller to Provisioning so the existing provisioning
// flow rolls the StatefulSet image. Returns true when it transitioned the
// phase (caller must skip the normal phase dispatch this tick). Design section 3.
func (r *Reconciler) reconcileVersionRoll(ctx context.Context, cr *v1alpha1.Controller) bool {
	gate := r.versionRollGate
	if gate == nil {
		gate = allowAllVersionRolls
	}
	desired := r.effectiveDesiredJenkinsImage(cr)
	computed, live, err := r.client.GetStatefulSetImages(ctx, controllerPrefix(cr), cr.Namespace)
	if err != nil {
		r.Logger.Warn("version-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		return false
	}
	if live == nil {
		return false // no StatefulSet yet: nothing to roll
	}
	applied, ok := computed["jenkins"]
	if !ok {
		applied = live["jenkins"] // pre-stamp fallback
	}
	if applied == "" {
		return false // defensive: unrecognizable StatefulSet
	}

	// Does the resourceOverlay declare the jenkins image? (for the converged message)
	overlayGoverns := false
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if _, ovOK, ovErr := overlay.ImageOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), "jenkins"); ovErr == nil && ovOK {
			overlayGoverns = true
		}
	}

	if applied == desired {
		msg := fmt.Sprintf("jenkins image converged: %s", desired)
		if overlayGoverns {
			msg += "; desired image declared by resourceOverlay"
		} else if live["jenkins"] != desired {
			msg += fmt.Sprintf("; live image %s preserved (out-of-band override)", live["jenkins"])
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionVersionRollPending,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.ReasonVersionConverged,
			Message: msg,
		})
		clearUpgradePending(cr)
		return false
	}

	allow, reason, message := gate(ctx, cr, applied, desired)
	if !allow {
		if reason == "" {
			reason = "VersionRollHeld"
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionVersionRollPending,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: message,
		})
		return false
	}

	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionVersionRollPending,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonVersionRollStarted,
		Message: fmt.Sprintf("rolling jenkins image %s -> %s", applied, desired),
	})
	now := metav1Now()
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	cr.Status.ProvisioningStartedAt = &now
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ProvisioningStarted",
		Message:            "re-provisioning to apply jenkins image change",
	})
	return true
}

// reconcileSharedHostMismatch sets or clears the Degraded/SharedHostMismatch
// condition depending on whether the path-mode controller's Host matches the
// dashboard host. Non-path-mode controllers get the condition cleared.
func (r *Reconciler) reconcileSharedHostMismatch(cr *v1alpha1.Controller) {
	if cr.Spec.IngressSpec == nil || cr.Spec.IngressSpec.RoutingMode() != v1alpha1.RoutingModePath {
		// Clear any stale SharedHostMismatch condition.
		cr.Status.Conditions = removeCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded, "SharedHostMismatch")
		return
	}
	frontendURL := varroaBaseURL(r.varroaRedirectURL)
	u, err := url.Parse(frontendURL)
	if err != nil || u.Hostname() == "" || u.Hostname() == cr.Spec.IngressSpec.Host {
		cr.Status.Conditions = removeCondition(cr.Status.Conditions, v1alpha1.ConditionDegraded, "SharedHostMismatch")
		return
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  "SharedHostMismatch",
		Message: fmt.Sprintf("ingressSpec.host %q does not match dashboard host %q; browser auth will fail", cr.Spec.IngressSpec.Host, u.Hostname()),
	})
}

func adminLockoutOverridden(cr *v1alpha1.Controller) bool {
	return cr.Annotations[annotationAllowAdminLockout] == "true"
}

// bundlePluginEntry is a single plugin entry from a bundle's plugins.yaml.
type bundlePluginEntry struct {
	ArtifactID string `yaml:"artifactId"`
	Version    string `yaml:"version"`
}

// parsePluginEntries parses a plugins.yaml file from a bundle and returns
// CRD PluginEntry values.
func parsePluginEntries(yamlContent string) ([]v1alpha1.PluginEntry, error) {
	type pluginsFile struct {
		Plugins []bundlePluginEntry `yaml:"plugins"`
	}
	var pf pluginsFile
	if err := yaml.Unmarshal([]byte(yamlContent), &pf); err != nil {
		return nil, err
	}
	var entries []v1alpha1.PluginEntry
	for _, p := range pf.Plugins {
		entries = append(entries, v1alpha1.PluginEntry{
			ArtifactId: p.ArtifactID,
			Version:    p.Version,
		})
	}
	return entries, nil
}

func (r *Reconciler) buildDesiredStateCommand(cr *v1alpha1.Controller, resolved *bundle.MaterializedBundle) *mitev1.DesiredStateCommand {
	logger := r.Logger.With("controller", cr.Namespace+"/"+cr.Name, "phase", cr.Status.Phase)
	cmd := &mitev1.DesiredStateCommand{
		CommandId: fmt.Sprintf("%d", time.Now().UnixNano()),
		// Config push always uses the MANAGE-gated /configuration-as-code/reload
		// path, never the admin-gated apply path, so the mite can run without
		// Jenkins.ADMINISTER. Reload is always true; the flag is retained on the
		// proto for compatibility.
		Reload: true,
	}

	// JCasC and items YAML from resolved bundle.
	// Varroa owns the authorization strategy — strip any
	// authorizationStrategy key the bundle may carry.
	if resolved != nil {
		// Varroa owns the authorization strategy; also inject the project
		// naming strategy so item/folder role patterns are enforced at
		// job-create time.
		cmd.JcascYaml = injectProjectNamingStrategy(
			stripAuthorizationStrategy(resolved.JenkinsYAML))
		cmd.ItemsYaml = resolved.ItemsYAML
	}

	// RBAC: generate from Group/JenkinsRole CRDs.
	// Authz is kept separate (RbacYaml) — not merged into JcascYaml —
	// so it can be written as a distinct file (rbac.yaml) for CASC boot
	// and reload. The same value is passed to both the ConfigMap bundle
	// and the mite command.
	rbacYAML, humanAdmin, err := r.rbacGenerator.GenerateWithAdminCheck(cr)
	switch {
	case err != nil:
		logger.Error("rbac generation failed", "error", err)
	case !humanAdmin && !adminLockoutOverridden(cr):
		logger.Warn("skipping RBAC push: generated authz would leave no human admin",
			"controller", cr.Namespace+"/"+cr.Name)
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionRBACLockoutRisk, Status: metav1.ConditionTrue,
			Reason:  "NoHumanAdmin",
			Message: "generated RBAC grants admin only to the operator mite; refusing to push (set annotation varroa.dev/allow-admin-lockout=true to override)",
		})
	default:
		cmd.RbacYaml = rbacYAML
		reason, msg := "HumanAdminPresent", "a human/group administrator is present in generated RBAC"
		if !humanAdmin {
			reason, msg = "LockoutOverridden", "no human admin, but varroa.dev/allow-admin-lockout=true was set"
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type: v1alpha1.ConditionRBACLockoutRisk, Status: metav1.ConditionFalse, Reason: reason, Message: msg,
		})
	}

	// Attach a mite Jenkins token to every desired-state command.
	// The token is cached in-memory and regenerated only when missing or
	// within the refresh window. Always attach the cached token so a
	// mite that restarts (losing in-memory state) gets a valid token on
	// the very next push — even if the desired state is unchanged.
	if r.miteTokenSigner != nil {
		key := cr.Namespace + "/" + cr.Name
		token, exp := r.mintOrGetMiteToken(key)
		cmd.MiteJenkinsToken = token
		cmd.MiteJenkinsTokenExp = exp
	}

	// Push ProvisioningDefaults fields. Snapshot under mutex to avoid
	// races with the provisioning-defaults runnable writer.
	r.provisioningDefaultsMu.Lock()
	defs := r.provisioningDefaults
	r.provisioningDefaultsMu.Unlock()
	if defs != nil && defs.Spec.CommandDeadlineSec > 0 {
		cmd.CommandDeadlineSec = int64(defs.Spec.CommandDeadlineSec)
	}
	if defs != nil && defs.Spec.LiveFingerprintIntervalSec != 0 {
		cmd.LiveFingerprintIntervalSec = int64(defs.Spec.LiveFingerprintIntervalSec)
	}

	cmd.DesiredStateHash = computeDesiredStateHash(cmd)
	return cmd
}

// MintMiteToken is the public entry point for token minting, callable from
// outside the reconciler (e.g., by the mite gRPC Server's TokenGrantFunc).
// It returns an empty token and 0 expiry on failure (signer not configured).
func (r *Reconciler) MintMiteToken(key string) (token string, expUnix int64, err error) {
	if r.miteTokenSigner == nil {
		return "", 0, fmt.Errorf("mite token signer not configured")
	}
	token, expUnix = r.mintOrGetMiteToken(key)
	return token, expUnix, nil
}

// MintMiteTokenForce is the force-mint variant used for token refresh grants.
// It always mints a fresh token whose expiry strictly exceeds any cached token
// for the same mite, updates the per-mite cache, and returns the new token and
// expiry. The full mint+cache update is serialized under miteTokenMu to prevent
// concurrent refreshes from regressing the cache.
func (r *Reconciler) MintMiteTokenForce(key string) (token string, expUnix int64, err error) {
	if r.miteTokenSigner == nil {
		return "", 0, fmt.Errorf("mite token signer not configured")
	}

	const defaultTTL = 60 * time.Minute

	r.miteTokenMu.Lock()
	defer r.miteTokenMu.Unlock()

	ttl := defaultTTL
	if existing, ok := r.miteTokens[key]; ok {
		minTTL := time.Until(existing.exp) + time.Second
		if minTTL > ttl {
			ttl = minTTL
		}
	}

	token, err = r.miteTokenSigner.GenerateMiteJenkinsToken(keyToName(key), keyToNamespace(key), ttl)
	if err != nil {
		return "", 0, fmt.Errorf("generate mite jenkins token: %w", err)
	}
	expUnix = time.Now().Add(ttl).Unix()

	r.miteTokens[key] = miteTokenEntry{token: token, exp: time.Unix(expUnix, 0)}
	return token, expUnix, nil
}

// mintOrGetMiteToken returns a cached or freshly minted Jenkins token for the
// mite identified by key ("ns/name"). It is guarded by miteTokenMu and mints
// only on miss or when the cached token is within the refresh window (default:
// <10m remaining of a 60m TTL). Concurrent callers see the same cached entry.
func (r *Reconciler) mintOrGetMiteToken(key string) (token string, expUnix int64) {
	const (
		tokenTTL      = 60 * time.Minute
		refreshWindow = 10 * time.Minute
	)
	r.miteTokenMu.Lock()
	defer r.miteTokenMu.Unlock()

	entry, ok := r.miteTokens[key]
	if !ok || time.Until(entry.exp) < refreshWindow {
		token, err := r.miteTokenSigner.GenerateMiteJenkinsToken(keyToName(key), keyToNamespace(key), tokenTTL)
		if err != nil {
			r.Logger.Warn("failed to generate mite jenkins token", "key", key, "error", err)
			// Return the existing entry if we had one (even if stale),
			// otherwise empty strings.
			if ok {
				return entry.token, entry.exp.Unix()
			}
			return "", 0
		}
		entry = miteTokenEntry{token: token, exp: time.Now().Add(tokenTTL)}
		r.miteTokens[key] = entry
	}
	return entry.token, entry.exp.Unix()
}

// keyToName extracts the controller name from a "ns/name" key.
func keyToName(key string) string {
	idx := strings.Index(key, "/")
	if idx < 0 {
		return key
	}
	return key[idx+1:]
}

// keyToNamespace extracts the namespace from a "ns/name" key.
func keyToNamespace(key string) string {
	idx := strings.Index(key, "/")
	if idx < 0 {
		return ""
	}
	return key[:idx]
}

func defaultMiteImage() string {
	if img := os.Getenv("VARROA_MITE_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/varroaci/varroa-jenkins:main"
}

// jenkinsImageForVersion returns the operator-computed Jenkins image for a
// spec.version value. When the matched profile pins spec.version to an LTS line
// (2-segment) and carries a ResolveVersion (the exact patch its plugin set was
// resolved against), ResolveVersion is deployed instead of the bare line —
// otherwise the running core can be older than the core the pinned plugins
// require, crash-looping plugins-init
// (AggregatePluginPrerequisitesNotMetException).
//
// The unpinned sentinels "" and "lts" resolve to the embedded plugin-lock
// baseline, NOT to a floating :latest or :lts tag. ResolveProfile returns
// (nil, MatchBaseline) for both unconditionally, and resolveCoreSet then pins
// plugins to pluginlock.Baseline() — so deploying a moving core against a
// build-time-pinned plugin set reproduces exactly the mismatch this
// function exists to avoid. Keeping both sides on Baseline() makes core and
// plugin set agree by construction, and keeps this in step with
// EvaluateCoreCompat, which reports both sentinels as baseline-backed.
func jenkinsImageForVersion(version string, profile *v1alpha1.JenkinsVersionProfile) string {
	if profile != nil && profile.Spec.ResolveVersion != "" {
		return "jenkins/jenkins:" + profile.Spec.ResolveVersion
	}
	// Trim to stay in step with ResolveProfile and EvaluateCoreCompat, which both
	// normalize before matching. Without this, a whitespace-only spec.version
	// misses the sentinel check here while hitting it there, and emits an invalid
	// "jenkins/jenkins: " tag.
	v := strings.TrimSpace(version)
	if v == "" || v == "lts" {
		return "jenkins/jenkins:" + pluginlock.Baseline()
	}
	return "jenkins/jenkins:" + v
}

// effectiveDesiredMiteImage returns the mite sidecar image that provisioning
// bakes into the StatefulSet (mite/init-groovy/casc-seed containers all share
// it) and that reconcileContainerSpecRoll compares against — a single function so
// the two call sites can never diverge on the defaulting rule. Precedence: an
// explicit resourceOverlay.statefulSet image for the
// mite container wins (mirrors effectiveDesiredJenkinsImage — CreateStatefulSet
// applies overlays before stamping varroa.dev/computed-images, so the stamped/
// live image reflects the overlay, not the class/default; comparing against the
// class/default here instead would desync from that stamp forever and the mite
// image would appear perpetually out of convergence); otherwise the Controller's
// spec.miteSpec.image (when set — takes precedence over the class tier below);
// otherwise the resolved ControllerClass's spec.mite.image (when class is
// configured and resolved); otherwise the operator-wide default from
// defaultMiteImage() (VARROA_MITE_IMAGE env var, or the baked-in fallback). An
// empty class image must never be treated as a live delta against that default.
func (r *Reconciler) effectiveDesiredMiteImage(cr *v1alpha1.Controller) string {
	desired := defaultMiteImage()
	// Class tier: class.Spec.Mite.Image wins over operator default (when class
	// is configured, resolved, and its mite image is non-empty).
	if class, ok := r.resolveClassForCr(context.TODO(), cr); ok && class != nil &&
		class.Spec.Mite != nil && class.Spec.Mite.Image != "" {
		desired = class.Spec.Mite.Image
	}
	// Controller-level tier wins over class/default, loses to resourceOverlay.
	if cr.Spec.MiteSpec != nil && cr.Spec.MiteSpec.Image != "" {
		desired = cr.Spec.MiteSpec.Image
	}
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if img, ok, err := overlay.ImageOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), overlay.MiteContainerName); err != nil {
			r.Logger.Debug("overlay image parse failed; using class/default mite image", "error", err)
		} else if ok {
			desired = img
		}
	}
	return desired
}

// resolveRunningMiteImage returns the mite image actually observed for cr,
// and whether any observation was possible at all (found).
//
// Read order (first usable wins):
//  1. Pod container image (ground truth: what the kubelet actually has
//     scheduled right now).
//  2. StatefulSet computed-images stamp, else StatefulSet live template
//     image — used for Stopped/Hibernated (no Pod) and for a Pod that
//     exists but hasn't picked up a mite container image yet.
//  3. Neither observable: return ("", false). Caller must leave any prior
//     MiteStatus.Image/condition untouched, not clear it.
//
// A genuine Pod-read error (not "not found") is NOT the same signal as "no
// Pod scheduled yet" — it skips this tick entirely rather than falling back
// to the StatefulSet tier and reporting a comparison that isn't ground-truth.
func (r *Reconciler) resolveRunningMiteImage(ctx context.Context, cr *v1alpha1.Controller) (image string, found bool) {
	pod, err := r.client.GetControllerPod(ctx, cr.Namespace, cr.Name)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Error("resolveRunningMiteImage: pod read failed, skipping this tick",
				"controller", cr.Name, "namespace", cr.Namespace, "error", err)
		}
		return "", false
	}
	if pod != nil {
		for _, c := range pod.Spec.Containers {
			if c.Name == overlay.MiteContainerName && c.Image != "" {
				return c.Image, true
			}
		}
	}
	computedMiteImage, liveMiteImage, _, _, _, _, stsFound, err := r.client.GetStatefulSetContainerSpecs(ctx, controllerPrefix(cr), cr.Namespace)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Error("resolveRunningMiteImage: statefulset read failed, skipping this tick",
				"controller", cr.Name, "namespace", cr.Namespace, "error", err)
		}
		return "", false
	}
	if !stsFound {
		return "", false
	}
	if computedMiteImage != "" {
		return computedMiteImage, true
	}
	return liveMiteImage, liveMiteImage != ""
}

// refreshMiteImageStaleness compares the running mite image (see
// resolveRunningMiteImage) against effectiveDesiredMiteImage(cr) and updates
// MiteStatus.Image, the MiteImageStale condition, and the
// varroa_controller_mite_image_stale gauge. Detection only — never
// initiates a roll. Safe to call every tick regardless of phase; a no-op
// when nothing is observable yet.
func (r *Reconciler) refreshMiteImageStaleness(ctx context.Context, cr *v1alpha1.Controller) {
	running, found := r.resolveRunningMiteImage(ctx, cr)
	if !found {
		return
	}
	if cr.Status.MiteStatus == nil {
		cr.Status.MiteStatus = &v1alpha1.MiteStatus{}
	}
	cr.Status.MiteStatus.Image = running

	desired := r.effectiveDesiredMiteImage(cr)
	stale := running != desired
	status, reason := metav1.ConditionFalse, v1alpha1.ReasonMiteImageCurrent
	if stale {
		status, reason = metav1.ConditionTrue, v1alpha1.ReasonMiteImageStale
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionMiteImageStale,
		Status:  status,
		Reason:  reason,
		Message: fmt.Sprintf("running mite image %q, desired %q", running, desired),
	})

	var gaugeVal int64
	if stale {
		gaugeVal = 1
	}
	miteImageStaleGauge.Record(ctx, gaugeVal, metric.WithAttributes(
		attribute.String("namespace", cr.Namespace),
		attribute.String("controller", cr.Name),
	))
}

// defaultMiteImagePullPolicy is the pull policy baked onto the mite sidecar
// (and the init containers that share the mite image) when
// spec.miteSpec.imagePullPolicy is unset.
const defaultMiteImagePullPolicy = "IfNotPresent"

const alwaysMiteImagePullPolicy = "Always"

// defaultPullPolicyForImage mirrors k8s SetDefaults_Container: an image tagged
// ":latest" or fully untagged (no tag, no digest) defaults to Always; a
// non-latest tag or a digest-pinned reference defaults to IfNotPresent.
func defaultPullPolicyForImage(image string) string {
	if image == "" {
		return defaultMiteImagePullPolicy
	}
	ref := image
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		if ref[lastColon+1:] == "latest" {
			return alwaysMiteImagePullPolicy
		}
		return defaultMiteImagePullPolicy
	}
	if strings.Contains(image, "@") {
		return defaultMiteImagePullPolicy
	}
	return alwaysMiteImagePullPolicy
}

// effectiveDesiredMiteImagePullPolicy returns the image pull policy the
// operator bakes onto the mite container. The default is derived from the
// mite image via k8s-mirroring defaultPullPolicyForImage (:latest/untagged →
// Always; otherwise IfNotPresent). Same precedence and overlay-desync
// hazard as effectiveDesiredMiteImage (a raw resourceOverlay.statefulSet
// patch can set imagePullPolicy on the mite container directly, and
// CreateStatefulSet applies it before stamping — so ignoring it here would
// desync from the live template the same way ignoring the image override
// did): an explicit resourceOverlay.statefulSet imagePullPolicy for the mite
// container wins; otherwise the Controller's spec.miteSpec.imagePullPolicy
// (when set — takes precedence over the class tier below); otherwise the
// resolved ControllerClass's spec.mite.imagePullPolicy (when class is
// configured and resolved); otherwise the k8s-mirroring default.
func (r *Reconciler) effectiveDesiredMiteImagePullPolicy(cr *v1alpha1.Controller) string {
	desired := defaultPullPolicyForImage(r.effectiveDesiredMiteImage(cr))
	// Class tier: class.Spec.Mite.ImagePullPolicy wins over operator default.
	if class, ok := r.resolveClassForCr(context.TODO(), cr); ok && class != nil &&
		class.Spec.Mite != nil && class.Spec.Mite.ImagePullPolicy != "" {
		desired = class.Spec.Mite.ImagePullPolicy
	}
	// Controller-level tier wins over class/default, loses to resourceOverlay.
	if cr.Spec.MiteSpec != nil && cr.Spec.MiteSpec.ImagePullPolicy != "" {
		desired = string(cr.Spec.MiteSpec.ImagePullPolicy)
	}
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if pp, ok, err := overlay.PullPolicyOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), overlay.MiteContainerName); err != nil {
			r.Logger.Debug("overlay image-pull-policy parse failed; using class/default mite pull policy", "error", err)
		} else if ok {
			desired = pp
		}
	}
	return desired
}

// resourceListsEqual generalizes the old resourceListsEqual(a, b string) scalar
// comparison to a map: two corev1.ResourceList values are equal iff every resource name
// present in either map compares equal by quantity value (not string spelling) in both.
// A resource name present in one list and absent in the other is a delta — matches the
// old function's "empty string never equals a set quantity" rule, generalized to
// "absent key never equals a present key" (same rule, map-shaped).
func resourceListsEqual(a, b corev1.ResourceList) bool {
	keys := map[corev1.ResourceName]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	for k := range keys {
		qa, okA := a[k]
		qb, okB := b[k]
		if okA != okB {
			return false
		}
		if okA && qa.Cmp(qb) != 0 {
			return false
		}
	}
	return true
}

// mergeResourceRequirementsOverride merges an overlay override into a base
// ResourceRequirements per-resource-key: each resource name present in
// override.Requests overwrites only that key in base.Requests (and likewise
// for Limits), and each resource name in nullRequests/nullLimits is deleted
// from the merged result — matching the strategic-merge DELETE semantics of
// an explicit YAML null value in the resource overlay patch (both per-key
// nulls like requests.cpu: null and map-level nulls like requests: null, which
// ResourcesOverride signals via dropAllRequests/dropAllLimits). When
// dropAllRequests is true, no base request key is copied and override.Requests
// is ignored (every request key is deleted, including keys like
// ephemeral-storage that are not in the per-key cpu/memory set). Same for
// dropAllLimits. An override of just requests.cpu must not clear
// requests.memory or limits.* inherited from spec. Returns a freshly allocated
// ResourceRequirements; nil base is treated as an empty struct.
func mergeResourceRequirementsOverride(base, override *corev1.ResourceRequirements, nullRequests, nullLimits []corev1.ResourceName, dropAllRequests, dropAllLimits bool) *corev1.ResourceRequirements {
	result := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if base != nil && !dropAllRequests {
		for k, v := range base.Requests {
			result.Requests[k] = v.DeepCopy()
		}
	}
	if base != nil && !dropAllLimits {
		for k, v := range base.Limits {
			result.Limits[k] = v.DeepCopy()
		}
	}
	if override != nil && !dropAllRequests {
		for k, v := range override.Requests {
			result.Requests[k] = v.DeepCopy()
		}
	}
	if override != nil && !dropAllLimits {
		for k, v := range override.Limits {
			result.Limits[k] = v.DeepCopy()
		}
	}
	for _, k := range nullRequests {
		delete(result.Requests, k)
	}
	for _, k := range nullLimits {
		delete(result.Limits, k)
	}
	// If both sub-maps ended up empty, return nil so the caller sees
	// "no resources" rather than an empty struct — matches the convention
	// that nil means "unset" everywhere in this file.
	if len(result.Requests) == 0 && len(result.Limits) == 0 {
		return nil
	}
	return result
}

// effectiveDesiredResources returns the desired corev1.ResourceRequirements
// for the named container on a Controller. For the mite container the source
// is cr.Spec.MiteSpec.Resources; for Jenkins it is cr.Spec.Resources.
// A resourceOverlay.statefulSet override, when present and parseable,
// partially overrides per-resource-key (mergeResourceRequirementsOverride) —
// an overlay declaring only requests.cpu leaves requests.memory and all
// limits from spec intact. Returns nil when neither spec nor overlay
// declares resources for the container (matching the old zero-value
// convention: an unset resources field never reads as drift against history,
// since the mite container's pre-existing zero-value behavior had no
// resource requests).
func (r *Reconciler) effectiveDesiredResources(cr *v1alpha1.Controller, containerName string) *corev1.ResourceRequirements {
	var desired *corev1.ResourceRequirements
	switch containerName {
	case overlay.MiteContainerName:
		if cr.Spec.MiteSpec != nil {
			desired = cr.Spec.MiteSpec.Resources
		}
	case overlay.JenkinsContainerName:
		desired = cr.Spec.Resources
	}
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if ov, nullReqs, nullLims, dropAllReqs, dropAllLims, ok, err := overlay.ResourcesOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), containerName); err != nil {
			r.Logger.Debug("overlay resources parse failed; using spec/default resources", "container", containerName, "error", err)
		} else if ok {
			desired = mergeResourceRequirementsOverride(desired, ov, nullReqs, nullLims, dropAllReqs, dropAllLims)
		}
	}
	return desired
}

// normalizeResourceList returns rr's Requests or Limits list, or an empty
// (non-nil) ResourceList if rr is nil — the single choke point every
// comparison in reconcileContainerSpecRoll must go through so that "no
// resources block" on either the live or desired side normalizes to the
// same empty value instead of panicking or being mistaken for a real empty
// map on only one side.
func normalizeResourceList(rr *corev1.ResourceRequirements, which string) corev1.ResourceList {
	if rr == nil {
		return corev1.ResourceList{}
	}
	switch which {
	case "requests":
		return rr.Requests
	case "limits":
		return rr.Limits
	}
	return corev1.ResourceList{}
}

// reconcileContainerSpecRoll compares the mite and Jenkins containers'
// independently editable fields — mite image (effectiveDesiredMiteImage,
// now reflecting the full four-tier precedence chain: resourceOverlay,
// spec.miteSpec, class, operator default — the new spec.miteSpec tier
// participates in the existing image/pull-policy diff with no
// special-casing), mite resources (effectiveDesiredResources(cr, "mite")),
// mite imagePullPolicy (effectiveDesiredMiteImagePullPolicy), and Jenkins
// resources (effectiveDesiredResources(cr, "jenkins")) — against what's live
// on the StatefulSet and, on ANY delta, transitions a Running/Connected
// controller to Provisioning so the existing provisioning flow rolls the
// containers.
//
// Resource deltas use a three-way decision driven by the
// varroa.dev/resources-source stamp written at provisioning time:
//  1. desired != nil → compare live vs desired (EDITS converge).
//  2. desired == nil AND source == "spec" → DELTA (the block was spec-managed
//     and is now gone; one roll re-provisions and re-stamps as "class"/"none",
//     then stable — no roll loop).
//  3. desired == nil AND source in {"class", "none", missing/""} → skip
//     (provision-gated, D-6, or pre-epic STS with no stamp yet).
//
// Returns true when it transitioned the phase (caller must skip the normal
// phase dispatch this tick).
func (r *Reconciler) reconcileContainerSpecRoll(ctx context.Context, cr *v1alpha1.Controller) bool {
	// Do not initiate a roll while the class reference is dangling — the mite
	// image/pull-policy values we would roll toward are exactly the ones that
	// failed to resolve. The rest of this reconcile tick's work is unaffected.
	if _, ok := r.resolveClassForCr(ctx, cr); !ok {
		return false
	}

	desiredImage := r.effectiveDesiredMiteImage(cr)
	desiredMiteResources := r.effectiveDesiredResources(cr, overlay.MiteContainerName)
	desiredJenkinsResources := r.effectiveDesiredResources(cr, overlay.JenkinsContainerName)
	desiredPullPolicy := r.effectiveDesiredMiteImagePullPolicy(cr)

	computedMiteImage, liveMiteImage, liveMiteResources, liveJenkinsResources, livePullPolicy, resourcesSource, found, err := r.client.GetStatefulSetContainerSpecs(ctx, controllerPrefix(cr), cr.Namespace)
	if err != nil {
		r.Logger.Warn("container-spec-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		return false
	}
	if !found {
		return false // no StatefulSet yet: nothing to roll
	}
	appliedImage := computedMiteImage
	if appliedImage == "" {
		appliedImage = liveMiteImage // pre-stamp fallback
	}
	if appliedImage == "" {
		return false // defensive: unrecognizable StatefulSet
	}

	// A live template that predates spec.miteSpec.imagePullPolicy being
	// baked onto the mite container has no imagePullPolicy set at all,
	// reading back as "". Defaulting it to the same k8s-mirroring rule
	// buildStatefulSet uses (defaultPullPolicyForImage) avoids treating
	// "runtime behavior already matches the default" as drift and
	// triggering an unnecessary fleet-wide Provisioning roll immediately
	// after this change deploys.
	if livePullPolicy == "" {
		livePullPolicy = defaultPullPolicyForImage(appliedImage)
	}

	imageDelta := appliedImage != desiredImage
	// Resource deltas use a three-way decision driven by the
	// varroa.dev/resources-source stamp written at provisioning time.
	//
	// 1. desired != nil → compare live vs desired (EDITS always converge).
	// 2. desired == nil AND source == "spec" → the block was previously
	//    spec-managed and is now gone; DELTA = true (one roll to re-provision,
	//    recompute fallback, re-stamp as "class"/"none", then stable).
	// 3. desired == nil AND source in {"class", "none", missing/""} →
	//    skip (provision-gated, D-6, or pre-epic STS with no stamp yet).
	resourceDelta := func(desiredRR, liveRR *corev1.ResourceRequirements, container string) bool {
		if desiredRR != nil {
			return !resourceListsEqual(
				normalizeResourceList(liveRR, "requests"),
				normalizeResourceList(desiredRR, "requests"),
			) || !resourceListsEqual(
				normalizeResourceList(liveRR, "limits"),
				normalizeResourceList(desiredRR, "limits"),
			)
		}
		// desired is nil: only a previously spec-managed block triggers a roll.
		src := resourcesSource[container]
		return src == "spec"
	}
	miteResourcesDelta := resourceDelta(desiredMiteResources, liveMiteResources, overlay.MiteContainerName)
	pullPolicyDelta := livePullPolicy != desiredPullPolicy
	jenkinsResourcesDelta := resourceDelta(desiredJenkinsResources, liveJenkinsResources, overlay.JenkinsContainerName)

	// Does the resourceOverlay declare the mite image? (for the converged message)
	overlayGoverns := false
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if _, ovOK, ovErr := overlay.ImageOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), overlay.MiteContainerName); ovErr == nil && ovOK {
			overlayGoverns = true
		}
	}

	if !imageDelta && !miteResourcesDelta && !pullPolicyDelta && !jenkinsResourcesDelta {
		msg := fmt.Sprintf("container specs converged: image=%s pullPolicy=%s", desiredImage, desiredPullPolicy)
		if overlayGoverns {
			msg += "; image declared by resourceOverlay"
		} else if liveMiteImage != desiredImage {
			msg += fmt.Sprintf("; live image %s preserved (out-of-band override)", liveMiteImage)
		}
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:    v1alpha1.ConditionMiteSpecRollPending,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.ReasonMiteSpecConverged,
			Message: msg,
		})
		return false
	}

	var deltas []string
	if imageDelta {
		deltas = append(deltas, fmt.Sprintf("image %s -> %s", appliedImage, desiredImage))
	}
	if miteResourcesDelta {
		deltas = append(deltas, "mite resources changed")
	}
	if jenkinsResourcesDelta {
		deltas = append(deltas, "jenkins resources changed")
	}
	if pullPolicyDelta {
		deltas = append(deltas, fmt.Sprintf("imagePullPolicy %s -> %s", livePullPolicy, desiredPullPolicy))
	}

	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionMiteSpecRollPending,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonMiteSpecRollStarted,
		Message: fmt.Sprintf("rolling container specs: %s", strings.Join(deltas, "; ")),
	})
	now := metav1Now()
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	cr.Status.ProvisioningStartedAt = &now
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ProvisioningStarted",
		Message:            "re-provisioning to apply container spec change",
	})
	return true
}

// effectiveDesiredJenkinsImage returns the image the operator will converge the
// jenkins container to: jenkinsImageForVersion(spec.version, matched profile),
// unless resourceOverlay.statefulSet declares a jenkins-container image
// (design section 1).
func (r *Reconciler) effectiveDesiredJenkinsImage(cr *v1alpha1.Controller) string {
	profile, _ := r.resolveProfileForCr(cr)
	desired := jenkinsImageForVersion(cr.Spec.Version, profile)
	if cr.Spec.ResourceOverlay != nil && cr.Spec.ResourceOverlay.StatefulSet != "" {
		if img, ok, err := overlay.ImageOverride([]byte(cr.Spec.ResourceOverlay.StatefulSet), "jenkins"); err != nil {
			r.Logger.Debug("overlay image parse failed; using version-computed image", "error", err)
		} else if ok {
			desired = img
		}
	}
	return desired
}

// varroaBaseURL returns scheme+host from an OIDC redirect URL (e.g. "https://varroa.example.com/callback" → "https://varroa.example.com").
func varroaBaseURL(redirectURL string) string {
	if redirectURL == "" {
		return ""
	}
	u, err := url.Parse(redirectURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func computeDesiredStateHash(cmd *mitev1.DesiredStateCommand) string {
	// Compute per-component hashes the same way the mite computes file hashes.
	jcascHash := sha256Hex([]byte(cmd.JcascYaml))
	rbacHash := sha256Hex([]byte(cmd.RbacYaml))
	itemsHash := sha256Hex([]byte(cmd.ItemsYaml))

	// Composite hash (without plugins, which converge via the init-container
	// pod roll and are no longer sent to the mite).
	h := sha256.New()
	h.Write([]byte(jcascHash))
	h.Write([]byte(rbacHash))
	h.Write([]byte(itemsHash))
	return hex.EncodeToString(h.Sum(nil))
}

// hasResultFailureForHash checks drained CommandResults for any failure
// with a matching AppliedHash. Only checks sections that were actually
// present in the desired command — skipped (empty) sections have their
// *Success fields left as false by the mite but are not failures.
func hasResultFailureForHash(results []*mitev1.CommandResult, desired *mitev1.DesiredStateCommand, hash string) bool {
	for _, res := range results {
		switch r := res.Result.(type) {
		case *mitev1.CommandResult_DesiredState:
			if r.DesiredState != nil && r.DesiredState.AppliedHash == hash {
				ds := r.DesiredState
				if desired.JcascYaml != "" && !ds.ConfigSuccess {
					return true
				}
				if desired.RbacYaml != "" && !ds.RbacSuccess {
					return true
				}
				if desired.ItemsYaml != "" && !ds.ItemsSuccess {
					return true
				}
			}
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// cascContentHash returns a deterministic hash of the CASC ConfigMap payload
// (the same key/value pairs written to the casc ConfigMap: realm.yaml,
// config.yaml, and, when RBAC is federated, rbac.yaml). encoding/json sorts
// map[string]string keys and escapes values unambiguously, so two distinct
// payloads can never encode identically the way a delimiter-joined
// concatenation could (e.g. {"a":"x\x00b\x00y"} vs {"a":"x","b":"y"}).
func cascContentHash(data map[string]string) string {
	b, err := json.Marshal(data)
	if err != nil {
		// Marshal of a map[string]string cannot fail; guarded only so a
		// future change to data's type can't turn this into a panic.
		return ""
	}
	return sha256Hex(b)
}

// bundleIdentOf returns the resolved bundle identity for a sibling controller.
//
// It is effectiveBundleRef under another name, and must stay that way: siblings
// on the starter bundle share an identity with each other, so a fleet of
// zero-config Controllers coordinates through the same wave gate as any other
// fleet. Special-casing a nil ref here would silently exempt them.
func (r *Reconciler) bundleIdentOf(s *v1alpha1.Controller) bundleIdentity {
	return r.effectiveBundleRef(s)
}

// siblingHealthyOn reports whether s is healthy on targetBundleHash:
// same AppliedBundleHash, successful apply, and fresh mite heartbeat.
func siblingHealthyOn(s *v1alpha1.Controller, targetBundleHash string) bool {
	if s.Status.AppliedBundleHash != targetBundleHash {
		return false
	}
	if s.Status.LastApplyResult == nil || !s.Status.LastApplyResult.Succeeded {
		return false
	}
	ms := s.Status.MiteStatus
	if ms == nil || !ms.Connected || ms.LastSeen == nil {
		return false
	}
	return time.Since(ms.LastSeen.Time) <= staleMiteThreshold
}

// waveGateCleared returns cleared=true when every earlier-wave Connected sibling is
// healthy on targetBundleHash. waitingOn lists "namespace/name" of the earlier-wave
// Connected siblings that are NOT yet healthy.
func (r *Reconciler) waveGateCleared(ctx context.Context, cr *v1alpha1.Controller, ident bundleIdentity, myWave int, targetBundleHash string) (cleared bool, waitingOn []string, err error) {
	sibs, err := crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
	if err != nil {
		return false, nil, err
	}
	for _, s := range sibs {
		if s.Namespace == cr.Namespace && s.Name == cr.Name {
			continue
		}
		if r.bundleIdentOf(s) != ident {
			continue
		}
		sWave := 0
		if s.Spec.ReconciliationPolicy != nil {
			sWave = s.Spec.ReconciliationPolicy.RolloutWave
		}
		if sWave >= myWave {
			continue
		}
		if s.Status.Phase != v1alpha1.ControllerPhaseConnected {
			continue
		}
		if !siblingHealthyOn(s, targetBundleHash) {
			waitingOn = append(waitingOn, s.Namespace+"/"+s.Name)
		}
	}
	sort.Strings(waitingOn)
	return len(waitingOn) == 0, waitingOn, nil
}

// stripAuthorizationStrategy removes the jenkins.authorizationStrategy key
// from a JCasC YAML document. Varroa owns the authorization strategy and
// injects it separately via RbacYaml; any authorizationStrategy carried by
// the bundle must not reach Jenkins.
func stripAuthorizationStrategy(yamlDoc string) string {
	if yamlDoc == "" {
		return ""
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
		return yamlDoc
	}
	jenkins, ok := doc["jenkins"].(map[string]any)
	if !ok {
		return yamlDoc // nothing to strip
	}
	if _, has := jenkins["authorizationStrategy"]; !has {
		return yamlDoc // nothing to strip
	}
	delete(jenkins, "authorizationStrategy")
	if len(jenkins) == 0 {
		delete(doc, "jenkins")
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return yamlDoc
	}
	return string(out)
}

// injectProjectNamingStrategy ensures jenkins.projectNamingStrategy is set
// to "roleBased" in the JCasC YAML. This enforces item/folder role patterns
// at job-create time. If the key is already set by the bundle it is
// overwritten (Varroa owns authz).
func injectProjectNamingStrategy(yamlDoc string) string {
	if yamlDoc == "" {
		return ""
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
		return yamlDoc
	}
	jenkins, ok := doc["jenkins"].(map[string]any)
	if !ok {
		jenkins = make(map[string]any)
		doc["jenkins"] = jenkins
	}
	jenkins["projectNamingStrategy"] = map[string]any{
		"roleBased": map[string]any{
			"forceExistingJobs": false,
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return yamlDoc
	}
	return string(out)
}

// emitOperatorEvent emits an activity event with source "operator".
func (r *Reconciler) emitOperatorEvent(eventType string, cr *v1alpha1.Controller, message string, reason string) {
	if r.activityPublisher == nil {
		return
	}
	r.activityPublisher.Publish(activity.Event{
		Type:       eventType,
		Source:     "operator",
		Controller: cr.Name,
		Namespace:  cr.Namespace,
		Message:    message,
		Reason:     reason,
	})
}

// emitPhaseEvent emits a phase transition event plus any associated
// provisioning lifecycle events (started, completed, failed).
func (r *Reconciler) emitPhaseEvent(cr *v1alpha1.Controller, oldPhase v1alpha1.ControllerPhase) {
	newPhase := cr.Status.Phase
	if r.activityPublisher == nil {
		return
	}

	// Phase transition event (source: mite, same as existing phase events).
	r.activityPublisher.Publish(activity.Event{
		Type:       "phase",
		Source:     "mite",
		Controller: cr.Name,
		Namespace:  cr.Namespace,
		Message:    "phase changed from " + string(oldPhase) + " to " + string(newPhase),
		Phase:      string(newPhase),
	})

	// Emit provisioning lifecycle events.
	if oldPhase == v1alpha1.ControllerPhasePending && newPhase == v1alpha1.ControllerPhaseProvisioning {
		r.emitOperatorEvent("provisioning.started", cr, "provisioning started", "")
	}
	if oldPhase == v1alpha1.ControllerPhaseProvisioning && newPhase == v1alpha1.ControllerPhaseRunning {
		r.emitOperatorEvent("provisioning.completed", cr, "provisioning completed", "")
	}
	if newPhase == v1alpha1.ControllerPhaseFailed {
		reason := "provisioning failed"
		msg := "provisioning failed"
		if len(cr.Status.Conditions) > 0 {
			latest := cr.Status.Conditions[len(cr.Status.Conditions)-1]
			if latest.Message != "" {
				reason = latest.Message
				msg = latest.Message
			}
		}
		r.emitOperatorEvent("provisioning.failed", cr, msg, reason)
	}
}

// generateRealmDocument returns a JCasC YAML document that selects
// VarroaSecurityRealm using the varroaMiteAuth symbol.  Values use
// JCasC ${ENV} interpolation so the realm picks up the OIDC issuer and
// claim names from the environment variables set on the Jenkins container.
func generateRealmDocument() string {
	return `jenkins:
  securityRealm:
    varroaMiteAuth:
      oidcIssuer: "${VARROA_OIDC_ISSUER}"
      userClaimNames: "${VARROA_OIDC_USER_CLAIM}"
      groupClaimName: "${VARROA_OIDC_GROUP_CLAIM}"
`
}

// hibernationIgnoreRegex extracts the activity ignore regex from the
// hibernation spec, or returns "" if not configured.
func hibernationIgnoreRegex(spec *v1alpha1.HibernationSpec) string {
	if spec != nil {
		return spec.ActivityIgnoreRegex
	}
	return ""
}

// mintWakeToken generates a wake token if needed per D5:
// when (hibernation.enabled OR status.hibernated) AND currentToken == "",
// a 128-bit crypto/rand hex token is returned. Otherwise returns "".
func mintWakeToken(spec *v1alpha1.HibernationSpec, hibernated bool, currentToken string) (string, error) {
	needed := (spec != nil && spec.Enabled) || hibernated
	if !needed || currentToken != "" {
		return "", nil
	}
	token := make([]byte, 16) // 128 bits
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate wake token: %w", err)
	}
	return hex.EncodeToString(token), nil
}

// shouldHibernate evaluates whether a controller should be hibernated,
// per D5. Returns (true, lastActivityAt) when all gates pass, or
// (false, lastActivityAt) when any gate fails.
//
// Gates (all must pass):
//  1. Hibernation spec is enabled
//  2. Grace period has elapsed since last activity
//  3. No running builds
//  4. No queued items
//  5. Gauges are not stale (received within 5 minutes)
//
// lastActivityAt is the max of three sources per D5:
// last_http_activity_unix, last_event_unix, and the Ready condition's
// lastTransitionTime for entering Connected.
func shouldHibernate(cr *v1alpha1.Controller, gauges *mitev1.IdleGauges, receivedAt, now time.Time) (bool, time.Time) {
	// Gate 1: Hibernation must be enabled.
	if cr.Spec.Hibernation == nil || !cr.Spec.Hibernation.Enabled {
		return false, time.Time{}
	}

	// Gate 5: Gauges must not be stale (within 5 minutes).
	if receivedAt.IsZero() || now.Sub(receivedAt) > 5*time.Minute {
		return false, time.Time{}
	}

	// Compute lastActivityAt per D5: max of three sources.
	var lastActivity time.Time
	if gauges.LastHttpActivityUnix > 0 {
		t := time.Unix(gauges.LastHttpActivityUnix, 0)
		if t.After(lastActivity) {
			lastActivity = t
		}
	}
	if gauges.LastEventUnix > 0 {
		t := time.Unix(gauges.LastEventUnix, 0)
		if t.After(lastActivity) {
			lastActivity = t
		}
	}
	// Connect-time floor: never park a controller before the grace period has
	// elapsed since it *last* entered Connected, even with zero activity gauges
	// (avoids instant-parking a freshly-provisioned or just-woken controller
	// before anyone has touched it). The Ready/JenkinsHealthy condition's
	// LastTransitionTime tracks the most recent Connected transition (setCondition
	// stamps it on transition and it is persisted each reconcile), so a woken
	// controller measures grace from the wake, not its first-ever connect.
	// FirstConnectedAt is a stable fallback if the condition is somehow absent.
	floor := time.Time{}
	for _, c := range cr.Status.Conditions {
		if c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			floor = c.LastTransitionTime.Time
			break
		}
	}
	if floor.IsZero() && cr.Status.FirstConnectedAt != nil {
		floor = cr.Status.FirstConnectedAt.Time
	}
	if floor.After(lastActivity) {
		lastActivity = floor
	}

	// Gate 2: Grace period must have elapsed.
	gracePeriod := time.Duration(cr.Spec.Hibernation.GracePeriodMinutes) * time.Minute
	if now.Sub(lastActivity) < gracePeriod {
		return false, lastActivity
	}

	// Gate 3: No running builds.
	if gauges.RunningBuilds > 0 {
		return false, lastActivity
	}

	// Gate 4: No queued items.
	if gauges.QueueLength > 0 {
		return false, lastActivity
	}

	return true, lastActivity
}

// cascConfigMapName returns the name of the per-controller CASC ConfigMap.
func cascConfigMapName(prefix string) string {
	return prefix + "-casc"
}

// ---------------------------------------------------------------------------
// Plugin inventory classification
// ---------------------------------------------------------------------------

// classifyPluginInventory reads the raw plugin inventory from the transport,
// classifies it against the declared set and bootstrap root, projects the
// bounded summary onto cr.Status.PluginInventory, sets the
// PluginInventoryDrift condition, and persists the full result to the read
// model (invc/ KV key).
func (r *Reconciler) classifyPluginInventory(ctx context.Context, cr *v1alpha1.Controller, coreSet []pluginlock.PluginEntry, desiredLines []string) {
	ns, name := cr.Namespace, cr.Name
	key := ns + "/" + name

	// Read raw inventory from transport.
	rawInv := r.miteTransport.PluginInventory(ns, name)

	// Freshness check.
	hbHash, _ := r.miteTransport.InstalledPluginsHash(ns, name)
	fresh := plugininv.HashRecognised(hbHash) && rawInv != nil && !rawInv.CollectionFailed &&
		rawInv.InstalledPluginsHash == hbHash

	// If fresh, classify. Otherwise retain last-known rows.
	var class plugininv.Classification
	var inv *plugininv.Inventory
	var stale bool
	if fresh {
		// Build the plugininv.Inventory from the proto form.
		inv = protoToInventory(rawInv)

		// Build declared plugins from managedPluginLines.
		declared := buildDeclaredPlugins(desiredLines, cr, coreSet)

		// Get bootstrap root.
		boot, bootMatched := pluginlock.Bootstrap(cr.Spec.Version)
		bootRoot := ""
		if len(boot) > 0 {
			bootRoot = boot[0].ArtifactID
		}
		bootMembers := make([]string, 0, len(boot)-1)
		for i := 1; i < len(boot); i++ {
			bootMembers = append(bootMembers, boot[i].ArtifactID)
		}

		class = plugininv.Classify(plugininv.Inputs{
			Inventory:        *inv,
			Declared:         declared,
			BootstrapRoot:    bootRoot,
			BootstrapMembers: bootMembers,
			BootstrapMatched: bootMatched,
		})
		stale = false
	} else {
		stale = true
		// Try to retain last-known classification rows.
		if prior, ok := r.miteTransport.PluginClassification(ns, name); ok && prior != nil {
			class.Plugins = pluginClassifiedToInternal(prior.Plugins)
			class.Total = prior.Envelope.Total
			class.Counts = prior.Envelope.Counts
			class.BootstrapApproximate = prior.Envelope.BootstrapApproximate
			for _, a := range prior.Advisories {
				class.Advisories = append(class.Advisories, plugininv.Advisory{
					Code: a.Code, Plugin: a.Plugin, Dependency: a.Dependency,
					Min: a.Min, Version: a.Version,
				})
			}
		}
	}

	// Determine degraded from source.
	degraded := rawInv != nil && rawInv.Source == "filesystem"

	// Project bounded summary to status.
	now := metav1Now()
	statusInv := &v1alpha1.PluginInventoryStatus{
		Hash:                 hbHash,
		ObservedAt:           &now,
		Source:               "",
		Stale:                stale,
		Degraded:             degraded,
		BootstrapApproximate: class.BootstrapApproximate,
		OptionalEdgesDropped: rawInv != nil && rawInv.OptionalEdgesDropped,
		Truncated:            rawInv != nil && rawInv.Truncated,
		Total:                class.Total,
		Counts:               class.Counts,
	}
	if rawInv != nil {
		statusInv.Source = rawInv.Source
		if rawInv.CollectedAt != "" {
			if t, err := time.Parse(time.RFC3339, rawInv.CollectedAt); err == nil {
				statusInv.CollectedAt = &metav1.Time{Time: t}
			}
		}
	}

	// Build capped drift and version-drift lists.
	// Class 6 before class 5, then by name.
	const maxDrift = 50
	var driftOrder []string
	for _, p := range class.Plugins {
		if p.Class == plugininv.ClassUnmanaged || p.Class == plugininv.ClassOptionalDependency {
			driftOrder = append(driftOrder, p.Name)
		}
	}
	// Sort: class 6 before class 5, then name.
	classOrder := map[string]int{plugininv.ClassUnmanaged: 0, plugininv.ClassOptionalDependency: 1}
	sort.Slice(driftOrder, func(i, j int) bool {
		pi := findPlugin(class.Plugins, driftOrder[i])
		pj := findPlugin(class.Plugins, driftOrder[j])
		if pi == nil || pj == nil {
			return driftOrder[i] < driftOrder[j]
		}
		ci, cj := classOrder[pi.Class], classOrder[pj.Class]
		if ci != cj {
			return ci < cj
		}
		return pi.Name < pj.Name
	})
	for i, pName := range driftOrder {
		if i >= maxDrift {
			statusInv.DriftTruncated = true
			break
		}
		p := findPlugin(class.Plugins, pName)
		if p == nil {
			continue
		}
		statusInv.Drift = append(statusInv.Drift, v1alpha1.PluginInventoryDriftEntry{
			Name:    p.Name,
			Version: p.Version,
			Class:   p.Class,
		})
	}

	// Version drift for class-2 only.
	for _, p := range class.Plugins {
		if p.Class != plugininv.ClassDeclared || p.VersionVerdict == plugininv.VerdictMatch || p.VersionVerdict == "" {
			continue
		}
		if len(statusInv.VersionDrift) >= maxDrift {
			statusInv.DriftTruncated = true
			break
		}
		statusInv.VersionDrift = append(statusInv.VersionDrift, v1alpha1.PluginInventoryDriftEntry{
			Name:    p.Name,
			Version: p.Version,
			Class:   p.Class,
			Verdict: p.VersionVerdict,
		})
	}

	// Carry forward PendingCollect only while the inventory is still stale. A
	// non-stale inventory means the collect landed, so retaining the marker
	// would publish an in-flight command that has already completed.
	//
	// The 60s deadline stays the backstop for a collect that failed or never
	// reported, and it is deliberately NOT cleared on failure: it doubles as
	// the retry backoff. Clearing it the moment a failure came back would
	// reissue COLLECT_PLUGIN_INVENTORY on every reconcile tick against a
	// controller that is already failing to collect.
	if stale && cr.Status.PluginInventory != nil && cr.Status.PluginInventory.PendingCollect != nil {
		pc := cr.Status.PluginInventory.PendingCollect
		if time.Since(pc.IssuedAt.Time) < 60*time.Second {
			statusInv.PendingCollect = pc
		}
	}

	cr.Status.PluginInventory = statusInv

	// Set PluginInventoryDrift condition.
	// Order is load-bearing: any state where the dependency closure cannot be
	// confirmed (degraded, truncated, all-edges-dropped, optional-edges-dropped)
	// must suppress the drift condition BEFORE the unmanaged-count test, or a
	// fresh filesystem inventory with an unmanaged plugin would report as
	// actionable drift when the closure cannot be verified.
	unmanagedCount := class.Counts[plugininv.ClassUnmanaged]
	indeterminate := degraded ||
		(rawInv != nil && rawInv.Truncated) ||
		(rawInv != nil && rawInv.AllEdgesDropped) ||
		(rawInv != nil && rawInv.OptionalEdgesDropped)

	condStatus, condReason, condMsg := pluginDriftCondition(
		stale, indeterminate, unmanagedCount)
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPluginInventoryDrift,
		Status:  condStatus,
		Reason:  condReason,
		Message: condMsg,
	})

	// Self-healing: issue COLLECT_PLUGIN_INVENTORY when stale but heartbeat
	// carries a recognised hash and no command is already in flight.
	if stale && plugininv.HashRecognised(hbHash) && statusInv.PendingCollect == nil {
		id := fmt.Sprintf("collect-plugin-inventory-%s-%d", cr.Name, time.Now().UnixNano())
		err := r.miteTransport.SendImperative(ctx, ns, name, &mitev1.ImperativeCommand{
			CommandId:   id,
			Type:        mitev1.CommandTypeCollectPluginInventory,
			DeadlineSec: 60,
		})
		if err == nil {
			statusInv.PendingCollect = &v1alpha1.PendingCollect{
				CommandID: id,
				IssuedAt:  *statusInv.ObservedAt,
			}
			cr.Status.PluginInventory = statusInv
		} else {
			r.Logger.Warn("failed to issue COLLECT_PLUGIN_INVENTORY",
				"controller", key, "error", err)
		}
	}

	// Persist full classification to read model (invc/), only when envelope changed.
	if inv != nil && fresh {
		r.persistClassification(ns, name, inv, &class, statusInv)
	} else if stale {
		r.persistStaleClassification(ns, name, statusInv)
	}
}

// persistClassification writes the full classification to the invc/ KV key.
func (r *Reconciler) persistClassification(ns, name string, _ *plugininv.Inventory, class *plugininv.Classification, statusInv *v1alpha1.PluginInventoryStatus) {
	env := transport.ClassifiedEnvelope{
		Hash:                 statusInv.Hash,
		Source:               statusInv.Source,
		Stale:                statusInv.Stale,
		Degraded:             statusInv.Degraded,
		BootstrapApproximate: statusInv.BootstrapApproximate,
		OptionalEdgesDropped: statusInv.OptionalEdgesDropped,
		Truncated:            statusInv.Truncated,
		Total:                statusInv.Total,
		Counts:               statusInv.Counts,
		DriftTruncated:       statusInv.DriftTruncated,
	}
	if statusInv.CollectedAt != nil {
		env.CollectedAt = statusInv.CollectedAt.Time
	}
	if statusInv.ObservedAt != nil {
		env.ObservedAt = statusInv.ObservedAt.Time
	}

	plugins := make([]transport.ClassifiedPlugin, 0, len(class.Plugins))
	for _, p := range class.Plugins {
		plugins = append(plugins, transport.ClassifiedPlugin{
			Name:           p.Name,
			Version:        p.Version,
			Class:          p.Class,
			DeclaredBy:     p.DeclaredBy,
			Contributors:   p.Contributors,
			ImpliedBy:      p.ImpliedBy,
			VersionVerdict: p.VersionVerdict,
			Enabled:        triToLabel(p.Enabled),
			Detached:       triToLabel(p.Detached),
			Bundled:        triToLabel(p.Bundled),
		})
	}
	advisories := make([]transport.Advisory, 0, len(class.Advisories))
	for _, a := range class.Advisories {
		advisories = append(advisories, transport.Advisory{
			Code: a.Code, Plugin: a.Plugin, Dependency: a.Dependency,
			Min: a.Min, Version: a.Version,
		})
	}

	// Compare against last stored; skip write if nothing changed. The envelope
	// alone is NOT sufficient: editing the controller's declared plugin set or
	// its composed bundle reclassifies provenance, contributors, version
	// verdicts and advisories while the installed inventory — and therefore
	// every envelope field, including the hash and counts — stays identical.
	// Comparing only the envelope would leave invc/ serving stale provenance.
	if prior, ok := r.miteTransport.PluginClassification(ns, name); ok && prior != nil {
		if classificationUnchanged(env, plugins, advisories, prior) {
			return
		}
	}

	_ = r.miteTransport.PutPluginClassification(context.Background(), ns, name, &transport.ClassifiedInventory{
		Envelope:   env,
		Plugins:    plugins,
		Advisories: advisories,
	})
}

// persistStaleClassification updates only the envelope of the stored classification
// when the inventory is stale, retaining prior plugin rows verbatim.
func (r *Reconciler) persistStaleClassification(ns, name string, statusInv *v1alpha1.PluginInventoryStatus) {
	prior, ok := r.miteTransport.PluginClassification(ns, name)
	if !ok || prior == nil {
		return // No prior record, nothing to retain.
	}

	env := transport.ClassifiedEnvelope{
		Hash:                 statusInv.Hash,
		Source:               statusInv.Source,
		Stale:                statusInv.Stale,
		Degraded:             statusInv.Degraded,
		BootstrapApproximate: statusInv.BootstrapApproximate,
		OptionalEdgesDropped: statusInv.OptionalEdgesDropped,
		Truncated:            statusInv.Truncated,
		Total:                statusInv.Total,
		Counts:               statusInv.Counts,
		DriftTruncated:       statusInv.DriftTruncated,
	}
	if statusInv.CollectedAt != nil {
		env.CollectedAt = statusInv.CollectedAt.Time
	}
	if statusInv.ObservedAt != nil {
		env.ObservedAt = statusInv.ObservedAt.Time
	}

	if envelopesEqual(env, prior.Envelope) {
		return
	}

	_ = r.miteTransport.PutPluginClassification(context.Background(), ns, name, &transport.ClassifiedInventory{
		Envelope:   env,
		Plugins:    prior.Plugins,
		Advisories: prior.Advisories,
	})
}

// envelopesEqual compares two ClassifiedEnvelope values for equality.
// classificationUnchanged reports whether a freshly built classification is
// identical to the stored one, across the envelope AND the classified rows and
// advisories it summarizes.
func classificationUnchanged(env transport.ClassifiedEnvelope, plugins []transport.ClassifiedPlugin, advisories []transport.Advisory, prior *transport.ClassifiedInventory) bool {
	if !envelopesEqual(env, prior.Envelope) {
		return false
	}
	if len(plugins) != len(prior.Plugins) || len(advisories) != len(prior.Advisories) {
		return false
	}
	for i := range plugins {
		if !classifiedPluginsEqual(plugins[i], prior.Plugins[i]) {
			return false
		}
	}
	for i := range advisories {
		if advisories[i] != prior.Advisories[i] {
			return false
		}
	}
	return true
}

// classifiedPluginsEqual compares two classified rows field by field. Both
// slice fields are order-significant: their producers emit them sorted, so a
// difference in order is a real difference in output.
func classifiedPluginsEqual(a, b transport.ClassifiedPlugin) bool {
	if a.Name != b.Name || a.Version != b.Version || a.Class != b.Class ||
		a.DeclaredBy != b.DeclaredBy || a.VersionVerdict != b.VersionVerdict ||
		a.Enabled != b.Enabled || a.Detached != b.Detached || a.Bundled != b.Bundled {
		return false
	}
	return stringsEqual(a.Contributors, b.Contributors) && stringsEqual(a.ImpliedBy, b.ImpliedBy)
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ObservedAt is deliberately NOT compared: it is restamped to time.Now() on
// every Connected reconcile, so including it made this function always report
// "changed" and the change-triggered write contract degenerate into a write on
// every tick. CollectedAt still participates — that one is the mite's own
// collection timestamp and only moves on a real recollection.
func envelopesEqual(a, b transport.ClassifiedEnvelope) bool {
	if a.Hash != b.Hash || a.Source != b.Source || a.Stale != b.Stale ||
		a.Degraded != b.Degraded || a.BootstrapApproximate != b.BootstrapApproximate ||
		a.OptionalEdgesDropped != b.OptionalEdgesDropped || a.Truncated != b.Truncated ||
		a.Total != b.Total || a.DriftTruncated != b.DriftTruncated ||
		!a.CollectedAt.Equal(b.CollectedAt) {
		return false
	}
	if len(a.Counts) != len(b.Counts) {
		return false
	}
	for k, v := range a.Counts {
		if b.Counts[k] != v {
			return false
		}
	}
	return true
}

// protoToInventory converts a mitev1.PluginInventory to plugininv.Inventory.
func protoToInventory(pi *mitev1.PluginInventory) *plugininv.Inventory {
	inv := &plugininv.Inventory{
		Source:           pi.Source,
		CollectionFailed: pi.CollectionFailed,
		CollectionError:  pi.CollectionError,
	}
	if pi.CollectedAt != "" {
		if t, err := time.Parse(time.RFC3339, pi.CollectedAt); err == nil {
			inv.CollectedAt = t
		}
	}
	for _, ip := range pi.Plugins {
		rec := plugininv.Record{
			Name:     ip.Name,
			Version:  ip.Version,
			Enabled:  stringToTri(ip.Enabled),
			Detached: stringToTri(ip.Detached),
			Bundled:  stringToTri(ip.Bundled),
		}
		for _, d := range ip.Deps {
			rec.Deps = append(rec.Deps, plugininv.Dep{
				Name: d.Name, Min: d.Min, Optional: d.Optional,
			})
		}
		inv.Records = append(inv.Records, rec)
	}
	return inv
}

// buildDeclaredPlugins converts managedPluginLines output to DeclaredPlugin slice.
func buildDeclaredPlugins(lines []string, cr *v1alpha1.Controller, coreSet []pluginlock.PluginEntry) []plugininv.DeclaredPlugin {
	coreIDs := make(map[string]bool, len(coreSet))
	for _, e := range coreSet {
		coreIDs[e.ArtifactID] = true
	}

	hasPluginSpec := cr.Spec.PluginSpec != nil && len(cr.Spec.PluginSpec.Entries) > 0

	out := make([]plugininv.DeclaredPlugin, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		name := parts[0]
		version := ""
		if len(parts) > 1 {
			version = parts[1]
		}

		tier := plugininv.DeclaredByBundle
		if coreIDs[name] {
			tier = plugininv.DeclaredByCore
		} else if hasPluginSpec {
			tier = plugininv.DeclaredByControllerSpec
		}

		out = append(out, plugininv.DeclaredPlugin{
			Name:    name,
			Version: version,
			Tier:    tier,
		})
	}
	return out
}

// findPlugin finds a ClassifiedPlugin by name.
func findPlugin(plugins []plugininv.ClassifiedPlugin, name string) *plugininv.ClassifiedPlugin {
	for i := range plugins {
		if plugins[i].Name == name {
			return &plugins[i]
		}
	}
	return nil
}

// pluginClassifiedToInternal converts transport.ClassifiedPlugin to plugininv.ClassifiedPlugin.
func pluginClassifiedToInternal(plugins []transport.ClassifiedPlugin) []plugininv.ClassifiedPlugin {
	out := make([]plugininv.ClassifiedPlugin, len(plugins))
	for i, p := range plugins {
		out[i] = plugininv.ClassifiedPlugin{
			Name:           p.Name,
			Version:        p.Version,
			Class:          p.Class,
			DeclaredBy:     p.DeclaredBy,
			Contributors:   p.Contributors,
			ImpliedBy:      p.ImpliedBy,
			VersionVerdict: p.VersionVerdict,
			Enabled:        labelToTri(p.Enabled),
			Detached:       labelToTri(p.Detached),
			Bundled:        labelToTri(p.Bundled),
		}
	}
	return out
}

// triToLabel converts a plugininv.Tri to a string label.
func triToLabel(t plugininv.Tri) string {
	switch t {
	case plugininv.TriTrue:
		return "true"
	case plugininv.TriFalse:
		return "false"
	default:
		return ""
	}
}

// stringToTri converts a proto string flag to plugininv.Tri.
func stringToTri(s string) plugininv.Tri {
	switch s {
	case "true":
		return plugininv.TriTrue
	case "false":
		return plugininv.TriFalse
	default:
		return plugininv.TriUnknown
	}
}

// labelToTri converts a transport label to plugininv.Tri.
func labelToTri(s string) plugininv.Tri {
	switch s {
	case "true":
		return plugininv.TriTrue
	case "false":
		return plugininv.TriFalse
	default:
		return plugininv.TriUnknown
	}
}

// pluginDriftCondition returns the status, reason, and message for the
// PluginInventoryDrift condition, given the stale/indeterminate flags
// and the unmanaged plugin count. Extracted as a pure function for table testing.
func pluginDriftCondition(stale, indeterminate bool, unmanagedCount int) (metav1.ConditionStatus, string, string) {
	switch {
	case stale:
		return metav1.ConditionFalse,
			v1alpha1.ReasonStale,
			"inventory is stale; mite disconnected, collection failed, or read model lost"
	case indeterminate:
		return metav1.ConditionFalse,
			v1alpha1.ReasonIndeterminate,
			"inventory is indeterminate (degraded source, truncated, or edges dropped); drift cannot be confirmed"
	case unmanagedCount > 0:
		return metav1.ConditionTrue,
			"UnmanagedPlugins",
			fmt.Sprintf("%d unmanaged plugin(s) installed", unmanagedCount)
	default:
		return metav1.ConditionFalse,
			v1alpha1.ReasonNoDrift,
			"no unmanaged plugins detected"
	}
}

// clearMiteConnection marks the mite as disconnected on a controller that is
// being powered off. Stopped/Hibernated scale the StatefulSet to 0, so the mite
// stream drops without ever reaching the Connected-phase status writer that
// owns these fields — leaving the last observed values pinned as live. Only the
// two liveness fields are reset; every other MiteStatus field (version, image,
// timestamps, cert expiry) is a historical observation and is preserved.
//
// JenkinsHealth is set to "unreachable" rather than cleared: it is an
// omitempty field, so an empty string is dropped from the status merge patch
// and the stale value would survive on the server.
func clearMiteConnection(cr *v1alpha1.Controller) {
	// MiteStreamDegraded describes a live connection being starved, so it stops
	// being meaningful the moment the mite is gone. Only handleConnected
	// reconciles it, and Stopped/Hibernated controllers never reach that path —
	// a stale True would otherwise persist for as long as they stay powered off.
	cr.Status.Conditions = removeConditionByType(cr.Status.Conditions,
		v1alpha1.ConditionMiteStreamDegraded)
	if cr.Status.MiteStatus == nil {
		return
	}
	cr.Status.MiteStatus.Connected = false
	cr.Status.MiteStatus.JenkinsHealth = "unreachable"
}
