package api

import (
	"net/http"
)

func init() {
	// ── C3: query-param variants not covered by C1 ────────────────────────
	//
	// C1 covers the target operations at their base paths; these rows add
	// the optional query-parameter variants that C1 omitted (only the path
	// was exercised). Operations where both path and query were already
	// covered (e.g. search?q, preview, pause, resume, sync, RBAC PUT/DELETE,
	// users, passwords, version-profiles, deployable namespaces, identity,
	// builtin-roles) are skipped as duplicates.

	registerContractCases(
		// C1: listActivity at /api/v1/activity (no query param).
		// New: same path with optional ?controller= filter.
		contractCase{
			Name:       "listActivity-filteredByController",
			Method:     "GET",
			Path:       "/api/v1/activity?controller=test-ns/test-ctrl",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},

		// C1: listCatalogItems at /api/v1/clusters/core/catalogitems (no query params).
		// New: same path with all three optional filters (source, type, q).
		contractCase{
			Name:       "listCatalogItems-filtered",
			Method:     "GET",
			Path:       "/api/v1/clusters/core/catalogitems?source=test-src&type=plugin&q=my-plugin",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},

		// C1: validateComposedBundle-503 at /api/v1/clusters/core/composedbundles/validate
		//     (no query param).
		// New: same path with optional ?namespace= filter.
		contractCase{
			Name:       "validateComposedBundle-withNamespace",
			Method:     "POST",
			Path:       "/api/v1/clusters/core/composedbundles/validate?namespace=default",
			Body:       map[string]interface{}{"inputs": []map[string]interface{}{}},
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
	)
}
