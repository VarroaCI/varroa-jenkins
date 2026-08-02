package userdirectory

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// UserStore is the subset of controller.ResourceClient that RecordLogin needs.
// Status fields (lastLogin, observedGroups) MUST be written via PatchUserStatus
// because the User CRD has a status subresource — ApplyUserCRD (Create/Update on
// the main resource) silently discards the status stanza.
type UserStore interface {
	GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error)
	ApplyUserCRD(ctx context.Context, user *v1alpha1.User) error
	PatchUserStatus(ctx context.Context, name, namespace string, status *v1alpha1.UserStatus) error
}

// RecordLogin creates or refreshes an idp-managed User CRD from OIDC
// claims on every login. It never modifies a local-managed user.
//
// Identity/metadata (email, display name, labels, annotations) are written via
// ApplyUserCRD; observed status (last-login, observed groups) is written via
// PatchUserStatus against the status subresource. Both run on every login so
// first login provisions and subsequent logins refresh.
func RecordLogin(ctx context.Context, store UserStore, userName, email, displayName string, groups []string, subject, preferredUsername, namespace string) error {
	if userName == "" || namespace == "" {
		return fmt.Errorf("record login: userName and namespace are required")
	}

	now := metav1.Now()
	annotations := map[string]string{}
	if subject != "" {
		annotations[v1alpha1.AnnotationOIDCSubject] = subject
	}
	if preferredUsername != "" {
		annotations[v1alpha1.AnnotationOIDCPreferredUsername] = preferredUsername
	}

	existing, err := store.GetUserCRD(ctx, userName, namespace)
	if err == nil && existing != nil {
		// Never modify local-managed users.
		if existing.Labels != nil && existing.Labels[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByLocal {
			return nil
		}

		// Refresh claim-derived identity/metadata.
		existing.Spec.Email = email
		existing.Spec.DisplayName = displayName
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[v1alpha1.LabelManagedBy] = v1alpha1.ManagedByIDP
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for k, v := range annotations {
			existing.Annotations[k] = v
		}
		if err := store.ApplyUserCRD(ctx, existing); err != nil {
			return fmt.Errorf("update user %s: %w", userName, err)
		}
		return patchObservedStatus(ctx, store, userName, namespace, &now, groups)
	}

	// First login: create the idp-managed user (spec + metadata only — status
	// is written separately below because of the status subresource).
	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      userName,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP,
			},
			Annotations: annotations,
		},
		Spec: v1alpha1.UserSpec{
			Email:       email,
			DisplayName: displayName,
		},
	}
	if err := store.ApplyUserCRD(ctx, user); err != nil {
		return fmt.Errorf("create user %s: %w", userName, err)
	}
	return patchObservedStatus(ctx, store, userName, namespace, &now, groups)
}

// patchObservedStatus writes the observed status fields via the status
// subresource. The merge patch preserves other status fields (e.g. preferences);
// observedGroups uses omitempty, so an empty group list leaves any prior list in
// place rather than clearing it — an accepted limitation of login-driven sync.
func patchObservedStatus(ctx context.Context, store UserStore, userName, namespace string, lastLogin *metav1.Time, groups []string) error {
	if err := store.PatchUserStatus(ctx, userName, namespace, &v1alpha1.UserStatus{
		LastLogin:      lastLogin,
		ObservedGroups: groups,
	}); err != nil {
		return fmt.Errorf("patch user status %s: %w", userName, err)
	}
	return nil
}
