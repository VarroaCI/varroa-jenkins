package api

import (
	"net/http"
)

func init() {
	// ── Cell usage reads (add-cell-usage-read) ───────────────────────────
	//
	// newContractServer wires no RecordStore, so all three routes answer
	// 503 with the uniform error envelope. That proves each route reaches
	// its handler and that the response matches the spec's
	// ServiceUnavailable shape.
	registerContractCases(
		contractCase{
			Name:       "listCellUsage-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/cells/usage",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "getCellUsage-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/cells/test-ns/cell-a/usage",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "getTeamCell-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/cell",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
	)
}
