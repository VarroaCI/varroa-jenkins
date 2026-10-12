//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// =============================================================================
// CellPool tools (3)
// =============================================================================

// Each tool calls the same internal/api service method as its HTTP sibling,
// so a tool and a route authorize, route to a cluster, and refuse a call
// identically.

// cellPoolResource is the full CellPool resource with a top-level cluster
// key. A pool name is unique inside one cluster only.
type cellPoolResource struct {
	Cluster string `json:"cluster"`
	*v1alpha1.CellPool
}

// cellPoolToolError maps a service refusal onto a tool error. The two
// gate refusals keep the verb in the text, so an agent knows which grant
// is missing.
func cellPoolToolError(serr *api.ServiceError, verb string) *mcp.CallToolResult {
	if serr.Status == http.StatusForbidden {
		return mcp.NewToolResultError("access denied: missing cellpools:" + verb + " permission")
	}
	return mcp.NewToolResultError(serr.Message)
}

func registerCellPoolTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listPools := mcp.NewTool("list_cell_pools",
		mcp.WithDescription("List the CellPools of every cluster of the brood, or of one cluster. "+
			"A CellPool holds the size, the capacity, and the template of one pool of cells that "+
			"serve the pipelines tenants of a single version profile. Each item names its cluster. "+
			"The summary carries every spec dial and every status count. clusters reports each "+
			"cluster the list read; a cluster with ok false added no item. Use get_cell_pool for "+
			"the full resource."),
		mcp.WithString("cluster", mcp.Description("List the pools of this cluster only. Default: every cluster.")),
		mcp.WithBoolean("verbose", mcp.Description("Return full CellPool resources instead of "+
			"summaries. A full resource carries the cell template and the condition list.")),
		withListOutput(cellPoolListOutputSchema),
	)
	addTool(mcpServer, kindRead, listPools, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		list, serr := api.NewServer(deps).ListCellPools(ctx, claims, strArg(args, "cluster"))
		if serr != nil {
			return cellPoolToolError(serr, "read"), nil
		}
		var items any
		if verbose, _ := args["verbose"].(bool); verbose {
			full := make([]cellPoolResource, 0, len(list.Items))
			for _, p := range list.Items {
				full = append(full, cellPoolResource{Cluster: p.Cluster, CellPool: p.Pool})
			}
			items = full
		} else {
			summaries := make([]cellPoolSummary, 0, len(list.Items))
			for _, p := range list.Items {
				summaries = append(summaries, summarizeCellPool(p.Cluster, p.Pool))
			}
			items = summaries
		}
		return resultJSON(map[string]any{
			"items":    items,
			"count":    len(list.Items),
			"clusters": list.Clusters,
		})
	})

	getPool := mcp.NewTool("get_cell_pool",
		mcp.WithDescription("Get one CellPool's spec and status. status reports warmCells, "+
			"placedCells, unreadyCells, freeTenantSlots, the tenant list, and the PoolReady "+
			"condition. The result names the pool's cluster."),
		mcp.WithString("name", mcp.Required(), mcp.Description("CellPool name")),
		mcp.WithString("cluster", mcp.Description("Cluster that holds the pool. Default: the core cluster.")),
	)
	addTool(mcpServer, kindRead, getPool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		pool, serr := api.NewServer(deps).GetCellPool(ctx, claims, strArg(args, "cluster"), strArg(args, "name"))
		if serr != nil {
			return cellPoolToolError(serr, "read"), nil
		}
		return resultJSON(cellPoolResource{Cluster: pool.Cluster, CellPool: pool.Pool})
	})

	updatePool := mcp.NewTool("update_cell_pool",
		mcp.WithDescription("Scale a CellPool on its own cluster. Writes only the dials the call "+
			"names and removes nothing. profileRef, cellTemplate, libraries, approvedSignatures, and mail are not API operations: edit "+
			"those with kubectl or through GitOps."),
		mcp.WithString("name", mcp.Required(), mcp.Description("CellPool name")),
		mcp.WithString("cluster", mcp.Description("Cluster that holds the pool. Default: the core cluster.")),
		mcp.WithNumber("warm", mcp.Description("Target count of free cells the pool keeps ready")),
		mcp.WithNumber("maxCells", mcp.Description("Cap on the pool's cells, placed cells included. 0 means unlimited")),
		mcp.WithNumber("maxTenantsPerCell", mcp.Description("Capacity a pool cell uses when its own spec.cell.maxTenants is zero")),
		mcp.WithString("unreadyGracePeriod", mcp.Description("How long a pool cell may stay unready before the operator rolls its pod, such as 5m")),
	)
	addTool(mcpServer, kindUpdatePartial, updatePool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanUpdateCellPools(claims) {
			return mcp.NewToolResultError("access denied: missing cellpools:update permission"), nil
		}

		args := getArgs(req)
		name := strArg(args, "name")
		dials, errMsg := cellPoolDialsFromArgs(args)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}

		pool, serr := api.NewServer(deps).ScaleCellPool(ctx, claims, strArg(args, "cluster"), name, dials)
		if serr != nil {
			return cellPoolToolError(serr, "update"), nil
		}
		emitActivityForCluster(deps, claims, pool.Cluster, activity.Event{
			Type:    "cellpool.updated",
			Message: "CellPool " + name + " scaled on cluster " + pool.Cluster,
		})
		return resultJSON(cellPoolResource{Cluster: pool.Cluster, CellPool: pool.Pool})
	})
}

// cellPoolSummary is the default projection of list_cell_pools. It carries
// every spec dial and every status count, so a caller surveys pool capacity
// without pulling the cell template and the condition list of each pool.
type cellPoolSummary struct {
	Cluster            string   `json:"cluster"`
	Name               string   `json:"name"`
	ProfileRef         string   `json:"profileRef"`
	Warm               int32    `json:"warm"`
	MaxCells           int32    `json:"maxCells"`
	MaxTenantsPerCell  int32    `json:"maxTenantsPerCell"`
	MaxRunsPerTenant   int32    `json:"maxRunsPerTenant"`
	UnreadyGracePeriod string   `json:"unreadyGracePeriod,omitempty"`
	WarmCells          int32    `json:"warmCells"`
	PlacedCells        int32    `json:"placedCells"`
	UnreadyCells       int32    `json:"unreadyCells"`
	FreeTenantSlots    int32    `json:"freeTenantSlots"`
	Tenants            []string `json:"tenants,omitempty"`
	PoolReady          string   `json:"poolReady,omitempty"`
	PoolReadyReason    string   `json:"poolReadyReason,omitempty"`
}

func summarizeCellPool(cluster string, pool *v1alpha1.CellPool) cellPoolSummary {
	if pool == nil {
		return cellPoolSummary{Cluster: cluster}
	}
	s := cellPoolSummary{
		Cluster:           cluster,
		Name:              pool.Name,
		ProfileRef:        pool.Spec.ProfileRef,
		Warm:              pool.Spec.Warm,
		MaxCells:          pool.Spec.MaxCells,
		MaxTenantsPerCell: pool.Spec.MaxTenantsPerCell,
		MaxRunsPerTenant:  pool.Spec.MaxRunsPerTenant,
		WarmCells:         pool.Status.WarmCells,
		PlacedCells:       pool.Status.PlacedCells,
		UnreadyCells:      pool.Status.UnreadyCells,
		FreeTenantSlots:   pool.Status.FreeTenantSlots,
		Tenants:           pool.Status.Tenants,
	}
	if pool.Spec.UnreadyGracePeriod != nil {
		s.UnreadyGracePeriod = pool.Spec.UnreadyGracePeriod.Duration.String()
	}
	for _, c := range pool.Status.Conditions {
		if c.Type == v1alpha1.CellPoolConditionPoolReady {
			s.PoolReady, s.PoolReadyReason = string(c.Status), c.Reason
		}
	}
	return s
}

// cellPoolListOutputSchema describes the result of list_cell_pools.
var cellPoolListOutputSchema = withClustersProperty(listOutputSchema("Default summary projection.", []schemaField{
	{Name: "cluster", Type: "string", Desc: "The cluster that holds the pool. A pool name is unique inside one cluster only."},
	{Name: "name", Type: "string", Desc: "CellPool name."},
	{Name: "profileRef", Type: "string", Desc: "The JenkinsVersionProfile the pool's cells run."},
	{Name: "warm", Type: "integer", Desc: "Target count of free cells the pool keeps ready."},
	{Name: "maxCells", Type: "integer", Desc: "Cap on the pool's cells, placed cells included. 0 means unlimited."},
	{Name: "maxTenantsPerCell", Type: "integer", Desc: "Capacity a pool cell uses when its own spec.cell.maxTenants is zero."},
	{Name: "maxRunsPerTenant", Type: "integer", Desc: "Cap on the builds one tenant runs at once on one cell of the pool. 0 means no cap. Set it on the CellPool object, not through update_cell_pool."},
	{Name: "unreadyGracePeriod", Type: "string", Desc: "How long a pool cell may stay unready before the operator rolls its pod."},
	{Name: "warmCells", Type: "integer", Desc: "Owned cells that hold no tenant."},
	{Name: "placedCells", Type: "integer", Desc: "Owned cells that hold at least one tenant."},
	{Name: "unreadyCells", Type: "integer", Desc: "Owned cells whose CellReady condition is not True."},
	{Name: "freeTenantSlots", Type: "integer", Desc: "Spare capacity of the placed cells only."},
	{Name: "tenants", Type: "array", Desc: "Every tenant on the pool, sorted."},
	{Name: "poolReady", Type: "string", Desc: "The PoolReady condition's status."},
	{Name: "poolReadyReason", Type: "string", Desc: "Ready, CellCapReached, ProfileMissing, RecordStoreUnsupported, BlobTierUnsupported, ArtifactSecretMissing, ArtifactPluginMissing, or SecretsMissing."},
}))

// cellPoolDialsFromArgs reads the four dials off the call arguments. It
// refuses a number that is not a whole int32 and a value below the CRD's
// floor, so a bad call is refused before any request. The remaining rules,
// such as maxCells against warm, run where the write runs.
func cellPoolDialsFromArgs(args map[string]any) (cells.CellPoolDials, string) {
	var d cells.CellPoolDials

	if v, ok := numberArg(args, "warm"); ok {
		n, msg := int32Dial("warm", v, 0)
		if msg != "" {
			return d, msg
		}
		d.Warm = &n
	}
	if v, ok := numberArg(args, "maxCells"); ok {
		n, msg := int32Dial("maxCells", v, 0)
		if msg != "" {
			return d, msg
		}
		d.MaxCells = &n
	}
	if v, ok := numberArg(args, "maxTenantsPerCell"); ok {
		n, msg := int32Dial("maxTenantsPerCell", v, 1)
		if msg != "" {
			return d, msg
		}
		d.MaxTenantsPerCell = &n
	}
	if s := strArg(args, "unreadyGracePeriod"); s != "" {
		dur, err := time.ParseDuration(s)
		if err != nil || dur < 0 {
			return d, "unreadyGracePeriod must be a duration such as 5m"
		}
		d.UnreadyGracePeriod = &s
	}
	if !d.Named() {
		return d, "name at least one of warm, maxCells, maxTenantsPerCell, or unreadyGracePeriod"
	}
	return d, ""
}

// int32Dial converts one JSON number to the int32 a spec dial holds, and
// answers a message instead when it cannot.
//
// JSON carries every number as a float, so the value must be tested before
// the conversion, never after. A fractional value is a different request
// from its truncation: warm 1.9 is not a request for 1. A value outside
// int32 is worse than wrong, because the conversion wraps: 2^31 becomes
// -2147483648, which passes a "0 or above" test performed on the int32 and
// reaches the apiserver as a negative the CRD refuses.
func int32Dial(name string, v float64, floor int32) (int32, string) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
		return 0, fmt.Sprintf("%s must be a whole number", name)
	}
	if v < float64(math.MinInt32) || v > float64(math.MaxInt32) {
		return 0, fmt.Sprintf("%s must be between %d and %d", name, math.MinInt32, math.MaxInt32)
	}
	n := int32(v)
	if n < floor {
		return 0, fmt.Sprintf("%s must be %d or above", name, floor)
	}
	return n, ""
}

// numberArg reads a JSON number argument. A JSON number arrives as float64
// or as json.Number, depending on how the request was decoded.
func numberArg(args map[string]any, key string) (float64, bool) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
