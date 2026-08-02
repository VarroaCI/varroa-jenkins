package rbac

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// LabelFederatedFrom marks a JenkinsRole/JenkinsRoleBinding as federation-owned
// (value = the source VarroaRole name); it drives GC and collision detection so
// hand-authored RBAC of the same name is never overwritten.
const LabelFederatedFrom = "varroa.dev/federated-from"

// DesiredFederatedCRs projects core VarroaRole bindings into Jenkins RBAC CRs
// that should exist on every non-core hive cluster.
func DesiredFederatedCRs(
	varroaRoleBindings []*v1alpha1.VarroaRoleBinding,
	getVarroaRole func(string) (*v1alpha1.VarroaRole, bool),
	getCoreJenkinsRole func(string) (*v1alpha1.JenkinsRole, bool),
) (roles []*v1alpha1.JenkinsRole, bindings []*v1alpha1.JenkinsRoleBinding, warnings []string) {
	roleByName := make(map[string]*v1alpha1.JenkinsRole)

	for _, vrb := range varroaRoleBindings {
		if vrb == nil {
			continue
		}
		vr, ok := getVarroaRole(vrb.Spec.RoleRef)
		if !ok || vr == nil {
			continue
		}

		roleName := ""
		var roleSpec v1alpha1.JenkinsRoleSpec
		switch {
		case vr.Spec.JenkinsRoleRef != "":
			jr, ok := getCoreJenkinsRole(vr.Spec.JenkinsRoleRef)
			if !ok || jr == nil {
				warnings = append(warnings, fmt.Sprintf("varroarole %s references missing JenkinsRole %s", vr.Name, vr.Spec.JenkinsRoleRef))
				continue
			}
			if jr.Spec.RoleType != "" && jr.Spec.RoleType != "Global" {
				warnings = append(warnings, fmt.Sprintf("varroarole %s references non-Global JenkinsRole %s", vr.Name, vr.Spec.JenkinsRoleRef))
				continue
			}
			roleName = vr.Spec.JenkinsRoleRef
			roleSpec = jenkinsRoleSpecCopy(jr.Spec)
		case len(vr.Spec.JenkinsPermissions) > 0:
			roleName = vr.Name
			roleSpec = v1alpha1.JenkinsRoleSpec{RoleType: "Global", Permissions: append([]string(nil), vr.Spec.JenkinsPermissions...)}
		default:
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

		bindings = append(bindings, &v1alpha1.JenkinsRoleBinding{
			TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "JenkinsRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{
				Name:   fedBindingName(vr, vrb),
				Labels: map[string]string{LabelFederatedFrom: vr.Name},
			},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:         roleName,
				Subjects:        append([]v1alpha1.SubjectRef(nil), vrb.Spec.Subjects...),
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
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	sort.Strings(warnings)
	return roles, bindings, warnings
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
