package rbac

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// BySubjectIndex is the index name for VarroaRoleBindings indexed by subject.
const BySubjectIndex = "by-subject"

// JenkinsBySubjectIndex is the index name for JenkinsRoleBindings indexed by subject.
const JenkinsBySubjectIndex = "jenkins-by-subject"

// SubjectIndexFunc indexes VarroaRoleBinding by each subject's kind:name key.
func SubjectIndexFunc(obj interface{}) ([]string, error) {
	rb, ok := obj.(*v1alpha1.VarroaRoleBinding)
	if !ok {
		return nil, nil
	}
	var keys []string
	for _, s := range rb.Spec.Subjects {
		keys = append(keys, subjectIndexKey(s.Kind, s.Name))
	}
	return keys, nil
}

// JenkinsSubjectIndexFunc indexes JenkinsRoleBinding by each subject's kind:name key.
func JenkinsSubjectIndexFunc(obj interface{}) ([]string, error) {
	rb, ok := obj.(*v1alpha1.JenkinsRoleBinding)
	if !ok {
		return nil, nil
	}
	var keys []string
	for _, s := range rb.Spec.Subjects {
		keys = append(keys, subjectIndexKey(s.Kind, s.Name))
	}
	return keys, nil
}

func subjectIndexKey(kind, name string) string {
	return kind + ":" + name
}

// APICapabilitySet maps resource -> verb -> allowed.
type APICapabilitySet map[string]map[string]bool

// ScopedCapabilities is the capability set granted by the caller's bindings that
// carry one particular scope, plus enough of the scope descriptor for a client to
// decide relevance. Namespaces is the scope's namespace allowlist (nil/empty ⇒ the
// scope is not namespace-bounded). HasControllerSelector is true when the scope
// carries a controller label selector (opaque to the client — it cannot evaluate
// controller labels).
type ScopedCapabilities struct {
	Namespaces            []string         `json:"namespaces"`
	HasControllerSelector bool             `json:"hasControllerSelector"`
	Capabilities          APICapabilitySet `json:"capabilities"`
}

// PermissionScopes is the scope-aware introspection result for /me/permissions.
// Global holds capabilities from cluster-wide (nil-scope) bindings ONLY; Scopes
// holds one entry per distinct scoped binding scope. Scoped grants are never
// flattened into Global.
type PermissionScopes struct {
	Global APICapabilitySet     `json:"global"`
	Scopes []ScopedCapabilities `json:"scopes"`
}

// Backend renders a list of RoleAssignments into a string (typically YAML).
type Backend interface {
	Render(assignments []RoleAssignment) (string, error)
}

// Resolver is the single authoritative interpreter of VarroaRole,
// VarroaRoleBinding, JenkinsRole, and JenkinsRoleBinding CRDs for
// both API authorization and Jenkins JCasC generation.
type Resolver struct {
	roleLister              cache.GenericLister
	roleBindingIndex        cache.Indexer
	jenkinsRoleLister       cache.GenericLister
	jenkinsRoleBindingIndex cache.Indexer
	controllerLister        cache.GenericLister
	defaultRead             bool
	userClaimNames          []string // e.g. ["sub", "preferred_username"]
	groupClaimNames         []string // e.g. ["groups"]
}

// NewResolver creates a new Resolver backed by the given informers.
// roleBindingInformer must be configured with BySubjectIndex.
// jenkinsRoleBindingInformer must be configured with JenkinsBySubjectIndex.
// userClaimNames and groupClaimNames map JWT claims to VarroaRoleBinding
// subject kinds (e.g. --oidc-user-claim=sub,preferred_username).
func NewResolver(
	roleInformer cache.SharedIndexInformer,
	roleBindingInformer cache.SharedIndexInformer,
	jenkinsRoleInformer cache.SharedIndexInformer,
	jenkinsRoleBindingInformer cache.SharedIndexInformer,
	controllerInformer cache.SharedIndexInformer,
	defaultRead bool,
	userClaimNames, groupClaimNames []string,
) *Resolver {
	return &Resolver{
		roleLister: cache.NewGenericLister(
			roleInformer.GetIndexer(),
			schema.GroupResource{Group: "varroa.dev", Resource: "varroaroles"},
		),
		roleBindingIndex: roleBindingInformer.GetIndexer(),
		jenkinsRoleLister: cache.NewGenericLister(
			jenkinsRoleInformer.GetIndexer(),
			schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"},
		),
		jenkinsRoleBindingIndex: jenkinsRoleBindingInformer.GetIndexer(),
		controllerLister: cache.NewGenericLister(
			controllerInformer.GetIndexer(),
			schema.GroupResource{Group: "varroa.dev", Resource: "controllers"},
		),
		defaultRead:     defaultRead,
		userClaimNames:  userClaimNames,
		groupClaimNames: groupClaimNames,
	}
}

// EffectiveAPICapabilities returns the union of apiRules from all bindings
// whose subjects match the caller's JWT claims and whose scope covers
// the given (namespace, controllerName).
//
// Algorithm:
// 1. Build subject keys from claims: each group -> "Group:<name>", user -> "User:<sub>"
// 2. For each subject key, look up bindings via the by-subject index
// 3. For each matching binding, check scope coverage
// 4. Look up the referenced VarroaRole and merge its apiRules
// 5. If no bindings matched and defaultRead is true, grant controllers:read
func (r *Resolver) EffectiveAPICapabilities(claims *auth.Claims, namespace, controllerName string) APICapabilitySet {
	caps := make(APICapabilitySet)

	if claims == nil {
		if r.defaultRead {
			caps["controllers"] = map[string]bool{"read": true}
		}
		return caps
	}

	// Build subject keys from the configured JWT claims.
	// Which claims are used is controlled by --oidc-user-claim and
	// --oidc-group-claim flags (e.g. "sub,preferred_username" and "groups").
	var subjectKeys []string
	for _, v := range claims.GroupValues(r.groupClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("Group", v))
	}
	for _, v := range claims.UserValues(r.userClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("User", v))
	}

	seenRoles := make(map[string]bool)

	for _, key := range subjectKeys {
		objs, err := r.roleBindingIndex.ByIndex(BySubjectIndex, key)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
			if !ok {
				continue
			}

			// Check scope. When both namespace and controllerName are empty
			// (e.g., /me/permissions introspection), skip scope filtering
			// to return the user's full capability set.
			if namespace != "" || controllerName != "" {
				matches, err := r.scopeMatches(binding.Spec.Scope, namespace, controllerName)
				if err != nil || !matches {
					continue
				}
			}

			// Look up the referenced role
			roleRef := binding.Spec.RoleRef
			if seenRoles[roleRef] {
				continue
			}
			seenRoles[roleRef] = true

			roleObj, err := r.roleLister.Get(roleRef)
			if err != nil {
				continue
			}
			role, ok := roleObj.(*v1alpha1.VarroaRole)
			if !ok {
				continue
			}

			// Merge apiRules
			for _, rule := range role.Spec.APIRules {
				for _, resource := range rule.Resources {
					if caps[resource] == nil {
						caps[resource] = make(map[string]bool)
					}
					for _, verb := range rule.Verbs {
						caps[resource][verb] = true
					}
				}
			}
		}
	}

	// Default read fallback
	if len(caps) == 0 && r.defaultRead {
		caps["controllers"] = map[string]bool{"read": true}
	}

	return caps
}

// EffectiveClusterAPICapabilities returns the capabilities granted to the caller
// by cluster-wide (nil-scope) role bindings ONLY. Namespace- and controller-scoped
// bindings are excluded. Used to authorize Varroa-global resources/events that are
// not tied to any single controller.
func (r *Resolver) EffectiveClusterAPICapabilities(claims *auth.Claims) APICapabilitySet {
	caps := make(APICapabilitySet)

	if claims == nil {
		if r.defaultRead {
			caps["controllers"] = map[string]bool{"read": true}
		}
		return caps
	}

	var subjectKeys []string
	for _, v := range claims.GroupValues(r.groupClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("Group", v))
	}
	for _, v := range claims.UserValues(r.userClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("User", v))
	}

	seenRoles := make(map[string]bool)

	for _, key := range subjectKeys {
		objs, err := r.roleBindingIndex.ByIndex(BySubjectIndex, key)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
			if !ok {
				continue
			}

			// Skip scoped bindings — only nil-scope (cluster-wide) bindings
			// count toward cluster-wide capabilities.
			if binding.Spec.Scope != nil {
				continue
			}

			// Look up the referenced role
			roleRef := binding.Spec.RoleRef
			if seenRoles[roleRef] {
				continue
			}
			seenRoles[roleRef] = true

			roleObj, err := r.roleLister.Get(roleRef)
			if err != nil {
				continue
			}
			role, ok := roleObj.(*v1alpha1.VarroaRole)
			if !ok {
				continue
			}

			// Merge apiRules
			for _, rule := range role.Spec.APIRules {
				for _, resource := range rule.Resources {
					if caps[resource] == nil {
						caps[resource] = make(map[string]bool)
					}
					for _, verb := range rule.Verbs {
						caps[resource][verb] = true
					}
				}
			}
		}
	}

	// Default read fallback
	if len(caps) == 0 && r.defaultRead {
		caps["controllers"] = map[string]bool{"read": true}
	}

	return caps
}

// EffectivePermissionScopes returns the caller's effective API capabilities
// partitioned by scope, for /me/permissions introspection. Unlike
// EffectiveAPICapabilities(claims,"",""), it does NOT report scoped grants as
// cluster-wide.
func (r *Resolver) EffectivePermissionScopes(claims *auth.Claims) PermissionScopes {
	global := make(APICapabilitySet)
	buckets := make(map[string]*ScopedCapabilities)
	var order []string

	if claims == nil {
		if r.defaultRead {
			global["controllers"] = map[string]bool{"read": true}
		}
		return PermissionScopes{Global: global, Scopes: nil}
	}

	// Build subject keys from the configured JWT claims.
	var subjectKeys []string
	for _, v := range claims.GroupValues(r.groupClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("Group", v))
	}
	for _, v := range claims.UserValues(r.userClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("User", v))
	}

	// seen dedupes a role per partition: bucketKey + "\x00" + roleRef.
	seen := make(map[string]bool)

	for _, key := range subjectKeys {
		objs, err := r.roleBindingIndex.ByIndex(BySubjectIndex, key)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
			if !ok {
				continue
			}

			sc := binding.Spec.Scope
			var bkey string
			if sc != nil {
				bkey = scopeBucketKey(sc)
			} else {
				bkey = ""
			}

			// Dedup: role merged once per partition.
			dedupKey := bkey + "\x00" + binding.Spec.RoleRef
			if seen[dedupKey] {
				continue
			}
			seen[dedupKey] = true

			// Look up the referenced role.
			roleObj, err := r.roleLister.Get(binding.Spec.RoleRef)
			if err != nil {
				continue
			}
			role, ok := roleObj.(*v1alpha1.VarroaRole)
			if !ok {
				continue
			}

			// Choose target: global (nil scope) or scoped bucket.
			var target APICapabilitySet
			if sc == nil {
				target = global
			} else {
				b := buckets[bkey]
				if b == nil {
					ns := append([]string(nil), sc.Namespaces...)
					sort.Strings(ns)
					b = &ScopedCapabilities{
						Namespaces:            ns,
						HasControllerSelector: sc.ControllerSelector != nil,
						Capabilities:          make(APICapabilitySet),
					}
					buckets[bkey] = b
					order = append(order, bkey)
				}
				target = b.Capabilities
			}

			// Merge apiRules.
			for _, rule := range role.Spec.APIRules {
				for _, resource := range rule.Resources {
					if target[resource] == nil {
						target[resource] = make(map[string]bool)
					}
					for _, verb := range rule.Verbs {
						target[resource][verb] = true
					}
				}
			}
		}
	}

	// Default read fallback: only when nothing matched at all.
	if len(global) == 0 && len(buckets) == 0 && r.defaultRead {
		global["controllers"] = map[string]bool{"read": true}
	}

	// Materialize scopes in canonical bucket-key order. First-seen order is
	// NOT deterministic (informer index iteration), so sort for a stable API.
	sort.Strings(order)
	scopes := make([]ScopedCapabilities, 0, len(order))
	for _, bkey := range order {
		scopes = append(scopes, *buckets[bkey])
	}

	return PermissionScopes{Global: global, Scopes: scopes}
}

// scopeBucketKey canonicalizes a scope into a dedup/order key.
// nil scope is handled by the caller (goes to Global) and never reaches here.
func scopeBucketKey(scope *v1alpha1.VarroaRoleBindingScope) string {
	ns := append([]string(nil), scope.Namespaces...)
	sort.Strings(ns)
	sel := "0"
	if scope.ControllerSelector != nil {
		sel = "1:" + metav1.FormatLabelSelector(scope.ControllerSelector)
	}
	return strings.Join(ns, ",") + "|" + sel
}

// CreateScopeNamespaces returns the namespaces in which the caller may create
// controllers, derived from effective VarroaRoleBinding scopes. It mirrors the
// create-time enforcement path (CanCreateController → EffectiveAPICapabilities →
// scopeMatches). unrestricted is true when the caller holds create authority that
// is not bounded to a namespace list. namespaces holds the explicit, deduped
// namespaces contributed by namespace-scoped bindings (may be empty).
func (r *Resolver) CreateScopeNamespaces(claims *auth.Claims) (namespaces []string, unrestricted bool) {
	if claims == nil {
		return nil, false
	}

	// Build subject keys exactly as EffectiveAPICapabilities does.
	var subjectKeys []string
	for _, v := range claims.GroupValues(r.groupClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("Group", v))
	}
	for _, v := range claims.UserValues(r.userClaimNames) {
		subjectKeys = append(subjectKeys, subjectIndexKey("User", v))
	}

	nsSet := make(map[string]struct{})

	for _, key := range subjectKeys {
		objs, err := r.roleBindingIndex.ByIndex(BySubjectIndex, key)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
			if !ok {
				continue
			}

			// Look up the role; skip on miss.
			roleObj, err := r.roleLister.Get(binding.Spec.RoleRef)
			if err != nil {
				continue
			}
			role, ok := roleObj.(*v1alpha1.VarroaRole)
			if !ok {
				continue
			}

			// Skip if the role does not grant "create" on "controllers".
			if !roleGrantsControllersCreate(role) {
				continue
			}

			// Apply the derivation truth table.
			sc := binding.Spec.Scope
			if sc != nil && sc.ControllerSelector != nil {
				continue
			}
			if sc == nil {
				unrestricted = true
				continue
			}
			if len(sc.Namespaces) == 0 {
				unrestricted = true
				continue
			}
			for _, ns := range sc.Namespaces {
				nsSet[ns] = struct{}{}
			}
		}
	}

	// Build result from nsSet keys.
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}
	return namespaces, unrestricted
}

// roleGrantsControllersCreate reports whether any APIRule in the role grants
// "create" on "controllers", honoring "*" resource and "*" verb wildcards.
func roleGrantsControllersCreate(role *v1alpha1.VarroaRole) bool {
	for _, rule := range role.Spec.APIRules {
		resMatch := contains(rule.Resources, "controllers") || contains(rule.Resources, "*")
		verbMatch := contains(rule.Verbs, "create") || contains(rule.Verbs, "*")
		if resMatch && verbMatch {
			return true
		}
	}
	return false
}

// contains reports whether a string slice contains a given string.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// SubjectAssignment pairs a subject kind with its name.
type SubjectAssignment struct {
	Kind string // "User" | "Group"
	Name string
}

// accKey is a dedup key for role assignments: (roleName, scopeKey).
type accKey struct {
	name     string
	scopeKey string
}

// RoleAssignment groups a role's permissions with its assigned subjects.
type RoleAssignment struct {
	RoleName    string
	RoleType    string // "Global" | "Item" | "Agent"; default "Global"
	Pattern     string // role-strategy regex for Item/Agent; "" for Global
	Permissions []string
	Subjects    []SubjectAssignment
}

// JenkinsAssignments returns RoleAssignments for a given controller by
// evaluating all VarroaRoleBindings against the controller's namespace and labels.
func (r *Resolver) JenkinsAssignments(controller *v1alpha1.Controller) ([]RoleAssignment, error) {
	// List all bindings from the index
	objs := r.roleBindingIndex.List()
	if len(objs) == 0 {
		return nil, nil
	}

	// Group by role: roleName -> set of subjects (kind:name)
	type roleEntry struct {
		role     *v1alpha1.VarroaRole
		subjects map[string]SubjectAssignment
	}
	roleMap := make(map[string]*roleEntry)

	for _, obj := range objs {
		binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
		if !ok {
			continue
		}

		matches, err := r.scopeMatches(binding.Spec.Scope, controller.Namespace, controller.Name)
		if err != nil || !matches {
			continue
		}

		roleRef := binding.Spec.RoleRef
		if roleMap[roleRef] == nil {
			roleObj, err := r.roleLister.Get(roleRef)
			if err != nil {
				continue
			}
			role, ok := roleObj.(*v1alpha1.VarroaRole)
			if !ok {
				continue
			}
			roleMap[roleRef] = &roleEntry{role: role, subjects: make(map[string]SubjectAssignment)}
		}

		for _, s := range binding.Spec.Subjects {
			key := subjectIndexKey(s.Kind, s.Name)
			roleMap[roleRef].subjects[key] = SubjectAssignment{Kind: s.Kind, Name: s.Name}
		}
	}

	// Collect and sort role names for deterministic output.
	var roleNames []string
	for name := range roleMap {
		roleNames = append(roleNames, name)
	}
	sort.Strings(roleNames)

	var assignments []RoleAssignment
	for _, roleName := range roleNames {
		entry := roleMap[roleName]
		if len(entry.role.Spec.JenkinsPermissions) == 0 || entry.role.Spec.JenkinsRoleRef != "" {
			continue
		}
		subjList := make([]SubjectAssignment, 0, len(entry.subjects))
		for _, s := range entry.subjects {
			subjList = append(subjList, s)
		}
		sort.Slice(subjList, func(i, j int) bool {
			return subjList[i].Kind+":"+subjList[i].Name < subjList[j].Kind+":"+subjList[j].Name
		})
		assignments = append(assignments, RoleAssignment{
			RoleName:    "varroa:" + roleName,
			RoleType:    "Global",
			Pattern:     "",
			Permissions: entry.role.Spec.JenkinsPermissions,
			Subjects:    subjList,
		})
	}

	return assignments, nil
}

// scopeMatches checks whether a binding's scope covers a given (namespace, controllerName).
// nil scope = cluster-wide (always matches).
// Non-nil: namespace must be in the allowlist (if non-empty) AND
// the controller's labels must match the selector (if set).
func (r *Resolver) scopeMatches(scope *v1alpha1.VarroaRoleBindingScope, namespace, controllerName string) (bool, error) {
	if scope == nil {
		return true, nil
	}

	// Check namespace allowlist
	if len(scope.Namespaces) > 0 {
		found := false
		for _, ns := range scope.Namespaces {
			if ns == namespace {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}

	// Check label selector against controller.
	// A controllerSelector requires a concrete controller target to evaluate.
	// When namespace/controllerName is empty (cluster-scoped query), a
	// selector-scoped binding does NOT match — otherwise it would silently
	// grant cluster-wide API permissions.
	if scope.ControllerSelector != nil {
		if namespace == "" || controllerName == "" {
			return false, nil
		}
		sel, err := metav1.LabelSelectorAsSelector(scope.ControllerSelector)
		if err != nil {
			return false, fmt.Errorf("invalid controllerSelector: %w", err)
		}
		obj, err := r.controllerLister.ByNamespace(namespace).Get(controllerName)
		if err != nil {
			return false, fmt.Errorf("get controller %s/%s: %w", namespace, controllerName, err)
		}
		ctrl, ok := obj.(*v1alpha1.Controller)
		if !ok {
			return false, nil
		}
		if !sel.Matches(labels.Set(ctrl.Labels)) {
			return false, nil
		}
	}

	return true, nil
}

// JenkinsRoleAssignments returns the full set of RoleAssignments for a controller
// by evaluating JenkinsRoleBindings, VarroaRole.JenkinsRoleRef, and the legacy
// VarroaRole.JenkinsPermissions path (without JenkinsRoleRef).
func (r *Resolver) JenkinsRoleAssignments(controller *v1alpha1.Controller) ([]RoleAssignment, error) {
	acc := make(map[accKey]*RoleAssignment)

	// Source 1: JenkinsRoleBinding -> JenkinsRole
	r.collectFromJenkinsRoleBindings(controller, acc)

	// Source 2: VarroaRoleBinding -> VarroaRole.JenkinsRoleRef -> JenkinsRole (Global only)
	r.collectFromVarroaRoleBindings(controller, acc)

	// Source 3: Legacy VarroaRole.JenkinsPermissions (no JenkinsRoleRef) -> Global
	legacy, err := r.JenkinsAssignments(controller)
	if err != nil {
		return nil, err
	}
	for _, a := range legacy {
		key := accKey{name: a.RoleName, scopeKey: ""}
		if existing, ok := acc[key]; ok {
			// Union subjects
			seen := make(map[string]bool)
			for _, s := range existing.Subjects {
				seen[s.Kind+":"+s.Name] = true
			}
			for _, s := range a.Subjects {
				if !seen[s.Kind+":"+s.Name] {
					existing.Subjects = append(existing.Subjects, s)
				}
			}
		} else {
			acc[key] = &a
		}
	}

	// Synthesize: the operator-signed mite JWT authenticates as principal
	// "varroa-mite" carrying the GROUP authority "ROLE:varroa:system-mite".
	// role-strategy matches an assigned SID against the principal's granted
	// authorities, and a USER:varroa-mite entry does NOT match this
	// (filter-injected, impersonated) principal — only the group authority
	// does (same constraint the matrix-auth bootstrap documents). So the
	// generated YAML must always assign this role to the GROUP authority,
	// even if no explicit JenkinsRoleBinding binds it.
	{
		miteKey := accKey{name: "varroa:system-mite", scopeKey: ""}
		miteSubj := SubjectAssignment{Kind: "Group", Name: "ROLE:varroa:system-mite"}
		if existing, ok := acc[miteKey]; ok {
			found := false
			for _, s := range existing.Subjects {
				if s.Kind == miteSubj.Kind && s.Name == miteSubj.Name {
					found = true
					break
				}
			}
			if !found {
				existing.Subjects = append(existing.Subjects, miteSubj)
			}
		} else {
			// Emit the mite role even if no JenkinsRoleBinding pulled it in.
			// Prefer the built-in JenkinsRole's permissions when that CRD exists,
			// but ALWAYS fall back to the minimal MANAGE-based set — the mite MUST
			// retain a working role under role-based auth or the operator can never
			// push again. Relying on the built-in JenkinsRole being reconciled first
			// is a lockout trap: the operator's JCasC push replaces the whole role
			// map, dropping the bootstrap role, so if this synthesis is skipped the
			// mite loses access on the very first push. After PR #172 the mite pushes
			// config via the MANAGE-gated reload endpoint, so MANAGE (not Administer)
			// is sufficient for the operator to recover.
			perms := MiteMinimalPermissions()
			if jrObj, err := r.jenkinsRoleLister.Get("varroa-system-mite"); err == nil {
				if jr, ok := jrObj.(*v1alpha1.JenkinsRole); ok && len(jr.Spec.Permissions) > 0 {
					perms = jr.Spec.Permissions
				}
			}
			acc[miteKey] = &RoleAssignment{
				RoleName:    "varroa:system-mite",
				RoleType:    "Global",
				Pattern:     "",
				Permissions: perms,
				Subjects:    []SubjectAssignment{miteSubj},
			}
		}
	}

	// Synthesize: the operator's short-lived executeGroovy JWT authenticates as
	// principal "varroa-operator" carrying the GROUP authority
	// "ROLE:varroa:system-operator". Same role-strategy constraint as the mite
	// above — only the group authority is matchable — so the generated YAML must
	// assign the varroa:system-operator role to that group. Emit unconditionally
	// so on-demand executeGroovy is authorized the moment a valid operator JWT is
	// presented; this authority is only ever granted to a bearer of an
	// operator-signed (~2-minute) JWT and can never attach to a human user. Unlike
	// the mite there is no lockout-trap minimal fallback (operator access is not
	// recovery-critical): use the built-in JenkinsRole's permissions when
	// reconciled, else the canonical SystemOperatorPermissions() ([Administer]).
	{
		opKey := accKey{name: "varroa:system-operator", scopeKey: ""}
		opSubj := SubjectAssignment{Kind: "Group", Name: "ROLE:varroa:system-operator"}
		if existing, ok := acc[opKey]; ok {
			found := false
			for _, s := range existing.Subjects {
				if s.Kind == opSubj.Kind && s.Name == opSubj.Name {
					found = true
					break
				}
			}
			if !found {
				existing.Subjects = append(existing.Subjects, opSubj)
			}
		} else {
			perms := SystemOperatorPermissions()
			if jrObj, err := r.jenkinsRoleLister.Get("varroa-system-operator"); err == nil {
				if jr, ok := jrObj.(*v1alpha1.JenkinsRole); ok && len(jr.Spec.Permissions) > 0 {
					perms = jr.Spec.Permissions
				}
			}
			acc[opKey] = &RoleAssignment{
				RoleName:    "varroa:system-operator",
				RoleType:    "Global",
				Pattern:     "",
				Permissions: perms,
				Subjects:    []SubjectAssignment{opSubj},
			}
		}
	}

	// Sort and return
	var result []RoleAssignment
	for _, a := range acc {
		if len(a.Permissions) == 0 {
			continue
		}
		sort.Slice(a.Subjects, func(i, j int) bool {
			return a.Subjects[i].Kind+":"+a.Subjects[i].Name < a.Subjects[j].Kind+":"+a.Subjects[j].Name
		})
		result = append(result, *a)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].RoleName != result[j].RoleName {
			return result[i].RoleName < result[j].RoleName
		}
		return result[i].Pattern < result[j].Pattern
	})
	return result, nil
}

// collectFromJenkinsRoleBindings evaluates JenkinsRoleBindings and adds them to acc.
func (r *Resolver) collectFromJenkinsRoleBindings(controller *v1alpha1.Controller, acc map[accKey]*RoleAssignment) {
	objs := r.jenkinsRoleBindingIndex.List()
	for _, obj := range objs {
		binding, ok := obj.(*v1alpha1.JenkinsRoleBinding)
		if !ok {
			continue
		}
		if !controllerScopeMatches(binding.Spec.ControllerScope, controller) {
			continue
		}
		jrObj, err := r.jenkinsRoleLister.Get(binding.Spec.RoleRef)
		if err != nil {
			continue
		}
		jr, ok := jrObj.(*v1alpha1.JenkinsRole)
		if !ok {
			continue
		}
		if len(jr.Spec.Permissions) == 0 {
			continue
		}
		roleType, pattern := lowerScope(jr.Spec.RoleType, binding.Spec.JenkinsScope)
		scopeKey := scopeSuffix(binding.Spec.JenkinsScope)
		name := binding.Spec.RoleRef
		if !strings.HasPrefix(name, "varroa:") {
			name = "varroa:" + name
		}
		name += scopeKey
		upsertAssignment(acc, name, roleType, pattern, jr.Spec.Permissions, binding.Spec.Subjects)
	}
}

// collectFromVarroaRoleBindings evaluates VarroaRoleBindings where the
// VarroaRole references a JenkinsRole via JenkinsRoleRef.
func (r *Resolver) collectFromVarroaRoleBindings(controller *v1alpha1.Controller, acc map[accKey]*RoleAssignment) {
	objs := r.roleBindingIndex.List()
	for _, obj := range objs {
		binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
		if !ok {
			continue
		}
		matches, err := r.scopeMatches(binding.Spec.Scope, controller.Namespace, controller.Name)
		if err != nil || !matches {
			continue
		}
		vrObj, err := r.roleLister.Get(binding.Spec.RoleRef)
		if err != nil {
			continue
		}
		vr, ok := vrObj.(*v1alpha1.VarroaRole)
		if !ok || vr.Spec.JenkinsRoleRef == "" {
			continue
		}
		jrObj, err := r.jenkinsRoleLister.Get(vr.Spec.JenkinsRoleRef)
		if err != nil {
			continue
		}
		jr, ok := jrObj.(*v1alpha1.JenkinsRole)
		if !ok || jr.Spec.RoleType != "" && jr.Spec.RoleType != "Global" {
			continue // only Global JenkinsRoles can be linked from VarroaRoles
		}
		if len(jr.Spec.Permissions) == 0 {
			continue
		}
		name := vr.Spec.JenkinsRoleRef
		if !strings.HasPrefix(name, "varroa:") {
			name = "varroa:" + name
		}
		upsertAssignment(acc, name, "Global", "", jr.Spec.Permissions, binding.Spec.Subjects)
	}
}

// upsertAssignment adds or merges a RoleAssignment into the accumulator.
func upsertAssignment(acc map[accKey]*RoleAssignment, name, roleType, pattern string, permissions []string, subjects []v1alpha1.SubjectRef) {
	key := accKey{name: name, scopeKey: scopeKey(roleType, pattern)}
	if existing, ok := acc[key]; ok {
		seen := make(map[string]bool)
		for _, s := range existing.Subjects {
			seen[s.Kind+":"+s.Name] = true
		}
		for _, s := range subjects {
			if !seen[s.Kind+":"+s.Name] {
				existing.Subjects = append(existing.Subjects, SubjectAssignment{Kind: s.Kind, Name: s.Name})
			}
		}
	} else {
		subjList := make([]SubjectAssignment, 0, len(subjects))
		for _, s := range subjects {
			subjList = append(subjList, SubjectAssignment{Kind: s.Kind, Name: s.Name})
		}
		acc[key] = &RoleAssignment{
			RoleName:    name,
			RoleType:    roleType,
			Pattern:     pattern,
			Permissions: permissions,
			Subjects:    subjList,
		}
	}
}

// scopeKey returns a dedup key for a (roleType, pattern) pair.
func scopeKey(roleType, pattern string) string {
	if roleType == "" || roleType == "Global" {
		return ""
	}
	return roleType + ":" + pattern
}

// controllerScopeMatches checks whether a JenkinsRoleBinding's ControllerScope
// covers the given controller. nil scope always matches. Otherwise it reuses
// the VarroaRoleBindingScope.match approach but without requiring a full
// Resolver.
func controllerScopeMatches(scope *v1alpha1.VarroaRoleBindingScope, controller *v1alpha1.Controller) bool {
	if scope == nil {
		return true
	}
	if len(scope.Namespaces) > 0 {
		found := false
		for _, ns := range scope.Namespaces {
			if ns == controller.Namespace {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if scope.ControllerSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(scope.ControllerSelector)
		if err != nil {
			return false
		}
		if !sel.Matches(labels.Set(controller.Labels)) {
			return false
		}
	}
	return true
}

// lowerScope converts a JenkinsRole.RoleType + JenkinsScope into a
// concrete (roleType, pattern) pair for role-strategy.
func lowerScope(roleType string, js *v1alpha1.JenkinsScope) (string, string) {
	// Agent roles go into the "agents" bucket regardless of scope.
	// TODO: thread agent-name pattern through for Agent-type roles
	// so that JenkinsScope.Pattern can scope an agent role to specific
	// agents by name regex.
	if roleType == "Agent" {
		return "Agent", ""
	}
	if js == nil || js.Type == "" || js.Type == "Global" {
		return "Global", ""
	}
	switch js.Type {
	case "Pattern":
		return "Item", js.Pattern
	case "Folder":
		f := regexp.QuoteMeta(js.Folder)
		switch js.Propagate {
		case "None", "":
			return "Item", "^" + f + "$"
		case "Children":
			return "Item", "^" + f + "/[^/]*$"
		case "Subtree":
			return "Item", "^" + f + "($|/.*)$"
		default:
			return "Item", "^" + f + "$"
		}
	default:
		return "Global", ""
	}
}

// scopeSuffix makes a role name unique per scope.
func scopeSuffix(js *v1alpha1.JenkinsScope) string {
	if js == nil || js.Type == "" || js.Type == "Global" {
		return ""
	}
	switch js.Type {
	case "Pattern":
		return ":pattern:" + js.Pattern
	case "Folder":
		return ":folder:" + js.Folder + ":" + js.Propagate
	default:
		return ""
	}
}
