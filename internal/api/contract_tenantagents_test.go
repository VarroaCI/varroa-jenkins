package api

import "net/http"

// The contract server has no record store and no Tenant agent source, so each
// Tenant agent route answers its unwired shape. The admin claims see every
// cluster-wide gate.
const contractTenantAgentTenant = "contract-tenant"

func init() {
	registerContractCases(
		contractCase{Name: "getTenantAgent-unwired", Method: "GET", Path: "/api/v1/pipelines/" + contractTenantAgentTenant + "/agent",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listTenantAgentSessions-unwired", Method: "GET", Path: "/api/v1/pipelines/" + contractTenantAgentTenant + "/agent/sessions",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "getTenantAgentCapabilities-unwired", Method: "GET", Path: "/api/v1/pipelines/" + contractTenantAgentTenant + "/agent/capabilities",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "runTenantAgent-unwired", Method: "POST", Path: "/api/v1/pipelines/" + contractTenantAgentTenant + "/agent/run",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listAgents-empty", Method: "GET", Path: "/api/v1/agents",
			Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "putAgentUnattended-missing", Method: "PUT", Path: "/api/v1/agents/test-ns/missing/unattended",
			Body: map[string]any{"enabled": true}, Claims: adminClaims, WantStatus: http.StatusNotFound},
	)
}
