package userdirectory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// UserStore is the subset of controller.ResourceClient that RecordLogin needs.
// Status fields (lastLogin, observedGroups) MUST be written via PatchUserStatus
// because the User CRD has a status subresource. CreateUserCRD and UpdateUserCRD
// write the main resource, and they discard the status stanza.
type UserStore interface {
	GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error)
	// CreateUserCRD creates the User. An existing object answers AlreadyExists.
	CreateUserCRD(ctx context.Context, user *v1alpha1.User) error
	// UpdateUserCRD writes the User with the resourceVersion it carries. A write since that version answers Conflict.
	UpdateUserCRD(ctx context.Context, user *v1alpha1.User) error
	PatchUserStatus(ctx context.Context, name, namespace string, status *v1alpha1.UserStatus) error
}

// RecordLogin creates or refreshes an idp-managed User CRD from OIDC
// claims on every login. It never modifies a local-managed user.
//
// Identity/metadata (email, display name, labels, annotations) are written via
// CreateUserCRD or UpdateUserCRD; observed status (last-login, observed groups)
// is written via PatchUserStatus against the status subresource. Both run on
// every login so first login provisions and subsequent logins refresh. extraClaims
// holds the configured custom user claims; a replaced value moves to the former
// identifiers. Each attempt reads the User again, so a concurrent write is kept.
func RecordLogin(ctx context.Context, store UserStore, userName, email, displayName string, groups []string, subject, preferredUsername string, extraClaims map[string]string, namespace string) error {
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

	var skip bool
	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		// NotFound means the User was deleted after the read. The next attempt creates it again.
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
	}, func() error {
		var e error
		skip, e = recordIdentity(ctx, store, userName, email, displayName, extraClaims, annotations, namespace)
		return e
	})
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	return patchObservedStatus(ctx, store, userName, namespace, &now, groups)
}

// recordIdentity writes the identity of one login attempt. It reports skip
// true for a local-managed user, which takes no status write. Any read error
// other than NotFound fails the attempt, so the record is never replaced.
func recordIdentity(ctx context.Context, store UserStore, userName, email, displayName string, extraClaims, annotations map[string]string, namespace string) (bool, error) {
	existing, err := store.GetUserCRD(ctx, userName, namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get user %s: %w", userName, err)
	}
	if err != nil || existing == nil {
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
		setUserClaims(user, extraClaims)
		if err := store.CreateUserCRD(ctx, user); err != nil {
			return false, fmt.Errorf("create user %s: %w", userName, err)
		}
		return false, nil
	}

	// Never modify local-managed users.
	if existing.Labels != nil && existing.Labels[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByLocal {
		return true, nil
	}

	// Deprovisioning reads the former names, so a replaced name keeps its record.
	former := FormerIdentifiers(existing)
	if prev := existing.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; prev != "" && prev != annotations[v1alpha1.AnnotationOIDCPreferredUsername] {
		former = append(former, prev)
	}
	if existing.Spec.Email != "" && existing.Spec.Email != email {
		former = append(former, existing.Spec.Email)
	}
	for k, v := range UserClaims(existing) {
		if extraClaims[k] != v {
			former = append(former, v)
		}
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
	// A login without the claim clears the name. The member role must not grant a name that the IdP no longer sends.
	delete(existing.Annotations, v1alpha1.AnnotationOIDCPreferredUsername)
	for k, v := range annotations {
		existing.Annotations[k] = v
	}
	setUserClaims(existing, extraClaims)
	setFormerIdentifiers(existing, former)
	if err := store.UpdateUserCRD(ctx, existing); err != nil {
		return false, fmt.Errorf("update user %s: %w", userName, err)
	}
	return false, nil
}

// FormerIdentifiers returns the preferred usernames, emails, and replaced
// custom user claim values that a User held before a later login. It returns
// nil for a nil user, a missing
// annotation, or a value that is not a JSON array.
func FormerIdentifiers(u *v1alpha1.User) []string {
	if u == nil {
		return nil
	}
	raw := u.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers]
	if raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	return values
}

// UserClaims returns the custom user claims that a login stored on the User.
// It returns nil for a nil user, a missing annotation, or a value that is not
// a JSON object of strings.
func UserClaims(u *v1alpha1.User) map[string]string {
	if u == nil {
		return nil
	}
	raw := u.Annotations[v1alpha1.AnnotationOIDCUserClaims]
	if raw == "" {
		return nil
	}
	var claims map[string]string
	if err := json.Unmarshal([]byte(raw), &claims); err != nil {
		return nil
	}
	return claims
}

// setUserClaims writes the non-empty custom user claims as a JSON object with
// sorted keys. It removes the annotation when no claim remains.
func setUserClaims(u *v1alpha1.User, claims map[string]string) {
	clean := make(map[string]string, len(claims))
	for k, v := range claims {
		if k != "" && v != "" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		delete(u.Annotations, v1alpha1.AnnotationOIDCUserClaims)
		return
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return
	}
	if u.Annotations == nil {
		u.Annotations = map[string]string{}
	}
	u.Annotations[v1alpha1.AnnotationOIDCUserClaims] = string(raw)
}

// setFormerIdentifiers writes the sorted, deduplicated former identifiers.
// It removes the annotation when no value remains.
func setFormerIdentifiers(u *v1alpha1.User, values []string) {
	clean := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			clean = append(clean, v)
		}
	}
	slices.Sort(clean)
	clean = slices.Compact(clean)
	if len(clean) == 0 {
		delete(u.Annotations, v1alpha1.AnnotationOIDCFormerIdentifiers)
		return
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return
	}
	if u.Annotations == nil {
		u.Annotations = map[string]string{}
	}
	u.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] = string(raw)
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
