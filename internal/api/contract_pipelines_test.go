package api

import (
	"net/http"
)

func init() {
	// ── Pipelines read paths (add-pipelines-ui, section 3) ────────────────
	//
	// newContractServer wires no RecordStore, so every read and write path
	// answers 503 with the uniform error envelope — the exact D12 contract
	// for an install with the record store disabled. That is enough to
	// prove each route is wired to its handler and its response matches
	// the spec's ServiceUnavailable shape. The log stream (streamRunLog)
	// stays excluded from coverage-gate-1 until section 5 replaces the
	// 501 stub and adds its own case here.
	registerContractCases(
		contractCase{
			Name:       "listPipelines-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "getPipelineJob-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/jobs/checkout-api",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "listRuns-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/jobs/checkout-api/runs",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "getRun-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "listRunArtifacts-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/artifacts",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "downloadRunArtifact-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/artifacts/report.html",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "listRunTests-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/tests",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "listRunChanges-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/changes",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},

		// ── Pipelines write paths (add-pipelines-ui, section 4) ──────────
		contractCase{
			Name:       "createRun-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/pipelines/payments/jobs/checkout-api/runs",
			Body:       createRunRequest{},
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "cancelRun-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/cancel",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "rerunRun-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/rerun",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},

		// The marker lint endpoint contacts no cell and reads no
		// record-store data, but it still resolves {team} through
		// TenantResolver, which newContractServer wires the same way it
		// leaves RecordStore unset — so this route answers the same
		// uniform 503 the other pipelines paths above answer.
		contractCase{
			Name:       "validatePipelineMarker-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/pipelines/payments/markers/validate",
			Body:       map[string]string{"marker": "template: {name: go-service, version: \"1.0.0\"}\n"},
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
	)
}
