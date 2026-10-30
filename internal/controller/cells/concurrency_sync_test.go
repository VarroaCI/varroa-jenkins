package cells

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestSyncPolicyFromPipelinesCarriesConcurrencyLimits(t *testing.T) {
	t.Parallel()

	limits := []v1alpha1.TenantConcurrencyLimit{{Name: "deploys", Jobs: []string{"deploy/*"}, Max: 1}}
	got := SyncPolicyFromPipelines(&v1alpha1.TenantPipelinesSpec{ConcurrencyLimits: limits})
	if len(got.ConcurrencyLimits) != 1 || got.ConcurrencyLimits[0].Name != "deploys" {
		t.Fatalf("ConcurrencyLimits = %+v, want the deploys limit", got.ConcurrencyLimits)
	}
}

func TestSyncPolicyEqualDetectsLimitChange(t *testing.T) {
	t.Parallel()

	a := SyncPolicy{ConcurrencyLimits: []v1alpha1.TenantConcurrencyLimit{{Name: "x", Jobs: []string{"*"}, Max: 1}}}
	b := SyncPolicy{ConcurrencyLimits: []v1alpha1.TenantConcurrencyLimit{{Name: "x", Jobs: []string{"*"}, Max: 2}}}
	if a.Equal(b) {
		t.Fatal("a changed limit max must not compare equal")
	}
	if !a.Equal(a) {
		t.Fatal("a policy must equal itself")
	}
}

func TestSyncPolicyEqualDetectsCellMaxRunsChange(t *testing.T) {
	t.Parallel()

	a := SyncPolicy{CellMaxRuns: 2}
	b := SyncPolicy{CellMaxRuns: 3}
	if a.Equal(b) {
		t.Fatal("a changed pool cap must not compare equal")
	}
}

func TestSyncPolicyFor_ResolvesPoolMaxRunsPerTenant(t *testing.T) {
	t.Parallel()

	tenant := &v1alpha1.Tenant{Spec: v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{CellPool: "pool-a"}}}
	in := Inputs{Pools: []CellPoolSummary{{Name: "pool-a", MaxRunsPerTenant: 4}}}
	if got := syncPolicyFor(in, tenant, &v1alpha1.TenantStatus{}).CellMaxRuns; got != 4 {
		t.Fatalf("CellMaxRuns = %d, want 4", got)
	}
}

func TestSummarizeCellPool_CarriesMaxRunsPerTenant(t *testing.T) {
	t.Parallel()

	pool := &v1alpha1.CellPool{}
	pool.Name = "pool-a"
	pool.Spec.MaxRunsPerTenant = 5
	if got := SummarizeCellPool(pool).MaxRunsPerTenant; got != 5 {
		t.Fatalf("MaxRunsPerTenant = %d, want 5", got)
	}
}
