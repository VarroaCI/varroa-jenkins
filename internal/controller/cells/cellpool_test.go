package cells

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestSummarizeCellPool(t *testing.T) {
	pool := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "big"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: testProfileName, MaxTenantsPerCell: 3},
		Status: v1alpha1.CellPoolStatus{
			Conditions: []metav1.Condition{{
				Type:   v1alpha1.CellPoolConditionPoolReady,
				Status: metav1.ConditionFalse,
				Reason: v1alpha1.CellPoolReasonCellCapReached,
			}},
		},
	}
	got := SummarizeCellPool(pool)
	if got.Name != "big" || got.ProfileRef != testProfileName || got.MaxTenantsPerCell != 3 {
		t.Fatalf("summary = %+v", got)
	}
	if !got.CapReached {
		t.Error("CapReached must follow the PoolReady=False/CellCapReached condition")
	}

	ready := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "small"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: testProfileName},
		Status: v1alpha1.CellPoolStatus{
			Conditions: []metav1.Condition{{
				Type:   v1alpha1.CellPoolConditionPoolReady,
				Status: metav1.ConditionTrue,
				Reason: v1alpha1.CellPoolReasonReady,
			}},
		},
	}
	if SummarizeCellPool(ready).CapReached {
		t.Error("a ready pool must not report CapReached")
	}
}

func TestEffectiveOwner(t *testing.T) {
	pools := []CellPoolSummary{
		{Name: "big", ProfileRef: testProfileName, MaxTenantsPerCell: 3},
		{Name: "small", ProfileRef: testProfileName, MaxTenantsPerCell: 1},
	}

	if got := effectiveOwner(pools, "big"); got == nil || got.Name != "big" {
		t.Fatalf("effectiveOwner(big) = %+v, want the big pool", got)
	}
	if got := effectiveOwner(pools, ""); got != nil {
		t.Errorf("an unlabelled cell has no effective owner, got %+v", got)
	}
	if got := effectiveOwner(pools, "gone"); got != nil {
		t.Errorf("a label naming a deleted pool has no effective owner, got %+v", got)
	}
}

func TestResolveCellCapacity(t *testing.T) {
	owner := &CellPoolSummary{Name: "big", ProfileRef: testProfileName, MaxTenantsPerCell: 3}
	tests := []struct {
		name         string
		own          int32
		owner        *CellPoolSummary
		chartDefault int32
		want         int32
	}{
		{name: "the cell's own value wins", own: 7, owner: owner, chartDefault: 10, want: 7},
		{name: "a pool cell uses its pool", own: 0, owner: owner, chartDefault: 10, want: 3},
		{name: "a cell of a deleted pool uses the chart value", own: 0, owner: nil, chartDefault: 10, want: 10},
		{name: "a pool that sets no cap uses the chart value", own: 0, owner: &CellPoolSummary{Name: "big"}, chartDefault: 10, want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveCellCapacity(tt.own, tt.owner, tt.chartDefault); got != tt.want {
				t.Errorf("resolveCellCapacity() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolvedPoolName(t *testing.T) {
	named := &v1alpha1.TenantPipelinesSpec{Profile: testProfileName, CellPool: "big"}
	if got := ResolvedPoolName(named); got != "big" {
		t.Errorf("ResolvedPoolName(named) = %q, want big", got)
	}
	def := &v1alpha1.TenantPipelinesSpec{Profile: testProfileName}
	if got := ResolvedPoolName(def); got != testProfileName {
		t.Errorf("ResolvedPoolName(default) = %q, want %q", got, testProfileName)
	}
	if got := ResolvedPoolName(nil); got != "" {
		t.Errorf("ResolvedPoolName(nil) = %q, want empty", got)
	}
}
