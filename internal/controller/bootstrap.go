package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"slices"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const (
	bootstrapSecretName      = "varroa-admin-credentials"
	bootstrapAnnotation      = "bootstrap.consumed"
	bootstrapAnnotationTime  = "bootstrap.timestamp"
	bootstrapGroupName       = "varroa-admins"
	bootstrapRoleBindingName = "varroa-admin-bootstrap"
	bootstrapAdminRole       = "admin"
	bootstrapAdminUser       = "admin"
	bootstrapPasswordLength  = 24
)

// BootstrapLocalAdmin ensures a local-admin User exists, bound to the
// admin VarroaRole. It is idempotent: once the bootstrap Secret is
// annotated bootstrap.consumed=true, it returns immediately.
//
// A re-run keeps the existing User, Group, and binding. It adds only the
// bootstrap subject to the Group and the binding, and keeps every other
// member and subject.
//
// If the Secret already has username+password keys, those are used;
// otherwise a random 24-char password is generated and persisted.
func BootstrapLocalAdmin(ctx context.Context, client ResourceClient, store crdstore.Backend, ns string, logger *slog.Logger) error {
	sec, err := client.GetSecret(ctx, bootstrapSecretName, ns)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get bootstrap secret: %w", err)
	}

	// Already bootstrapped.
	if sec != nil && string(sec[bootstrapAnnotation]) == "true" {
		return nil
	}

	var username, password string
	generated := false

	if sec != nil && len(sec["username"]) > 0 && len(sec["password"]) > 0 {
		username = string(sec["username"])
		password = string(sec["password"])
	} else {
		username = bootstrapAdminUser
		password = generatePassword(bootstrapPasswordLength)
		generated = true
	}

	// Hash the password and persist the User CRD.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("hash bootstrap password: %w", err)
	}

	now := metav1.Now()
	// A delete between the create and the status patch makes the patch answer
	// NotFound; create the User again once.
	for attempt := 0; attempt < 2; attempt++ {
		user := &v1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:      username,
				Namespace: ns,
				Labels:    map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal},
			},
			Spec: v1alpha1.UserSpec{
				Email:       username + "@local",
				DisplayName: "Administrator",
			},
		}
		if err := crdstore.Create[v1alpha1.User](ctx, store, user); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create bootstrap user: %w", err)
			}
			logger.Info("bootstrap user exists; keeping it", "user", username)
		}

		err := crdstore.PatchStatus[v1alpha1.User](ctx, store, username, ns, &v1alpha1.UserStatus{
			Credentials: &v1alpha1.UserCredentials{
				PasswordHash: string(hash),
				LastChanged:  &now,
			},
		})
		if err == nil {
			break
		}
		if apierrors.IsNotFound(err) && attempt == 0 {
			continue
		}
		return fmt.Errorf("patch bootstrap user status: %w", err)
	}

	if err := ensureBootstrapGroup(ctx, store, username); err != nil {
		return err
	}
	if err := ensureBootstrapRoleBinding(ctx, store, username); err != nil {
		return err
	}

	// Persist or update the Secret with the consumed annotation.
	secretData := map[string][]byte{
		"username":              []byte(username),
		"password":              []byte(password),
		bootstrapAnnotation:     []byte("true"),
		bootstrapAnnotationTime: []byte(now.Format("2006-01-02T15:04:05Z")),
	}
	if err := client.CreateOrUpdateSecret(ctx, bootstrapSecretName, ns, secretData); err != nil {
		return fmt.Errorf("create/update bootstrap secret: %w", err)
	}

	if generated {
		logger.Info("Created bootstrap admin", "secret", bootstrapSecretName, "namespace", ns)
	}

	return nil
}

// ensureBootstrapGroup creates the admin Group, or adds username to the
// existing Group. It keeps every other member and field.
func ensureBootstrapGroup(ctx context.Context, store crdstore.Backend, username string) error {
	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapGroupName},
		Spec: v1alpha1.GroupSpec{
			DisplayName: "Varroa Administrators",
			Members:     []string{username},
		},
	}
	err := crdstore.CreateOrMutate[v1alpha1.Group](ctx, store, group, func(g *v1alpha1.Group) (bool, error) {
		if slices.Contains(g.Spec.Members, username) {
			return false, nil
		}
		g.Spec.Members = append(g.Spec.Members, username)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("ensure bootstrap group: %w", err)
	}
	return nil
}

// ensureBootstrapRoleBinding creates the admin role binding, or adds the
// bootstrap User and Group subjects to the existing binding. It keeps
// every other subject, and it never changes RoleRef or Scope.
//
// The binding has a Group subject as well as the User. User-deprovisioning
// strips User subjects from bindings. A deleted-and-recreated admin regains
// access by being re-added to the group.
func ensureBootstrapRoleBinding(ctx context.Context, store crdstore.Backend, username string) error {
	wanted := []v1alpha1.SubjectRef{
		{Kind: "User", Name: username},
		{Kind: "Group", Name: bootstrapGroupName},
	}
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapRoleBindingName},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: wanted,
			RoleRef:  bootstrapAdminRole,
		},
	}
	err := crdstore.CreateOrMutate[v1alpha1.VarroaRoleBinding](ctx, store, binding, func(b *v1alpha1.VarroaRoleBinding) (bool, error) {
		changed := false
		for _, want := range wanted {
			if slices.ContainsFunc(b.Spec.Subjects, func(sub v1alpha1.SubjectRef) bool {
				return sub.Kind == want.Kind && sub.Name == want.Name
			}) {
				continue
			}
			b.Spec.Subjects = append(b.Spec.Subjects, want)
			changed = true
		}
		return changed, nil
	})
	if err != nil {
		return fmt.Errorf("ensure bootstrap role binding: %w", err)
	}
	return nil
}

// generatePassword returns a random alphanumeric password of the
// given length, suitable for a bootstrap admin credential.
func generatePassword(length int) string {
	b := make([]byte, length*2)
	if _, err := rand.Read(b); err != nil {
		// Fallback: should never happen on modern kernels.
		panic(fmt.Sprintf("generate password: crypto/rand read failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)[:length]
}
