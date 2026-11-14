package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
)

// Kinds of the objects whose reconcilers write a VarroaRoleBinding.
const (
	BindingOwnerTenantAgent   = "TenantAgent"
	BindingOwnerBroodSchedule = "BroodSchedule"
)

// BindingOwner names the object whose reconciler writes a VarroaRoleBinding.
// It is not a CRD type and needs no DeepCopy.
type BindingOwner struct {
	// Kind is BindingOwnerTenantAgent or BindingOwnerBroodSchedule.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// TenantAgentBindingName returns the name of the VarroaRoleBinding that the
// operator writes for the TenantAgent namespace/name. The hash keeps the name
// short and valid for any namespace and name.
func TenantAgentBindingName(namespace, name string) string {
	return "tenantagent-" + ownerHash(namespace, name)[:16]
}

// BroodScheduleBindingName returns the name of the VarroaRoleBinding that the
// operator writes for the BroodSchedule namespace/name. The name has 62
// characters for any input.
func BroodScheduleBindingName(namespace, name string) string {
	return "broodschedule-" + ownerHash(namespace, name)[:48]
}

// ownerHash returns the hex SHA-256 of namespace/name. A slash occurs in
// neither part, so each object gets its own hash.
func ownerHash(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return hex.EncodeToString(sum[:])
}

// VarroaRoleBindingOwner returns the TenantAgent or the BroodSchedule whose
// reconciler writes b. The labels of a TenantAgent binding or the annotations
// of a BroodSchedule binding name the object. They are not proof, because a
// copy of a generated binding keeps them. The name must also be the derived
// name of that object. Every consumer calls this function. Do not copy the rule.
func VarroaRoleBindingOwner(b *VarroaRoleBinding) (BindingOwner, bool) {
	if b == nil {
		return BindingOwner{}, false
	}
	if b.Labels[LabelTenantAgent] == "true" {
		ns, name := b.Labels[LabelTenantAgentNamespace], b.Labels[LabelTenantAgentName]
		if ns != "" && name != "" && b.Name == TenantAgentBindingName(ns, name) {
			return BindingOwner{Kind: BindingOwnerTenantAgent, Namespace: ns, Name: name}, true
		}
	}
	ns, name := b.Annotations[AnnotationBroodScheduleNamespace], b.Annotations[AnnotationBroodScheduleName]
	if ns != "" && name != "" && b.Name == BroodScheduleBindingName(ns, name) {
		return BindingOwner{Kind: BindingOwnerBroodSchedule, Namespace: ns, Name: name}, true
	}
	return BindingOwner{}, false
}
