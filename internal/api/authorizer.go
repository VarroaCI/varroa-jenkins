package api

import (
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// Authorizer wraps the RBAC resolver to provide Can* authorization methods
// for BFF API handlers.
type Authorizer struct {
	resolver    *rbac.Resolver
	defaultRead bool
}

// NewAuthorizer creates an Authorizer.
func NewAuthorizer(resolver *rbac.Resolver, defaultRead bool) *Authorizer {
	return &Authorizer{resolver: resolver, defaultRead: defaultRead}
}

// effective returns the capability set for the given claims and scope.
// If claims is nil, the caller is unauthenticated.
func (a *Authorizer) effective(claims *auth.Claims, namespace, controllerName string) rbac.APICapabilitySet {
	if claims == nil {
		if a.defaultRead {
			return rbac.APICapabilitySet{"controllers": {"read": true}}
		}
		return rbac.APICapabilitySet{}
	}
	return a.resolver.EffectiveAPICapabilities(claims, namespace, controllerName)
}

// hasVerb checks if a resource+verb combination is allowed in the capability set.
// Wildcards are supported: "*" resource or "*" verb grants all.
func hasVerb(caps rbac.APICapabilitySet, resource, verb string) bool {
	if caps["*"] != nil && (caps["*"]["*"] || caps["*"][verb]) {
		return true
	}
	if caps[resource] != nil && (caps[resource]["*"] || caps[resource][verb]) {
		return true
	}
	return false
}

// CanReadController checks if the caller can read a specific controller.
func (a *Authorizer) CanReadController(claims *auth.Claims, namespace, name string) bool {
	return hasVerb(a.effective(claims, namespace, name), "controllers", "read")
}

// CanCreateController checks if the caller can create a controller.
func (a *Authorizer) CanCreateController(claims *auth.Claims, namespace, name string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, name), "controllers", "create")
}

// IsAdmin reports whether the caller holds full administrative access, i.e. the
// wildcard "*" verb on the wildcard "*" resource. This is the capability of the
// built-in varroa:admin role and is the gate for the admin-only Settings hub
// sections (Users, Groups, Identity, Built-in roles). Note this is strictly
// narrower than CanCreateController, which the operator role also satisfies.
func (a *Authorizer) IsAdmin(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	caps := a.globalEffective(claims)
	return caps["*"] != nil && caps["*"]["*"]
}

// CanUpdateController checks if the caller can update a controller.
func (a *Authorizer) CanUpdateController(claims *auth.Claims, namespace, name string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, name), "controllers", "update")
}

// CanDeleteController checks if the caller can delete a controller.
func (a *Authorizer) CanDeleteController(claims *auth.Claims, namespace, name string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, name), "controllers", "delete")
}

// CanApproveRestart checks if the caller can approve a restart on a controller.
func (a *Authorizer) CanApproveRestart(claims *auth.Claims, namespace, name string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, name), "controllers", "approve-restart")
}

// CanApproveDeletion checks if the caller can approve an item deletion on a controller.
func (a *Authorizer) CanApproveDeletion(claims *auth.Claims, namespace, name string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, name), "controllers", "approve-deletion")
}

// CanManageController checks if the caller can manage a controller (power, restart, reprovision, ingress).
func (a *Authorizer) CanManageController(claims *auth.Claims, namespace, name string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, name), "controllers", "manage")
}

// CanReadRoles checks if the caller can read VarroaRoles.
func (a *Authorizer) CanReadRoles(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "roles", "read")
}

// CanCreateRole checks if the caller can create VarroaRoles.
func (a *Authorizer) CanCreateRole(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "roles", "create")
}

// CanUpdateRole checks if the caller can update VarroaRoles.
func (a *Authorizer) CanUpdateRole(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "roles", "update")
}

// CanDeleteRole checks if the caller can delete VarroaRoles.
func (a *Authorizer) CanDeleteRole(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "roles", "delete")
}

// CanReadRoleBindings checks if the caller can read VarroaRoleBindings.
func (a *Authorizer) CanReadRoleBindings(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "rolebindings", "read")
}

// CanCreateRoleBinding checks if the caller can create VarroaRoleBindings.
func (a *Authorizer) CanCreateRoleBinding(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "rolebindings", "create")
}

// CanUpdateRoleBinding checks if the caller can update VarroaRoleBindings.
func (a *Authorizer) CanUpdateRoleBinding(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "rolebindings", "update")
}

// CanDeleteRoleBinding checks if the caller can delete VarroaRoleBindings.
func (a *Authorizer) CanDeleteRoleBinding(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "rolebindings", "delete")
}

// CanReadJenkinsRoles checks if the caller can read JenkinsRoles.
func (a *Authorizer) CanReadJenkinsRoles(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsroles", "read")
}

// CanCreateJenkinsRole checks if the caller can create JenkinsRoles.
func (a *Authorizer) CanCreateJenkinsRole(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsroles", "create")
}

// CanUpdateJenkinsRole checks if the caller can update JenkinsRoles.
func (a *Authorizer) CanUpdateJenkinsRole(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsroles", "update")
}

// CanDeleteJenkinsRole checks if the caller can delete JenkinsRoles.
func (a *Authorizer) CanDeleteJenkinsRole(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsroles", "delete")
}

// CanReadJenkinsRoleBindings checks if the caller can read JenkinsRoleBindings.
func (a *Authorizer) CanReadJenkinsRoleBindings(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsrolebindings", "read")
}

// CanCreateJenkinsRoleBinding checks if the caller can create JenkinsRoleBindings.
func (a *Authorizer) CanCreateJenkinsRoleBinding(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsrolebindings", "create")
}

// CanUpdateJenkinsRoleBinding checks if the caller can update JenkinsRoleBindings.
func (a *Authorizer) CanUpdateJenkinsRoleBinding(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsrolebindings", "update")
}

// CanDeleteJenkinsRoleBinding checks if the caller can delete JenkinsRoleBindings.
func (a *Authorizer) CanDeleteJenkinsRoleBinding(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "jenkinsrolebindings", "delete")
}

// CanUpdateProvisioningDefaults checks if the caller can update ProvisioningDefaults.
func (a *Authorizer) CanUpdateProvisioningDefaults(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "provisioningdefaults", "update")
}

// CanUploadPlugin checks if the caller can push a plugin artifact into the
// update center. `updatecenter` is cluster-scoped (the UpdateCenter CR is a
// cluster singleton), so globalEffective — not clusterEffective or effective —
// is correct.
func (a *Authorizer) CanUploadPlugin(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "updatecenter", "upload")
}

// CanCreateVersionProfile checks if the caller can create JenkinsVersionProfiles.
func (a *Authorizer) CanCreateVersionProfile(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "version-profiles", "create")
}

// CanUpdateVersionProfile checks if the caller can update JenkinsVersionProfiles.
func (a *Authorizer) CanUpdateVersionProfile(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "version-profiles", "update")
}

// CanDeleteVersionProfile checks if the caller can delete JenkinsVersionProfiles.
func (a *Authorizer) CanDeleteVersionProfile(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.globalEffective(claims), "version-profiles", "delete")
}

// CanReadCatalogSources checks if the caller can read CatalogSources.
func (a *Authorizer) CanReadCatalogSources(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, "", ""), "catalogsources", "read")
}

// CanManageCatalogSources checks if the caller can manage CatalogSources with the given verb.
func (a *Authorizer) CanManageCatalogSources(claims *auth.Claims, verb string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, "", ""), "catalogsources", verb)
}

// CanManageCatalogSourcesInNamespace checks the CatalogSource write capability
// against a specific target namespace, so a namespace-scoped binding cannot act
// on (or publish into) another tenant's namespace when the namespace comes from
// the request. Mirrors CanWriteComposedBundlesInNamespace.
func (a *Authorizer) CanManageCatalogSourcesInNamespace(claims *auth.Claims, verb, namespace string) bool {
	if claims == nil {
		return false
	}
	// An empty namespace must deny, not fall through: EffectiveAPICapabilities
	// skips scope filtering entirely when both namespace and controllerName are
	// empty (the /me/permissions introspection path), which would let a
	// namespace-scoped binding pass this write gate. Callers that accept the
	// namespace from request input (MCP tools, query params) can reach here
	// with "".
	if namespace == "" {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, ""), "catalogsources", verb)
}

// CanReadCatalogItems checks if the caller can read CatalogItems.
func (a *Authorizer) CanReadCatalogItems(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, "", ""), "catalogitems", "read")
}

// CanReadComposedBundles checks if the caller can read ComposedBundles.
func (a *Authorizer) CanReadComposedBundles(claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, "", ""), "composedbundles", "read")
}

// CanWriteComposedBundles checks if the caller can write ComposedBundles with the given verb.
func (a *Authorizer) CanWriteComposedBundles(claims *auth.Claims, verb string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, "", ""), "composedbundles", verb)
}

// CanWriteComposedBundlesInNamespace checks the ComposedBundle write capability
// against a specific target namespace, so a namespace-scoped binding cannot act
// on (or resolve secrets/catalog items in) another tenant's namespace when the
// namespace comes from the request.
func (a *Authorizer) CanWriteComposedBundlesInNamespace(claims *auth.Claims, verb, namespace string) bool {
	if claims == nil {
		return false
	}
	return hasVerb(a.resolver.EffectiveAPICapabilities(claims, namespace, ""), "composedbundles", verb)
}

// clusterEffective returns the capability set from cluster-wide (nil-scope)
// bindings only. Mirrors the effective() pattern but uses
// EffectiveClusterAPICapabilities instead of EffectiveAPICapabilities.
func (a *Authorizer) clusterEffective(claims *auth.Claims) rbac.APICapabilitySet {
	if claims == nil {
		if a.defaultRead {
			return rbac.APICapabilitySet{"controllers": {"read": true}}
		}
		return rbac.APICapabilitySet{}
	}
	return a.resolver.EffectiveClusterAPICapabilities(claims)
}

// globalEffective returns the caller's capabilities from cluster-wide (nil-scope)
// bindings only, via the scope-partitioned resolver. It gates
// cluster-scoped Varroa resources (roles, rolebindings, jenkinsroles,
// jenkinsrolebindings, provisioningdefaults, and the admin wildcard) so a
// namespace-scoped binding cannot satisfy them cluster-wide.
func (a *Authorizer) globalEffective(claims *auth.Claims) rbac.APICapabilitySet {
	return a.resolver.EffectivePermissionScopes(claims).Global
}

// CanReadGlobalActivity reports whether the caller may read Varroa-global activity
// events (events with no controller scope). True only for callers holding a
// cluster-wide (nil-scope) binding that grants controllers:read; admins qualify
// via the wildcard. Honors defaultRead for nil claims.
func (a *Authorizer) CanReadGlobalActivity(claims *auth.Claims) bool {
	return hasVerb(a.clusterEffective(claims), "controllers", "read")
}

// CanReadActivityEvent reports whether the caller may see a single activity event.
// Controller-scoped events (Controller != "") gate on CanReadController for the
// event's namespace+controller; global events (Controller == "") gate on
// CanReadGlobalActivity.
func (a *Authorizer) CanReadActivityEvent(claims *auth.Claims, e activity.Event) bool {
	if e.Controller != "" {
		return a.CanReadController(claims, e.Namespace, e.Controller)
	}
	return a.CanReadGlobalActivity(claims)
}

// EffectivePermissions returns the caller's scope-partitioned capabilities for
// /me/permissions. nil claims honor defaultRead in the Global partition.
func (a *Authorizer) EffectivePermissions(claims *auth.Claims) rbac.PermissionScopes {
	return a.resolver.EffectivePermissionScopes(claims)
}

// DeployableNamespaces contains the set of namespaces the caller may deploy
// controllers into, along with a sensible default and degradation indicator.
type DeployableNamespaces struct {
	Namespaces       []string `json:"namespaces"`       // sorted, deduped, never null
	DefaultNamespace string   `json:"defaultNamespace"` // ∈ Namespaces when non-empty, else ""
	AllowFreeform    bool     `json:"allowFreeform"`
	Degraded         bool     `json:"degraded"` // true only when a remote target's inputs were unavailable
}

// DeployableNamespaces computes the caller's create-authorized namespace set.
//
//	managed:        runtime managedNamespaces (nil/empty ⇒ cluster-wide mode)
//	curated:        ProvisioningDefaults.Namespaces (curated hint list; used only in the
//	                unrestricted + cluster-wide cell)
//	curatedDefault: ProvisioningDefaults.DefaultNamespace (already resolved to "varroa" fallback)
func (a *Authorizer) DeployableNamespaces(claims *auth.Claims, managed, curated []string, curatedDefault string) DeployableNamespaces {
	explicit, unrestricted := a.resolver.CreateScopeNamespaces(claims)
	scoped := len(managed) > 0

	var out []string
	allowFreeform := false

	switch {
	case unrestricted && scoped:
		out = dedupeSort(managed)
	case unrestricted && !scoped:
		out = dedupeSort(curated)
		allowFreeform = true
	case !unrestricted && scoped:
		out = dedupeSort(intersect(explicit, managed))
	case !unrestricted && !scoped:
		out = dedupeSort(explicit)
	}

	if out == nil {
		out = []string{}
	}

	def := ""
	if len(out) > 0 {
		if curatedDefault != "" && sliceContains(out, curatedDefault) {
			def = curatedDefault
		} else {
			def = out[0]
		}
	}

	return DeployableNamespaces{
		Namespaces:       out,
		DefaultNamespace: def,
		AllowFreeform:    allowFreeform,
	}
}
