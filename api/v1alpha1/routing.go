package v1alpha1

const (
	// RoutingModeSubdomain is the default routing mode; each controller gets its own host.
	RoutingModeSubdomain = "subdomain"
	// RoutingModePath serves controllers at /jenkins/<ns>/<name> on the shared host.
	RoutingModePath = "path"
)

// RoutingMode returns the normalized routing mode: RoutingModePath if Mode is "path", else RoutingModeSubdomain.
func (s *IngressSpec) RoutingMode() string {
	if s != nil && s.Mode == RoutingModePath {
		return RoutingModePath
	}
	return RoutingModeSubdomain
}

// PathPrefix returns the URL path prefix for a controller: /jenkins/<namespace>/<name>.
func PathPrefix(namespace, name string) string {
	return "/jenkins/" + namespace + "/" + name
}

// ResolveHost returns the hostname used by a controller's ingress. An explicit
// host always wins; otherwise subdomain routing derives one from rootDomain.
func ResolveHost(cr *Controller, rootDomain string) string {
	if cr == nil {
		return ""
	}
	if cr.Spec.IngressSpec != nil && cr.Spec.IngressSpec.Host != "" {
		return cr.Spec.IngressSpec.Host
	}
	if cr.Spec.IngressSpec == nil || cr.Spec.IngressSpec.RoutingMode() == RoutingModeSubdomain {
		if rootDomain != "" {
			return cr.Name + "." + rootDomain
		}
	}
	return ""
}

// MergeStringMaps merges two string maps with key-level nil-safe semantics.
// On key conflict, override's value wins. Returns nil when both are nil.
func MergeStringMaps(base, override map[string]string) map[string]string {
	if len(override) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// MergeIngressAnnotations layers a controller's per-controller ingress annotations
// over the cluster-wide defaults. On key conflict, the controller-scoped value wins.
func MergeIngressAnnotations(defaults, override map[string]string) map[string]string {
	return MergeStringMaps(defaults, override)
}
