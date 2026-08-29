package api

import (
	"net/http"
	"strings"
)

// NewRouter returns an http.Handler that routes /api/v1/... requests.
func NewRouter(deps *Dependencies) http.Handler {
	srv := NewServer(deps)
	mux := http.NewServeMux()

	mux.HandleFunc("/me", srv.HandleMe)
	mux.HandleFunc("/me/preferences", srv.HandleUpdatePreferences)
	mux.HandleFunc("/me/password", srv.HandleChangePassword)
	mux.HandleFunc("/logout", srv.HandleLogout)
	// API key self-service.
	mux.HandleFunc("/me/apikeys", srv.handleMeApiKeys)
	mux.HandleFunc("/me/apikeys/", srv.handleMeApiKeyDispatch)
	// Auth config (unauthenticated) + local login.
	mux.HandleFunc("/auth-config", srv.HandleAuthConfig)
	mux.HandleFunc("/login", srv.HandleLogin)
	// Users list/create (admin) + sub-resource dispatch (password, apikeys, delete).
	mux.HandleFunc("/users", srv.HandleUsers)
	mux.HandleFunc("/users/", srv.handleUsersDispatch)
	// Identity settings (admin, read-only).
	mux.HandleFunc("/identity-settings", srv.HandleIdentitySettings)
	// Built-in roles reference (admin, read-only, live-CRD-sourced).
	mux.HandleFunc("/builtin-roles", srv.HandleBuiltinRoles)
	// Groups list/create (admin) + detail dispatch (delete).
	mux.HandleFunc("/groups", srv.HandleGroups)
	mux.HandleFunc("/groups/", srv.HandleGroupDispatch)
	// Teams list/create (admin) + detail dispatch (get/update/delete).
	mux.HandleFunc("/teams", srv.HandleTeams)
	mux.HandleFunc("/teams/", srv.HandleTeamDispatch)
	// Controllers: GET list (creation is declarative via the Kubernetes API).
	mux.HandleFunc("/controllers", srv.HandleControllers)
	// Clusters: GET list, + cluster-scoped dispatch.
	mux.HandleFunc("/clusters", srv.handleClusters)
	mux.HandleFunc("/clusters/", srv.handleClusterDispatch)
	// RBAC roles: GET list, POST create, GET/PUT/DELETE detail.
	mux.HandleFunc("/roles", srv.handleVarroaRoles)
	mux.HandleFunc("/roles/", srv.handleVarroaRoleDispatch)
	// RBAC role bindings: GET list, POST create, GET/PUT/DELETE detail.
	mux.HandleFunc("/rolebindings", srv.handleVarroaRoleBindings)
	mux.HandleFunc("/rolebindings/", srv.handleVarroaRoleBindingDispatch)
	// VarroaRole /roles and /rolebindings stay flat (core-only).
	// Current user permissions.
	mux.HandleFunc("/me/permissions", srv.handleMePermissions)
	// Activity feed.
	mux.HandleFunc("/activity", srv.handleActivity)
	mux.HandleFunc("/activity/stream", srv.handleActivityStream)
	// Search.
	mux.HandleFunc("/search", srv.handleSearch)
	// Brood-wide SSE stream.
	if deps.Broadcaster != nil {
		mux.HandleFunc("/stream/brood", srv.handleBroodStreamSSE)
	}
	// Brood operations.
	mux.HandleFunc("/brood-operations", srv.handleBroodOperations)
	mux.HandleFunc("/brood-operations/", srv.handleBroodOperations)
	// Brood schedules.
	mux.HandleFunc("/brood-schedules", srv.handleBroodSchedules)
	mux.HandleFunc("/brood-schedules/", srv.handleBroodSchedules)
	// SSE stream-ticket issuance (header-less EventSource auth).
	mux.HandleFunc("POST /stream/ticket", srv.handleStreamTicket)
	// OpenAPI spec (unauthenticated).
	mux.HandleFunc("/openapi.json", srv.HandleOpenAPISpec)
	// RapiDoc docs interface (unauthenticated).
	mux.HandleFunc("/docs", srv.HandleDocs)
	mux.HandleFunc("/docs/", srv.HandleDocs)
	// Update Center.
	mux.HandleFunc("GET /updatecenter", srv.HandleUpdateCenterStatus)
	mux.HandleFunc("GET /updatecenter/plugins", srv.HandleUpdateCenterPlugins)
	mux.HandleFunc("POST /updatecenter/plugins", srv.HandleUpdateCenterUpload)

	// Fleet plugin inventory.
	mux.HandleFunc("/fleet/plugins", srv.HandleFleetPlugins)
	mux.HandleFunc("/fleet/plugins/", srv.HandleFleetPluginDetail)

	return http.StripPrefix("/api/v1", mux)
}

// TrimPrefix is a utility for sub-mux routing.
func TrimPrefix(path, prefix string) string {
	return strings.TrimPrefix(path, prefix)
}
