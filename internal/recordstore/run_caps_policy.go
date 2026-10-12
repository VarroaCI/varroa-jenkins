package recordstore

import (
	"encoding/json"
)

// ConcurrencyLimit is one named set of a tenant's jobs and the number of
// its builds that run at one time. It mirrors Team.spec.pipelines
// .concurrencyLimits as the operator announces it.
type ConcurrencyLimit struct {
	Name string   `json:"name"`
	Jobs []string `json:"jobs"`
	Max  int      `json:"max"`
}

// TenantCapPolicy is the concurrency policy a tenant announces: its named
// job-set limits and the per-tenant cap of its cell's pool.
type TenantCapPolicy struct {
	Limits      []ConcurrencyLimit `json:"concurrencyLimits"`
	CellMaxRuns int                `json:"cellMaxRuns"`
}

// decodeCapPolicy reads the cap keys of a tenant_placements policy. A
// malformed policy answers the zero value, which caps nothing. A limit
// with no job glob or a max below one is dropped.
func decodeCapPolicy(policy json.RawMessage) TenantCapPolicy {
	var raw TenantCapPolicy
	if len(policy) == 0 || json.Unmarshal(policy, &raw) != nil {
		return TenantCapPolicy{}
	}
	out := TenantCapPolicy{CellMaxRuns: raw.CellMaxRuns}
	if out.CellMaxRuns < 0 {
		out.CellMaxRuns = 0
	}
	for _, l := range raw.Limits {
		if len(l.Jobs) == 0 || l.Max < 1 {
			continue
		}
		out.Limits = append(out.Limits, l)
	}
	return out
}

// matching answers the limits whose job globs match a run's job path. The
// globs are folder-relative, so the tenant folder prefix is removed first.
// The answer keeps the spec order.
func (p TenantCapPolicy) matching(tenant, jobPath string) []ConcurrencyLimit {
	rel, ok := stripTenantPrefix(tenant, jobPath)
	if !ok {
		rel = jobPath
	}
	var out []ConcurrencyLimit
	for _, l := range p.Limits {
		if limitMatchesRel(l, rel) {
			out = append(out, l)
		}
	}
	return out
}

// limitMatches reports whether a run's job path, tenant-qualified, falls
// under the limit's job globs.
func limitMatches(tenant, jobPath string, l ConcurrencyLimit) bool {
	rel, ok := stripTenantPrefix(tenant, jobPath)
	if !ok {
		rel = jobPath
	}
	return limitMatchesRel(l, rel)
}

// limitMatchesRel reports whether a folder-relative job path matches one of
// the limit's globs.
func limitMatchesRel(l ConcurrencyLimit, rel string) bool {
	for _, g := range l.Jobs {
		if matchGlob(g, rel) {
			return true
		}
	}
	return false
}
