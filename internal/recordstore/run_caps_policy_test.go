package recordstore

import (
	"testing"
)

func TestDecodeCapPolicy_ReadsLimitsAndCellMax(t *testing.T) {
	t.Parallel()

	got := decodeCapPolicy([]byte(`{"concurrencyLimits":[{"name":"builds","jobs":["app/*"],"max":2}],"cellMaxRuns":3}`))
	if len(got.Limits) != 1 || got.Limits[0].Name != "builds" || got.Limits[0].Max != 2 {
		t.Fatalf("limits = %+v, want one builds limit of 2", got.Limits)
	}
	if got.CellMaxRuns != 3 {
		t.Fatalf("CellMaxRuns = %d, want 3", got.CellMaxRuns)
	}
}

func TestDecodeCapPolicy_MalformedAnswersZero(t *testing.T) {
	t.Parallel()

	if got := decodeCapPolicy([]byte(`{"concurrencyLimits":"nope"`)); got.CellMaxRuns != 0 || len(got.Limits) != 0 {
		t.Fatalf("malformed policy = %+v, want zero value", got)
	}
	got := decodeCapPolicy([]byte(`{"concurrencyLimits":[{"name":"empty","jobs":[],"max":2},{"name":"zero","jobs":["*"],"max":0}],"cellMaxRuns":-1}`))
	if len(got.Limits) != 0 || got.CellMaxRuns != 0 {
		t.Fatalf("invalid entries = %+v, want none kept", got)
	}
}

func TestCapPolicyMatching_FolderRelativeGlob(t *testing.T) {
	t.Parallel()

	p := TenantCapPolicy{Limits: []ConcurrencyLimit{
		{Name: "deploys", Jobs: []string{"deploy/*"}, Max: 1},
		{Name: "all", Jobs: []string{"**"}, Max: 4},
	}}
	got := p.matching("acme", "acme/deploy/prod")
	if len(got) != 2 || got[0].Name != "deploys" || got[1].Name != "all" {
		t.Fatalf("matching = %+v, want deploys then all in spec order", got)
	}
	got = p.matching("acme", "acme/build")
	if len(got) != 1 || got[0].Name != "all" {
		t.Fatalf("matching = %+v, want only all", got)
	}
}
