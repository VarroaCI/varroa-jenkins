package api

import (
	"net/http"
)

func init() {
	// ── apikeys — named-key contract cases (C2) ──────────────────────────
	registerContractCases(
		contractCase{
			Name:       "createNamedApiKey",
			Method:     "POST",
			Path:       "/api/v1/me/apikeys",
			Body:       map[string]string{"name": "ci-key"},
			Claims:     adminClaims,
			WantStatus: http.StatusCreated,
		},
		contractCase{
			Name:       "rotateApiKeyWithName",
			Method:     "POST",
			Path:       "/api/v1/me/apikeys/abcdefg/rotate",
			Body:       map[string]string{"name": "new-key-name"},
			Claims:     adminClaims,
			WantStatus: http.StatusNotFound,
		},
		contractCase{
			Name:       "listApiKeysWithName",
			Method:     "GET",
			Path:       "/api/v1/me/apikeys",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
	)
}
