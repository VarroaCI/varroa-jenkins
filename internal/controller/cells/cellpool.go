package cells

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// LabelCellPool names the CellPool that owns a cell. Every pool stamps it on
// each cell it creates. The profile's default pool also stamps it on a cell
// of its profile that carries no pool label.
const LabelCellPool = "varroa.dev/cell-pool"

// MaxCellTenants is the capacity ceiling. It matches the CRD item limit on
// status.cell.tenants: a placed cell cannot hold more tenants than that list
// accepts.
const MaxCellTenants = 64

// CellPoolSummary is one pool's read-only view for the placement pass. The
// caller gathers every summary once per tick, so Place performs no read.
type CellPoolSummary struct {
	// Name is the pool's object name.
	Name string
	// ProfileRef names the JenkinsVersionProfile the pool's cells run.
	ProfileRef string
	// MaxTenantsPerCell is the capacity a pool cell uses when its own
	// spec.cell.maxTenants is zero.
	MaxTenantsPerCell int32
	// MaxRunsPerTenant is the pool's per-tenant build cap on one cell. Zero
	// means no cap.
	MaxRunsPerTenant int32
	// CapReached mirrors PoolReady=False with reason CellCapReached.
	CapReached bool
}

// SummarizeCellPool builds the placement view of one live pool.
func SummarizeCellPool(pool *v1alpha1.CellPool) CellPoolSummary {
	s := CellPoolSummary{
		Name:              pool.Name,
		ProfileRef:        pool.Spec.ProfileRef,
		MaxTenantsPerCell: pool.Spec.MaxTenantsPerCell,
		MaxRunsPerTenant:  pool.Spec.MaxRunsPerTenant,
	}
	for _, c := range pool.Status.Conditions {
		if c.Type != v1alpha1.CellPoolConditionPoolReady {
			continue
		}
		s.CapReached = c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.CellPoolReasonCellCapReached
	}
	return s
}

// ResolvedPoolName answers the pool a tenant uses: spec.pipelines.cellPool
// when set, otherwise the pool whose name equals spec.pipelines.profile.
func ResolvedPoolName(p *v1alpha1.TeamPipelinesSpec) string {
	if p == nil {
		return ""
	}
	if p.CellPool != "" {
		return p.CellPool
	}
	return p.Profile
}

// effectiveOwner answers the pool a cell's label names, and only while that
// pool exists. A cell with no label, or with a label naming a deleted pool,
// has no effective owner. Every membership and capacity test reads the
// effective owner, never the raw label.
func effectiveOwner(pools []CellPoolSummary, label string) *CellPoolSummary {
	if label == "" {
		return nil
	}
	for i := range pools {
		if pools[i].Name == label {
			return &pools[i]
		}
	}
	return nil
}

// resolveCellCapacity answers a cell's tenant cap: the cell's own
// spec.cell.maxTenants when above zero, then the effective owner's
// maxTenantsPerCell when above zero, then the chart value. It is never the
// minimum of two values.
func resolveCellCapacity(own int32, owner *CellPoolSummary, chartDefault int32) int32 {
	if own > 0 {
		return own
	}
	if owner != nil && owner.MaxTenantsPerCell > 0 {
		return owner.MaxTenantsPerCell
	}
	return chartDefault
}
