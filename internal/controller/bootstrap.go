package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"

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

	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      username,
			Namespace: ns,
		},
		Spec: v1alpha1.UserSpec{
			Email:       username + "@local",
			DisplayName: "Administrator",
		},
	}
	if err := crdstore.Apply[v1alpha1.User](ctx, store, user); err != nil {
		return fmt.Errorf("apply bootstrap user: %w", err)
	}

	now := metav1.Now()
	if err := crdstore.PatchStatus[v1alpha1.User](ctx, store, username, ns, &v1alpha1.UserStatus{
		Credentials: &v1alpha1.UserCredentials{
			PasswordHash: string(hash),
			LastChanged:  &now,
		},
	}); err != nil {
		return fmt.Errorf("patch bootstrap user status: %w", err)
	}

	// Create the admin group.
	group := &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: bootstrapGroupName,
		},
		Spec: v1alpha1.GroupSpec{
			DisplayName: "Varroa Administrators",
			Members:     []string{username},
		},
	}
	if err := crdstore.Apply[v1alpha1.Group](ctx, store, group); err != nil {
		return fmt.Errorf("apply bootstrap group: %w", err)
	}

	// Bind both the admin user and the varroa-admins group to the admin
	// role. The group subject is what makes membership in varroa-admins
	// actually grant admin: user-deprovisioning strips User subjects from
	// bindings, so a deleted-and-recreated admin regains access by being
	// re-added to the group rather than silently losing it forever.
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: bootstrapRoleBindingName,
		},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{
				{Kind: "User", Name: username},
				{Kind: "Group", Name: bootstrapGroupName},
			},
			RoleRef: bootstrapAdminRole,
		},
	}
	if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](ctx, store, binding); err != nil {
		return fmt.Errorf("apply bootstrap role binding: %w", err)
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
