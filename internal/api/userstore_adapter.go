package api

import (
	"context"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// userStoreAdapter adapts crdstore.Backend into userdirectory.UserStore.
type userStoreAdapter struct {
	store crdstore.Backend
}

// GetUserCRD implements userdirectory.UserStore.
func (a userStoreAdapter) GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error) {
	return crdstore.Get[v1alpha1.User](ctx, a.store, name, namespace)
}

// ApplyUserCRD implements userdirectory.UserStore.
func (a userStoreAdapter) ApplyUserCRD(ctx context.Context, user *v1alpha1.User) error {
	return crdstore.Apply[v1alpha1.User](ctx, a.store, user)
}

// PatchUserStatus implements userdirectory.UserStore.
func (a userStoreAdapter) PatchUserStatus(ctx context.Context, name, namespace string, status *v1alpha1.UserStatus) error {
	return crdstore.PatchStatus[v1alpha1.User](ctx, a.store, name, namespace, status)
}
