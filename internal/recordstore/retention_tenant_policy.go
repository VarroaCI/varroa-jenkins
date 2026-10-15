package recordstore

import (
	"encoding/json"
	"time"
)

// JobRetentionRule is one per-job run window. Job is a folder-relative
// glob, and Runs is the window that replaces the tenant window for the
// jobs it matches.
type JobRetentionRule struct {
	Job  string
	Runs time.Duration
}

// TenantRetentionPolicy holds one tenant's record-store windows, decoded
// from the "retention" key of tenant_placements.policy. The zero value
// sets no window.
type TenantRetentionPolicy struct {
	// Runs is the tenant window. Zero means unset.
	Runs time.Duration
	// Jobs lists the per-job rules in order. The first match wins.
	Jobs []JobRetentionRule
}

// IsZero reports whether the policy sets no window at all.
func (p TenantRetentionPolicy) IsZero() bool {
	return p.Runs == 0 && len(p.Jobs) == 0
}

// retentionWire is the JSON shape of the "retention" key in a tenant
// placement policy. It mirrors v1alpha1.TeamPipelinesRetention.
type retentionWire struct {
	Retention *struct {
		Runs string `json:"runs"`
		Jobs []struct {
			Job  string `json:"job"`
			Runs string `json:"runs"`
		} `json:"jobs"`
	} `json:"retention"`
}

// decodeRetention reads the retention windows from a tenant placement
// policy. A malformed policy answers the zero policy. An unparsable tenant
// window leaves Runs zero. An unparsable job rule is dropped.
func decodeRetention(policy json.RawMessage) TenantRetentionPolicy {
	var w retentionWire
	if len(policy) == 0 || json.Unmarshal(policy, &w) != nil || w.Retention == nil {
		return TenantRetentionPolicy{}
	}
	var p TenantRetentionPolicy
	if d, err := ParseRetention(w.Retention.Runs); err == nil {
		p.Runs = d
	}
	for _, r := range w.Retention.Jobs {
		d, err := ParseRetention(r.Runs)
		if err != nil || r.Job == "" {
			continue
		}
		p.Jobs = append(p.Jobs, JobRetentionRule{Job: r.Job, Runs: d})
	}
	return p
}

// EffectiveRunRetention answers the run window that applies to one job.
// The store-wide window caps every answer.
//
// A matching job rule replaces the tenant window. The first matching rule
// wins. The job path is matched without its tenant folder prefix, so
// rules name folder-relative jobs. Without a matching rule, the tenant
// window applies when set. Otherwise the store-wide window applies.
func EffectiveRunRetention(store time.Duration, p TenantRetentionPolicy, tenant, jobPath string) time.Duration {
	rel := jobPath
	if r, ok := stripTenantPrefix(tenant, jobPath); ok {
		rel = r
	}
	for _, r := range p.Jobs {
		if matchGlob(r.Job, rel) {
			return min(store, r.Runs)
		}
	}
	if p.Runs > 0 {
		return min(store, p.Runs)
	}
	return store
}

// shortest answers the smallest window any job of the tenant can get,
// capped by the store-wide window. The round uses it to skip a tenant
// whose windows never cut a run.
func (p TenantRetentionPolicy) shortest(store time.Duration) time.Duration {
	out := store
	if p.Runs > 0 {
		out = min(out, p.Runs)
	}
	for _, r := range p.Jobs {
		out = min(out, r.Runs)
	}
	return out
}
