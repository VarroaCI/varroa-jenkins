package datashare

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// noTenantResolver answers no tenant for any lookup.
type noTenantResolver struct{}

func (noTenantResolver) RunTenant(context.Context, string) (string, error)    { return "", nil }
func (noTenantResolver) TenantTenant(context.Context, string) (string, error) { return "", nil }

func TestCheckFleetCallUsesLowestTenantLevel(t *testing.T) {
	levels := map[string]agent.DataSharing{"a": agent.DataMetadata, "b": agent.DataNone}

	got, err := Check(context.Background(), noTenantResolver{}, levels, agent.DataLogs,
		"list_runs", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Level != agent.DataNone || got.Tenant != "b" {
		t.Fatalf("fleet list_runs = %+v; want level none for tenant b", got)
	}

	got, err = Check(context.Background(), noTenantResolver{}, levels, agent.DataLogs,
		"get_pipelines_overview", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Level != agent.DataNone || got.Tenant != "b" {
		t.Fatalf("get_pipelines_overview = %+v; want level none for tenant b", got)
	}
}

func TestCheckFleetCallWithNoTenantLevelsTakesTheSessionLevel(t *testing.T) {
	got, err := Check(context.Background(), noTenantResolver{}, nil, agent.DataMetadata,
		"get_pipelines_overview", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Level != agent.DataMetadata || got.Tenant != "" {
		t.Fatalf("got %+v; want the session level alone", got)
	}
}

func TestCheckListRunsWithTenantKeepsThePerTenantRule(t *testing.T) {
	levels := map[string]agent.DataSharing{"acme": agent.DataMetadata, "other": agent.DataNone}
	resolver := tenantStubResolver{tenant: "acme"}
	got, err := Check(context.Background(), resolver, levels, agent.DataLogs,
		"list_runs", json.RawMessage(`{"tenant":"acme","job":"x"}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Tenant != "acme" || got.Level != agent.DataMetadata {
		t.Fatalf("per-tenant list_runs = %+v; want acme at metadata", got)
	}
}

// tenantStubResolver maps every tenant argument to one tenant name.
type tenantStubResolver struct{ tenant string }

func (r tenantStubResolver) RunTenant(context.Context, string) (string, error) { return r.tenant, nil }
func (r tenantStubResolver) TenantTenant(context.Context, string) (string, error) {
	return r.tenant, nil
}
