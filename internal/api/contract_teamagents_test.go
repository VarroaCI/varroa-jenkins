package api

import "net/http"

// The contract server has no record store and no Team agent source, so each
// Team agent route answers its unwired shape. The admin claims see every
// cluster-wide gate.
const contractTeamAgentTeam = "contract-team"

func init() {
	registerContractCases(
		contractCase{Name: "getTeamAgent-unwired", Method: "GET", Path: "/api/v1/pipelines/" + contractTeamAgentTeam + "/agent",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listTeamAgentSessions-unwired", Method: "GET", Path: "/api/v1/pipelines/" + contractTeamAgentTeam + "/agent/sessions",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "getTeamAgentCapabilities-unwired", Method: "GET", Path: "/api/v1/pipelines/" + contractTeamAgentTeam + "/agent/capabilities",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "runTeamAgent-unwired", Method: "POST", Path: "/api/v1/pipelines/" + contractTeamAgentTeam + "/agent/run",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listAgents-empty", Method: "GET", Path: "/api/v1/agents",
			Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "putAgentUnattended-missing", Method: "PUT", Path: "/api/v1/agents/test-ns/missing/unattended",
			Body: map[string]any{"enabled": true}, Claims: adminClaims, WantStatus: http.StatusNotFound},
	)
}
