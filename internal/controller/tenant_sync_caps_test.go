package controller

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

func TestMarshalSyncPolicy_CarriesConcurrencyLimitsAndCellMaxRuns(t *testing.T) {
	t.Parallel()

	raw, err := marshalSyncPolicy(cells.SyncPolicy{
		ConcurrencyLimits: []v1alpha1.TenantConcurrencyLimit{{Name: "deploys", Jobs: []string{"deploy/*"}, Max: 1}},
		CellMaxRuns:       3,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		ConcurrencyLimits []v1alpha1.TenantConcurrencyLimit `json:"concurrencyLimits"`
		CellMaxRuns       int32                             `json:"cellMaxRuns"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ConcurrencyLimits) != 1 || got.ConcurrencyLimits[0].Name != "deploys" || got.CellMaxRuns != 3 {
		t.Fatalf("wire policy = %s, want the limit and cellMaxRuns 3", raw)
	}
}
