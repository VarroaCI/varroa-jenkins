package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellPoolHandlerTimeout bounds each cellpools.* handler. It stays below the
// BFF's 10-second scale wait, so the BFF does not report a member as
// unreachable while this operator still writes.
const cellPoolHandlerTimeout = 8 * time.Second

// HandleCellPoolsList handles operator.<cluster>.cellpools.list. Each item
// is a full CellPool object.
func (c *ConfigCRUD) HandleCellPoolsList(data []byte) []byte {
	var req bus.ConfigListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), cellPoolHandlerTimeout)
	defer cancel()
	items, err := crdstore.List[v1alpha1.CellPool](ctx, c.Store, "", "")
	if err != nil {
		return c.configReply(bus.ConfigListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}
	rows, next, err := pageList(items, req.After, bus.ListPageBudget, func(cr *v1alpha1.CellPool) (json.RawMessage, error) {
		return marshalCR(cr)
	})
	if err != nil {
		msg := listPageErr(err)
		return c.configReply(bus.ConfigListResponse{Error: msg, Code: bus.CodeInternal})
	}
	return c.listPageReply(rows, next)
}

// HandleCellPoolsGet handles operator.<cluster>.cellpools.get.
func (c *ConfigCRUD) HandleCellPoolsGet(data []byte) []byte {
	var req bus.ConfigGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), cellPoolHandlerTimeout)
	defer cancel()
	cr, err := crdstore.Get[v1alpha1.CellPool](ctx, c.Store, req.Name, "")
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	raw, err := marshalCR(cr)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigGetResponse{Item: raw})
}

// HandleCellPoolsScale handles operator.<cluster>.cellpools.scale. It runs
// the read-modify-write against this cluster's own apiserver, so a write of
// the pool reconciler on this cluster is retried rather than overwritten.
// The request type holds no structural field, so a profileRef or a
// cellTemplate cannot reach the write.
func (c *ConfigCRUD) HandleCellPoolsScale(data []byte) []byte {
	var req bus.CellPoolScaleRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), cellPoolHandlerTimeout)
	defer cancel()
	pool, err := cells.ScaleCellPool(ctx, c.Store, req.Name, cells.CellPoolDials{
		Warm:               req.Warm,
		MaxCells:           req.MaxCells,
		MaxTenantsPerCell:  req.MaxTenantsPerCell,
		UnreadyGracePeriod: req.UnreadyGracePeriod,
	})
	if err != nil {
		code := bus.CodeInternal
		switch {
		case errors.Is(err, cells.ErrCellPoolInvalid):
			code = bus.CodeInvalid
		case errors.Is(err, cells.ErrCellPoolNotFound):
			code = bus.CodeNotFound
		case errors.Is(err, cells.ErrCellPoolConflict):
			code = bus.CodeConflict
		}
		if code == bus.CodeInternal {
			c.Logger.Error("cell pool scale failed", "name", req.Name, "error", err)
		}
		return c.configReply(bus.ConfigGetResponse{Error: err.Error(), Code: code})
	}
	raw, err := marshalCR(pool)
	if err != nil {
		return c.configReply(bus.ConfigGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigGetResponse{Item: raw})
}
