package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TeamHeld reports whether any member cell of team, active or standby,
// records an ItemsHeld hold for the Team (v1alpha1.CellHoldsTenant). It
// reads the cells in v1alpha1.PlacementCells order and stops at the first
// hold. A member cell that does not exist holds nothing. Any other read
// error is returned.
func TeamHeld(ctx context.Context, store crdstore.Backend, team *v1alpha1.Team) (bool, error) {
	if team == nil {
		return false, nil
	}
	for _, ref := range v1alpha1.PlacementCells(team.Status.Placement) {
		cell, err := crdstore.Get[v1alpha1.Controller](ctx, store, ref.Name, ref.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if v1alpha1.CellHoldsTenant(cell, team.Name) {
			return true, nil
		}
	}
	return false, nil
}
