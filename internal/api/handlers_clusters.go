package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

type resolvedBundleInput struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Type      string `json:"type,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Status    string `json:"status"`
}

func projectResolvedInputs(cb *v1alpha1.ComposedBundle) []resolvedBundleInput {
	inputs := make([]resolvedBundleInput, len(cb.Spec.Inputs))
	missing := make(map[string]bool, len(cb.Status.MissingItems))
	drifted := make(map[string]bool, len(cb.Status.DriftedItems))
	for _, value := range cb.Status.MissingItems {
		missing[value] = true
	}
	for _, value := range cb.Status.DriftedItems {
		drifted[value] = true
	}
	for i, input := range cb.Spec.Inputs {
		resolved := resolvedBundleInput{Index: i, Status: "Unknown"}
		if input.ItemRef != nil {
			resolved.Name = input.ItemRef.Name
			resolved.Kind = "itemRef"
			resolved.Namespace = input.ItemRef.Namespace
		} else if input.GitSource != nil {
			resolved.Name = input.GitSource.RepoURL + "#" + input.GitSource.Path
			resolved.Kind = "gitSource"
			resolved.Type = "git"
		}
		if i < len(cb.Status.InputSummary) {
			summary := cb.Status.InputSummary[i]
			resolved.Kind = summary.Kind
			resolved.Type = summary.Type
			resolved.Namespace = summary.Namespace
		}
		index := strconv.Itoa(i)
		resolved.Revision = cb.Status.ObservedRevisions[index]
		if resolved.Revision == "" && input.ItemRef != nil {
			resolved.Revision = cb.Status.ObservedRevisions[input.ItemRef.Name]
		}
		switch {
		case missing[index]:
			resolved.Status = "Missing"
		case drifted[index]:
			resolved.Status = "Drifted"
		case resolved.Revision != "":
			resolved.Status = "Resolved"
		}
		inputs[i] = resolved
	}
	return inputs
}

// handleClusters handles GET /api/v1/clusters.
// Returns the list of known clusters with their status.
func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if s.deps.Brood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "brood not configured")
		return
	}

	clusters, err := s.deps.Brood.Clusters(r.Context())
	if err != nil {
		s.deps.Logger.Error("list clusters failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list clusters")
		return
	}

	s.writeJSON(w, http.StatusOK, itemsEnvelope(clusters))
}

// controllerRoute pairs a required HTTP method with a name-level controller
// handler. An empty method means the handler enforces methods itself.
type controllerRoute struct {
	method  string
	handler func(*Server, http.ResponseWriter, *http.Request, string, string, string)
}

// controllerRoutes maps the sub-resource path under
// /clusters/{cluster}/controllers/{ns}/{name} to its handler ("" = the bare resource).
var controllerRoutes = map[string]controllerRoute{
	"":                 {"", (*Server).handleControllerResource},
	"yaml":             {"", (*Server).handleControllerYAML},
	"reconcile":        {http.MethodPost, (*Server).handleReconcile},
	"hibernate":        {http.MethodPost, (*Server).handleHibernate},
	"wake":             {http.MethodPost, (*Server).handleWake},
	"approve":          {http.MethodPost, (*Server).handleApproveRestart},
	"approve-deletion": {http.MethodPost, (*Server).handleApproveDeletion},
	"reprovision":      {http.MethodPost, (*Server).handleReprovision},
	"restart":          {http.MethodPost, (*Server).handleRestartController},
	"preview":          {http.MethodPost, (*Server).handlePreviewController},
	"diff":             {http.MethodGet, (*Server).handleControllerDiff},
	"logs":             {"", (*Server).handleControllerLogs},
	"events":           {http.MethodGet, (*Server).handleControllerEvents},
	"mite/stream":      {"", (*Server).handleMiteStreamSSE},
	"plugins":          {http.MethodGet, (*Server).handleControllerPlugins},
}

// configDispatchRoutes maps the second segment of a cluster-scoped config path
// to its dispatch handler. Each handler takes (cluster string, restSegments []string).
type configDispatchFunc func(*Server, http.ResponseWriter, *http.Request, string, []string)

var configDispatchRoutes = map[string]configDispatchFunc{
	"controllers":          (*Server).dispatchControllers,
	"composedbundles":      (*Server).dispatchComposedBundles,
	"catalogitems":         (*Server).dispatchCatalogItems,
	"catalogsources":       (*Server).dispatchCatalogSources,
	"jenkinsroles":         (*Server).dispatchJenkinsRoles,
	"jenkinsrolebindings":  (*Server).dispatchJenkinsRoleBindings,
	"provisioningdefaults": (*Server).dispatchProvisioningDefaults,
	"provisioning":         (*Server).dispatchProvisioning,
	"version-profiles":     (*Server).dispatchVersionProfiles,
	"controller-classes":   (*Server).dispatchControllerClasses,
}

// handleClusterDispatch routes /clusters/{cluster}/controllers[...] paths.
// Unknown clusters return 404. Unknown sub-resources return 404.
func (s *Server) handleClusterDispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/clusters/") {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(path, "/clusters/")
	segments := strings.Split(suffix, "/")

	// Require at least a cluster name.
	if len(segments) < 1 || segments[0] == "" {
		http.NotFound(w, r)
		return
	}
	cluster := segments[0]

	// Validate cluster is known.
	if s.deps.Brood != nil && !s.deps.Brood.IsKnown(r.Context(), cluster) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown cluster"})
		return
	}

	// /clusters/{cluster}/drain — drain lifecycle endpoint.
	if len(segments) == 2 && segments[1] == "drain" {
		s.handleClusterDrain(w, r, cluster)
		return
	}

	// /clusters/{cluster}/namespaces/deployable — per-caller deployable namespaces.
	if len(segments) >= 2 && segments[1] == "namespaces" {
		if len(segments) == 3 && segments[2] == "deployable" {
			s.HandleDeployableNamespaces(w, r, cluster)
			return
		}
		http.NotFound(w, r)
		return
	}

	// /clusters/{cluster}/controllers is the only sub-resource tree.
	if len(segments) < 2 || segments[1] == "" {
		http.NotFound(w, r)
		return
	}

	resource := segments[1]
	dispatch, ok := configDispatchRoutes[resource]
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Dispatch to the appropriate handler.
	dispatch(s, w, r, cluster, segments[2:])
}

// dispatchControllers dispatches cluster-scoped controller paths.
// segments does NOT include the cluster name or the "controllers" segment.
func (s *Server) dispatchControllers(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if len(segments) == 0 {
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleControllersFiltered(w, r, cluster)
		return
	}

	ns := segments[0]
	if ns == "" {
		http.NotFound(w, r)
		return
	}

	// Namespace-level POST routes:
	var nsHandler func(http.ResponseWriter, *http.Request, string, string)
	createOrPreflight := false
	switch {
	case len(segments) == 1:
		nsHandler = s.handleCreateController
		createOrPreflight = true
	case len(segments) == 2 && segments[1] == "preflight":
		nsHandler = s.handlePreflightController
		createOrPreflight = true
	case len(segments) == 2 && segments[1] == "render":
		nsHandler = s.handleRenderController
	}
	if nsHandler != nil {
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if createOrPreflight && s.deps.Brood != nil {
			if state, err := s.deps.Brood.StateOf(r.Context(), cluster); err == nil && state != bus.ClusterStateActive {
				s.writeJSON(w, http.StatusConflict, map[string]string{"error": "cluster " + cluster + " is " + state})
				return
			}
		}
		nsHandler(w, r, cluster, ns)
		return
	}

	name := segments[1]
	if name == "" {
		http.NotFound(w, r)
		return
	}

	sub := strings.Join(segments[2:], "/")
	if sub == "" && len(segments) != 2 {
		http.NotFound(w, r)
		return
	}
	route, ok := controllerRoutes[sub]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if route.method != "" && r.Method != route.method {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	route.handler(s, w, r, cluster, ns, name)
}

// handleControllerResource serves GET/DELETE/PATCH on
// /clusters/{cluster}/controllers/{ns}/{name}.
// handleClusterDrain handles POST/DELETE /clusters/{cluster}/drain.
func (s *Server) handleClusterDrain(w http.ResponseWriter, r *http.Request, cluster string) {
	claims := auth.ClaimsFromContext(r.Context())

	// Admin-only gate.
	if s.deps.Authorizer == nil || !s.deps.Authorizer.IsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handleDrainCreate(w, r, cluster, claims)
	case http.MethodDelete:
		s.handleDrainCancel(w, r, cluster, claims)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ActorFrom resolves the audit actor for a caller: the human-readable
// username when the IdP supplies one, then email, then the raw subject.
// API-key callers carry PreferredUsername, so the audit trail resolves to an
// identity rather than a UUID.
func ActorFrom(claims *auth.Claims) string {
	if claims == nil {
		return ""
	}
	if claims.PreferredUsername != "" {
		return claims.PreferredUsername
	}
	if claims.Email != "" {
		return claims.Email
	}
	return claims.Subject
}

func (s *Server) handleDrainCreate(w http.ResponseWriter, r *http.Request, cluster string, claims *auth.Claims) {
	if s.deps.Brood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "brood not configured")
		return
	}

	// Core guard: the core cluster cannot be drained.
	if cluster == s.deps.Brood.LocalCluster() {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the core cluster cannot be drained"})
		return
	}

	// Confirm body: {confirm: clusterName}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.Confirm != cluster {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm must equal the cluster name"})
		return
	}

	state, err := s.deps.Brood.Drain(r.Context(), cluster, ActorFrom(claims))
	if err != nil {
		writeBroodError(w, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": state})
}

func (s *Server) handleDrainCancel(w http.ResponseWriter, r *http.Request, cluster string, claims *auth.Claims) {
	if s.deps.Brood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "brood not configured")
		return
	}

	state, err := s.deps.Brood.DrainCancel(r.Context(), cluster, ActorFrom(claims))
	if err != nil {
		writeBroodError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"state": state})
}

func (s *Server) handleControllerResource(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	switch r.Method {
	case http.MethodGet:
		s.handleControllerDetail(w, r, cluster, ns, name)
	case http.MethodDelete:
		s.handleDeleteController(w, r, cluster, ns, name)
	case http.MethodPatch:
		s.handleUpdateController(w, r, cluster, ns, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleControllerEvents serves the per-controller SSE event stream.
func (s *Server) handleControllerEvents(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadController(claims, ns, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Broadcaster == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "SSE not available")
		return
	}
	sse.HandleControllerStream(s.deps.Broadcaster, cluster+"/"+ns+"/"+name)(w, r)
}

// handleMiteStreamSSE serves the per-controller mite SSE stream.
func (s *Server) handleMiteStreamSSE(w http.ResponseWriter, r *http.Request, cluster, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadController(claims, ns, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if s.deps.Broadcaster == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "SSE not available")
		return
	}
	sse.HandleMiteStream(s.deps.Broadcaster, cluster, ns, name)(w, r)
}

// writeConfigBroodError maps config brood errors to HTTP status codes (DP3).
// not_found → 404, conflict → 409, invalid → 400, internal → 500,
// ErrClusterUnreachable → 502 with {error: "cluster <name> unreachable"}.
func writeConfigBroodError(w http.ResponseWriter, err error, cluster string) {
	if err == nil {
		return
	}
	body := func(msg string) map[string]string {
		return map[string]string{"error": msg}
	}
	var fe *BroodError
	if errors.As(err, &fe) {
		switch fe.Code {
		case bus.CodeNotFound:
			writeJSON(w, http.StatusNotFound, body(fe.Msg))
		case bus.CodeConflict:
			writeJSON(w, http.StatusConflict, body(fe.Msg))
		case bus.CodeInvalid:
			writeJSON(w, http.StatusBadRequest, body(fe.Msg))
		default:
			writeJSON(w, http.StatusInternalServerError, body(fe.Msg))
		}
		return
	}
	var unreachable *ErrClusterUnreachable
	if errors.As(err, &unreachable) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cluster " + cluster + " unreachable"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, body(err.Error()))
}

// --- Config dispatch handlers ---
// Each routes through deps.ConfigBrood uniformly (never special-casing
// the local cluster in HTTP handlers). Authz is unchanged; activity events
// gain Cluster: cluster.

// dispatchComposedBundles handles /clusters/{cluster}/composedbundles[...]
func (s *Server) dispatchComposedBundles(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// /clusters/{cluster}/composedbundles — list
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadComposedBundles(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		items, err := s.deps.ConfigBrood.ListComposedBundles(r.Context(), cluster, "")
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		s.writeJSON(w, http.StatusOK, itemsEnvelope(items))
		return
	}

	// /clusters/{cluster}/composedbundles/validate?namespace=...
	if len(segments) == 1 && segments[0] == "validate" {
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		validateNS := r.URL.Query().Get("namespace")
		if validateNS == "" {
			validateNS = "default"
		}
		if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", validateNS) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var spec v1alpha1.ComposedBundleSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		specJSON, _ := json.Marshal(spec)
		preview, err := s.deps.ConfigBrood.ComposeBundle(r.Context(), cluster, validateNS, specJSON)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		valid := len(preview.Errors) == 0 && len(preview.Missing) == 0
		errs := preview.Errors
		if errs == nil {
			errs = []string{}
		}
		warns := preview.Warnings
		if warns == nil {
			warns = []string{}
		}
		uvars := preview.UnresolvedVariables
		if uvars == nil {
			uvars = []string{}
		}
		pinPreflight := preview.PinPreflight
		if pinPreflight.Conflicts == nil {
			pinPreflight.Conflicts = []bus.PinConflict{}
		}
		if pinPreflight.Missing == nil {
			pinPreflight.Missing = []bus.MissingPlugin{}
		}
		resp := map[string]interface{}{
			"valid":               valid,
			"errors":              errs,
			"warnings":            warns,
			"unresolvedVariables": uvars,
			"pinPreflight":        pinPreflight,
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	// /clusters/{cluster}/composedbundles/preview?namespace=...
	if len(segments) == 1 && segments[0] == "preview" {
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		previewNS := r.URL.Query().Get("namespace")
		if previewNS == "" {
			previewNS = "default"
		}
		if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", previewNS) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var spec v1alpha1.ComposedBundleSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		specJSON, _ := json.Marshal(spec)
		preview, err := s.deps.ConfigBrood.ComposeBundle(r.Context(), cluster, previewNS, specJSON)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		normalizePreview(preview)
		s.writeJSON(w, http.StatusOK, preview)
		return
	}

	ns := segments[0]
	if ns == "" {
		http.NotFound(w, r)
		return
	}

	// /clusters/{cluster}/composedbundles/{ns} — create (but NOT "validate" or "preview")
	if len(segments) == 1 && segments[0] != "validate" && segments[0] != "preview" {
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", ns) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var cb v1alpha1.ComposedBundle
		if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		cb.Namespace = ns
		cb.APIVersion = "varroa.dev/v1alpha1"
		cb.Kind = "ComposedBundle"
		obj, err := json.Marshal(cb)
		if err != nil {
			s.writeJSONError(w, http.StatusInternalServerError, "marshal failed")
			return
		}
		item, err := s.deps.ConfigBrood.CreateComposedBundle(r.Context(), cluster, ns, cb.Name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var created v1alpha1.ComposedBundle
		_ = json.Unmarshal(item, &created)
		s.notifyActivity(activity.Event{
			Type:      "composedbundle.created",
			Actor:     ActorFrom(claims),
			Namespace: ns,
			Cluster:   cluster,
			Message:   "ComposedBundle " + cb.Name + " created in " + ns,
		})
		s.writeJSON(w, http.StatusCreated, created)
		return
	}

	if len(segments) >= 2 {
		name := segments[1]

		// Sub-resource on named bundle: pause, resume
		if len(segments) >= 3 {
			sub := strings.Join(segments[2:], "/")
			switch sub {
			case "pause":
				if r.Method != http.MethodPost {
					s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
					return
				}
				if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "update", ns) {
					s.writeJSONError(w, http.StatusForbidden, "forbidden")
					return
				}
				if err := s.deps.ConfigBrood.PauseComposedBundle(r.Context(), cluster, ns, name, true); err != nil {
					writeConfigBroodError(w, err, cluster)
					return
				}
				s.writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
				return
			case "resume":
				if r.Method != http.MethodPost {
					s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
					return
				}
				if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "update", ns) {
					s.writeJSONError(w, http.StatusForbidden, "forbidden")
					return
				}
				if err := s.deps.ConfigBrood.PauseComposedBundle(r.Context(), cluster, ns, name, false); err != nil {
					writeConfigBroodError(w, err, cluster)
					return
				}
				s.writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
				return
			default:
				http.NotFound(w, r)
				return
			}
		}

		// /clusters/{cluster}/composedbundles/{ns}/preview — compose preview
		if name == "preview" {
			if r.Method != http.MethodPost {
				s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", ns) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var spec v1alpha1.ComposedBundleSpec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			specJSON, _ := json.Marshal(spec)
			preview, err := s.deps.ConfigBrood.ComposeBundle(r.Context(), cluster, ns, specJSON)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			normalizePreview(preview)
			s.writeJSON(w, http.StatusOK, preview)
			return
		}

		// /clusters/{cluster}/composedbundles/{ns}/validate
		if name == "validate" {
			if r.Method != http.MethodPost {
				s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			validateNS := ns
			if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "create", validateNS) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var spec v1alpha1.ComposedBundleSpec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			specJSON, _ := json.Marshal(spec)
			preview, err := s.deps.ConfigBrood.ComposeBundle(r.Context(), cluster, validateNS, specJSON)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			valid := len(preview.Errors) == 0 && len(preview.Missing) == 0
			resp := map[string]interface{}{
				"valid":               valid,
				"errors":              preview.Errors,
				"warnings":            preview.Warnings,
				"unresolvedVariables": preview.UnresolvedVariables,
				"pinPreflight":        preview.PinPreflight,
			}
			s.writeJSON(w, http.StatusOK, resp)
			return
		}

		if name == "" {
			http.NotFound(w, r)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadComposedBundles(claims) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			item, err := s.deps.ConfigBrood.GetComposedBundle(r.Context(), cluster, ns, name)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			var cb v1alpha1.ComposedBundle
			_ = json.Unmarshal(item, &cb)
			var response map[string]interface{}
			_ = json.Unmarshal(item, &response)
			response["resolvedInputs"] = projectResolvedInputs(&cb)
			s.writeJSON(w, http.StatusOK, response)
		case http.MethodPut:
			if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "update", ns) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var cb v1alpha1.ComposedBundle
			if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			cb.Namespace = ns
			cb.Name = name
			cb.APIVersion = "varroa.dev/v1alpha1"
			cb.Kind = "ComposedBundle"
			obj, err := json.Marshal(cb)
			if err != nil {
				s.writeJSONError(w, http.StatusInternalServerError, "marshal failed")
				return
			}
			item, err := s.deps.ConfigBrood.UpdateComposedBundle(r.Context(), cluster, ns, name, obj)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			var updated v1alpha1.ComposedBundle
			_ = json.Unmarshal(item, &updated)
			s.notifyActivity(activity.Event{
				Type:      "composedbundle.updated",
				Actor:     ActorFrom(claims),
				Namespace: ns,
				Cluster:   cluster,
				Message:   "ComposedBundle " + name + " updated in " + ns,
			})
			s.writeJSON(w, http.StatusOK, updated)
		case http.MethodDelete:
			if !s.deps.Authorizer.CanWriteComposedBundlesInNamespace(claims, "delete", ns) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			if err := s.deps.ConfigBrood.DeleteComposedBundle(r.Context(), cluster, ns, name); err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			s.notifyActivity(activity.Event{
				Type:      "composedbundle.deleted",
				Actor:     ActorFrom(claims),
				Namespace: ns,
				Cluster:   cluster,
				Message:   "ComposedBundle " + name + " deleted from " + ns,
			})
			w.WriteHeader(http.StatusNoContent)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	http.NotFound(w, r)
}

// dispatchCatalogItems handles /clusters/{cluster}/catalogitems[...]
func (s *Server) dispatchCatalogItems(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())

	// /clusters/{cluster}/catalogitems — list
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		if r.Method != http.MethodGet {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadCatalogItems(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		ns := r.URL.Query().Get("namespace")
		filter := CatalogItemFilter{
			Type:   r.URL.Query().Get("type"),
			Source: r.URL.Query().Get("source"),
			Q:      r.URL.Query().Get("q"),
		}
		items, operatorNs, err := s.deps.ConfigBrood.ListCatalogItems(r.Context(), cluster, ns, filter)
		if err != nil {
			s.deps.Logger.Error("list catalog items failed", "cluster", cluster, "namespace", ns, "error", err)
			writeConfigBroodError(w, err, cluster)
			return
		}
		resp := map[string]interface{}{
			"items":             items,
			"operatorNamespace": operatorNs,
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	ns := segments[0]
	if ns == "" {
		http.NotFound(w, r)
		return
	}
	name := segments[1]
	if name == "" {
		http.NotFound(w, r)
		return
	}

	// /clusters/{cluster}/catalogitems/{ns}/{name} — get (read-only)
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadCatalogItems(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	item, err := s.deps.ConfigBrood.GetCatalogItem(r.Context(), cluster, ns, name)
	if err != nil {
		writeConfigBroodError(w, err, cluster)
		return
	}
	var ci v1alpha1.CatalogItem
	_ = json.Unmarshal(item, &ci)
	// The response wraps the item so the per-profile lock-pin projection has
	// somewhere to live. The pins are cluster state and are deliberately not
	// stored on the item, where they would go stale.
	s.writeJSON(w, http.StatusOK, CatalogItemDetailResponse{
		Item:     ci,
		LockPins: s.buildCatalogItemLockPins(r.Context(), &ci),
	})
}

// dispatchCatalogSources handles /clusters/{cluster}/catalogsources[...]
func (s *Server) dispatchCatalogSources(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())

	// /clusters/{cluster}/catalogsources — list/create
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		switch r.Method {
		case http.MethodGet:
			if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadCatalogSources(claims) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			ns := r.URL.Query().Get("namespace")
			items, err := s.deps.ConfigBrood.ListCatalogSources(r.Context(), cluster, ns)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			s.writeJSON(w, http.StatusOK, itemsEnvelope(items))
		case http.MethodPost:
			ns := r.URL.Query().Get("namespace")
			if ns == "" {
				s.writeJSONError(w, http.StatusBadRequest, "namespace query parameter required")
				return
			}
			if !s.deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "create", ns) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var src v1alpha1.CatalogSource
			if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			src.Namespace = ns
			src.APIVersion = "varroa.dev/v1alpha1"
			src.Kind = "CatalogSource"
			obj, _ := json.Marshal(src)
			item, err := s.deps.ConfigBrood.CreateCatalogSource(r.Context(), cluster, ns, src.Name, obj)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			var created v1alpha1.CatalogSource
			_ = json.Unmarshal(item, &created)
			s.notifyActivity(activity.Event{
				Type:      "catalogsource.created",
				Actor:     ActorFrom(claims),
				Namespace: ns,
				Cluster:   cluster,
				Message:   "CatalogSource " + src.Name + " created in " + ns,
			})
			s.writeJSON(w, http.StatusCreated, created)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// /clusters/{cluster}/catalogsources/{ns} — create (legacy path-style namespace)
	if len(segments) == 1 && segments[0] != "" {
		ns := segments[0]
		if r.Method == http.MethodPost {
			if !s.deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "create", ns) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var src v1alpha1.CatalogSource
			if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			src.Namespace = ns
			src.APIVersion = "varroa.dev/v1alpha1"
			src.Kind = "CatalogSource"
			obj, _ := json.Marshal(src)
			item, err := s.deps.ConfigBrood.CreateCatalogSource(r.Context(), cluster, ns, src.Name, obj)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			var created v1alpha1.CatalogSource
			_ = json.Unmarshal(item, &created)
			s.notifyActivity(activity.Event{
				Type:      "catalogsource.created",
				Actor:     ActorFrom(claims),
				Namespace: ns,
				Cluster:   cluster,
				Message:   "CatalogSource " + src.Name + " created in " + ns,
			})
			s.writeJSON(w, http.StatusCreated, created)
			return
		}
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
	}

	ns := segments[0]
	if ns == "" {
		http.NotFound(w, r)
		return
	}

	// /clusters/{cluster}/catalogsources/{ns}/{name}[/sync]
	name := segments[1]
	if name == "" {
		// /clusters/{cluster}/catalogsources/{ns}/sync (slash-action sync by namespace?)
		if segments[1] == "sync" && len(segments) == 2 {
			// No name specified; multi-source sync not supported
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	sub := strings.Join(segments[2:], "/")
	if sub == "sync" {
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !s.deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "update", ns) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := s.deps.ConfigBrood.SyncCatalogSource(r.Context(), cluster, ns, name); err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		if !s.deps.Authorizer.CanManageCatalogSources(claims, "read") {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		item, err := s.deps.ConfigBrood.GetCatalogSource(r.Context(), cluster, ns, name)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var src v1alpha1.CatalogSource
		_ = json.Unmarshal(item, &src)
		s.writeJSON(w, http.StatusOK, src)
	case http.MethodPut:
		if !s.deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "update", ns) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var src v1alpha1.CatalogSource
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		src.Namespace = ns
		src.Name = name
		src.APIVersion = "varroa.dev/v1alpha1"
		src.Kind = "CatalogSource"
		obj, _ := json.Marshal(src)
		item, err := s.deps.ConfigBrood.UpdateCatalogSource(r.Context(), cluster, ns, name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var updated v1alpha1.CatalogSource
		_ = json.Unmarshal(item, &updated)
		s.notifyActivity(activity.Event{
			Type:      "catalogsource.updated",
			Actor:     ActorFrom(claims),
			Namespace: ns,
			Cluster:   cluster,
			Message:   "CatalogSource " + name + " updated in " + ns,
		})
		s.writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if !s.deps.Authorizer.CanManageCatalogSourcesInNamespace(claims, "delete", ns) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := s.deps.ConfigBrood.DeleteCatalogSource(r.Context(), cluster, ns, name); err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		s.notifyActivity(activity.Event{
			Type:      "catalogsource.deleted",
			Actor:     ActorFrom(claims),
			Namespace: ns,
			Cluster:   cluster,
			Message:   "CatalogSource " + name + " deleted from " + ns,
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// dispatchJenkinsRoles handles /clusters/{cluster}/jenkinsroles[...]
// JenkinsRole is cluster-scoped — no namespace segment.
func (s *Server) dispatchJenkinsRoles(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())

	// /clusters/{cluster}/jenkinsroles — list/create
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		switch r.Method {
		case http.MethodGet:
			if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadJenkinsRoles(claims) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			items, err := s.deps.ConfigBrood.ListJenkinsRoles(r.Context(), cluster)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			s.writeJSON(w, http.StatusOK, itemsEnvelope(items))
		case http.MethodPost:
			if s.deps.Authorizer == nil || !s.deps.Authorizer.CanCreateJenkinsRole(claims) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var role v1alpha1.JenkinsRole
			if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			role.APIVersion = "varroa.dev/v1alpha1"
			role.Kind = "JenkinsRole"
			obj, _ := json.Marshal(role)
			item, err := s.deps.ConfigBrood.CreateJenkinsRole(r.Context(), cluster, role.Name, obj)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			var created v1alpha1.JenkinsRole
			_ = json.Unmarshal(item, &created)
			s.notifyActivity(activity.Event{
				Type:    "jenkinsrole.created",
				Actor:   ActorFrom(claims),
				Cluster: cluster,
				Message: "JenkinsRole " + role.Name + " created",
			})
			s.writeJSON(w, http.StatusCreated, created)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// /clusters/{cluster}/jenkinsroles/{name} — get/update/delete
	name := segments[0]
	if name == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadJenkinsRoles(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		item, err := s.deps.ConfigBrood.GetJenkinsRole(r.Context(), cluster, name)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var role v1alpha1.JenkinsRole
		_ = json.Unmarshal(item, &role)
		s.writeJSON(w, http.StatusOK, role)
	case http.MethodPut:
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateJenkinsRole(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var role v1alpha1.JenkinsRole
		if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		role.Name = name
		role.APIVersion = "varroa.dev/v1alpha1"
		role.Kind = "JenkinsRole"
		obj, _ := json.Marshal(role)
		item, err := s.deps.ConfigBrood.UpdateJenkinsRole(r.Context(), cluster, name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var updated v1alpha1.JenkinsRole
		_ = json.Unmarshal(item, &updated)
		s.notifyActivity(activity.Event{
			Type:    "jenkinsrole.updated",
			Actor:   ActorFrom(claims),
			Cluster: cluster,
			Message: "JenkinsRole " + name + " updated",
		})
		s.writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanDeleteJenkinsRole(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := s.deps.ConfigBrood.DeleteJenkinsRole(r.Context(), cluster, name); err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		s.notifyActivity(activity.Event{
			Type:    "jenkinsrole.deleted",
			Actor:   ActorFrom(claims),
			Cluster: cluster,
			Message: "JenkinsRole " + name + " deleted",
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// dispatchJenkinsRoleBindings handles /clusters/{cluster}/jenkinsrolebindings[...]
// JenkinsRoleBinding is cluster-scoped — no namespace segment.
func (s *Server) dispatchJenkinsRoleBindings(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())

	// /clusters/{cluster}/jenkinsrolebindings — list/create
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		switch r.Method {
		case http.MethodGet:
			if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadJenkinsRoleBindings(claims) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			items, err := s.deps.ConfigBrood.ListJenkinsRoleBindings(r.Context(), cluster)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			s.writeJSON(w, http.StatusOK, itemsEnvelope(items))
		case http.MethodPost:
			if s.deps.Authorizer == nil || !s.deps.Authorizer.CanCreateJenkinsRoleBinding(claims) {
				s.writeJSONError(w, http.StatusForbidden, "forbidden")
				return
			}
			var binding v1alpha1.JenkinsRoleBinding
			if err := json.NewDecoder(r.Body).Decode(&binding); err != nil {
				s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			binding.APIVersion = "varroa.dev/v1alpha1"
			binding.Kind = "JenkinsRoleBinding"
			obj, _ := json.Marshal(binding)
			item, err := s.deps.ConfigBrood.CreateJenkinsRoleBinding(r.Context(), cluster, binding.Name, obj)
			if err != nil {
				writeConfigBroodError(w, err, cluster)
				return
			}
			var created v1alpha1.JenkinsRoleBinding
			_ = json.Unmarshal(item, &created)
			s.notifyActivity(activity.Event{
				Type:    "jenkinsrolebinding.created",
				Actor:   ActorFrom(claims),
				Cluster: cluster,
				Message: "JenkinsRoleBinding " + binding.Name + " created",
			})
			s.writeJSON(w, http.StatusCreated, created)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// /clusters/{cluster}/jenkinsrolebindings/{name} — get/update/delete
	name := segments[0]
	if name == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadJenkinsRoleBindings(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		item, err := s.deps.ConfigBrood.GetJenkinsRoleBinding(r.Context(), cluster, name)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var binding v1alpha1.JenkinsRoleBinding
		_ = json.Unmarshal(item, &binding)
		s.writeJSON(w, http.StatusOK, binding)
	case http.MethodPut:
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateJenkinsRoleBinding(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var binding v1alpha1.JenkinsRoleBinding
		if err := json.NewDecoder(r.Body).Decode(&binding); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		binding.Name = name
		binding.APIVersion = "varroa.dev/v1alpha1"
		binding.Kind = "JenkinsRoleBinding"
		obj, _ := json.Marshal(binding)
		item, err := s.deps.ConfigBrood.UpdateJenkinsRoleBinding(r.Context(), cluster, name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var updated v1alpha1.JenkinsRoleBinding
		_ = json.Unmarshal(item, &updated)
		s.notifyActivity(activity.Event{
			Type:    "jenkinsrolebinding.updated",
			Actor:   ActorFrom(claims),
			Cluster: cluster,
			Message: "JenkinsRoleBinding " + name + " updated",
		})
		s.writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanDeleteJenkinsRoleBinding(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := s.deps.ConfigBrood.DeleteJenkinsRoleBinding(r.Context(), cluster, name); err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		s.notifyActivity(activity.Event{
			Type:    "jenkinsrolebinding.deleted",
			Actor:   ActorFrom(claims),
			Cluster: cluster,
			Message: "JenkinsRoleBinding " + name + " deleted",
		})
		w.WriteHeader(http.StatusNoContent)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleControllerPlugins serves GET /clusters/{cluster}/controllers/{ns}/{name}/plugins.
func (s *Server) handleControllerPlugins(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadController(claims, namespace, name) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// The classified inventory is held in THIS BFF's mite registry and the
	// local CRD store; there is no cross-cluster read path for it. Reject a
	// remote cluster explicitly instead of falling through: the reads below are
	// keyed only on namespace/name, so a remote URL whose namespace/name happen
	// to exist locally would return the LOCAL controller's inventory under the
	// remote cluster's URL — the wrong controller's software list, presented as
	// the right one.
	if s.deps.Brood != nil && cluster != s.deps.Brood.LocalCluster() {
		s.writeJSONError(w, http.StatusNotImplemented,
			"plugin inventory is available for the local cluster only")
		return
	}

	ctx := r.Context()
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, name, namespace)
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "controller not found")
		return
	}

	classified, hasClassification := s.deps.MiteRegistry.PluginClassification(namespace, name)

	type classPluginJSON struct {
		Name           string   `json:"name"`
		Version        string   `json:"version"`
		Class          string   `json:"class"`
		DeclaredBy     string   `json:"declaredBy,omitempty"`
		Contributors   []string `json:"contributors,omitempty"`
		ImpliedBy      []string `json:"impliedBy,omitempty"`
		VersionVerdict string   `json:"versionVerdict,omitempty"`
		Enabled        string   `json:"enabled,omitempty"`
		Detached       string   `json:"detached,omitempty"`
		Bundled        string   `json:"bundled,omitempty"`
	}
	type advJSON struct {
		Code       string `json:"code"`
		Plugin     string `json:"plugin"`
		Dependency string `json:"dependency"`
		Min        string `json:"min"`
		Version    string `json:"version"`
	}
	type pluginsResponse struct {
		Hash                 string            `json:"hash"`
		CollectedAt          string            `json:"collectedAt,omitempty"`
		ObservedAt           string            `json:"observedAt,omitempty"`
		Source               string            `json:"source"`
		Stale                bool              `json:"stale"`
		Degraded             bool              `json:"degraded"`
		BootstrapApproximate bool              `json:"bootstrapApproximate"`
		OptionalEdgesDropped bool              `json:"optionalEdgesDropped"`
		Truncated            bool              `json:"truncated"`
		Total                int               `json:"total"`
		Counts               map[string]int    `json:"counts,omitempty"`
		DriftTruncated       bool              `json:"driftTruncated"`
		Plugins              []classPluginJSON `json:"plugins,omitempty"`
		Advisories           []advJSON         `json:"advisories,omitempty"`
		DetailStale          bool              `json:"detailStale"`
		DetailAvailable      bool              `json:"detailAvailable"`
	}

	resp := pluginsResponse{DetailAvailable: false}

	if hasClassification && classified != nil {
		resp.DetailAvailable = true
		resp.Hash = classified.Envelope.Hash
		resp.Source = classified.Envelope.Source
		resp.Stale = classified.Envelope.Stale
		resp.Degraded = classified.Envelope.Degraded
		resp.BootstrapApproximate = classified.Envelope.BootstrapApproximate
		resp.OptionalEdgesDropped = classified.Envelope.OptionalEdgesDropped
		resp.Truncated = classified.Envelope.Truncated
		resp.Total = classified.Envelope.Total
		resp.Counts = classified.Envelope.Counts
		resp.DriftTruncated = classified.Envelope.DriftTruncated
		if !classified.Envelope.CollectedAt.IsZero() {
			resp.CollectedAt = classified.Envelope.CollectedAt.Format(time.RFC3339)
		}
		if !classified.Envelope.ObservedAt.IsZero() {
			resp.ObservedAt = classified.Envelope.ObservedAt.Format(time.RFC3339)
		}

		if cr.Status.PluginInventory != nil {
			si := cr.Status.PluginInventory
			if resp.Hash != si.Hash || resp.Stale != si.Stale ||
				resp.Degraded != si.Degraded || resp.BootstrapApproximate != si.BootstrapApproximate ||
				resp.Source != si.Source || resp.Truncated != si.Truncated ||
				resp.OptionalEdgesDropped != si.OptionalEdgesDropped || resp.DriftTruncated != si.DriftTruncated {
				resp.DetailStale = true
			}
		} else {
			resp.DetailStale = true
		}

		for _, p := range classified.Plugins {
			resp.Plugins = append(resp.Plugins, classPluginJSON{
				Name:           p.Name,
				Version:        p.Version,
				Class:          p.Class,
				DeclaredBy:     p.DeclaredBy,
				Contributors:   p.Contributors,
				ImpliedBy:      p.ImpliedBy,
				VersionVerdict: p.VersionVerdict,
				Enabled:        p.Enabled,
				Detached:       p.Detached,
				Bundled:        p.Bundled,
			})
		}
		for _, a := range classified.Advisories {
			resp.Advisories = append(resp.Advisories, advJSON{
				Code: a.Code, Plugin: a.Plugin, Dependency: a.Dependency,
				Min: a.Min, Version: a.Version,
			})
		}
	} else {
		if cr.Status.PluginInventory != nil {
			pi := cr.Status.PluginInventory
			resp.Hash = pi.Hash
			resp.Source = pi.Source
			resp.Stale = pi.Stale
			resp.Degraded = pi.Degraded
			resp.BootstrapApproximate = pi.BootstrapApproximate
			resp.OptionalEdgesDropped = pi.OptionalEdgesDropped
			resp.Truncated = pi.Truncated
			resp.Total = pi.Total
			resp.Counts = pi.Counts
			resp.DriftTruncated = pi.DriftTruncated
			if pi.CollectedAt != nil {
				resp.CollectedAt = pi.CollectedAt.Format(time.RFC3339)
			}
			if pi.ObservedAt != nil {
				resp.ObservedAt = pi.ObservedAt.Format(time.RFC3339)
			}
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}
