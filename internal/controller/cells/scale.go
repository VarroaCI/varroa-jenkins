package cells

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellPoolScaleAttempts bounds the read-modify-write retry. A conflict past
// this bound is reported, so a caller retries with a fresh read rather than
// the loop running on.
const cellPoolScaleAttempts = 3

var (
	// ErrCellPoolInvalid wraps a dial refusal. The wrapped message is the
	// text a caller shows.
	ErrCellPoolInvalid = errors.New("invalid cell pool scale")
	// ErrCellPoolNotFound means the named pool does not exist.
	ErrCellPoolNotFound = errors.New("cell pool not found")
	// ErrCellPoolConflict means every attempt of the write conflicted.
	ErrCellPoolConflict = errors.New("the cell pool changed during the update")
)

// CellPoolDials holds the four scale dials. A nil dial keeps its stored
// value.
type CellPoolDials struct {
	Warm               *int32
	MaxCells           *int32
	MaxTenantsPerCell  *int32
	UnreadyGracePeriod *string
}

// Named reports whether the set names at least one dial.
func (d CellPoolDials) Named() bool {
	return d.Warm != nil || d.MaxCells != nil || d.MaxTenantsPerCell != nil || d.UnreadyGracePeriod != nil
}

// invalid builds an ErrCellPoolInvalid whose Error() is msg alone.
func invalid(msg string) error {
	return &invalidDialError{msg: msg}
}

type invalidDialError struct{ msg string }

func (e *invalidDialError) Error() string { return e.msg }
func (e *invalidDialError) Unwrap() error { return ErrCellPoolInvalid }

// ApplyCellPoolDials merges the named dials onto spec and repeats the CRD's
// bounds. The CRD schema stays the authority. This check exists so that a
// bad scale returns a readable refusal instead of a raw apiserver rejection.
func ApplyCellPoolDials(spec *v1alpha1.CellPoolSpec, d CellPoolDials) error {
	if !d.Named() {
		return invalid("spec must name at least one of warm, maxCells, maxTenantsPerCell, or unreadyGracePeriod")
	}
	if d.Warm != nil {
		if *d.Warm < 0 {
			return invalid("spec.warm must be 0 or above")
		}
		spec.Warm = *d.Warm
	}
	if d.MaxCells != nil {
		if *d.MaxCells < 0 {
			return invalid("spec.maxCells must be 0 or above")
		}
		spec.MaxCells = *d.MaxCells
	}
	if d.MaxTenantsPerCell != nil {
		if *d.MaxTenantsPerCell < 1 {
			return invalid("spec.maxTenantsPerCell must be 1 or above")
		}
		if *d.MaxTenantsPerCell > MaxCellTenants {
			return invalid("spec.maxTenantsPerCell must be 64 or below")
		}
		spec.MaxTenantsPerCell = *d.MaxTenantsPerCell
	}
	if d.UnreadyGracePeriod != nil {
		dur, err := time.ParseDuration(*d.UnreadyGracePeriod)
		if err != nil || dur < 0 {
			return invalid("spec.unreadyGracePeriod must be a duration such as 5m")
		}
		spec.UnreadyGracePeriod = &metav1.Duration{Duration: dur}
	}
	if spec.MaxCells > 0 && spec.MaxCells < spec.Warm {
		return invalid("spec.maxCells must be 0 or at least spec.warm")
	}
	return nil
}

// ScaleCellPool applies the dials to the named pool with a read-modify-write
// against store. crdstore exposes no spec-level merge patch, and a server-side
// apply field manager would fight the pool reconciler, which writes the spec
// of an auto-created pool on its own tick. A conflict is therefore retried up
// to three times.
func ScaleCellPool(ctx context.Context, store crdstore.Backend, name string, d CellPoolDials) (*v1alpha1.CellPool, error) {
	if !d.Named() {
		return nil, ApplyCellPoolDials(&v1alpha1.CellPoolSpec{}, d)
	}
	for attempt := 0; attempt < cellPoolScaleAttempts; attempt++ {
		existing, err := crdstore.Get[v1alpha1.CellPool](ctx, store, name, "")
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, ErrCellPoolNotFound
			}
			return nil, fmt.Errorf("read cell pool %s: %w", name, err)
		}
		updated := existing.DeepCopy()
		if err := ApplyCellPoolDials(&updated.Spec, d); err != nil {
			return nil, err
		}
		err = crdstore.Update(ctx, store, updated)
		if err == nil {
			return updated, nil
		}
		if !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("update cell pool %s: %w", name, err)
		}
	}
	return nil, ErrCellPoolConflict
}
