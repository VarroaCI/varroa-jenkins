package cells

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func i32(v int32) *int32   { return &v }
func str(v string) *string { return &v }

func seedScalePool(store *crdstore.Fake) {
	crdstore.MustSeed(store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:        "lts-2-555",
			Warm:              1,
			MaxCells:          6,
			MaxTenantsPerCell: 10,
		},
	})
}

func TestApplyCellPoolDials_Bounds(t *testing.T) {
	tests := []struct {
		name string
		d    CellPoolDials
		want string
	}{
		{"no dial", CellPoolDials{}, "spec must name at least one of warm, maxCells, maxTenantsPerCell, or unreadyGracePeriod"},
		{"negative warm", CellPoolDials{Warm: i32(-1)}, "spec.warm must be 0 or above"},
		{"negative maxCells", CellPoolDials{MaxCells: i32(-1)}, "spec.maxCells must be 0 or above"},
		{"zero tenants", CellPoolDials{MaxTenantsPerCell: i32(0)}, "spec.maxTenantsPerCell must be 1 or above"},
		{"too many tenants", CellPoolDials{MaxTenantsPerCell: i32(65)}, "spec.maxTenantsPerCell must be 64 or below"},
		{"bad grace", CellPoolDials{UnreadyGracePeriod: str("soon")}, "spec.unreadyGracePeriod must be a duration such as 5m"},
		{"negative grace", CellPoolDials{UnreadyGracePeriod: str("-5m")}, "spec.unreadyGracePeriod must be a duration such as 5m"},
		{"maxCells under warm", CellPoolDials{Warm: i32(7)}, "spec.maxCells must be 0 or at least spec.warm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := v1alpha1.CellPoolSpec{Warm: 1, MaxCells: 6, MaxTenantsPerCell: 10}
			err := ApplyCellPoolDials(&spec, tt.d)
			if !errors.Is(err, ErrCellPoolInvalid) {
				t.Fatalf("err = %v, want ErrCellPoolInvalid", err)
			}
			if err.Error() != tt.want {
				t.Errorf("message = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestApplyCellPoolDials_AppliesEachDial(t *testing.T) {
	spec := v1alpha1.CellPoolSpec{Warm: 1, MaxCells: 6, MaxTenantsPerCell: 10}
	err := ApplyCellPoolDials(&spec, CellPoolDials{
		Warm: i32(2), MaxCells: i32(0), MaxTenantsPerCell: i32(4), UnreadyGracePeriod: str("5m"),
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if spec.Warm != 2 || spec.MaxCells != 0 || spec.MaxTenantsPerCell != 4 {
		t.Errorf("spec = %+v", spec)
	}
	if spec.UnreadyGracePeriod == nil || spec.UnreadyGracePeriod.Duration != 5*time.Minute {
		t.Errorf("unreadyGracePeriod = %v, want 5m", spec.UnreadyGracePeriod)
	}
}

func TestApplyCellPoolDials_AcceptsCeiling(t *testing.T) {
	spec := v1alpha1.CellPoolSpec{Warm: 1, MaxCells: 6, MaxTenantsPerCell: 10}
	if err := ApplyCellPoolDials(&spec, CellPoolDials{MaxTenantsPerCell: i32(64)}); err != nil {
		t.Fatalf("apply 64: %v", err)
	}
	if spec.MaxTenantsPerCell != 64 {
		t.Errorf("MaxTenantsPerCell = %d, want 64", spec.MaxTenantsPerCell)
	}
}

func TestScaleCellPool_WritesOnlyTheNamedDial(t *testing.T) {
	store := crdstore.NewFake()
	seedScalePool(store)

	got, err := ScaleCellPool(context.Background(), store, "p", CellPoolDials{Warm: i32(3)})
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if got.Spec.Warm != 3 {
		t.Errorf("returned warm = %d, want 3", got.Spec.Warm)
	}
	stored, err := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "p", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.Warm != 3 || stored.Spec.ProfileRef != "lts-2-555" || stored.Spec.MaxCells != 6 || stored.Spec.MaxTenantsPerCell != 10 {
		t.Errorf("stored spec = %+v", stored.Spec)
	}
}

func TestScaleCellPool_MissingPool(t *testing.T) {
	_, err := ScaleCellPool(context.Background(), crdstore.NewFake(), "nope", CellPoolDials{Warm: i32(1)})
	if !errors.Is(err, ErrCellPoolNotFound) {
		t.Fatalf("err = %v, want ErrCellPoolNotFound", err)
	}
}

func TestScaleCellPool_InvalidDialWritesNothing(t *testing.T) {
	store := crdstore.NewFake()
	seedScalePool(store)
	_, err := ScaleCellPool(context.Background(), store, "p", CellPoolDials{Warm: i32(-1)})
	if !errors.Is(err, ErrCellPoolInvalid) {
		t.Fatalf("err = %v, want ErrCellPoolInvalid", err)
	}
	stored, _ := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "p", "")
	if stored.Spec.Warm != 1 {
		t.Errorf("warm = %d, want the stored 1", stored.Spec.Warm)
	}
}

func TestScaleCellPool_ConflictRetriedThenReported(t *testing.T) {
	store := crdstore.NewFake()
	seedScalePool(store)
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	store.FailAlways("update", gvr, apierrors.NewConflict(
		schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, "p", nil))

	_, err := ScaleCellPool(context.Background(), store, "p", CellPoolDials{Warm: i32(2)})
	if !errors.Is(err, ErrCellPoolConflict) {
		t.Fatalf("err = %v, want ErrCellPoolConflict", err)
	}
}

func TestScaleCellPool_ConflictOnceThenSuccess(t *testing.T) {
	store := crdstore.NewFake()
	seedScalePool(store)
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	store.FailNext("update", gvr, apierrors.NewConflict(
		schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, "p", nil))

	got, err := ScaleCellPool(context.Background(), store, "p", CellPoolDials{Warm: i32(2)})
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if got.Spec.Warm != 2 {
		t.Errorf("warm = %d, want 2", got.Spec.Warm)
	}
}
