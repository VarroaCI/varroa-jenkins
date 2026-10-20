package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/userdirectory"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ErrUserNotFound reports that the User to delete does not exist.
var ErrUserNotFound = errors.New("user not found")

// DeleteUser strips the user from every role binding and, for a local
// account, from every group. Then it deletes the User object. Every delete
// surface calls this function, so no surface leaves a binding behind.
func DeleteUser(ctx context.Context, store crdstore.Backend, namespace, name, mode string) error {
	user, err := crdstore.Get[v1alpha1.User](ctx, store, name, namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %v", ErrUserNotFound, err)
		}
		return fmt.Errorf("get user: %w", err)
	}
	if err := deprovisionUser(ctx, store, user, mode); err != nil {
		return fmt.Errorf("deprovision failed: %w", err)
	}
	if err := crdstore.Delete[v1alpha1.User](ctx, store, name, namespace); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

// deprovisionUser strips a user from all VarroaRoleBindings and
// JenkinsRoleBindings, deletes bindings left empty, and removes a local
// account from Group.Members[].
func deprovisionUser(ctx context.Context, store crdstore.Backend, user *v1alpha1.User, mode string) error {
	localAccount := mode == string(auth.AuthModeLocal) ||
		user.Labels[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByLocal
	// Compute the set of identifiers that may appear in a binding's
	// SubjectRef.Name for this user: subject, username, preferred_username, and
	// email (a binding may reference any of these per --oidc-user-claim, default
	// preferred_username,sub), plus the custom claim values stored at login.
	// Display name is deliberately excluded — it is a human label, never a
	// binding subject, and would risk over-matching. Former preferred usernames,
	// emails, and custom claim values count too, so a renamed person loses every
	// binding that names an old identifier.
	identifiers := map[string]bool{}
	identifiers[user.Name] = true // local username; harmless oidc-hash for OIDC
	if localAccount && mode != string(auth.AuthModeLocal) {
		// A local account beside OIDC is bound as "local:<name>".
		identifiers[auth.LocalSubjectPrefix+user.Name] = true
	}
	if sub := user.Annotations[v1alpha1.AnnotationOIDCSubject]; sub != "" {
		identifiers[sub] = true
	}
	if pu := user.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; pu != "" {
		identifiers[pu] = true
	}
	if user.Spec.Email != "" {
		identifiers[user.Spec.Email] = true
	}
	for _, v := range userdirectory.UserClaims(user) {
		identifiers[v] = true
	}
	for _, v := range userdirectory.FormerIdentifiers(user) {
		identifiers[v] = true
	}
	// An identifier that another live user holds stays in the bindings. The
	// departing user cannot own a value that a signed-in person now holds.
	held, err := liveIdentifierHolders(ctx, store, user)
	if err != nil {
		return err
	}
	for v := range identifiers {
		if owner, ok := held[v]; ok {
			slog.Default().Info(fmt.Sprintf("deprovision kept %s: held by %s", v, owner))
			delete(identifiers, v)
		}
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

	// --- Group membership (local accounts only) ---
	if localAccount {
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

// liveIdentifierHolders maps each current preferred username, email, and
// subject, custom user claim value of the other idp-managed users in the namespace to
// the holder's User name. The user being deprovisioned does not count.
func liveIdentifierHolders(ctx context.Context, store crdstore.Backend, self *v1alpha1.User) (map[string]string, error) {
	users, err := crdstore.List[v1alpha1.User](ctx, store, self.Namespace, "")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	held := map[string]string{}
	for _, u := range users {
		if u.Name == self.Name || u.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByIDP {
			continue
		}
		if sub := u.Annotations[v1alpha1.AnnotationOIDCSubject]; sub != "" {
			held[sub] = u.Name
		}
		if pu := u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; pu != "" {
			held[pu] = u.Name
		}
		if u.Spec.Email != "" {
			held[u.Spec.Email] = u.Name
		}
		for _, v := range userdirectory.UserClaims(u) {
			held[v] = u.Name
		}
	}
	return held, nil
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
