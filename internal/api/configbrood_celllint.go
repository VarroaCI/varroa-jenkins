package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// configCellLintTimeout bounds one cells.lint relay. It must exceed the
// member's 20 second linter timeout, so the member answers before the relay
// gives up.
const configCellLintTimeout = 25 * time.Second

// CellLint sends one Jenkinsfile to the member operator of cluster for
// linting on the named cell. The member operator calls its own cell's
// linter. The local cluster refuses here, because the caller lints on its own
// cells. A reply that carries a Phase or a LinterError is data for the caller,
// so it returns with a nil error.
func (f *busConfigBrood) CellLint(ctx context.Context, cluster string, req bus.CellLintRequest) (bus.CellLintResponse, error) {
	if f.isLocal(cluster) {
		return bus.CellLintResponse{}, &BroodError{Code: bus.CodeInternal, Msg: "the local cluster's cells are linted on the local resolver"}
	}
	respData, err := f.readTo(ctx, bus.OperatorCellsSubject(cluster, "lint"), req, configCellLintTimeout)
	if err != nil {
		return bus.CellLintResponse{}, err
	}
	var resp bus.CellLintResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return bus.CellLintResponse{}, fmt.Errorf("cell lint decode: %w", err)
	}
	if resp.Phase != "" || resp.LinterError != "" {
		return resp, nil
	}
	if resp.Code != "" {
		return bus.CellLintResponse{}, f.codeToErr(resp.Code, resp.Error)
	}
	return resp, nil
}
