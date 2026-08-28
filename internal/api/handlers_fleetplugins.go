package api

import (
	"context"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/pluginrange"
)

// ---------------------------------------------------------------------------
// Common request pipeline — shared between rollup and drilldown
// ---------------------------------------------------------------------------

// fleetPluginParams holds the parsed and validated request state for both
// fleet plugin endpoints. The pipeline runs in the order fixed by design.md §5.
type fleetPluginParams struct {
	affected  pluginrange.Expr
	cluster   string
	namespace string
	claims    *auth.Claims
}

// parseFleetPluginParams performs steps 1–4 of the pipeline:
// GET-only → parse affected → validate cluster → nil-reader guard.
// Returns params and nil on success. On failure writes the error and returns nil.
func (s *Server) parseFleetPluginParams(w http.ResponseWriter, r *http.Request) *fleetPluginParams {
	// Step 1: GET only.
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return nil
	}

	// Step 2: Parse affected.
	// Use Has("affected"), never a non-empty check — a present-but-blank value
	// must be rejected with 400.
	var affected pluginrange.Expr
	if r.URL.Query().Has("affected") {
		affectedRaw := r.URL.Query().Get("affected")
		// A present-but-blank value (including whitespace-only) is a caller
		// mistake — a truncated paste or a template that did not interpolate.
		// The safe response is 400, not silently returning the unfiltered fleet.
		if strings.TrimSpace(affectedRaw) == "" {
			s.writeJSONError(w, http.StatusBadRequest, "pluginrange: empty clause in clause \"\"")
			return nil
		}
		var err error
		affected, err = pluginrange.Parse(affectedRaw)
		if err != nil {
			s.writeJSONError(w, http.StatusBadRequest, err.Error())
			return nil
		}
	}

	cluster := r.URL.Query().Get("cluster")
	namespace := r.URL.Query().Get("namespace")
	claims := auth.ClaimsFromContext(r.Context())

	// Step 3: Validate cluster against Brood.IsKnown.
	ctx := context.Background()
	if cluster != "" && s.deps.Brood != nil && !s.deps.Brood.IsKnown(ctx, cluster) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown cluster"})
		return nil
	}

	// Step 4: Nil-reader guard.
	// This is an explicit check, not an emergent per-row property: an empty
	// fleet, a fully filtered fleet, or Authorizer == nil all yield zero rows,
	// and every one of them must still return 502. The guard runs after request
	// validation (so a malformed affected still 400s) and before the fan-out
	// (so a broken BFF costs no cluster reads).
	if s.deps.FleetPluginInventory == nil {
		s.deps.Logger.Error("fleet plugin inventory dependency is unwired")
		s.writeJSONError(w, http.StatusBadGateway, "fleet plugin inventory is not available")
		return nil
	}

	return &fleetPluginParams{
		affected:  affected,
		cluster:   cluster,
		namespace: namespace,
		claims:    claims,
	}
}

// fleetFanOutResult holds the output of the fan-out step.
type fleetFanOutResult struct {
	statuses []ClusterFanoutStatus
	rows     []controllerRow
}

// fleetFanOut performs steps 5–8 of the pipeline:
// fan out → mark coverage → authorize → read.
func (s *Server) fleetFanOut(ctx context.Context, p *fleetPluginParams) *fleetFanOutResult {
	// Step 5: Fan out with one unfiltered call.
	// Retain every returned ClusterFanoutStatus as the coverage status set.
	// Apply the cluster query parameter to rows only.
	// A filtered ListAll returns only that cluster's status (brood.go:236) and
	// must not be the call that produces coverage.
	type enriched struct {
		cluster, ns, name string
		phase             v1alpha1.ControllerPhase
		pluginInv         *v1alpha1.PluginInventoryStatus
	}

	var statuses []ClusterFanoutStatus
	var enrichedRows []enriched

	if s.deps.Brood != nil {
		// Unfiltered call: namespace="" so we see all controllers.
		cc, cs, err := s.deps.Brood.ListAll(ctx, p.namespace, "")
		if err != nil {
			s.deps.Logger.Error("list all controllers failed for fleet plugins", "error", err)
		}
		statuses = cs

		// Apply cluster query parameter to rows only.
		for _, c := range cc {
			if p.cluster != "" && c.Cluster != p.cluster {
				continue
			}
			var pi *v1alpha1.PluginInventoryStatus
			if c.CR.Status.PluginInventory != nil {
				pi = c.CR.Status.PluginInventory
			}
			enrichedRows = append(enrichedRows, enriched{
				cluster:   c.Cluster,
				ns:        c.CR.Namespace,
				name:      c.CR.Name,
				phase:     c.CR.Status.Phase,
				pluginInv: pi,
			})
		}
	} else {
		// Fallback: local-only via visibleControllers.
		controllers, err := s.visibleControllers(ctx, p.claims, p.namespace)
		if err != nil {
			s.deps.Logger.Error("list controllers failed for fleet plugins", "error", err)
		}
		for _, cr := range controllers {
			enrichedRows = append(enrichedRows, enriched{
				cluster:   s.localCluster(),
				ns:        cr.Namespace,
				name:      cr.Name,
				phase:     cr.Status.Phase,
				pluginInv: cr.Status.PluginInventory,
			})
		}
		statuses = []ClusterFanoutStatus{{Name: s.localCluster(), OK: true}}
	}

	// Step 6: Mark every non-local cluster ok: false with an explanatory error.
	// Done from the FULL cluster set — independently of the cluster filter and
	// of whether a remote cluster has any caller-visible controllers (R22, R28).
	// Drop rows from non-local clusters.
	localCluster := s.localCluster()
	finalStatuses := make([]ClusterFanoutStatus, 0, len(statuses))
	localRows := make([]enriched, 0, len(enrichedRows))
	for _, st := range statuses {
		if st.Name != localCluster {
			// Remote cluster: mark not-covered and suppress its rows.
			finalStatuses = append(finalStatuses, ClusterFanoutStatus{
				Name:  st.Name,
				OK:    false,
				Error: "v1 covers the local cluster only (R22)",
			})
		} else {
			finalStatuses = append(finalStatuses, st)
		}
	}
	for _, row := range enrichedRows {
		if row.cluster == localCluster {
			localRows = append(localRows, row)
		}
	}

	// Step 7: Deny by default.
	// This deliberately diverges from handleControllersFiltered (handlers.go:596),
	// whose Brood branch admits every row when the authorizer is unwired. Copying
	// that fallthrough into a surface whose purpose is disclosing where software
	// is installed would be a disclosure bug.
	if s.deps.Authorizer == nil {
		return &fleetFanOutResult{
			statuses: finalStatuses,
			rows:     nil,
		}
	}

	// Step 8: Authorization + read.
	// Collect local keys for the reader call, after RBAC.
	var localKeys []types.NamespacedName
	localKeySet := make(map[types.NamespacedName]enriched)
	for _, row := range localRows {
		if !s.deps.Authorizer.CanReadController(p.claims, row.ns, row.name) {
			continue
		}
		key := types.NamespacedName{Namespace: row.ns, Name: row.name}
		localKeys = append(localKeys, key)
		localKeySet[key] = row
	}

	// Step 9: Call FleetPluginInventory.List once.
	invMap := s.deps.FleetPluginInventory.List(localKeys)

	// Build controllerRow slice for the aggregator.
	rows := make([]controllerRow, 0, len(localKeys))
	for _, key := range localKeys {
		er := localKeySet[key]
		inv, hasInv := invMap[key]

		var pi *v1alpha1.PluginInventoryStatus
		if er.pluginInv != nil {
			pi = er.pluginInv
		}

		r := controllerRow{
			Cluster:   er.cluster,
			Namespace: er.ns,
			Name:      er.name,
			Phase:     er.phase,
			HasInv:    hasInv,
		}

		if hasInv {
			r.Inv = inv
			r.Envelope = inv.Envelope
			// Copy flags from status, never recompute.
			if pi != nil {
				r.Stale = pi.Stale
				r.Degraded = pi.Degraded
				r.Truncated = pi.Truncated
				r.OptionalEdgesDropped = pi.OptionalEdgesDropped
				r.BootstrapApproximate = pi.BootstrapApproximate
				r.Source = pi.Source
			}
			// Cross-check envelope vs status summary.
			if pi != nil {
				r.DetailStale = checkEnvelope(
					inv.Envelope,
					pi.Hash, pi.Source,
					pi.Stale, pi.Degraded, pi.Truncated,
					pi.OptionalEdgesDropped, pi.BootstrapApproximate,
					pi.DriftTruncated,
				)
			} else {
				// No status summary to compare against → detailStale.
				r.DetailStale = true
			}
		}

		rows = append(rows, r)
	}

	return &fleetFanOutResult{
		statuses: finalStatuses,
		rows:     rows,
	}
}

// ---------------------------------------------------------------------------
// HandleFleetPlugins — rollup
// ---------------------------------------------------------------------------

// HandleFleetPlugins responds GET /api/v1/fleet/plugins.
func (s *Server) HandleFleetPlugins(w http.ResponseWriter, r *http.Request) {
	p := s.parseFleetPluginParams(w, r)
	if p == nil {
		return
	}

	ctx := context.Background()
	fr := s.fleetFanOut(ctx, p)

	// Build fleetInput.
	in := fleetInput{
		Rows:     fr.rows,
		Statuses: fr.statuses,
	}

	items, cov := Rollup(in, r.URL.Query().Get("q"), p.affected)

	resp := map[string]any{
		"items":    items,
		"coverage": cov,
		"clusters": fr.statuses,
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// HandleFleetPluginDetail — drilldown
// ---------------------------------------------------------------------------

// HandleFleetPluginDetail responds GET /api/v1/fleet/plugins/{name}.
// The router registers /fleet/plugins/ as a bare path; the handler parses its
// own {name} segment, matching handleBroodOperations and handleClusterDispatch.
func (s *Server) HandleFleetPluginDetail(w http.ResponseWriter, r *http.Request) {
	// Manual {name} segment parsing.
	// The URL path after StripPrefix("/api/v1", mux) is /fleet/plugins/<name>.
	// Everything after /fleet/plugins/ is the plugin name.
	path := r.URL.Path
	const prefix = "/fleet/plugins/"
	if len(path) <= len(prefix) {
		// Empty name segment — trailing slash with nothing after.
		s.writeJSONError(w, http.StatusNotFound, "plugin name is required")
		return
	}

	name := path[len(prefix):]
	// Ignore any further path segments for simplicity — the mux shouldn't route
	// deeper paths here, but be safe.

	if name == "" {
		s.writeJSONError(w, http.StatusNotFound, "plugin name is required")
		return
	}

	p := s.parseFleetPluginParams(w, r)
	if p == nil {
		return
	}

	ctx := context.Background()
	fr := s.fleetFanOut(ctx, p)

	// Build fleetInput.
	in := fleetInput{
		Rows:     fr.rows,
		Statuses: fr.statuses,
	}

	drillItems, versions, cov := Drill(in, name, p.affected)

	resp := map[string]any{
		"name":     name,
		"items":    drillItems,
		"versions": versions,
		"coverage": cov,
		"clusters": fr.statuses,
	}

	s.writeJSON(w, http.StatusOK, resp)
}
