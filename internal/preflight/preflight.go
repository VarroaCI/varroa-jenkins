package preflight

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

// Check represents the result of a single preflight validation.
type Check struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // "pass" | "warn" | "fail"
	Message string `json:"message"`
}

// Deps is the narrow interface the preflight package needs.
type Deps interface {
	crdstore.Backend
	ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error)
	ListIngressHosts(ctx context.Context) (map[string][]string, error)
	GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error)
}

// Options holds call-site configuration for preflight checks.
type Options struct {
	OperatorNamespace string
	ManagedNamespaces string // raw MANAGED_NAMESPACES value; "" ⇒ cluster-wide mode
	ForUpdate         bool   // draft is an update of an existing controller
	PriorVersion      string // existing controller's spec.version (only meaningful when ForUpdate)
}

// Run executes all preflight checks against the given draft Controller.
func Run(ctx context.Context, deps Deps, draft *v1alpha1.Controller, inlineBundle *v1alpha1.ComposedBundleSpec, opts Options) []Check {
	defaults, _ := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, deps, "varroa-defaults", "")

	set := tenancy.NewManagedSet(opts.ManagedNamespaces, opts.OperatorNamespace)

	return []Check{
		checkName(ctx, deps, draft, opts),
		checkBundle(ctx, deps, draft, inlineBundle, opts),
		checkVersion(ctx, deps, draft, opts),
		checkPluginCoreCompat(ctx, deps, draft, opts),
		checkQuota(ctx, deps, draft, defaults),
		checkIngressHost(ctx, deps, draft, defaults),
		checkRBAC(ctx, deps, draft, opts),
		checkTargetNamespace(ctx, deps, draft, opts, set),
	}
}
