package controller

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const bcryptCost = 12

// UserReconciler watches User CRDs and hashes spec.password into
// status.credentials.passwordHash. It runs on a 15-second ticker in
// local auth mode.
type UserReconciler struct {
	client ResourceClient
	store  crdstore.Backend
	ns     string
	Logger *slog.Logger
}

// NewUserReconciler creates a UserReconciler.
func NewUserReconciler(client ResourceClient, store crdstore.Backend, ns string) *UserReconciler {
	return &UserReconciler{client: client, store: store, ns: ns, Logger: slog.Default()}
}

// Reconcile processes all User CRDs. For each user with a non-empty
// spec.password, it bcrypts the password, writes the hash to
// status.credentials, then clears spec.password.
func (r *UserReconciler) Reconcile(ctx context.Context) error {
	users, err := crdstore.List[v1alpha1.User](ctx, r.store, r.ns, "")
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}

	for _, user := range users {
		if user.Spec.Password == "" {
			continue
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(user.Spec.Password), bcryptCost)
		if err != nil {
			return fmt.Errorf("hash password for %s: %w", user.Name, err)
		}

		now := metav1.Now()
		if err := crdstore.PatchStatus[v1alpha1.User](ctx, r.store, user.Name, r.ns, &v1alpha1.UserStatus{
			Credentials: &v1alpha1.UserCredentials{
				PasswordHash: string(hash),
				LastChanged:  &now,
			},
		}); err != nil {
			return fmt.Errorf("patch user status %s: %w", user.Name, err)
		}

		if err := r.client.ClearUserPassword(ctx, user.Name, r.ns); err != nil {
			return fmt.Errorf("clear password for %s: %w", user.Name, err)
		}

		r.Logger.Info("hashed password for user", "user", user.Name)
	}

	return nil
}
