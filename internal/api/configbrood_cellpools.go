package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// configCellPoolScaleTimeout covers up to three read-modify-write attempts
// on the member's own apiserver. The member handler stops at 8 seconds.
const configCellPoolScaleTimeout = 10 * time.Second

// refuseLocalCellPools guards the member-only cell-pool verbs. The BFF
// service reads and scales the local cluster's pools on its own store.
func (f *busConfigBrood) refuseLocalCellPools(cluster string) error {
	if f.isLocal(cluster) {
		return &BroodError{Code: bus.CodeInternal, Msg: "the local cluster's cell pools are read on the local store"}
	}
	return nil
}

func (f *busConfigBrood) ListCellPools(_ context.Context, cluster string) ([]v1alpha1.CellPool, error) {
	if err := f.refuseLocalCellPools(cluster); err != nil {
		return nil, err
	}
	respData, err := f.requestTo(bus.OperatorCellPoolsSubject(cluster, "list"), bus.ConfigListRequest{}, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("list cellpools decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	pools := make([]v1alpha1.CellPool, 0, len(resp.Items))
	for _, raw := range resp.Items {
		var pool v1alpha1.CellPool
		if err := json.Unmarshal(raw, &pool); err != nil {
			return nil, fmt.Errorf("list cellpools decode item: %w", err)
		}
		pools = append(pools, pool)
	}
	return pools, nil
}

func (f *busConfigBrood) GetCellPool(_ context.Context, cluster, name string) (*v1alpha1.CellPool, error) {
	if err := f.refuseLocalCellPools(cluster); err != nil {
		return nil, err
	}
	respData, err := f.requestTo(bus.OperatorCellPoolsSubject(cluster, "get"), bus.ConfigGetRequest{Name: name}, configGetTimeout)
	if err != nil {
		return nil, err
	}
	return f.decodeCellPoolReply(respData, "get")
}

func (f *busConfigBrood) ScaleCellPool(_ context.Context, cluster string, req bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	if err := f.refuseLocalCellPools(cluster); err != nil {
		return nil, err
	}
	respData, err := f.requestTo(bus.OperatorCellPoolsSubject(cluster, "scale"), req, configCellPoolScaleTimeout)
	if err != nil {
		return nil, err
	}
	return f.decodeCellPoolReply(respData, "scale")
}

func (f *busConfigBrood) decodeCellPoolReply(respData []byte, verb string) (*v1alpha1.CellPool, error) {
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("%s cellpool decode: %w", verb, err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	var pool v1alpha1.CellPool
	if err := json.Unmarshal(resp.Item, &pool); err != nil {
		return nil, fmt.Errorf("%s cellpool decode item: %w", verb, err)
	}
	return &pool, nil
}
