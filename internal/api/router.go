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
	// Runners: external agent registration (list/create) + detail dispatch
	// (get/patch/delete). Runner is cluster-scoped, so no namespace segment.
	mux.HandleFunc("/runners", srv.HandleRunners)
	mux.HandleFunc("/runners/", srv.HandleRunnerDispatch)
	mux.HandleFunc("/cellpools", srv.HandleCellPools)
	mux.HandleFunc("/cellpools/", srv.HandleCellPoolDispatch)
	// Fleet locks: the cluster-wide lock list, beside the cell-pool
	// family because both are flat fleet views.
	mux.HandleFunc("GET /locks", srv.handleListAllFleetLocks)
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

	// Upgrade tracking: clusterless ProfileCandidate list/get/promote.
	mux.HandleFunc("/version-candidates", srv.HandleVersionCandidates)
	mux.HandleFunc("/version-candidates/", srv.HandleVersionCandidateDispatch)

	// Cells: the flat cell usage family. A cell usage row is a fleet
	// view, so it does not live under /clusters/{cluster}/controllers/.
	mux.HandleFunc("GET /cells/usage", srv.handleListCellUsage)
	mux.HandleFunc("GET /cells/{namespace}/{name}/usage", srv.handleCellUsage)
	mux.HandleFunc("GET /cells/{namespace}/{name}/grants", srv.handleCellGrants)

	// Pipelines: team-scoped job listing, job detail, run list/create,
	// and one test case's history.
	mux.HandleFunc("GET /pipelines/{team}", srv.handleListPipelines)
	mux.HandleFunc("GET /pipelines/{team}/jobs/{job}", srv.handleGetPipelineJob)
	mux.HandleFunc("GET /pipelines/{team}/jobs/{job}/runs", srv.handleListRuns)
	mux.HandleFunc("POST /pipelines/{team}/jobs/{job}/runs", srv.handleCreateRun)
	mux.HandleFunc("GET /pipelines/{team}/jobs/{job}/tests/{case}", srv.handleListJobTestCaseHistory)
	mux.HandleFunc("POST /pipelines/{team}/markers/validate", srv.handleValidatePipelineMarker)
	mux.HandleFunc("GET /pipelines/{team}/cell", srv.handleGetTeamCell)
	mux.HandleFunc("GET /pipelines/{team}/locks", srv.handleListTeamFleetLocks)
	mux.HandleFunc("GET /pipelines/{team}/stream", srv.handleTeamRunStream)
	// Runs: detail, log stream, artifacts, tests, changes, script, cancel,
	// rerun, replay, and the two input routes.
	mux.HandleFunc("GET /runs/{run}", srv.handleGetRun)
	mux.HandleFunc("GET /runs/{run}/log", srv.handleStreamRunLog)
	mux.HandleFunc("GET /runs/{run}/artifacts", srv.handleListRunArtifacts)
	mux.HandleFunc("GET /runs/{run}/artifacts/{path...}", srv.handleDownloadRunArtifact)
	mux.HandleFunc("GET /runs/{run}/tests", srv.handleListRunTests)
	mux.HandleFunc("GET /runs/{run}/changes", srv.handleListRunChanges)
	mux.HandleFunc("GET /runs/{run}/script", srv.handleGetRunScript)
	mux.HandleFunc("POST /runs/{run}/cancel", srv.handleCancelRun)
	mux.HandleFunc("POST /runs/{run}/rerun", srv.handleRerunRun)
	mux.HandleFunc("POST /runs/{run}/replay", srv.handleReplayRun)
	mux.HandleFunc("POST /runs/{run}/inputs/{input}/proceed", srv.handleProceedRunInput)
	mux.HandleFunc("POST /runs/{run}/inputs/{input}/abort", srv.handleAbortRunInput)

	return http.StripPrefix("/api/v1", mux)
}

// TrimPrefix is a utility for sub-mux routing.
func TrimPrefix(path, prefix string) string {
	return strings.TrimPrefix(path, prefix)
}
