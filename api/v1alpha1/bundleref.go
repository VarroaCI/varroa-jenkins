package v1alpha1

// StarterBundleName is the ComposedBundle the operator seeds into its own
// namespace from content embedded in its binary. A Controller that names no
// composedBundleRef uses it.
//
// It lives here rather than in the controller package because every consumer
// that answers "which bundle is this controller using?" needs it: the
// reconciler, the BFF's config-diff and controller DTOs, and brood-operation
// target filtering. Each of those previously read spec.composedBundleRef
// directly and reported "none" for a nil ref, which is now wrong.
const StarterBundleName = "varroa-starter"

// EffectiveBundleRef resolves the ComposedBundle a Controller actually uses.
//
// A nil spec.composedBundleRef is not "no bundle" — it resolves by convention to
// the built-in starter bundle in the operator's namespace. A set ref resolves to
// its own name, defaulting the namespace to the Controller's.
//
// This is the single answer to that question. Reading spec.composedBundleRef
// directly anywhere is a bug: it reports zero-config controllers as unconfigured,
// which silently excludes them from bundle-filtered brood operations and makes
// the config-diff UI show an empty incoming configuration for a controller that
// is in fact running the starter.
func EffectiveBundleRef(cr *Controller, operatorNamespace string) (name, namespace string) {
	if cr == nil {
		return "", ""
	}
	if cr.Spec.ComposedBundleRef == nil {
		return StarterBundleName, operatorNamespace
	}
	ns := cr.Spec.ComposedBundleRef.Namespace
	if ns == "" {
		ns = cr.Namespace
	}
	return cr.Spec.ComposedBundleRef.Name, ns
}
