package datashare

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
)

// TenantResolver maps a run or a tenant to the tenant that owns it. A lookup
// that finds nothing returns the empty tenant and no error.
type TenantResolver interface {
	RunTenant(ctx context.Context, runID string) (string, error)
	TenantTenant(ctx context.Context, tenant string) (string, error)
}

// CallLevel is the level that applies to one tool call, and the tenant it
// was decided for.
type CallLevel struct {
	Tenant string
	Level  agent.DataSharing
}

// Check decides the level of one tool call. The tenant comes from the run
// argument, or else from the tenant argument. A call that names no tenant
// uses the session level alone. Otherwise the level is the lower of the
// session level and the tenant's current level.
func Check(ctx context.Context, r TenantResolver, levels map[string]agent.DataSharing, session agent.DataSharing, tool string, args json.RawMessage) (CallLevel, error) {
	a := policy.ParseArgs(args)
	if isFleetCall(tool, a) {
		return fleetCallLevel(session, levels), nil
	}
	var tenant string
	var err error
	switch {
	case a.Run != "":
		tenant, err = r.RunTenant(ctx, a.Run)
	case a.Tenant != "":
		tenant, err = r.TenantTenant(ctx, a.Tenant)
	}
	if err != nil {
		return CallLevel{}, err
	}
	if tenant == "" {
		return CallLevel{Level: session}, nil
	}
	return CallLevel{Tenant: tenant, Level: Min(session, TenantLevel(levels, tenant))}, nil
}

// isFleetCall reports whether a call reads every tenant the caller can read:
// the overview, and the run list without a tenant.
func isFleetCall(tool string, a policy.Args) bool {
	return tool == "get_pipelines_overview" || (tool == "list_runs" && a.Tenant == "")
}

// fleetCallLevel answers the level of a fleet call. The level is the lowest of
// the session level and every tenant's level, so one tenant at none refuses the
// call. The reported tenant is the lowest one, with the smallest name on a tie.
// With no tenant levels, the call takes the session level alone.
func fleetCallLevel(session agent.DataSharing, levels map[string]agent.DataSharing) CallLevel {
	if len(levels) == 0 {
		return CallLevel{Level: session}
	}
	names := make([]string, 0, len(levels))
	for name := range levels {
		names = append(names, name)
	}
	sort.Strings(names)
	lowestName := names[0]
	lowest := TenantLevel(levels, names[0])
	for _, name := range names[1:] {
		if level := TenantLevel(levels, name); Rank(level) < Rank(lowest) {
			lowest, lowestName = level, name
		}
	}
	return CallLevel{Tenant: lowestName, Level: Min(session, lowest)}
}

// Refusal is the result text of a call that the tenant's level refuses.
func Refusal(tenant string) string {
	return "refused: data sharing is off for tenant " + tenant
}
