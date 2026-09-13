package api

import (
	"net/http"
)

func init() {
	// ── The cell grants read (add-operator-cell-views) ───────────────────
	//
	// One case carries one status, so the route needs three: the 200 path
	// with both gates held, the 404 for a controller that is not a cell,
	// and the 403 for a caller who holds the controller read grant and no
	// cluster-wide rolebindings:read.
	registerContractCases(
		contractCase{
			Name:       "getCellGrants-bothGatesHeld",
			Method:     "GET",
			Path:       "/api/v1/cells/test-ns/test-cell/grants",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "getCellGrants-notACell",
			Method:     "GET",
			Path:       "/api/v1/cells/test-ns/test-ctrl/grants",
			Claims:     adminClaims,
			WantStatus: http.StatusNotFound,
		},
		contractCase{
			Name:       "getCellGrants-noRoleBindingRead",
			Method:     "GET",
			Path:       "/api/v1/cells/test-ns/test-cell/grants",
			Claims:     operatorClaims,
			WantStatus: http.StatusForbidden,
		},
	)
}
