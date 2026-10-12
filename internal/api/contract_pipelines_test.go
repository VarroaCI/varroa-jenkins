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
		// The landing-page list answers the local pipelines Teams with
		// recordStore false when no record store is wired, not 503.
		contractCase{
			Name:       "listPipelineTeams-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines",
			Claims:     adminClaims,
			WantStatus: http.StatusOK,
		},
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
			Name:       "listJobBranches-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/jobs/checkout-api/branches",
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
			Name:       "listJobTestCaseHistory-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/jobs/checkout-api/tests/com.example.FooTest%2Fbar",
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
		contractCase{
			Name:       "listRunDownstream-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/downstream",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "getRunScript-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/script",
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
		contractCase{
			Name:       "replayRun-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/replay",
			Body:       replayRunRequest{Script: "echo 'edited'"},
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "getRunRestart-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/restart",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "restartRun-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/restart",
			Body:       restartRunRequest{Stage: "deploy"},
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "proceedRunInput-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/inputs/in-1/proceed",
			Body:       map[string]any{"parameters": map[string]string{}},
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "abortRunInput-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/runs/0194f6b2-0000-7000-8000-000000000000/inputs/in-1/abort",
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

		// The team run stream reads no record-store row, but it answers
		// 503 through pipelinesUnavailable while RecordStore is unset, so
		// the handler refuses the stream rather than opening a dead one.
		contractCase{
			Name:       "streamTeamRuns-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/stream",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
	)

	// ── Fleet locks (add-fleet-locks-view) ───────────────────────────────
	//
	// Both lock reads answer from the record store, and newContractServer
	// wires none, so each route answers the same uniform 503 the pipelines
	// paths above answer: ListFleetLocks checks the store before it
	// resolves the team, and the fleet-wide read checks it before the
	// gate. That proves both routes are wired to their handler and their
	// response matches the spec's ServiceUnavailable shape.
	// handlers_pipelines_locks_test.go covers the 200 shape, the row key
	// set, and the fleetlocks:read gate over a fakeRecordReader.
	registerContractCases(
		contractCase{
			Name:       "listTeamFleetLocks-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/pipelines/payments/locks",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		contractCase{
			Name:       "listAllFleetLocks-recordStoreDisabled",
			Method:     "GET",
			Path:       "/api/v1/locks",
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
		// The force release checks the gate and the body before the
		// releaser, so an admin with no record store gets the 503 shape.
		// fleetlock_release_test.go covers every other status.
		contractCase{
			Name:       "releaseFleetLock-recordStoreDisabled",
			Method:     "POST",
			Path:       "/api/v1/locks/release",
			Body:       map[string]string{"name": "payments/deploy", "runId": "run-1", "reason": "the old pod is gone"},
			Claims:     adminClaims,
			WantStatus: http.StatusServiceUnavailable,
		},
	)
}
