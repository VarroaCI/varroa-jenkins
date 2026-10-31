// Package api: the two operator cell usage reads of the cell-controllers
// capability. The family is flat: a cell usage row is a fleet view, not a
// controller sub-resource.
package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// defaultCellMaxTenants is the last tier of the effective tenant cap. It
// is the operator's own compiled fallback, so both processes agree when
// the chart renders no value.
const defaultCellMaxTenants = 10

// cellUsageCountsJSON holds the four load counts. They carry no
// omitempty, so a tenant with no build still reports four zeros.
type cellUsageCountsJSON struct {
	RunningNow    int64 `json:"runningNow"`
	RunsLast24h   int64 `json:"runsLast24h"`
	FailedLast24h int64 `json:"failedLast24h"`
	Jobs          int64 `json:"jobs"`
}

// cellTenantUsageJSON names one tenant and carries that tenant's four
// counts beside the name.
type cellTenantUsageJSON struct {
	Tenant string `json:"tenant"`
	// Role is that tenant's role on this cell: "slot <n>" for an active
	// member and "standby" for a standby.
	Role string `json:"role,omitempty"`
	// AgentNamespace is absent until the tenant's own cluster announces
	// one.
	AgentNamespace      string `json:"agentNamespace,omitempty"`
	PlacementGeneration int64  `json:"placementGeneration"`
	cellUsageCountsJSON
}

// cellUsageJSON is one cell's load. An optional field carries omitempty,
// because an absent value must be absent rather than zero. queueLength
// is a pointer for the same reason: zero means an empty queue.
type cellUsageJSON struct {
	Cluster        string                `json:"cluster"`
	Namespace      string                `json:"namespace"`
	Name           string                `json:"name"`
	WindowStart    time.Time             `json:"windowStart"`
	CellPool       string                `json:"cellPool,omitempty"`
	Profile        string                `json:"profile,omitempty"`
	JenkinsVersion string                `json:"jenkinsVersion,omitempty"`
	JenkinsHealth  string                `json:"jenkinsHealth,omitempty"`
	QueueLength    *int64                `json:"queueLength,omitempty"`
	QueueLengthAt  *time.Time            `json:"queueLengthAt,omitempty"`
	Tenants        []cellTenantUsageJSON `json:"tenants"`
	Totals         cellUsageCountsJSON   `json:"totals"`
	MaxTenants     int32                 `json:"maxTenants"`
	FreeSlots      int32                 `json:"freeSlots"`
}

// cellPresentation holds the fields a cell Controller and the mite
// registry supply. An absent value stays absent.
type cellPresentation struct {
	CellPool       string
	Profile        string
	JenkinsVersion string
	JenkinsHealth  string
	QueueLength    *int64
	QueueLengthAt  *time.Time
}

// cellCapResolver resolves the effective tenant cap of every cell one
// request touches. The chain is first match, never a minimum: the cell's
// own spec.cell.maxTenants when that value is not zero, else the owning
// pool's maxTenantsPerCell, else the chart value the environment carries,
// else the compiled default. The pool tier reads the pools of the cell's
// own cluster, because a pool name is unique inside one cluster only. The
// resolver keeps one pool map for each cluster and reads a cluster's pools
// on the first cell of that cluster that needs the pool tier, so a listing
// of many cells costs one read for each cluster. A failed read drops the
// pool tier of that cluster for the request and is logged once.
type cellCapResolver struct {
	s     *Server
	ctx   context.Context
	pools map[string]map[string]int32
}

// cellCaps starts a resolver bound to one request context.
func (s *Server) cellCaps(ctx context.Context) *cellCapResolver {
	return &cellCapResolver{s: s, ctx: ctx, pools: map[string]map[string]int32{}}
}

// resolveCellCap resolves the cap of one cell of cluster. A path that
// touches many cells holds one cellCaps resolver instead.
func (s *Server) resolveCellCap(ctx context.Context, cluster string, cr *v1alpha1.Controller) int32 {
	return s.cellCaps(ctx).resolve(cluster, cr)
}

// poolCap answers the maxTenantsPerCell of the named pool on cluster, or
// zero when the pool is gone: a cell whose pool no longer exists has no
// pool tier.
func (c *cellCapResolver) poolCap(cluster, name string) int32 {
	key := cluster
	if c.s.isLocalCluster(cluster) {
		key = ""
	}
	byName, loaded := c.pools[key]
	if !loaded {
		byName = map[string]int32{}
		c.pools[key] = byName
		target := cluster
		if key == "" {
			target = c.s.localCluster()
		}
		pools, err := c.s.cellPoolsOn(c.ctx, target)
		if err != nil {
			c.s.deps.Logger.Warn("cell cap: the pool list failed; the pool tier is skipped",
				"cluster", target, "error", err)
			return 0
		}
		for i := range pools {
			byName[pools[i].Name] = pools[i].Spec.MaxTenantsPerCell
		}
	}
	return byName[name]
}

func (c *cellCapResolver) resolve(cluster string, cr *v1alpha1.Controller) int32 {
	if cr == nil {
		return c.chartCap()
	}
	if cr.Spec.Cell != nil && cr.Spec.Cell.MaxTenants > 0 {
		return cr.Spec.Cell.MaxTenants
	}
	if pool := cr.Labels[cells.LabelCellPool]; pool != "" {
		if n := c.poolCap(cluster, pool); n > 0 {
			return n
		}
	}
	return c.chartCap()
}

func (c *cellCapResolver) chartCap() int32 {
	if c.s.deps.CellMaxTenants > 0 {
		return int32(c.s.deps.CellMaxTenants) //nolint:gosec // a tenant cap is small; the chart bounds the value
	}
	return defaultCellMaxTenants
}

// cellPlacedTenants counts the tenants the placement pass bound to cr.
// status.cell.tenants is the count source: status.cell.appliedTenants
// lags behind it.
func cellPlacedTenants(cr *v1alpha1.Controller) int32 {
	if cr == nil || cr.Status.Cell == nil {
		return 0
	}
	return int32(len(cr.Status.Cell.Tenants)) //nolint:gosec // the CRD caps the list at 64 entries
}

// cellFreeSlots returns cap minus the placed tenant count, never below
// zero.
func cellFreeSlots(cap32, placed int32) int32 {
	if free := cap32 - placed; free > 0 {
		return free
	}
	return 0
}

// cellPresentationFor reads a cell Controller's presentation fields.
// cellPool and profile come from labels, so this surface reads no
// CellPool object. jenkinsVersion and jenkinsHealth come from the live
// mite snapshot, else from status.miteStatus. The gauges cover the local
// cluster only, so a remote cell reports no queue length at all.
func (s *Server) cellPresentationFor(cr *v1alpha1.Controller, cluster string) cellPresentation {
	out := cellPresentation{}
	if cr == nil {
		return out
	}
	out.CellPool = cr.Labels[cells.LabelCellPool]
	out.Profile = cr.Labels[cells.LabelCellProfile]
	if ms := cr.Status.MiteStatus; ms != nil {
		if ms.JenkinsVersion != "" && ms.JenkinsVersion != "NORMAL" {
			out.JenkinsVersion = ms.JenkinsVersion
		}
		out.JenkinsHealth = ms.JenkinsHealth
	}

	if s.deps.MiteRegistry == nil || !s.isLocalCluster(cluster) {
		return out
	}
	if snap := s.deps.MiteRegistry.Snapshot(cr.Namespace, cr.Name); snap != nil {
		if snap.JenkinsVersion != "" && snap.JenkinsVersion != "NORMAL" {
			out.JenkinsVersion = snap.JenkinsVersion
		}
		if snap.JenkinsHealth != "" {
			out.JenkinsHealth = snap.JenkinsHealth
		}
	}
	if gauges, receivedAt, ok := s.deps.MiteRegistry.IdleGauges(cr.Namespace, cr.Name); ok && gauges != nil {
		queue := int64(gauges.QueueLength)
		at := receivedAt.UTC()
		out.QueueLength = &queue
		out.QueueLengthAt = &at
	}
	return out
}

// isLocalCluster reports whether cluster is the BFF's own cluster. An
// empty name is the local cluster, the default of both usage paths.
func (s *Server) isLocalCluster(cluster string) bool {
	if cluster == "" {
		return true
	}
	if s.deps.Brood == nil {
		return true
	}
	return cluster == s.deps.Brood.LocalCluster()
}

// cellUsageCluster resolves the cluster of a usage request: the optional
// cluster parameter, else the local cluster.
func (s *Server) cellUsageCluster(r *http.Request) string {
	if cluster := r.URL.Query().Get("cluster"); cluster != "" {
		return cluster
	}
	if s.deps.Brood != nil {
		return s.deps.Brood.LocalCluster()
	}
	return ""
}

// handleCellUsage serves GET /cells/{namespace}/{name}/usage. It answers
// 404 for an unknown controller, a controller that is not a cell, and a
// caller the controller read check refuses. One code for all three keeps
// the surface from confirming that a cell exists.
func (s *Server) handleCellUsage(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	usage, serr := s.CellUsage(r.Context(), claims, s.cellUsageCluster(r), r.PathValue("namespace"), r.PathValue("name"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, usage)
}

// handleListCellUsage serves GET /cells/usage. It drops each cell the
// controller read check refuses and answers the surviving list.
func (s *Server) handleListCellUsage(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || claims == nil {
		s.writeJSON(w, http.StatusOK, itemsEnvelope([]cellUsageJSON{}))
		return
	}

	cluster := s.cellUsageCluster(r)
	namespace := r.URL.Query().Get("namespace")
	controllers, err := s.listCellControllers(r, cluster, namespace)
	if err != nil {
		s.deps.Logger.Error("list cells", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list cells failed")
		return
	}

	admitted := make([]*v1alpha1.Controller, 0, len(controllers))
	for _, cr := range controllers {
		if !s.deps.Authorizer.CanReadController(claims, cr.Namespace, cr.Name) {
			continue
		}
		admitted = append(admitted, cr)
	}

	rows := make([]cellUsageJSON, 0, len(admitted))
	if len(admitted) == 0 {
		s.writeJSON(w, http.StatusOK, itemsEnvelope(rows))
		return
	}

	// One reader call for every surviving cell: the page costs one
	// request and one database round trip.
	byNamespace := map[string][]string{}
	for _, cr := range admitted {
		byNamespace[cr.Namespace] = append(byNamespace[cr.Namespace], cr.Name)
	}
	usageByKey := map[string]cellUsageJSON{}
	for ns, cellNames := range byNamespace {
		usage, err := s.deps.RecordStore.CellUsageMany(r.Context(), cluster, ns, cellNames)
		if s.handleReaderError(w, err, "read cell usage") {
			return
		}
		for name, value := range usage {
			usageByKey[ns+"/"+name] = cellUsageRowJSON(value)
		}
	}

	caps := s.cellCaps(r.Context())
	for _, cr := range admitted {
		row := usageByKey[cr.Namespace+"/"+cr.Name]
		row.Cluster = cluster
		row.Namespace = cr.Namespace
		row.Name = cr.Name
		s.fillCellUsagePresentation(&row, cr, cluster, caps)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	s.writeJSON(w, http.StatusOK, itemsEnvelope(rows))
}

// listCellControllers lists the cluster's cells, narrowed by an optional
// namespace.
func (s *Server) listCellControllers(r *http.Request, cluster, namespace string) ([]*v1alpha1.Controller, error) {
	if s.deps.Brood == nil {
		return nil, nil
	}
	found, _, err := s.deps.Brood.ListAll(r.Context(), namespace, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.Controller, 0, len(found))
	for _, entry := range found {
		cr := entry.CR
		if !v1alpha1.IsCell(cr) {
			continue
		}
		if namespace != "" && cr.Namespace != namespace {
			continue
		}
		out = append(out, cr)
	}
	return out, nil
}

// handleCellGrants serves GET /cells/{namespace}/{name}/grants. It answers
// which subjects may build on each tenant of the cell.
func (s *Server) handleCellGrants(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	grants, serr := s.CellGrants(r.Context(), claims, s.cellUsageCluster(r), r.PathValue("namespace"), r.PathValue("name"))
	if serr != nil {
		s.writeJSONError(w, serr.Status, serr.Message)
		return
	}
	s.writeJSON(w, http.StatusOK, grants)
}
