package rbac

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/rbac/jobglob"
)

// MaxTenantGrants bounds one tenant's grant list. A cluster-wide binding
// reaches every tenant, so a fleet with many such bindings produces a long
// list without the cap.
const MaxTenantGrants = 50

// Grant sources, in the order TenantGrantSources reports them. A narrowing
// grant matters more than a cluster-wide one, so the narrowing sources come
// first. A federated copy ranks with the tenant-scoped sources and before
// the cluster-wide binding. The all-tenants copy uses the same rank, and its
// AllTenants field marks it.
const (
	GrantSourceTenantMembership    = "tenantMembership"
	GrantSourceTenantScopedBinding = "tenantScopedBinding"
	GrantSourceClusterWideBinding  = "clusterWideBinding"
	GrantSourceFederatedBinding    = "federatedBinding"
)

// sourceRank orders the four grant sources.
var sourceRank = map[string]int{
	GrantSourceTenantMembership:    0,
	GrantSourceTenantScopedBinding: 1,
	GrantSourceFederatedBinding:    2,
	GrantSourceClusterWideBinding:  3,
}

// TenantGrantSource is one grant that reaches a tenant, with the subjects
// it names. Jobs is nil when the grant narrows nothing: an empty list and
// an absent list mean different things to a reader.
//
// For a federatedBinding source, Binding names the JenkinsRoleBinding copy,
// RoleRef names the source VarroaRole, and FederatedFrom names the core
// VarroaRoleBindings the copy stands for.
type TenantGrantSource struct {
	Source        string
	Binding       string
	RoleRef       string
	Subjects      []v1alpha1.SubjectRef
	Verbs         []string
	Jobs          []string
	FederatedFrom []string
	// AllTenants is true for the all-tenants copy of a core cluster-wide
	// binding. That copy reaches every tenant that a cell hosts. Every other
	// source leaves it false.
	AllTenants bool
}

// TenantItemRoleName returns the generated Jenkins item role name of a
// tenant. The name comes from the tenant name alone, so a caller resolves
// it without reading a JenkinsRole object.
func TenantItemRoleName(tenant string) string {
	return "varroa:tenant:" + tenant
}

// TenantItemRolePattern returns the folder pattern of the generated Jenkins
// item role of a tenant.
func TenantItemRolePattern(tenant string) string {
	return "^" + regexp.QuoteMeta(tenant) + "($|/.*)$"
}

// TenantMemberRoleName returns the generated Jenkins item role name that
// grants a tenant's write holders the build and replay permissions on the
// tenant folder subtree. A narrowed grant appends its binding name.
//
// No name this package generates may equal TenantItemRoleName of a tenant:
// the plugin's VarroaTenantResolver looks that exact name up, and a
// collision would hand a member role to the service identity. Tenant names
// hold no ":", so the "tenant-member:" prefix cannot collide.
func TenantMemberRoleName(tenant string) string {
	return "varroa:tenant-member:" + tenant
}

// TenantMemberRolePattern returns the folder pattern of the member role of
// a tenant that a cluster-wide or Tenant-membership write grant produces. It
// covers the tenant folder itself and every job under it.
func TenantMemberRolePattern(tenant string) string {
	return TenantItemRolePattern(tenant)
}

// TenantMemberNarrowedRoleName returns the member role name of one
// tenant-scoped binding that narrows the grant to job globs.
func TenantMemberNarrowedRoleName(tenant, binding string) string {
	return "varroa:tenant-member:" + tenant + ":" + binding
}

// TenantMemberNarrowedPattern returns the folder pattern of a narrowed
// member role. Each job glob is folder-relative, so the pattern joins the
// tenant name and the glob body.
func TenantMemberNarrowedPattern(tenant string, jobs []string) string {
	bodies := make([]string, 0, len(jobs))
	for _, g := range jobs {
		bodies = append(bodies, jobglob.RegexpBody(g))
	}
	return "^" + regexp.QuoteMeta(tenant) + "/(?:" + strings.Join(bodies, "|") + ")$"
}

// tenantMemberPermissions lists the Jenkins permissions of a member role.
// They cover the acting-user routes that check the person, not the service
// identity: scan, run, replay, restart, and input answers.
var tenantMemberPermissions = []string{
	"hudson.model.Item.Read",
	"hudson.model.Item.Discover",
	"hudson.model.Item.Build",
	"hudson.model.Item.Cancel",
	"hudson.model.Run.Replay",
	"hudson.model.View.Read",
}

// TenantMemberRole is one generated Jenkins member role of a tenant.
type TenantMemberRole struct {
	Name        string
	Pattern     string
	Binding     string // "" for the role on the whole folder subtree
	Permissions []string
	Subjects    []v1alpha1.SubjectRef // deduplicated, sorted by Kind+":"+Name
}

// TenantMemberRoles returns the generated member roles of one pipelines
// tenant, the same roles JenkinsRoleAssignments emits for it. The operator
// and the cell grants read both call it, so they cannot disagree. It returns
// nil for a nil Resolver or a tenant that is not a pipelines Tenant.
func (r *Resolver) TenantMemberRoles(tenant ParsedTenant) []TenantMemberRole {
	if r == nil || tenant.Kind != TenantTenant || tenant.Tenant == "" {
		return nil
	}
	return r.tenantMemberRoles(tenant.Tenant, r.userPrincipalAliases())
}

// tenantMemberRoles builds the member roles of one tenant. The subtree role
// comes first, then one narrowed role per binding in first-seen order.
//
// The member role grants the person checks. The acting-user routes (scan,
// run, replay, restart, input answers) check the caller's own Jenkins
// permissions, so each subject with pipelines write on the tenant gets the
// item permissions here. A narrowed grant gets only its job globs. The walk
// is uncapped: a cut list would drop a subject silently. One binding can
// narrow the same tenant in several entries. Each entry names the same role,
// so the job globs of one binding merge into one pattern. A separate role per
// entry would collide by name, and Jenkins keeps one of them.
func (r *Resolver) tenantMemberRoles(tenant string, aliases map[string][]string) []TenantMemberRole {
	var subtree *TenantMemberRole
	narrowed := map[string]*tenantMemberNarrowing{}
	var narrowedOrder []string
	grants := r.allTenantGrantSources(ParsedTenant{Kind: TenantTenant, Tenant: tenant, Folder: tenant})
	for _, src := range grants {
		if !slices.Contains(src.Verbs, "write") {
			continue
		}
		subjects := expandMemberUserSubjects(src.Subjects, aliases)
		if src.Jobs == nil {
			if subtree == nil {
				subtree = &TenantMemberRole{
					Name:        TenantMemberRoleName(tenant),
					Pattern:     TenantMemberRolePattern(tenant),
					Permissions: slices.Clone(tenantMemberPermissions),
				}
			}
			subtree.Subjects = mergeMemberSubjects(subtree.Subjects, subjects)
			continue
		}
		if len(src.Jobs) == 0 {
			continue
		}
		n, ok := narrowed[src.Binding]
		if !ok {
			n = &tenantMemberNarrowing{}
			narrowed[src.Binding] = n
			narrowedOrder = append(narrowedOrder, src.Binding)
		}
		// Every entry of one binding names the same subjects, so the
		// subjects are set once, not appended once per entry.
		if n.subjects == nil {
			n.subjects = subjects
		}
		n.jobs = append(n.jobs, src.Jobs...)
	}

	var out []TenantMemberRole
	if subtree != nil {
		out = append(out, *subtree)
	}
	for _, binding := range narrowedOrder {
		n := narrowed[binding]
		out = append(out, TenantMemberRole{
			Name:        TenantMemberNarrowedRoleName(tenant, binding),
			Pattern:     TenantMemberNarrowedPattern(tenant, n.jobs),
			Binding:     binding,
			Permissions: slices.Clone(tenantMemberPermissions),
			Subjects:    mergeMemberSubjects(nil, n.subjects),
		})
	}
	return out
}

// mergeMemberSubjects appends the subjects that dst does not hold yet.
// The result is sorted by Kind+":"+Name.
func mergeMemberSubjects(dst, add []v1alpha1.SubjectRef) []v1alpha1.SubjectRef {
	seen := make(map[string]bool, len(dst)+len(add))
	out := make([]v1alpha1.SubjectRef, 0, len(dst)+len(add))
	for _, s := range slices.Concat(dst, add) {
		key := s.Kind + ":" + s.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Kind+":"+out[i].Name < out[j].Kind+":"+out[j].Name
	})
	return out
}

// tenantMemberNarrowing collects the subjects and job globs of one binding
// that narrows a tenant, across every scope entry the binding names.
type tenantMemberNarrowing struct {
	subjects []v1alpha1.SubjectRef
	jobs     []string
}

// TenantGrantSources lists every grant that reaches one pipelines tenant.
// It is the reverse of EffectiveTenantCapabilities: it walks the same four
// paths (Tenant membership, tenant-scoped and cluster-wide bindings, and the
// federated tenant-member copies) with the subject filter removed, so it
// takes no claims.
//
// The method reports. It decides nothing, and every authorization decision
// keeps calling the tenant checks the authorizer exposes.
//
// It resolves a pipelines tenant only. A cell hosts pipelines tenants as
// top-level folders, so a synthetic controller tenant reaches no cell.
//
// It returns at most MaxTenantGrants entries. A caller that must tell a
// full list from a cut list calls TenantGrantSourcesTruncated instead: a
// list of exactly MaxTenantGrants entries is not proof of a cut.
func (r *Resolver) TenantGrantSources(tenant ParsedTenant) []TenantGrantSource {
	grants, _ := r.TenantGrantSourcesTruncated(tenant)
	return grants
}

// TenantGrantSourcesTruncated returns the same grants TenantGrantSources
// returns, and reports whether the cap dropped a grant.
func (r *Resolver) TenantGrantSourcesTruncated(tenant ParsedTenant) ([]TenantGrantSource, bool) {
	out := r.allTenantGrantSources(tenant)
	if len(out) > MaxTenantGrants {
		return out[:MaxTenantGrants], true
	}
	return out, false
}

// allTenantGrantSources returns every grant that reaches one pipelines
// tenant, sorted, with no cap. It covers the same four sources
// TenantGrantSources reports. Callers that generate Jenkins grants use it,
// because a cut list would silently drop a subject from a Jenkins role.
func (r *Resolver) allTenantGrantSources(tenant ParsedTenant) []TenantGrantSource {
	if r == nil || tenant.Kind != TenantTenant || tenant.Tenant == "" {
		return nil
	}

	var out []TenantGrantSource
	if g, ok := r.tenantMembershipGrantSource(tenant.Tenant); ok {
		out = append(out, g)
	}
	out = append(out, r.bindingGrantSources(tenant.Tenant)...)
	out = append(out, r.federatedTenantMemberSources(tenant.Tenant)...)

	sort.SliceStable(out, func(i, j int) bool {
		if sourceRank[out[i].Source] != sourceRank[out[j].Source] {
			return sourceRank[out[i].Source] < sourceRank[out[j].Source]
		}
		return out[i].Binding < out[j].Binding
	})
	return out
}

// VarroaRolePipelinesVerbs lists the pipelines verbs a VarroaRole grants.
// The federation projection uses it to decide whether a tenant-scoped
// binding needs a member copy on the hives.
func VarroaRolePipelinesVerbs(vr *v1alpha1.VarroaRole) []string {
	return grantedPipelinesVerbs(roleAPICapabilities(vr))
}

// federatedTenantMemberSources lists the tenant-member copies that the
// federation projection placed on this cluster for one tenant. A copy names
// the tenant or the all-tenants value. Each copy is a grant with pipelines
// write. A copy with an unreadable job list is skipped, so the grant fails
// closed. An all-tenants copy sets AllTenants.
func (r *Resolver) federatedTenantMemberSources(tenant string) []TenantGrantSource {
	if r == nil || r.jenkinsRoleBindingIndex == nil {
		return nil
	}
	var out []TenantGrantSource
	for _, obj := range r.jenkinsRoleBindingIndex.List() {
		b, ok := obj.(*v1alpha1.JenkinsRoleBinding)
		if !ok || b.Labels[LabelFederatedTenantMember] != "true" || b.Labels[LabelFederatedFrom] == "" {
			continue
		}
		annotated := b.Annotations[AnnotationFederatedTenant]
		if (annotated != tenant && annotated != FederatedAllTenants) || len(b.Spec.Subjects) == 0 {
			continue
		}
		if _, narrowed := b.Annotations[AnnotationFederatedTenantJobs]; narrowed && annotated == FederatedAllTenants {
			// A cluster-wide grant never narrows. A copy that carries a job list is malformed and fails closed.
			continue
		}
		var jobs []string
		if raw, present := b.Annotations[AnnotationFederatedTenantJobs]; present {
			if err := json.Unmarshal([]byte(raw), &jobs); err != nil || len(jobs) == 0 {
				continue
			}
		}
		out = append(out, TenantGrantSource{
			Source:        GrantSourceFederatedBinding,
			Binding:       b.Name,
			RoleRef:       b.Labels[LabelFederatedFrom],
			Subjects:      append([]v1alpha1.SubjectRef(nil), b.Spec.Subjects...),
			Verbs:         []string{"write"},
			Jobs:          jobs,
			FederatedFrom: federatedFromNames(b.Annotations[AnnotationFederatedBindings]),
			AllTenants:    annotated == FederatedAllTenants,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Binding < out[j].Binding })
	return out
}

// federatedFromNames splits the source-binding annotation of a copy. It
// returns nil for an absent annotation, so the JSON omits the field.
func federatedFromNames(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// tenantMembershipGrantSource resolves the Tenant-membership path. It applies
// the rules of the forward walk: the managed Group counts only when
// status.groupRef names it and TenantRBACReady is true, an "admin" roleRef
// yields no grant, and an empty roleRef means "developer".
func (r *Resolver) tenantMembershipGrantSource(tenantName string) (TenantGrantSource, bool) {
	if r.tenantLister == nil {
		return TenantGrantSource{}, false
	}
	obj, err := r.tenantLister.Get(tenantName)
	if err != nil {
		return TenantGrantSource{}, false
	}
	tenant, ok := obj.(*v1alpha1.Tenant)
	if !ok {
		return TenantGrantSource{}, false
	}

	role := tenant.Spec.RoleRef
	if role == "" {
		role = "developer"
	}
	if role == "admin" {
		return TenantGrantSource{}, false
	}
	caps, ok := r.roleCapabilities(role)
	if !ok {
		return TenantGrantSource{}, false
	}

	subjects := tenantEffectiveSubjects(tenant)
	if len(subjects) == 0 {
		return TenantGrantSource{}, false
	}
	return TenantGrantSource{
		Source:   GrantSourceTenantMembership,
		RoleRef:  role,
		Subjects: subjects,
		Verbs:    grantedPipelinesVerbs(caps),
	}, true
}

// tenantEffectiveSubjects returns the Tenant's effective subject set: the
// managed Group, when status.groupRef names it and TenantRBACReady is true,
// plus every spec.subjects entry. spec.members is never a subject: those
// usernames belong to the managed Group.
func tenantEffectiveSubjects(tenant *v1alpha1.Tenant) []v1alpha1.SubjectRef {
	var out []v1alpha1.SubjectRef
	wantGroupRef := "tenant-" + tenant.Name
	if tenant.Status.GroupRef == wantGroupRef && tenantRBACReady(tenant) {
		out = append(out, v1alpha1.SubjectRef{Kind: "Group", Name: wantGroupRef})
	}
	out = append(out, tenant.Spec.Subjects...)
	return out
}

// bindingGrantSources resolves the tenant-scoped and cluster-wide binding
// paths over every stored VarroaRoleBinding. It keeps one entry for each
// matching scope.tenants entry, so two bindings that narrow differently
// stay separate.
func (r *Resolver) bindingGrantSources(tenant string) []TenantGrantSource {
	if r.roleBindingIndex == nil {
		return nil
	}
	var out []TenantGrantSource
	for _, obj := range r.roleBindingIndex.List() {
		binding, ok := obj.(*v1alpha1.VarroaRoleBinding)
		if !ok {
			continue
		}
		caps, ok := r.roleCapabilities(binding.Spec.RoleRef)
		if !ok {
			continue
		}
		sc := binding.Spec.Scope
		if sc == nil {
			out = append(out, TenantGrantSource{
				Source:   GrantSourceClusterWideBinding,
				Binding:  binding.Name,
				RoleRef:  binding.Spec.RoleRef,
				Subjects: binding.Spec.Subjects,
				Verbs:    grantedPipelinesVerbs(caps),
			})
			continue
		}
		for _, t := range sc.Tenants {
			if t.Name != tenant {
				continue
			}
			out = append(out, TenantGrantSource{
				Source:   GrantSourceTenantScopedBinding,
				Binding:  binding.Name,
				RoleRef:  binding.Spec.RoleRef,
				Subjects: binding.Spec.Subjects,
				Verbs:    grantedPipelinesVerbs(caps),
				Jobs:     t.Jobs,
			})
		}
	}
	return out
}

// grantedPipelinesVerbs lists the pipelines verbs a capability set grants.
// It honours the "*" resource and the "*" verb the same way
// GrantsPipelinesVerb does, and it reports the concrete verbs rather than
// the wildcard. The caller asks who may build, so every other resource is
// noise.
func grantedPipelinesVerbs(caps APICapabilitySet) []string {
	var out []string
	for _, verb := range pipelinesVerbs {
		if caps["*"] != nil && (caps["*"]["*"] || caps["*"][verb]) {
			out = append(out, verb)
			continue
		}
		if caps["pipelines"] != nil && (caps["pipelines"]["*"] || caps["pipelines"][verb]) {
			out = append(out, verb)
		}
	}
	return out
}
