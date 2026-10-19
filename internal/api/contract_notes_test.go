package api

import "net/http"

func init() {
	registerContractCases(
		contractCase{
			Name:       "listRunNotes-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0190a1f0-0000-7000-8000-000000000001/notes",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "createRunNote-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0190a1f0-0000-7000-8000-000000000001/notes",
			Claims:     adminClaims,
			Body:       map[string]any{"body": "a note"},
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "listJobNotes-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/jobs/checkout-api/notes",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
	)
}
