package datashare

import (
	"context"
	"encoding/json"

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

// Refusal is the result text of a call that the tenant's level refuses.
func Refusal(tenant string) string {
	return "refused: data sharing is off for tenant " + tenant
}
