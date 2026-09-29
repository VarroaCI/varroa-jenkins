package rbac

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// Skip reason codes a BindingOutcome carries. Each one applies to every hive,
// because the projection is the same for every hive.
const (
	SkipNoSubjects           = "NoSubjects"
	SkipTenantScope          = "TenantScope"
	SkipRoleNotFound         = "RoleNotFound"
	SkipNoJenkinsProjection  = "NoJenkinsProjection"
	SkipJenkinsRoleNotFound  = "JenkinsRoleNotFound"
	SkipNonGlobalJenkinsRole = "NonGlobalJenkinsRole"
	SkipReservedRole         = "ReservedRole"
)

// BindingOutcome is the projection result for one source VarroaRoleBinding.
// A skipped binding sets SkipReason and SkipMessage. A projected binding sets
// RoleName and BindingName to its desired JenkinsRole and JenkinsRoleBinding.
type BindingOutcome struct {
	SkipReason  string
	SkipMessage string
	RoleName    string
	BindingName string
}

// DesiredFederatedCRs projects core VarroaRole bindings into Jenkins RBAC CRs
// that should exist on every non-core hive cluster. The outcomes map holds one
// entry for each named source binding.
func DesiredFederatedCRs(
	varroaRoleBindings []*v1alpha1.VarroaRoleBinding,
	getVarroaRole func(string) (*v1alpha1.VarroaRole, bool),
	getCoreJenkinsRole func(string) (*v1alpha1.JenkinsRole, bool),
) (roles []*v1alpha1.JenkinsRole, bindings []*v1alpha1.JenkinsRoleBinding, warnings []string, outcomes map[string]BindingOutcome) {
	roleByName := make(map[string]*v1alpha1.JenkinsRole)
	bindingByName := make(map[string]bool)
	ownersByChild := make(map[string][]string)
	outcomes = make(map[string]BindingOutcome)
	skip := func(vrb *v1alpha1.VarroaRoleBinding, reason, message string) {
		outcomes[vrb.Name] = BindingOutcome{SkipReason: reason, SkipMessage: message}
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
		if vrb.Spec.Scope != nil && len(vrb.Spec.Scope.Tenants) > 0 {
			// controllerScope on the federated JenkinsRoleBinding is copied
			// verbatim from this scope, and a hive has no tenant resolution
			// to evaluate a tenants entry with. A tenant grant stays core-only.
			warnings = append(warnings, fmt.Sprintf("varroarolebinding %s scope carries tenants; skipped", vrb.Name))
			skip(vrb, SkipTenantScope, "a tenant scope stays on the core cluster")
			continue
		}

		vr, ok := getVarroaRole(vrb.Spec.RoleRef)
		if !ok || vr == nil {
			skip(vrb, SkipRoleNotFound, fmt.Sprintf("VarroaRole %s not found", vrb.Spec.RoleRef))
			continue
		}

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
		default:
			skip(vrb, SkipNoJenkinsProjection, fmt.Sprintf("VarroaRole %s grants no Jenkins permissions", vr.Name))
			continue
		}

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

		childName := fedBindingName(vr, vrb)
		outcomes[vrb.Name] = BindingOutcome{RoleName: roleName, BindingName: childName}
		ownersByChild[childName] = append(ownersByChild[childName], vrb.Name)
		// Bindings with the same role, scope, and subjects hash to one child
		// name. The child is projected once, so no two desired bindings share
		// a name.
		if bindingByName[childName] {
			continue
		}
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

	roleNames := make([]string, 0, len(roleByName))
	for name := range roleByName {
		roleNames = append(roleNames, name)
	}
	sort.Strings(roleNames)
	for _, name := range roleNames {
		roles = append(roles, roleByName[name])
	}
	for _, binding := range bindings {
		owners := ownersByChild[binding.Name]
		sort.Strings(owners)
		binding.Annotations = map[string]string{AnnotationFederatedBindings: strings.Join(owners, ",")}
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
