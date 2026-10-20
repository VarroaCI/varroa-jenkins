package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// configCellGrantsTimeout bounds one cells.grants walk. The member answers every
// tenant of the cell in one reply, so the walk needs more than a single-object read.
const configCellGrantsTimeout = 10 * time.Second

// CellGrants reads the grants of the tenants of one member cell. The member's
// operator answers from its own RBAC state. The local cluster refuses here,
// because the caller reads its own resolver.
func (f *busConfigBrood) CellGrants(ctx context.Context, cluster string, tenants []string) ([]bus.CellTenantGrants, error) {
	if f.isLocal(cluster) {
		return nil, &BroodError{Code: bus.CodeInternal, Msg: "the local cluster's cell grants are read on the local resolver"}
	}
	respData, err := f.readTo(ctx, bus.OperatorCellsSubject(cluster, "grants"), bus.CellGrantsRequest{Tenants: tenants}, configCellGrantsTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.CellGrantsResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("cell grants decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Tenants, nil
}
