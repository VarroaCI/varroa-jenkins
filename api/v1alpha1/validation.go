package v1alpha1

import (
	"fmt"
	"strings"
)

// ValidateJenkinsRole validates a JenkinsRole spec.
func ValidateJenkinsRole(r *JenkinsRole) error {
	if r == nil {
		return fmt.Errorf("jenkins role is nil")
	}
	if r.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}

	spec := r.Spec
	switch spec.RoleType {
	case "Global", "Item", "Agent", "":
		// valid (empty defaults to Global per CRD schema)
	default:
		return fmt.Errorf("spec.roleType must be one of Global, Item, Agent (got %q)", spec.RoleType)
	}

	if len(spec.Permissions) == 0 {
		return fmt.Errorf("spec.permissions must have at least one entry")
	}
	for i, p := range spec.Permissions {
		if p == "" {
			return fmt.Errorf("spec.permissions[%d] is empty", i)
		}
		// Permission must be a dot-separated identifier, e.g. "hudson.model.Item.Read"
		if strings.Count(p, ".") < 1 {
			return fmt.Errorf("spec.permissions[%d] %q: must be a qualified permission (e.g. hudson.model.Item.Read)", i, p)
		}
	}

	return nil
}

// ValidateJenkinsRoleBinding validates a JenkinsRoleBinding spec.
func ValidateJenkinsRoleBinding(b *JenkinsRoleBinding) error {
	if b == nil {
		return fmt.Errorf("jenkins role binding is nil")
	}
	if b.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if b.Spec.RoleRef == "" {
		return fmt.Errorf("spec.roleRef is required")
	}
	if len(b.Spec.Subjects) == 0 {
		return fmt.Errorf("spec.subjects must have at least one entry")
	}
	for i, s := range b.Spec.Subjects {
		if s.Name == "" {
			return fmt.Errorf("spec.subjects[%d].name is required", i)
		}
		switch s.Kind {
		case "User", "Group":
			// valid
		default:
			return fmt.Errorf("spec.subjects[%d].kind must be User or Group (got %q)", i, s.Kind)
		}
	}

	// Validate JenkinsScope if present.
	if js := b.Spec.JenkinsScope; js != nil {
		switch js.Type {
		case "Global", "Folder", "Pattern", "":
			// valid (empty = Global)
		default:
			return fmt.Errorf("spec.jenkinsScope.type must be Global, Folder, or Pattern (got %q)", js.Type)
		}
	}

	return nil
}
