package rbac

import (
	"regexp"
	"sort"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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
	if r == nil || tenant.Kind != TenantTeam || tenant.Team == "" {
		return nil, false
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
	if len(out) > MaxTenantGrants {
		return out[:MaxTenantGrants], true
	}
	return out, false
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
