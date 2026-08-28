package api

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// deprovisionUser strips a user from all VarroaRoleBindings and
// JenkinsRoleBindings, deletes bindings left empty, and removes the
// user from Group.Members[] in local mode.
func deprovisionUser(ctx context.Context, store crdstore.Backend, user *v1alpha1.User, mode string) error {
	// Compute the set of identifiers that may appear in a binding's
	// SubjectRef.Name for this user: subject, username, preferred_username, and
	// email (a binding may reference any of these per --oidc-user-claim, default
	// preferred_username,sub). Display name is deliberately excluded — it is a
	// human label, never a binding subject, and would risk over-matching.
	identifiers := map[string]bool{}
	identifiers[user.Name] = true // local username; harmless oidc-hash for OIDC
	if sub := user.Annotations[v1alpha1.AnnotationOIDCSubject]; sub != "" {
		identifiers[sub] = true
	}
	if pu := user.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; pu != "" {
		identifiers[pu] = true
	}
	if user.Spec.Email != "" {
		identifiers[user.Spec.Email] = true
	}

	// --- VarroaRoleBindings (control-plane) ---
	vrbList, err := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, store, "", "")
	if err != nil {
		return fmt.Errorf("list varroa role bindings: %w", err)
	}
	for _, rb := range vrbList {
		remaining := filterSubjects(rb.Spec.Subjects, identifiers)
		if len(remaining) < len(rb.Spec.Subjects) {
			if len(remaining) == 0 {
				if err := crdstore.Delete[v1alpha1.VarroaRoleBinding](ctx, store, rb.Name, ""); err != nil {
					return fmt.Errorf("delete emptied varroa role binding %s: %w", rb.Name, err)
				}
			} else {
				rb.Spec.Subjects = remaining
				resetObjectMetaForApply(&rb.ObjectMeta)
				if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](ctx, store, rb); err != nil {
					return fmt.Errorf("update varroa role binding %s: %w", rb.Name, err)
				}
			}
		}
	}

	// --- JenkinsRoleBindings (data-plane) ---
	jrbList, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, store, "", "")
	if err != nil {
		return fmt.Errorf("list jenkins role bindings: %w", err)
	}
	for _, rb := range jrbList {
		remaining := filterSubjects(rb.Spec.Subjects, identifiers)
		if len(remaining) < len(rb.Spec.Subjects) {
			if len(remaining) == 0 {
				if err := crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, store, rb.Name, ""); err != nil {
					return fmt.Errorf("delete emptied jenkins role binding %s: %w", rb.Name, err)
				}
			} else {
				rb.Spec.Subjects = remaining
				resetObjectMetaForApply(&rb.ObjectMeta)
				if err := crdstore.Apply[v1alpha1.JenkinsRoleBinding](ctx, store, rb); err != nil {
					return fmt.Errorf("update jenkins role binding %s: %w", rb.Name, err)
				}
			}
		}
	}

	// --- Group membership (local mode only) ---
	if mode == string(auth.AuthModeLocal) {
		groupList, err := crdstore.List[v1alpha1.Group](ctx, store, "", "")
		if err != nil {
			return fmt.Errorf("list groups: %w", err)
		}
		for _, g := range groupList {
			newMembers := removeString(g.Spec.Members, user.Name)
			if len(newMembers) < len(g.Spec.Members) {
				g.Spec.Members = newMembers
				resetObjectMetaForApply(&g.ObjectMeta)
				if err := crdstore.Apply[v1alpha1.Group](ctx, store, g); err != nil {
					return fmt.Errorf("update group %s: %w", g.Name, err)
				}
			}
		}
	}

	return nil
}

// filterSubjects returns a copy of subjects with entries matching any
// of the given identifiers removed. Only Kind:"User" subjects are filtered.
func filterSubjects(subjects []v1alpha1.SubjectRef, identifiers map[string]bool) []v1alpha1.SubjectRef {
	if len(subjects) == 0 {
		return subjects
	}
	var filtered []v1alpha1.SubjectRef
	for _, s := range subjects {
		if s.Kind == "User" && identifiers[s.Name] {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

// resetObjectMetaForApply clears fields that would cause a "resourceVersion
// should not be set on objects to be created" error when an object retrieved
// from a List/Get is passed to an Apply (create-or-update) method.
func resetObjectMetaForApply(meta *metav1.ObjectMeta) {
	meta.ResourceVersion = ""
	meta.UID = ""
	meta.Generation = 0
	meta.CreationTimestamp = metav1.Time{}
	meta.DeletionTimestamp = nil
	meta.DeletionGracePeriodSeconds = nil
	meta.ManagedFields = nil
}

// removeString removes the first occurrence of target from slice and
// returns the result. It preserves order of remaining elements.
func removeString(slice []string, target string) []string {
	for i, s := range slice {
		if s == target {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}
