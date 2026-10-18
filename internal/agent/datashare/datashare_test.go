package datashare

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

func TestEffective(t *testing.T) {
	t.Parallel()

	levels := map[string]agent.DataSharing{"acme": agent.DataLogs}
	cases := []struct {
		name string
		in   Inputs
		want agent.DataSharing
	}{
		{"external with default ceiling is metadata", Inputs{
			User: agent.DataLogs, Team: "acme", TenantLevels: levels,
			Trust: agent.TrustExternal, ExternalCeiling: agent.DataMetadata,
		}, agent.DataMetadata},
		{"internal provider takes logs", Inputs{
			User: agent.DataLogs, Team: "acme", TenantLevels: levels, Trust: agent.TrustInternal,
		}, agent.DataLogs},
		{"tenant without an entry is logs", Inputs{
			User: agent.DataLogs, Team: "other", TenantLevels: levels, Trust: agent.TrustInternal,
		}, agent.DataLogs},
		{"empty team is logs", Inputs{
			User: agent.DataLogs, Team: "", TenantLevels: levels, Trust: agent.TrustInternal,
		}, agent.DataLogs},
		{"empty user level counts as logs", Inputs{
			Team: "acme", TenantLevels: levels, Trust: agent.TrustInternal,
		}, agent.DataLogs},
		{"tenant none wins", Inputs{
			User: agent.DataLogs, Team: "acme",
			TenantLevels: map[string]agent.DataSharing{"acme": agent.DataNone}, Trust: agent.TrustInternal,
		}, agent.DataNone},
	}
	for _, c := range cases {
		if got := Effective(c.in); got != c.want {
			t.Fatalf("%s: Effective = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestVisibleHidesLogToolsBelowLogs(t *testing.T) {
	t.Parallel()

	tools := []agent.ToolDef{
		{Name: "get_run_log", Class: agent.ClassRead},
		{Name: "get_run", Class: agent.ClassRead},
		{Name: "list_controllers", Class: agent.ClassRead},
		{Name: "run_pipeline", Class: agent.ClassWrite},
	}
	names := func(ts []agent.ToolDef) []string {
		out := make([]string, 0, len(ts))
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return out
	}

	got := names(Visible(agent.DataMetadata, tools))
	want := []string{"get_run", "list_controllers", "run_pipeline"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("metadata visible = %v, want %v", got, want)
	}
	got = names(Visible(agent.DataLogs, tools))
	if len(got) != 4 {
		t.Fatalf("logs visible = %v, want all four", got)
	}
	if got := names(Visible(agent.DataNone, tools)); strings.Join(got, ",") != "run_pipeline" {
		t.Fatalf("none visible = %v, want only the write tool", got)
	}
}

func TestUnclassifiedReadHiddenBelowLogs(t *testing.T) {
	t.Parallel()

	tools := []agent.ToolDef{{Name: "brand_new_read", Class: agent.ClassRead}}
	if got := Visible(agent.DataMetadata, tools); len(got) != 0 {
		t.Fatalf("unclassified read visible at metadata: %v", got)
	}
	if got := Visible(agent.DataLogs, tools); len(got) != 1 {
		t.Fatalf("unclassified read hidden at logs")
	}
}

func TestFilterStripsLogTail(t *testing.T) {
	t.Parallel()

	in := `{"digest":{"stage":"build","error":"boom","logTail":"secret lines"},"id":"r1"}`
	out := Filter("get_run", agent.DataMetadata, in)
	if strings.Contains(out, "logTail") || strings.Contains(out, "secret lines") {
		t.Fatalf("logTail survived at metadata: %s", out)
	}
	var obj struct {
		Digest map[string]any `json:"digest"`
	}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("filtered result is not JSON: %v", err)
	}
	if obj.Digest["stage"] != "build" || obj.Digest["error"] != "boom" {
		t.Fatalf("stage or error lost: %v", obj.Digest)
	}
	if got := Filter("get_run", agent.DataLogs, in); got != in {
		t.Fatalf("logs level changed the result: %s", got)
	}
	if got := Filter("get_run_log", agent.DataMetadata, in); got != in {
		t.Fatal("a non-get_run tool was filtered")
	}
	if got := Filter("get_run", agent.DataMetadata, "plain text"); got != "plain text" {
		t.Fatal("non-JSON content was changed")
	}
}

// stubResolver maps runs and teams to tenants.
type stubResolver struct {
	runs  map[string]string
	teams map[string]string
	err   error
}

func (s stubResolver) RunTenant(_ context.Context, runID string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.runs[runID], nil
}

func (s stubResolver) TeamTenant(_ context.Context, team string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.teams[team], nil
}

func TestCheckLoweredTenantLevelAppliesNextCall(t *testing.T) {
	t.Parallel()

	r := stubResolver{runs: map[string]string{"r-1": "acme"}}
	levels := map[string]agent.DataSharing{"acme": agent.DataLogs}
	args := json.RawMessage(`{"run":"r-1"}`)

	first, err := Check(context.Background(), r, levels, agent.DataLogs, "get_run_log", args)
	if err != nil || first.Level != agent.DataLogs || first.Tenant != "acme" {
		t.Fatalf("first call = (%+v, %v), want logs for acme", first, err)
	}

	levels["acme"] = agent.DataNone
	second, err := Check(context.Background(), r, levels, agent.DataLogs, "get_run_log", args)
	if err != nil || second.Level != agent.DataNone {
		t.Fatalf("second call = (%+v, %v), want none", second, err)
	}
	if Refusal(second.Tenant) != "refused: data sharing is off for tenant acme" {
		t.Fatalf("refusal text = %q", Refusal(second.Tenant))
	}
}

func TestCheckUsesRunThenTeam(t *testing.T) {
	t.Parallel()

	r := stubResolver{
		runs:  map[string]string{"r-1": "from-run"},
		teams: map[string]string{"acme": "from-team"},
	}
	levels := map[string]agent.DataSharing{"from-run": agent.DataNone, "from-team": agent.DataMetadata}

	both, err := Check(context.Background(), r, levels, agent.DataLogs, "get_run",
		json.RawMessage(`{"run":"r-1","team":"acme"}`))
	if err != nil || both.Tenant != "from-run" {
		t.Fatalf("run and team = (%+v, %v), want the run's tenant", both, err)
	}

	onlyTeam, err := Check(context.Background(), r, levels, agent.DataLogs, "list_runs",
		json.RawMessage(`{"team":"acme"}`))
	if err != nil || onlyTeam.Tenant != "from-team" || onlyTeam.Level != agent.DataMetadata {
		t.Fatalf("team only = (%+v, %v), want metadata for the team's tenant", onlyTeam, err)
	}

	none, err := Check(context.Background(), r, levels, agent.DataLogs, "list_controllers", json.RawMessage(`{}`))
	if err != nil || none.Tenant != "" || none.Level != agent.DataLogs {
		t.Fatalf("no tenant = (%+v, %v), want the session level", none, err)
	}

	boom := stubResolver{err: errors.New("db down")}
	if _, err := Check(context.Background(), boom, levels, agent.DataLogs, "get_run", json.RawMessage(`{"run":"r-1"}`)); err == nil {
		t.Fatal("resolver error was swallowed")
	}
}
