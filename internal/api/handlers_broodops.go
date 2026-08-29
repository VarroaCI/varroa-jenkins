package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// broodCreateRequest is the JSON body for POST /brood-operations.
type broodCreateRequest struct {
	Namespace string                      `json:"namespace,omitempty"`
	Spec      v1alpha1.BroodOperationSpec `json:"spec"`
	Clusters  []string                    `json:"clusters,omitempty"`
}

// broodPreviewResponse is the JSON body for POST /brood-operations/preview.
type broodPreviewResponse struct {
	Clusters []clusterPreviewSection `json:"clusters"`
}

type clusterPreviewSection struct {
	Cluster string               `json:"cluster"`
	OK      bool                 `json:"ok"`
	Error   string               `json:"error,omitempty"`
	Targets []broodPreviewTarget `json:"targets,omitempty"`
}

type broodPreviewTarget struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Wave       int32  `json:"wave"`
	Applicable bool   `json:"applicable"`
	Reason     string `json:"reason,omitempty"`
}

// suspendRequest is the JSON body for POST /brood-operations/{ns}/{name}/suspend.
type suspendRequest struct {
	Suspend bool `json:"suspend"`
}

// BroodRun is the detail DTO for a logical brood operation run.
type BroodRun struct {
	Namespace string                       `json:"namespace"`
	Name      string                       `json:"name"`
	Verb      v1alpha1.BroodVerb           `json:"verb"`
	Phase     v1alpha1.BroodOperationPhase `json:"phase"`
	Summary   v1alpha1.BroodSummary        `json:"summary"`
	StartedBy string                       `json:"startedBy,omitempty"`
	CreatedAt string                       `json:"createdAt,omitempty"`
	Clusters  []BroodRunCluster            `json:"clusters"`
}

// BroodRunCluster is a per-cluster section in a BroodRun detail.
type BroodRunCluster struct {
	Cluster string                   `json:"cluster"`
	OK      bool                     `json:"ok"`
	Error   string                   `json:"error,omitempty"`
	Op      *v1alpha1.BroodOperation `json:"op,omitempty"`
}

// BroodRunClusterStatus is a per-cluster fan-out outcome row on a list/detail
// response. It mirrors the internal ClusterFanoutStatus but uses the API's
// `cluster` field name (the shared ClusterFanoutStatus serializes `name`).
type BroodRunClusterStatus struct {
	Cluster string `json:"cluster"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// fanoutStatuses maps internal fan-out status rows to the API DTO.
func fanoutStatuses(in []ClusterFanoutStatus) []BroodRunClusterStatus {
	out := make([]BroodRunClusterStatus, 0, len(in))
	for _, st := range in {
		out = append(out, BroodRunClusterStatus{Cluster: st.Name, OK: st.OK, Error: st.Error})
	}
	return out
}

// BroodRunSummaryRow is a list item for a logical brood operation run.
type BroodRunSummaryRow struct {
	Namespace string                       `json:"namespace"`
	Name      string                       `json:"name"`
	Verb      v1alpha1.BroodVerb           `json:"verb"`
	Phase     v1alpha1.BroodOperationPhase `json:"phase"`
	Summary   v1alpha1.BroodSummary        `json:"summary"`
	StartedBy string                       `json:"startedBy,omitempty"`
	CreatedAt string                       `json:"createdAt,omitempty"`
	Clusters  []string                     `json:"clusters"`
}

// BroodCreateFailure is the error body when zero clusters succeed on create.
type BroodCreateFailure struct {
	Error    string                `json:"error"`
	Clusters []ClusterCreateResult `json:"clusters"`
}

// handleBroodOperations routes /brood-operations and /brood-operations/ paths.
func (s *Server) handleBroodOperations(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/brood-operations") {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(path, "/brood-operations")

	switch suffix {
	case "", "/":
		switch r.Method {
		case http.MethodGet:
			s.handleBroodList(w, r)
		case http.MethodPost:
			s.handleBroodCreate(w, r)
		default:
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "/preview":
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleBroodPreview(w, r)
	default:
		segments := strings.Split(strings.Trim(suffix, "/"), "/")
		switch {
		case len(segments) == 2 && segments[0] != "" && segments[1] != "":
			switch r.Method {
			case http.MethodGet:
				s.handleBroodDetail(w, r, segments[0], segments[1])
			case http.MethodDelete:
				s.handleBroodDelete(w, r, segments[0], segments[1])
			default:
				s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case len(segments) == 3 && segments[0] != "" && segments[1] != "":
			switch segments[2] {
			case "suspend":
				if r.Method != http.MethodPost {
					s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
					return
				}
				s.handleBroodSuspend(w, r, segments[0], segments[1])
			case "stream":
				if r.Method != http.MethodGet {
					s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
					return
				}
				s.handleBroodStream(w, r, segments[0], segments[1])
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}
}

// broodOpAccess is the RBAC gate for brood-operation endpoints: namespace-
// scoped (a BroodOperation name is not a Controller name, so per-name checks
// would resolve the wrong scope), with operator-namespace runs admin-only.
func (s *Server) broodOpAccess(claims *auth.Claims, ns string, manage bool) bool {
	if claims == nil || s.deps.Authorizer == nil {
		return false
	}
	if ns == s.deps.OperatorNamespace && !s.deps.Authorizer.IsAdmin(claims) {
		return false
	}
	if manage {
		return s.deps.Authorizer.CanManageController(claims, ns, "")
	}
	return s.deps.Authorizer.CanReadController(claims, ns, "")
}

// --- 4.3 Create ---

// handleBroodCreate handles POST /brood-operations (operator+).
func (s *Server) handleBroodCreate(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req broodCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Determine target namespace.
	ns := req.Namespace
	if ns == "" {
		ns = s.deps.OperatorNamespace
	}

	if !s.broodOpAccess(claims, ns, true) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Tenancy pre-check (best-effort, core operatorNS).
	if err := controller.ValidateBroodTenancy(req.Spec, ns, s.deps.OperatorNamespace); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verb policy pre-check. This is UX, not enforcement: kubectl and GitOps
	// create BroodOperation objects directly and never reach this handler, so
	// the operator's gate is the real boundary. Checking here only spares the
	// user an object that would be created and immediately fail. A read failure
	// falls through to the operator for the same reason it does there — absence
	// and error are indistinguishable, and the verb is enabled by default.
	//
	// Scoped to local-only operations on purpose. Each cluster's operator
	// enforces ITS OWN ProvisioningDefaults, so this BFF, which can read only
	// the local one, has no authority over a remote target cluster. Applying the
	// local policy to a fan-out would reject an operation a remote cluster
	// permits — and could not have approved one it forbids anyway. Mixed
	// outcomes surface per-cluster in the fan-out response instead.
	if req.Spec.Action.Verb == v1alpha1.BroodVerbExecuteGroovy && s.localOnlyRequest(req) {
		if defaults, dErr := crdstore.Get[v1alpha1.ProvisioningDefaults](
			r.Context(), s.deps.Store, provisioningDefaultsName, ""); dErr == nil && defaults != nil {
			if ok, why := defaults.Spec.BroodPolicy.ExecuteGroovyAllowed(ns); !ok {
				s.writeJSONError(w, http.StatusForbidden, why)
				return
			}
		}
	}

	// Grammar validation.
	specs, err := validateAndPartition(req, s.deps.Brood.LocalCluster(), func(cluster string) bool {
		return s.deps.Brood.IsKnown(r.Context(), cluster)
	}, s.deps.OperatorNamespace)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Gather ordered cluster list from specs keys.
	orderedClusters := make([]string, 0, len(specs))
	for c := range specs {
		orderedClusters = append(orderedClusters, c)
	}

	// Mint name.
	name := mintBroodOpName(req.Spec.Action.Verb)

	// Fan-out create.
	results := s.deps.BroodOps.Create(r.Context(), orderedClusters, ns, name, specs, claims.PreferredUsername)

	// Build response per D-7.
	var succeeded []ClusterCreateResult
	var failed []ClusterCreateResult
	for _, r := range results {
		if r.OK {
			succeeded = append(succeeded, r)
		} else {
			failed = append(failed, r)
		}
	}

	if len(succeeded) > 0 {
		// 201 with BroodRun body.
		run := buildBroodRunFromCreateResults(ns, name, results)
		s.writeJSON(w, http.StatusCreated, run)
		return
	}

	// Zero successes.
	anyTransport := false
	for _, f := range failed {
		if strings.Contains(f.Error, "unreachable") {
			anyTransport = true
			break
		}
	}

	msg := "all clusters failed"
	if anyTransport {
		s.writeJSONErrorStatus(w, http.StatusBadGateway, msg, map[string]interface{}{
			"clusters": results,
		})
		return
	}

	// Use first cluster's code.
	code := mapCodeToHTTP(failed[0].Code)
	s.writeJSONErrorStatus(w, code, msg, map[string]interface{}{
		"clusters": results,
	})
}

func buildBroodRunFromCreateResults(ns, name string, results []ClusterCreateResult) *BroodRun {
	run := &BroodRun{
		Namespace: ns,
		Name:      name,
		Clusters:  make([]BroodRunCluster, 0, len(results)),
	}
	var verb v1alpha1.BroodVerb
	for _, r := range results {
		fc := BroodRunCluster{
			Cluster: r.Cluster,
			OK:      r.OK,
			Error:   r.Error,
		}
		if r.OK && r.Op != nil {
			fc.Op = r.Op
			if verb == "" {
				verb = r.Op.Spec.Action.Verb
			}
		}
		run.Clusters = append(run.Clusters, fc)
	}
	run.Verb = verb

	// Compute phase and summary from reachable children.
	var phases []v1alpha1.BroodOperationPhase
	var summaries []v1alpha1.BroodSummary
	for _, r := range results {
		if r.OK && r.Op != nil {
			phases = append(phases, r.Op.Status.Phase)
			summaries = append(summaries, r.Op.Status.Summary)
		}
	}
	if phase, ok := aggregatePhase(phases); ok {
		run.Phase = phase
	}
	run.Summary = sumSummaries(summaries)
	return run
}

// --- 4.4 Preview ---

// handleBroodPreview handles POST /brood-operations/preview (viewer+).
func (s *Server) handleBroodPreview(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req broodCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = s.deps.OperatorNamespace
	}

	if !s.broodOpAccess(claims, ns, false) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Grammar validation (same path as create).
	specs, err := validateAndPartition(req, s.deps.Brood.LocalCluster(), func(cluster string) bool {
		return s.deps.Brood.IsKnown(r.Context(), cluster)
	}, s.deps.OperatorNamespace)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	orderedClusters := make([]string, 0, len(specs))
	for c := range specs {
		orderedClusters = append(orderedClusters, c)
	}

	// Fan-out preview.
	results := s.deps.BroodOps.Preview(r.Context(), orderedClusters, ns, specs)

	// Build response.
	resp := broodPreviewResponse{
		Clusters: make([]clusterPreviewSection, 0, len(results)),
	}
	var succeeded, failed int
	for _, r := range results {
		section := clusterPreviewSection{
			Cluster: r.Cluster,
			OK:      r.OK,
			Error:   r.Error,
		}
		if r.OK {
			section.Targets = make([]broodPreviewTarget, len(r.Targets))
			for i, t := range r.Targets {
				section.Targets[i] = broodPreviewTarget{
					Namespace:  t.Namespace,
					Name:       t.Name,
					Wave:       t.Wave,
					Applicable: t.Applicable,
					Reason:     t.Reason,
				}
			}
			succeeded++
		} else {
			failed++
		}
		resp.Clusters = append(resp.Clusters, section)
	}

	if succeeded > 0 {
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	// Zero resolved.
	anyTransport := false
	for _, r := range results {
		if strings.Contains(r.Error, "unreachable") {
			anyTransport = true
			break
		}
	}

	if anyTransport {
		s.writeJSONErrorStatus(w, http.StatusBadGateway, "all clusters unreachable", map[string]interface{}{
			"clusters": results,
		})
		return
	}

	s.writeJSONErrorStatus(w, mapCodeToHTTP(results[0].Code), "all clusters failed", map[string]interface{}{
		"clusters": results,
	})
}

// --- 4.5 List / Get ---

// handleBroodList handles GET /brood-operations[?namespace=][&cluster=] (viewer+).
func (s *Server) handleBroodList(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	ns := r.URL.Query().Get("namespace")
	clusterFilter := r.URL.Query().Get("cluster")
	isAdmin := s.deps.Authorizer != nil && s.deps.Authorizer.IsAdmin(claims)

	if !isAdmin && ns != "" && !s.broodOpAccess(claims, ns, false) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Validate ?cluster= if set.
	if clusterFilter != "" {
		if !s.deps.Brood.IsKnown(r.Context(), clusterFilter) {
			s.writeJSONError(w, http.StatusNotFound, "unknown cluster: "+clusterFilter)
			return
		}
	}

	// Fan-out list.
	ops, statuses, err := s.deps.BroodOps.List(r.Context(), ns, clusterFilter)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Optional ?startedBy= filter (task 4.3).
	if startedBy := r.URL.Query().Get("startedBy"); startedBy != "" {
		var filtered []ClusterBroodOp
		for _, op := range ops {
			if op.Op != nil && op.Op.Status.StartedBy == startedBy {
				filtered = append(filtered, op)
			}
		}
		ops = filtered
	}

	// Group by (namespace, name).
	groups := groupRuns(ops)

	// Apply namespace-visibility filter for non-admin callers.
	items := make([]BroodRunSummaryRow, 0, len(groups))
	for _, g := range groups {
		if !isAdmin && !s.broodOpAccess(claims, g.Namespace, false) {
			continue
		}
		row := buildSummaryRow(g)
		items = append(items, row)
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":    items,
		"clusters": fanoutStatuses(statuses),
	})
}

func buildSummaryRow(g broodRunGroup) BroodRunSummaryRow {
	row := BroodRunSummaryRow{
		Namespace: g.Namespace,
		Name:      g.Name,
		Clusters:  make([]string, 0, len(g.Children)),
	}
	var phases []v1alpha1.BroodOperationPhase
	var summaries []v1alpha1.BroodSummary
	for _, child := range g.Children {
		row.Clusters = append(row.Clusters, child.Cluster)
		if child.Op != nil {
			phases = append(phases, child.Op.Status.Phase)
			summaries = append(summaries, child.Op.Status.Summary)
			if row.Verb == "" {
				row.Verb = child.Op.Spec.Action.Verb
			}
			if row.StartedBy == "" {
				row.StartedBy = child.Op.Status.StartedBy
			}
			if row.CreatedAt == "" && !child.Op.CreationTimestamp.IsZero() {
				row.CreatedAt = child.Op.CreationTimestamp.Format(time.RFC3339)
			}
		}
	}
	if phase, ok := aggregatePhase(phases); ok {
		row.Phase = phase
	}
	row.Summary = sumSummaries(summaries)
	return row
}

// handleBroodDetail handles GET /brood-operations/{ns}/{name} (viewer+).
func (s *Server) handleBroodDetail(w http.ResponseWriter, r *http.Request, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if !s.broodOpAccess(claims, ns, false) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Fan-out get to all clusters.
	ops, statuses, err := s.deps.BroodOps.Get(r.Context(), ns, name)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Check reachable children.
	reachable := make([]ClusterBroodOp, 0, len(ops))
	var unreachableClusters []string
	for _, st := range statuses {
		if !st.OK {
			unreachableClusters = append(unreachableClusters, st.Name)
		}
	}
	reachable = append(reachable, ops...)

	if len(reachable) == 0 {
		if len(unreachableClusters) > 0 {
			// 502 — at least one cluster unreachable and no reachable child.
			s.writeJSONError(w, http.StatusBadGateway, "run not found on reachable clusters, unreachable: "+strings.Join(unreachableClusters, ","))
			return
		}
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	// Build BroodRun DTO.
	run := buildBroodRunFromDetail(reachable, statuses)
	s.writeJSON(w, http.StatusOK, run)
}

func buildBroodRunFromDetail(children []ClusterBroodOp, statuses []ClusterFanoutStatus) *BroodRun {
	run := &BroodRun{
		Clusters: make([]BroodRunCluster, 0, len(children)+len(statuses)),
	}
	var phases []v1alpha1.BroodOperationPhase
	var summaries []v1alpha1.BroodSummary

	// Add cluster sections for reachable children.
	for _, child := range children {
		if child.Op == nil {
			continue
		}
		fc := BroodRunCluster{
			Cluster: child.Cluster,
			OK:      true,
			Op:      child.Op,
		}
		run.Clusters = append(run.Clusters, fc)
		if run.Namespace == "" {
			run.Namespace = child.Op.Namespace
		}
		if run.Name == "" {
			run.Name = child.Op.Name
		}
		if run.Verb == "" {
			run.Verb = child.Op.Spec.Action.Verb
		}
		if run.StartedBy == "" {
			run.StartedBy = child.Op.Status.StartedBy
		}
		if run.CreatedAt == "" && !child.Op.CreationTimestamp.IsZero() {
			run.CreatedAt = child.Op.CreationTimestamp.Format(time.RFC3339)
		}
		phases = append(phases, child.Op.Status.Phase)
		summaries = append(summaries, child.Op.Status.Summary)
	}

	// Add error sections for unreachable clusters.
	for _, st := range statuses {
		if !st.OK {
			found := false
			for _, child := range children {
				if child.Cluster == st.Name {
					found = true
					break
				}
			}
			if !found {
				run.Clusters = append(run.Clusters, BroodRunCluster{
					Cluster: st.Name,
					OK:      false,
					Error:   st.Error,
				})
			}
		}
	}

	if phase, ok := aggregatePhase(phases); ok {
		run.Phase = phase
	}
	run.Summary = sumSummaries(summaries)
	return run
}

// --- 4.6 Cancel / Suspend ---

// handleBroodDelete handles DELETE /brood-operations/{ns}/{name} (operator+).
func (s *Server) handleBroodDelete(w http.ResponseWriter, r *http.Request, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if !s.broodOpAccess(claims, ns, true) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Resolve member clusters via Get fan-out.
	children, _, err := s.deps.BroodOps.Get(r.Context(), ns, name)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Check if any cluster holds the run.
	holdingClusters := make([]string, 0)
	for _, child := range children {
		if child.Op != nil {
			holdingClusters = append(holdingClusters, child.Cluster)
		}
	}
	if len(holdingClusters) == 0 {
		s.writeJSONError(w, http.StatusNotFound, "run not found on any cluster")
		return
	}

	// Fan out cancel.
	results := s.deps.BroodOps.Cancel(r.Context(), holdingClusters, ns, name)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"clusters": results,
	})
}

// handleBroodSuspend handles POST /brood-operations/{ns}/{name}/suspend (operator+).
func (s *Server) handleBroodSuspend(w http.ResponseWriter, r *http.Request, ns, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		s.writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if !s.broodOpAccess(claims, ns, true) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req suspendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Resolve member clusters via Get fan-out.
	children, _, err := s.deps.BroodOps.Get(r.Context(), ns, name)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	holdingClusters := make([]string, 0)
	for _, child := range children {
		if child.Op != nil {
			holdingClusters = append(holdingClusters, child.Cluster)
		}
	}
	if len(holdingClusters) == 0 {
		s.writeJSONError(w, http.StatusNotFound, "run not found on any cluster")
		return
	}

	// Fan out suspend.
	results := s.deps.BroodOps.Suspend(r.Context(), holdingClusters, ns, name, req.Suspend)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"clusters": results,
	})
}

// --- 4.7 SSE Stream ---

// broodStreamMaxDuration bounds the total lifetime of a brood-operation SSE
// watch stream (and its poll loop). An operation whose reachable target set
// is empty — e.g. every target cluster is currently unreachable over the bus
// fan-out — is deliberately NOT treated as terminal by itself, since targets
// may become reachable again later (a hibernated controller waking up, a
// transient bus RPC timeout clearing). Without a hard cap, that combination
// polls forever and hangs a `varroactl broodop run --watch` client.
// One hour matches scheduleJobActiveDeadlineSeconds
// (broodschedule_controller.go) — the bound already accepted elsewhere in
// this codebase for how long we wait on a whole brood operation to finish
// end-to-end.
const broodStreamMaxDuration = time.Hour

// terminalBroodPhase aggregates the phase across reachable children (those
// with a non-nil Op) and reports whether that aggregate is terminal
// (Succeeded/Failed/Canceled). It returns false both when no reachable
// child currently reports a phase and when the aggregate is non-terminal.
func terminalBroodPhase(children []ClusterBroodOp) bool {
	var phases []v1alpha1.BroodOperationPhase
	for _, child := range children {
		if child.Op != nil {
			phases = append(phases, child.Op.Status.Phase)
		}
	}
	if len(phases) == 0 {
		return false
	}
	phase, ok := aggregatePhase(phases)
	if !ok {
		return false
	}
	switch phase {
	case v1alpha1.BroodOperationPhaseSucceeded,
		v1alpha1.BroodOperationPhaseFailed,
		v1alpha1.BroodOperationPhaseCanceled:
		return true
	default:
		return false
	}
}

// handleBroodStream handles GET /brood-operations/{ns}/{name}/stream (viewer+).
func (s *Server) handleBroodStream(w http.ResponseWriter, r *http.Request, ns, name string) {
	s.handleBroodStreamWithPoll(w, r, ns, name, 2*time.Second, broodStreamMaxDuration)
}

// handleBroodStreamWithPoll is like handleBroodStream but with adjustable poll
// interval and max stream duration, for testability.
func (s *Server) handleBroodStreamWithPoll(w http.ResponseWriter, r *http.Request, ns, name string, pollInterval, maxDuration time.Duration) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil || !s.deps.Authorizer.CanReadController(claims, ns, "") {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Pre-check: do one BroodOps.Get to decide 404 vs 502 vs proceed.
	children, statuses, err := s.deps.BroodOps.Get(r.Context(), ns, name)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var reachable bool
	var unreachable bool
	for _, child := range children {
		if child.Op != nil {
			reachable = true
			break
		}
	}
	for _, st := range statuses {
		if !st.OK {
			unreachable = true
			break
		}
	}

	if !reachable {
		if unreachable {
			s.writeJSONError(w, http.StatusBadGateway, "run unreachable")
			return
		}
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSONError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	lastDTO := buildBroodRunFromDetail(children, statuses)
	sendSSEBroodRun(w, flusher, lastDTO)

	// The operation may already be terminal at connect time (e.g. it
	// finished and its CR was garbage-collected in the gap between the
	// pre-check above and the first poll tick below). Catch that now,
	// rather than only inside the poll loop — otherwise the very next poll
	// could observe zero reachable children and poll forever without ever
	// having noticed the terminal phase.
	if terminalBroodPhase(children) {
		sendSSEClosed(w, flusher, "", "")
		return
	}

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()

	// Server-side deadline: an empty/never-terminal reachable target set
	// must not hold this stream (and its poll loop) open forever.
	deadline := time.NewTimer(maxDuration)
	defer deadline.Stop()

	lastSnapshot := snapshotFromBroodRun(lastDTO)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			sendSSEClosed(w, flusher, "deadline_exceeded",
				fmt.Sprintf("watch exceeded max duration of %s without reaching a terminal phase; the operation may still be running — check its status separately", maxDuration))
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-poll.C:
			children, statuses, err := s.deps.BroodOps.Get(r.Context(), ns, name)
			if err != nil {
				sendSSEClosed(w, flusher, "", "")
				return
			}

			dto := buildBroodRunFromDetail(children, statuses)
			snap := snapshotFromBroodRun(dto)

			if snap != lastSnapshot {
				lastSnapshot = snap
				sendSSEBroodRun(w, flusher, dto)
			}

			// Check if the aggregated phase is terminal. This applies
			// regardless of whether every target is currently reachable —
			// as long as at least one child reports a phase, a terminal
			// aggregate closes the stream immediately rather than waiting
			// for the deadline. The final status was already emitted above
			// (the snap != lastSnapshot check unconditionally syncs
			// lastSnapshot before we get here), so there's nothing left to
			// send but the close event.
			if terminalBroodPhase(children) {
				sendSSEClosed(w, flusher, "", "")
				return
			}

			// On a genuinely empty reachable set (no child reports a
			// phase at all): emit nothing, keep polling — targets may
			// become reachable later (e.g. a hibernated controller waking
			// up, or a disconnected cluster reconnecting). The deadline
			// timer above is what eventually bounds this case; it is
			// deliberately not treated as terminal here.
		}
	}
}

// snapshotFromBroodRun returns a string that changes when any child's state changes.
func snapshotFromBroodRun(run *BroodRun) string {
	if run == nil {
		return ""
	}
	var parts []string
	for _, c := range run.Clusters {
		rv := ""
		if c.Op != nil {
			rv = c.Op.ResourceVersion
		}
		parts = append(parts, c.Cluster+":"+rv)
	}
	return strings.Join(parts, "|")
}

func sendSSEBroodRun(w http.ResponseWriter, flusher http.Flusher, run *BroodRun) {
	js, err := json.Marshal(run)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", js)
	flusher.Flush()
}

// sendSSEClosed emits the terminal "closed" SSE event that tells watch
// clients (varroactl broodop run --watch) to stop reading and exit. reason
// and message are optional: when both are empty, the event carries a bare
// `{}` payload (the normal terminal-phase / fan-out-error close). When set,
// message becomes the informative status the client surfaces on exit — used
// for the deadline-expiry close, where the stream ends without ever
// observing a terminal phase.
func sendSSEClosed(w http.ResponseWriter, flusher http.Flusher, reason, message string) {
	if reason == "" && message == "" {
		fmt.Fprintf(w, "event: closed\ndata: {}\n\n")
		flusher.Flush()
		return
	}
	payload := map[string]string{}
	if reason != "" {
		payload["reason"] = reason
	}
	if message != "" {
		payload["message"] = message
	}
	js, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(w, "event: closed\ndata: {}\n\n")
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "event: closed\ndata: %s\n\n", js)
	flusher.Flush()
}

// --- Helpers ---

func mapCodeToHTTP(code string) int {
	switch code {
	case "not_found":
		return http.StatusNotFound
	case "conflict":
		return http.StatusConflict
	case "invalid":
		return http.StatusBadRequest
	case "internal":
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) writeJSONErrorStatus(w http.ResponseWriter, status int, msg string, extra map[string]interface{}) {
	body := map[string]interface{}{
		"error": msg,
	}
	for k, v := range extra {
		body[k] = v
	}
	s.writeJSON(w, status, body)
}

// provisioningDefaultsName is the cluster-scoped ProvisioningDefaults singleton.
// Mirrors the operator-side constant; the object is optional, so absence and
// read failure both mean "no policy".
const provisioningDefaultsName = "varroa-defaults"

// localOnlyRequest reports whether a create request targets nothing but the
// local cluster, which is the only case where the BFF's view of
// ProvisioningDefaults is the same one that will enforce the operation.
func (s *Server) localOnlyRequest(req broodCreateRequest) bool {
	if s.deps.Brood == nil {
		return true
	}
	local := s.deps.Brood.LocalCluster()
	for _, c := range req.Clusters {
		if c != "" && c != local {
			return false
		}
	}
	return true
}
