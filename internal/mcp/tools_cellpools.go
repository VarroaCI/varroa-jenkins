//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellPoolUpdateAttempts bounds the read-modify-write retry, matching the
// HTTP sibling's own bound.
const cellPoolUpdateAttempts = 3

// =============================================================================
// CellPool tools (3)
// =============================================================================

func registerCellPoolTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listPools := mcp.NewTool("list_cell_pools",
		mcp.WithDescription("List every CellPool, sorted by name. A CellPool holds the size, "+
			"the capacity, and the template of one pool of cells that serve the pipelines "+
			"tenants of a single version profile. The summary carries every spec dial and "+
			"every status count. Use get_cell_pool for the full resource."),
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
		if !deps.Authorizer.CanReadCellPools(claims) {
			return mcp.NewToolResultError("access denied: missing cellpools:read permission"), nil
		}
		pools, err := crdstore.List[v1alpha1.CellPool](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError("failed to list cell pools: " + err.Error()), nil
		}
		sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
		if verbose, _ := getArgs(req)["verbose"].(bool); verbose {
			return resultJSON(pools)
		}
		summaries := make([]cellPoolSummary, 0, len(pools))
		for _, pool := range pools {
			summaries = append(summaries, summarizeCellPool(pool))
		}
		return resultJSON(summaries)
	})

	getPool := mcp.NewTool("get_cell_pool",
		mcp.WithDescription("Get one CellPool's spec and status. status reports warmCells, "+
			"placedCells, unreadyCells, freeTenantSlots, the tenant list, and the PoolReady "+
			"condition."),
		mcp.WithString("name", mcp.Required(), mcp.Description("CellPool name")),
	)
	addTool(mcpServer, kindRead, getPool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if !deps.Authorizer.CanReadCellPools(claims) {
			return mcp.NewToolResultError("access denied: missing cellpools:read permission"), nil
		}
		name := strArg(getArgs(req), "name")
		pool, err := crdstore.Get[v1alpha1.CellPool](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError("cell pool not found: " + err.Error()), nil
		}
		return resultJSON(pool)
	})

	updatePool := mcp.NewTool("update_cell_pool",
		mcp.WithDescription("Scale a CellPool. Writes only the dials the call names and removes "+
			"nothing. profileRef and cellTemplate are not API operations: edit those with "+
			"kubectl or through GitOps."),
		mcp.WithString("name", mcp.Required(), mcp.Description("CellPool name")),
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
		apply, errMsg := cellPoolDialsFromArgs(args)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}

		for attempt := 0; attempt < cellPoolUpdateAttempts; attempt++ {
			existing, getErr := crdstore.Get[v1alpha1.CellPool](ctx, deps.Store, name, "")
			if getErr != nil {
				return mcp.NewToolResultError("cell pool not found: " + getErr.Error()), nil
			}
			updated := existing.DeepCopy()
			if msg := apply(&updated.Spec); msg != "" {
				return mcp.NewToolResultError(msg), nil
			}
			updateErr := crdstore.Update(ctx, deps.Store, updated)
			if updateErr == nil {
				emitActivity(deps, claims, activity.Event{
					Type:    "cellpool.updated",
					Message: "CellPool " + name + " scaled",
				})
				return resultJSON(updated)
			}
			if !apierrors.IsConflict(updateErr) {
				return mcp.NewToolResultError(fmt.Sprintf("failed to update cell pool: %v", updateErr)), nil
			}
		}
		return mcp.NewToolResultError("the cell pool changed during the update; retry the call"), nil
	})
}

// cellPoolSummary is the default projection of list_cell_pools. It carries
// every spec dial and every status count, so a caller surveys pool capacity
// without pulling the cell template and the condition list of each pool.
type cellPoolSummary struct {
	Name               string   `json:"name"`
	ProfileRef         string   `json:"profileRef"`
	Warm               int32    `json:"warm"`
	MaxCells           int32    `json:"maxCells"`
	MaxTenantsPerCell  int32    `json:"maxTenantsPerCell"`
	UnreadyGracePeriod string   `json:"unreadyGracePeriod,omitempty"`
	WarmCells          int32    `json:"warmCells"`
	PlacedCells        int32    `json:"placedCells"`
	UnreadyCells       int32    `json:"unreadyCells"`
	FreeTenantSlots    int32    `json:"freeTenantSlots"`
	Tenants            []string `json:"tenants,omitempty"`
	PoolReady          string   `json:"poolReady,omitempty"`
	PoolReadyReason    string   `json:"poolReadyReason,omitempty"`
}

func summarizeCellPool(pool *v1alpha1.CellPool) cellPoolSummary {
	if pool == nil {
		return cellPoolSummary{}
	}
	s := cellPoolSummary{
		Name:              pool.Name,
		ProfileRef:        pool.Spec.ProfileRef,
		Warm:              pool.Spec.Warm,
		MaxCells:          pool.Spec.MaxCells,
		MaxTenantsPerCell: pool.Spec.MaxTenantsPerCell,
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
var cellPoolListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "CellPool name."},
	{Name: "profileRef", Type: "string", Desc: "The JenkinsVersionProfile the pool's cells run."},
	{Name: "warm", Type: "integer", Desc: "Target count of free cells the pool keeps ready."},
	{Name: "maxCells", Type: "integer", Desc: "Cap on the pool's cells, placed cells included. 0 means unlimited."},
	{Name: "maxTenantsPerCell", Type: "integer", Desc: "Capacity a pool cell uses when its own spec.cell.maxTenants is zero."},
	{Name: "unreadyGracePeriod", Type: "string", Desc: "How long a pool cell may stay unready before the operator rolls its pod."},
	{Name: "warmCells", Type: "integer", Desc: "Owned cells that hold no tenant."},
	{Name: "placedCells", Type: "integer", Desc: "Owned cells that hold at least one tenant."},
	{Name: "unreadyCells", Type: "integer", Desc: "Owned cells whose CellReady condition is not True."},
	{Name: "freeTenantSlots", Type: "integer", Desc: "Spare capacity of the placed cells only."},
	{Name: "tenants", Type: "array", Desc: "Every tenant on the pool, sorted."},
	{Name: "poolReady", Type: "string", Desc: "The PoolReady condition's status."},
	{Name: "poolReadyReason", Type: "string", Desc: "Ready, CellCapReached, or ProfileMissing."},
})

// cellPoolDialsFromArgs reads the four dials off the call arguments and
// returns a function that merges them onto a spec. The numeric bounds repeat
// the CRD's own, so a bad call is refused before the write and the caller
// receives a readable message rather than a raw apiserver rejection.
func cellPoolDialsFromArgs(args map[string]any) (func(*v1alpha1.CellPoolSpec) string, string) {
	var warm, maxCells, maxTenants *int32
	var grace *metav1.Duration
	named := false

	if v, ok := numberArg(args, "warm"); ok {
		n, msg := int32Dial("warm", v, 0)
		if msg != "" {
			return nil, msg
		}
		warm, named = &n, true
	}
	if v, ok := numberArg(args, "maxCells"); ok {
		n, msg := int32Dial("maxCells", v, 0)
		if msg != "" {
			return nil, msg
		}
		maxCells, named = &n, true
	}
	if v, ok := numberArg(args, "maxTenantsPerCell"); ok {
		n, msg := int32Dial("maxTenantsPerCell", v, 1)
		if msg != "" {
			return nil, msg
		}
		maxTenants, named = &n, true
	}
	if s := strArg(args, "unreadyGracePeriod"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return nil, "unreadyGracePeriod must be a duration such as 5m"
		}
		grace, named = &metav1.Duration{Duration: d}, true
	}
	if !named {
		return nil, "name at least one of warm, maxCells, maxTenantsPerCell, or unreadyGracePeriod"
	}

	return func(spec *v1alpha1.CellPoolSpec) string {
		if warm != nil {
			spec.Warm = *warm
		}
		if maxCells != nil {
			spec.MaxCells = *maxCells
		}
		if maxTenants != nil {
			spec.MaxTenantsPerCell = *maxTenants
		}
		if grace != nil {
			spec.UnreadyGracePeriod = grace
		}
		if spec.MaxCells > 0 && spec.MaxCells < spec.Warm {
			return "maxCells must be 0 or at least warm"
		}
		return ""
	}, ""
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
