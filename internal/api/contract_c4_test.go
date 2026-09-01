package api

import (
	"net/http"
)

func init() {
	// ── version-candidates — upgrade tracking contract cases (C4) ────────
	registerContractCases(
		contractCase{
			Name:       "listVersionCandidates",
			Method:     "GET",
			Path:       "/api/v1/version-candidates",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
		contractCase{
			Name:       "getVersionCandidate",
			Method:     "GET",
			Path:       "/api/v1/version-candidates/nonexistent",
			Claims:     adminClaims,
			WantStatus: http.StatusNotFound,
		},
		contractCase{
			Name:       "promoteVersionCandidate",
			Method:     "POST",
			Path:       "/api/v1/version-candidates/nonexistent/promote",
			Claims:     adminClaims,
			WantStatus: http.StatusNotFound,
		},
	)
}
