package api

import (
	"net/http"
)

// The contract server runs without an assistant runtime, so every assistant
// route answers its disabled shape. The /me/assistant answer is the one
// 200 body that a disabled assistant still gives.
const contractAssistantID = "0190d1e2-0000-7000-8000-000000000001"

func init() {
	registerContractCases(
		contractCase{Name: "createAssistantSession-disabled", Method: "POST", Path: "/api/v1/assistant/sessions",
			Body: map[string]any{"message": "why did it fail"}, Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listAssistantSessions-disabled", Method: "GET", Path: "/api/v1/assistant/sessions",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "getAssistantSession-disabled", Method: "GET", Path: "/api/v1/assistant/sessions/" + contractAssistantID,
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "postAssistantMessage-disabled", Method: "POST", Path: "/api/v1/assistant/sessions/" + contractAssistantID + "/messages",
			Body: map[string]any{"message": "again"}, Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "streamAssistantEvents-disabled", Method: "GET", Path: "/api/v1/assistant/sessions/" + contractAssistantID + "/events",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "cancelAssistantSession-disabled", Method: "POST", Path: "/api/v1/assistant/sessions/" + contractAssistantID + "/cancel",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listAssistantApprovals-disabled", Method: "GET", Path: "/api/v1/assistant/approvals",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "approveAssistantApproval-disabled", Method: "POST", Path: "/api/v1/assistant/approvals/" + contractAssistantID + "/approve",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "denyAssistantApproval-disabled", Method: "POST", Path: "/api/v1/assistant/approvals/" + contractAssistantID + "/deny",
			Body: map[string]any{"reason": "no"}, Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "getAssistantSettings-disabled", Method: "GET", Path: "/api/v1/assistant/settings",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "putAssistantSettings-disabled", Method: "PUT", Path: "/api/v1/assistant/settings",
			Body: map[string]any{"paused": true}, Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "listAssistantProviders-disabled", Method: "GET", Path: "/api/v1/assistant/providers",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "getMyAssistant-disabled", Method: "GET", Path: "/api/v1/me/assistant",
			Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "putMyAssistant-disabled", Method: "PUT", Path: "/api/v1/me/assistant",
			Body: map[string]any{"enabled": true}, Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "putMyAssistantKey-disabled", Method: "PUT", Path: "/api/v1/me/assistant/key",
			Body: map[string]any{"provider": "smoke", "apiKey": "key"}, Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "deleteMyAssistantKey-disabled", Method: "DELETE", Path: "/api/v1/me/assistant/key",
			Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
	)
}
