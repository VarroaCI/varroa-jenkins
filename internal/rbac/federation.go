package rbac

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// LabelFederatedFrom marks a JenkinsRole/JenkinsRoleBinding as federation-owned
// (value = the source VarroaRole name); it drives GC and collision detection so
// hand-authored RBAC of the same name is never overwritten.
const LabelFederatedFrom = "varroa.dev/federated-from"

// AnnotationFederatedBindings names the source VarroaRoleBindings of a
// federated JenkinsRoleBinding: sorted and comma-separated. One child can
// serve several source bindings, and a label value holds at most 63
// characters, so the names go in an annotation.
const AnnotationFederatedBindings = "varroa.dev/federated-bindings"

// AnnotationFederatedRoles names the source VarroaRoles of a federated
// JenkinsRole: sorted and comma-separated. It names only the VarroaRoles that
// have at least one projected binding. Several VarroaRoles can share one
// jenkinsRoleRef, and the label names only one of them.
const AnnotationFederatedRoles = "varroa.dev/federated-roles"

// Skip reason codes a BindingOutcome carries. Each one applies to every hive,
// because the projection is the same for every hive.
const (
	SkipNoSubjects           = "NoSubjects"
	SkipNoTenantWrite        = "NoTenantWrite"
	SkipNoTenantJobs         = "NoTenantJobs"
	SkipRoleNotFound         = "RoleNotFound"
	SkipNoJenkinsProjection  = "NoJenkinsProjection"
	SkipJenkinsRoleNotFound  = "JenkinsRoleNotFound"
	SkipNonGlobalJenkinsRole = "NonGlobalJenkinsRole"
	SkipReservedRole         = "ReservedRole"
)

// LabelFederatedTenantMember marks a federated JenkinsRoleBinding that carries
// a tenant-scoped grant ("true"). A hive holds one copy for each tenant the
// grant names, and one all-tenants copy for a cluster-wide pipelines write
// grant. A member cell reads the copies for its own tenant only, and the
// generic JenkinsRoleBinding collector ignores them.
const LabelFederatedTenantMember = "varroa.dev/federated-tenant-member"

// AnnotationFederatedTenant names the tenant of a tenant-member copy. The
// value FederatedAllTenants marks a copy that reaches every tenant on a cell.
const AnnotationFederatedTenant = "varroa.dev/federated-tenant"

// FederatedAllTenants is the tenant annotation value of a copy that reaches
// every tenant on a cell. It cannot be a Team name.
const FederatedAllTenants = "*"

// AnnotationFederatedTenantJobs holds the job globs of a tenant-member copy as
// a JSON array of strings. An absent annotation means the whole tenant folder.
const AnnotationFederatedTenantJobs = "varroa.dev/federated-tenant-jobs"

// BindingOutcome is the projection result for one source VarroaRoleBinding.
// A skipped binding sets SkipReason and SkipMessage. A projected binding sets
// RoleName to its desired JenkinsRole and BindingNames to the desired
// JenkinsRoleBindings, in sorted order. A binding with tenants can project
// one JenkinsRoleBinding for each tenant.
type BindingOutcome struct {
	SkipReason   string
	SkipMessage  string
	RoleName     string
	BindingNames []string
}

// DesiredFederatedCRs projects core VarroaRole bindings into Jenkins RBAC CRs
// that should exist on every non-core hive cluster. The outcomes map holds one
// entry for each named source binding. The expandSubjects function, when set,
// rewrites the subjects of a tenant-member copy before the copy is stored. It
// is nil when the caller needs no expansion.
func DesiredFederatedCRs(
	varroaRoleBindings []*v1alpha1.VarroaRoleBinding,
	getVarroaRole func(string) (*v1alpha1.VarroaRole, bool),
	getCoreJenkinsRole func(string) (*v1alpha1.JenkinsRole, bool),
	expandSubjects func([]v1alpha1.SubjectRef) []v1alpha1.SubjectRef,
) (roles []*v1alpha1.JenkinsRole, bindings []*v1alpha1.JenkinsRoleBinding, warnings []string, outcomes map[string]BindingOutcome) {
	roleByName := make(map[string]*v1alpha1.JenkinsRole)
	bindingByName := make(map[string]bool)
	ownersByChild := make(map[string][]string)
	sourcesByRole := make(map[string]map[string]bool)
	outcomes = make(map[string]BindingOutcome)
	skip := func(vrb *v1alpha1.VarroaRoleBinding, reason, message string) {
		outcomes[vrb.Name] = BindingOutcome{SkipReason: reason, SkipMessage: message}
	}
	expand := func(in []v1alpha1.SubjectRef) []v1alpha1.SubjectRef {
		if expandSubjects == nil {
			return in
		}
		return expandSubjects(in)
	}

	for _, vrb := range varroaRoleBindings {
		// A binding with no subjects grants nothing, and the JenkinsRoleBinding
		// CRD requires a subjects list. A Team with no members is a normal
		// state, so the skip comes before every check that warns. The skip
		// also comes before the role is recorded: a role that only empty
		// bindings reference is not federated.
		if vrb == nil {
			continue
		}
		if len(vrb.Spec.Subjects) == 0 {
			skip(vrb, SkipNoSubjects, "binding has no subjects")
			continue
		}
		vr, ok := getVarroaRole(vrb.Spec.RoleRef)
		if !ok || vr == nil {
			skip(vrb, SkipRoleNotFound, fmt.Sprintf("VarroaRole %s not found", vrb.Spec.RoleRef))
			continue
		}

		if vrb.Spec.Scope != nil && len(vrb.Spec.Scope.Tenants) > 0 {
			// A tenant grant never becomes a controllerScope: a hive has no
			// tenant resolution. The hive gets one copy for each tenant
			// instead. A member cell reads the copy for its own tenant only.
			if !slices.Contains(VarroaRolePipelinesVerbs(vr), "write") {
				skip(vrb, SkipNoTenantWrite, "a tenant grant without pipelines write stays on the core cluster")
				continue
			}
			var names []string
			grants := tenantJobsByTeam(vrb.Spec.Scope.Tenants)
			for _, team := range sortedKeys(grants) {
				grant := grants[team]
				if !grant.whole && len(grant.jobs) == 0 {
					continue
				}
				childName := fedTenantBindingName(vr, vrb, team, grant.jobs, grant.whole)
				names = append(names, childName)
				if bindingByName[childName] {
					continue
				}
				bindingByName[childName] = true
				annotations := map[string]string{AnnotationFederatedTenant: team}
				if !grant.whole {
					raw, _ := json.Marshal(grant.jobs)
					annotations[AnnotationFederatedTenantJobs] = string(raw)
				}
				bindings = append(bindings, &v1alpha1.JenkinsRoleBinding{
					TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRoleBinding"},
					ObjectMeta: metav1.ObjectMeta{
						Name: childName,
						Labels: map[string]string{
							LabelFederatedFrom:         vr.Name,
							LabelFederatedTenantMember: "true",
						},
						Annotations: annotations,
					},
					Spec: v1alpha1.JenkinsRoleBindingSpec{
						RoleRef:  TenantMemberRoleName(team),
						Subjects: copySubjects(expand(vrb.Spec.Subjects)),
					},
				})
			}
			if len(names) == 0 {
				skip(vrb, SkipNoTenantJobs, "a tenant grant names no job and no folder")
				continue
			}
			for _, name := range names {
				ownersByChild[name] = append(ownersByChild[name], vrb.Name)
			}
			outcomes[vrb.Name] = BindingOutcome{BindingNames: names}
			continue
		}

		// A cluster-wide grant with pipelines write also reaches every tenant
		// on a member cell. Its Global projection is optional: the all-tenants
		// copy below is written even when the role has no Jenkins projection.
		memberWrite := vrb.Spec.Scope == nil && slices.Contains(VarroaRolePipelinesVerbs(vr), "write")

		roleName := ""
		var roleSpec v1alpha1.JenkinsRoleSpec
		switch {
		case vr.Spec.JenkinsRoleRef != "":
			jr, ok := getCoreJenkinsRole(vr.Spec.JenkinsRoleRef)
			if !ok || jr == nil {
				warnings = append(warnings, fmt.Sprintf("varroarole %s references missing JenkinsRole %s", vr.Name, vr.Spec.JenkinsRoleRef))
				skip(vrb, SkipJenkinsRoleNotFound, fmt.Sprintf("JenkinsRole %s not found", vr.Spec.JenkinsRoleRef))
				continue
			}
			if jr.Spec.RoleType != "" && jr.Spec.RoleType != "Global" {
				warnings = append(warnings, fmt.Sprintf("varroarole %s references non-Global JenkinsRole %s", vr.Name, vr.Spec.JenkinsRoleRef))
				skip(vrb, SkipNonGlobalJenkinsRole, fmt.Sprintf("JenkinsRole %s is not Global", vr.Spec.JenkinsRoleRef))
				continue
			}
			roleName = vr.Spec.JenkinsRoleRef
			roleSpec = jenkinsRoleSpecCopy(jr.Spec)
		case len(vr.Spec.JenkinsPermissions) > 0:
			roleName = vr.Name
			roleSpec = v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: append([]string(nil), vr.Spec.JenkinsPermissions...)}
		case memberWrite:
			// No Global projection. The all-tenants copy below is the only object federated.
		default:
			skip(vrb, SkipNoJenkinsProjection, fmt.Sprintf("VarroaRole %s grants no Jenkins permissions", vr.Name))
			continue
		}

		if roleName != "" {
			// Never project onto the reserved system-mite / system-operator roles:
			// source-1 derivation forms the Jenkins role name as varroa:<roleRef>, so a
			// roleName of "system-mite" or "system-operator" would collide with the
			// resolver's synthesized varroa:system-mite / varroa:system-operator
			// assignment — attaching extra subjects to, or overriding the forced
			// Administer permission of, those reserved machine-identity roles.
			if roleName == "system-mite" || roleName == "system-operator" {
				warnings = append(warnings, fmt.Sprintf("varroarole %s projects to the reserved %s role; skipped", vr.Name, roleName))
				skip(vrb, SkipReservedRole, fmt.Sprintf("role %s is reserved", roleName))
				continue
			}

			if existing, exists := roleByName[roleName]; !exists {
				roleByName[roleName] = &v1alpha1.JenkinsRole{
					TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRole"},
					ObjectMeta: metav1.ObjectMeta{
						Name:   roleName,
						Labels: map[string]string{LabelFederatedFrom: vr.Name},
					},
					Spec: roleSpec,
				}
			} else if vr.Name < existing.Labels[LabelFederatedFrom] {
				// Multiple VarroaRoles can map to one roleName (e.g. two roles sharing
				// a jenkinsRoleRef). Informer iteration order is unspecified, so pin the
				// federated-from label to the lexicographically-smallest source name for
				// a deterministic desired state (avoids reflect.DeepEqual drift churn).
				existing.Labels[LabelFederatedFrom] = vr.Name
			}

			if sourcesByRole[roleName] == nil {
				sourcesByRole[roleName] = map[string]bool{}
			}
			sourcesByRole[roleName][vr.Name] = true
		}

		names := []string{}
		if roleName != "" {
			childName := fedBindingName(vr, vrb)
			names = append(names, childName)
			ownersByChild[childName] = append(ownersByChild[childName], vrb.Name)
			// Bindings with the same role, scope, and subjects hash to one child
			// name. The child is projected once, so no two desired bindings share
			// a name.
			if !bindingByName[childName] {
				bindingByName[childName] = true
				bindings = append(bindings, &v1alpha1.JenkinsRoleBinding{
					TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRoleBinding"},
					ObjectMeta: metav1.ObjectMeta{
						Name:   childName,
						Labels: map[string]string{LabelFederatedFrom: vr.Name},
					},
					Spec: v1alpha1.JenkinsRoleBindingSpec{
						RoleRef:         roleName,
						Subjects:        copySubjects(vrb.Spec.Subjects),
						ControllerScope: vrb.Spec.Scope,
					},
				})
			}
		}
		if memberWrite {
			allName := fedAllTenantsBindingName(vr, vrb)
			names = append(names, allName)
			ownersByChild[allName] = append(ownersByChild[allName], vrb.Name)
			if !bindingByName[allName] {
				bindingByName[allName] = true
				bindings = append(bindings, &v1alpha1.JenkinsRoleBinding{
					TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRoleBinding"},
					ObjectMeta: metav1.ObjectMeta{
						Name: allName,
						Labels: map[string]string{
							LabelFederatedFrom:         vr.Name,
							LabelFederatedTenantMember: "true",
						},
						Annotations: map[string]string{AnnotationFederatedTenant: FederatedAllTenants},
					},
					Spec: v1alpha1.JenkinsRoleBindingSpec{
						RoleRef:  TenantMemberRoleName(FederatedAllTenants),
						Subjects: copySubjects(expand(vrb.Spec.Subjects)),
					},
				})
			}
		}
		slices.Sort(names)
		outcomes[vrb.Name] = BindingOutcome{RoleName: roleName, BindingNames: names}
	}

	roleNames := make([]string, 0, len(roleByName))
	for name := range roleByName {
		roleNames = append(roleNames, name)
	}
	sort.Strings(roleNames)
	for _, name := range roleNames {
		sources := make([]string, 0, len(sourcesByRole[name]))
		for source := range sourcesByRole[name] {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		role := roleByName[name]
		role.Annotations = map[string]string{AnnotationFederatedRoles: strings.Join(sources, ",")}
		roles = append(roles, role)
	}
	for _, binding := range bindings {
		owners := ownersByChild[binding.Name]
		sort.Strings(owners)
		if binding.Annotations == nil {
			binding.Annotations = map[string]string{}
		}
		binding.Annotations[AnnotationFederatedBindings] = strings.Join(owners, ",")
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	sort.Strings(warnings)
	return roles, bindings, warnings, outcomes
}

// copySubjects always returns a non-nil slice. JenkinsRoleBindingSpec.Subjects
// has no omitempty, so a nil slice encodes as null, and the API server rejects
// null as a missing required field.
func copySubjects(in []v1alpha1.SubjectRef) []v1alpha1.SubjectRef {
	out := make([]v1alpha1.SubjectRef, 0, len(in))
	return append(out, in...)
}

func jenkinsRoleSpecCopy(in v1alpha1.JenkinsRoleSpec) v1alpha1.JenkinsRoleSpec {
	in.Permissions = append([]string(nil), in.Permissions...)
	return in
}

func fedBindingName(vr *v1alpha1.VarroaRole, vrb *v1alpha1.VarroaRoleBinding) string {
	payload := struct {
		Scope    *v1alpha1.VarroaRoleBindingScope `json:"scope,omitempty"`
		Subjects []v1alpha1.SubjectRef            `json:"subjects"`
	}{Scope: vrb.Spec.Scope, Subjects: append([]v1alpha1.SubjectRef(nil), vrb.Spec.Subjects...)}
	sort.Slice(payload.Subjects, func(i, j int) bool {
		if payload.Subjects[i].Kind != payload.Subjects[j].Kind {
			return payload.Subjects[i].Kind < payload.Subjects[j].Kind
		}
		return payload.Subjects[i].Name < payload.Subjects[j].Name
	})
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "varroa-fed-" + vr.Name + "-" + hex.EncodeToString(sum[:])[:12]
}

// tenantGrant is the job selection one tenant gets from a tenant-scoped
// binding. whole means the grant covers the whole tenant folder.
type tenantGrant struct {
	jobs  []string
	whole bool
}

// tenantJobsByTeam merges the scope entries of one binding by tenant name.
// An entry without jobs covers the whole folder. Otherwise the jobs of the
// entries are sorted and deduplicated.
func tenantJobsByTeam(tenants []v1alpha1.TenantScope) map[string]tenantGrant {
	out := map[string]tenantGrant{}
	for _, t := range tenants {
		g := out[t.Name]
		if t.Jobs == nil {
			g.whole = true
		}
		g.jobs = append(g.jobs, t.Jobs...)
		out[t.Name] = g
	}
	for team, g := range out {
		slices.Sort(g.jobs)
		g.jobs = slices.Compact(g.jobs)
		out[team] = g
	}
	return out
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fedTenantBindingName names the tenant-member copy of one source binding.
// The name hashes the role, the tenant, the job globs, and the source subjects.
// A change to any of them gives the copy a new name.
func fedTenantBindingName(vr *v1alpha1.VarroaRole, vrb *v1alpha1.VarroaRoleBinding, team string, jobs []string, whole bool) string {
	payload := struct {
		Role     string                `json:"role"`
		Team     string                `json:"team"`
		Jobs     []string              `json:"jobs"`
		Whole    bool                  `json:"whole"`
		Subjects []v1alpha1.SubjectRef `json:"subjects"`
	}{Role: vr.Name, Team: team, Jobs: jobs, Whole: whole, Subjects: sortedSubjects(vrb.Spec.Subjects)}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "varroa-fed-tenant-" + hex.EncodeToString(sum[:])[:12]
}

// fedAllTenantsBindingName names the all-tenants copy of one cluster-wide
// source binding. The name hashes the role and the source subjects.
func fedAllTenantsBindingName(vr *v1alpha1.VarroaRole, vrb *v1alpha1.VarroaRoleBinding) string {
	payload := struct {
		Role       string                `json:"role"`
		AllTenants bool                  `json:"allTenants"`
		Subjects   []v1alpha1.SubjectRef `json:"subjects"`
	}{Role: vr.Name, AllTenants: true, Subjects: sortedSubjects(vrb.Spec.Subjects)}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "varroa-fed-tenant-all-" + hex.EncodeToString(sum[:])[:12]
}

// sortedSubjects returns a sorted copy of the subjects.
func sortedSubjects(in []v1alpha1.SubjectRef) []v1alpha1.SubjectRef {
	out := append([]v1alpha1.SubjectRef(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}
