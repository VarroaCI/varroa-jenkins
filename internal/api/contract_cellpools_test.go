package api

import (
	"net/http"
)

func init() {
	// ── Cell-pool read and scale surface (add-cell-pool-crd, D10) ─────────
	//
	// The contract coverage gate needs one case per registered operationId.
	// newContractServer seeds CellPool/test-pool and authorizes the caller
	// through the admin wildcard, so each case exercises the real handler
	// and validates its response against the spec.
	registerContractCases(
		contractCase{
			Name:       "listCellPools",
			Method:     "GET",
			Path:       "/api/v1/cellpools",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "getCellPool",
			Method:     "GET",
			Path:       "/api/v1/cellpools/test-pool",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "listCellPools-oneCluster",
			Method:     "GET",
			Path:       "/api/v1/cellpools?cluster=core",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "getCellPool-namedCluster",
			Method:     "GET",
			Path:       "/api/v1/cellpools/test-pool?cluster=core",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "patchCellPool-namedCluster",
			Method:     "PATCH",
			Path:       "/api/v1/cellpools/test-pool?cluster=core",
			Body:       map[string]any{"spec": map[string]any{"maxCells": 4}},
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "patchCellPool",
			Method:     "PATCH",
			Path:       "/api/v1/cellpools/test-pool",
			Body:       map[string]any{"spec": map[string]any{"warm": 2}},
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
	)
}
