package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TenantHeld reports whether any member cell of tenant, active or standby,
// records an ItemsHeld hold for the Tenant (v1alpha1.CellHoldsTenant). It
// reads the cells in v1alpha1.PlacementCells order and stops at the first
// hold. A member cell that does not exist holds nothing. Any other read
// error is returned.
func TenantHeld(ctx context.Context, store crdstore.Backend, tenant *v1alpha1.Tenant) (bool, error) {
	if tenant == nil {
		return false, nil
	}
	for _, ref := range v1alpha1.PlacementCells(tenant.Status.Placement) {
		cell, err := crdstore.Get[v1alpha1.Controller](ctx, store, ref.Name, ref.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if v1alpha1.CellHoldsTenant(cell, tenant.Name) {
			return true, nil
		}
	}
	return false, nil
}
