package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
)

// RBACSpecRoles returns the roles that RBACGroupBinding.Role accepts. The Enum
// marker on RBACGroupBinding.Role and the built-in role table of the operator
// must list the same roles. Tests compare all three.
func RBACSpecRoles() []string {
	return []string{"admin", "operator", "developer", "viewer"}
}

// RBACSpecBindingName returns the name of the JenkinsRoleBinding that the
// operator writes for one rbacSpec role of the controller namespace/name. A
// slash occurs in neither part, so the hash gives each controller its own
// names. The name has at most 31 characters for any input.
func RBACSpecBindingName(namespace, name, role string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return "ctrl-" + hex.EncodeToString(sum[:])[:16] + "-" + role
}

// RBACSpecBindingOwner returns the controller whose spec.rbacSpec writes b.
// The labels name the controller. They are not proof, because a copy of a
// generated binding keeps them. The name must also be the RBACSpecBindingName
// of that controller for a role in RBACSpecRoles, so the earlier dash-joined
// name has no owner. Every consumer calls this function. Do not copy the rule.
func RBACSpecBindingOwner(b *JenkinsRoleBinding) (namespace, name string, ok bool) {
	if b == nil || b.Labels[LabelManagedBy] != ManagedByOperator {
		return "", "", false
	}
	namespace = b.Labels[LabelControllerNamespace]
	name = b.Labels[LabelControllerName]
	if namespace == "" || name == "" {
		return "", "", false
	}
	for _, role := range RBACSpecRoles() {
		if b.Name == RBACSpecBindingName(namespace, name, role) {
			return namespace, name, true
		}
	}
	return "", "", false
}
