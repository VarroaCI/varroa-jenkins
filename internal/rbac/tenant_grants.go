package rbac

import (
	"regexp"
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
// first.
const (
	GrantSourceTeamMembership      = "teamMembership"
	GrantSourceTenantScopedBinding = "tenantScopedBinding"
	GrantSourceClusterWideBinding  = "clusterWideBinding"
)

// sourceRank orders the three grant sources.
var sourceRank = map[string]int{
	GrantSourceTeamMembership:      0,
	GrantSourceTenantScopedBinding: 1,
	GrantSourceClusterWideBinding:  2,
}

// TenantGrantSource is one grant that reaches a tenant, with the subjects
// it names. Jobs is nil when the grant narrows nothing: an empty list and
// an absent list mean different things to a reader.
type TenantGrantSource struct {
	Source   string
	Binding  string
	RoleRef  string
	Subjects []v1alpha1.SubjectRef
	Verbs    []string
	Jobs     []string
}

// TenantItemRoleName returns the generated Jenkins item role name of a
// tenant. The name comes from the tenant name alone, so a caller resolves
// it without reading a JenkinsRole object.
func TenantItemRoleName(team string) string {
	return "varroa:tenant:" + team
}

// TenantItemRolePattern returns the folder pattern of the generated Jenkins
// item role of a tenant.
func TenantItemRolePattern(team string) string {
	return "^" + regexp.QuoteMeta(team) + "($|/.*)$"
}

// TenantMemberRoleName returns the generated Jenkins item role name that
// grants a tenant's write holders the build and replay permissions on the
// tenant folder subtree. A narrowed grant appends its binding name.
//
// No name this package generates may equal TenantItemRoleName of a team:
// the plugin's VarroaTenantResolver looks that exact name up, and a
// collision would hand a member role to the service identity. Team names
// hold no ":", so the "tenant-member:" prefix cannot collide.
func TenantMemberRoleName(team string) string {
	return "varroa:tenant-member:" + team
}

// TenantMemberRolePattern returns the folder pattern of the member role of
// a team that a cluster-wide or Team-membership write grant produces. It
// covers the tenant folder itself and every job under it.
func TenantMemberRolePattern(team string) string {
	return TenantItemRolePattern(team)
}

// TenantMemberNarrowedRoleName returns the member role name of one
// tenant-scoped binding that narrows the grant to job globs.
func TenantMemberNarrowedRoleName(team, binding string) string {
	return "varroa:tenant-member:" + team + ":" + binding
}

// TenantMemberNarrowedPattern returns the folder pattern of a narrowed
// member role. Each job glob is folder-relative, so the pattern joins the
// team name and the glob body.
func TenantMemberNarrowedPattern(team string, jobs []string) string {
	bodies := make([]string, 0, len(jobs))
	for _, g := range jobs {
		bodies = append(bodies, jobglob.RegexpBody(g))
	}
	return "^" + regexp.QuoteMeta(team) + "/(?:" + strings.Join(bodies, "|") + ")$"
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

// tenantMemberNarrowing collects the subjects and job globs of one binding
// that narrows a tenant, across every scope entry the binding names.
type tenantMemberNarrowing struct {
	subjects []v1alpha1.SubjectRef
	jobs     []string
}

// TenantGrantSources lists every grant that reaches one pipelines tenant.
// It is the reverse of EffectiveTenantCapabilities: it walks the same three
// paths with the subject filter removed, so it takes no claims.
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
// tenant, sorted, with no cap. Callers that generate Jenkins grants use it,
// because a cut list would silently drop a subject from a Jenkins role.
func (r *Resolver) allTenantGrantSources(tenant ParsedTenant) []TenantGrantSource {
	if r == nil || tenant.Kind != TenantTeam || tenant.Team == "" {
		return nil
	}

	var out []TenantGrantSource
	if g, ok := r.teamMembershipGrantSource(tenant.Team); ok {
		out = append(out, g)
	}
	out = append(out, r.bindingGrantSources(tenant.Team)...)

	sort.SliceStable(out, func(i, j int) bool {
		if sourceRank[out[i].Source] != sourceRank[out[j].Source] {
			return sourceRank[out[i].Source] < sourceRank[out[j].Source]
		}
		return out[i].Binding < out[j].Binding
	})
	return out
}

// teamMembershipGrantSource resolves the Team-membership path. It applies
// the rules of the forward walk: the managed Group counts only when
// status.groupRef names it and TeamRBACReady is true, an "admin" roleRef
// yields no grant, and an empty roleRef means "developer".
func (r *Resolver) teamMembershipGrantSource(teamName string) (TenantGrantSource, bool) {
	if r.teamLister == nil {
		return TenantGrantSource{}, false
	}
	obj, err := r.teamLister.Get(teamName)
	if err != nil {
		return TenantGrantSource{}, false
	}
	team, ok := obj.(*v1alpha1.Team)
	if !ok {
		return TenantGrantSource{}, false
	}

	role := team.Spec.RoleRef
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

	subjects := teamEffectiveSubjects(team)
	if len(subjects) == 0 {
		return TenantGrantSource{}, false
	}
	return TenantGrantSource{
		Source:   GrantSourceTeamMembership,
		RoleRef:  role,
		Subjects: subjects,
		Verbs:    grantedPipelinesVerbs(caps),
	}, true
}

// teamEffectiveSubjects returns the Team's effective subject set: the
// managed Group, when status.groupRef names it and TeamRBACReady is true,
// plus every spec.subjects entry. spec.members is never a subject: those
// usernames belong to the managed Group.
func teamEffectiveSubjects(team *v1alpha1.Team) []v1alpha1.SubjectRef {
	var out []v1alpha1.SubjectRef
	wantGroupRef := "team-" + team.Name
	if team.Status.GroupRef == wantGroupRef && teamRBACReady(team) {
		out = append(out, v1alpha1.SubjectRef{Kind: "Group", Name: wantGroupRef})
	}
	out = append(out, team.Spec.Subjects...)
	return out
}

// bindingGrantSources resolves the tenant-scoped and cluster-wide binding
// paths over every stored VarroaRoleBinding. It keeps one entry for each
// matching scope.tenants entry, so two bindings that narrow differently
// stay separate.
func (r *Resolver) bindingGrantSources(team string) []TenantGrantSource {
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
			if t.Name != team {
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
