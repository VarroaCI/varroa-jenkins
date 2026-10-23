package api

import (
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// defaultRecordStoreRunRetention is the store-wide runs window the BFF
// assumes when VARROA_RETENTION_RUNS is unset. It matches the chart
// default.
const defaultRecordStoreRunRetention = 365 * 24 * time.Hour

// pipelineRetentionJSON is the retention block of GET /pipelines/{tenant}.
// Every value is in whole days. The store window caps each tenant and job
// window, so EffectiveDays never exceeds StoreDays.
type pipelineRetentionJSON struct {
	// StoreDays is the store-wide runs window.
	StoreDays int `json:"storeDays"`
	// TenantDays is the tenant window. It is absent when the tenant sets none.
	TenantDays *int `json:"tenantDays,omitempty"`
	// EffectiveDays is the window a job without a matching rule gets.
	EffectiveDays int `json:"effectiveDays"`
	// Jobs lists the per-job rules in the order they match.
	Jobs []pipelineJobRetentionJSON `json:"jobs"`
}

// pipelineJobRetentionJSON is one per-job rule of the retention block.
type pipelineJobRetentionJSON struct {
	// Job is the folder-relative job glob.
	Job string `json:"job"`
	// Days is the window the rule names, before the store cap.
	Days int `json:"days"`
	// EffectiveDays is the window the rule gives after the store cap.
	EffectiveDays int `json:"effectiveDays"`
}

// buildPipelineRetention reports one tenant's retention windows. A zero
// store window means the 365d default. A job rule's effective window is
// its own window, capped by the store window.
func buildPipelineRetention(store time.Duration, p recordstore.TenantRetentionPolicy) pipelineRetentionJSON {
	if store <= 0 {
		store = defaultRecordStoreRunRetention
	}
	effective := store
	out := pipelineRetentionJSON{
		StoreDays: retentionDays(store),
		Jobs:      make([]pipelineJobRetentionJSON, 0, len(p.Jobs)),
	}
	if p.Runs > 0 {
		days := retentionDays(p.Runs)
		out.TenantDays = &days
		effective = min(store, p.Runs)
	}
	out.EffectiveDays = retentionDays(effective)
	for _, r := range p.Jobs {
		out.Jobs = append(out.Jobs, pipelineJobRetentionJSON{
			Job:           r.Job,
			Days:          retentionDays(r.Runs),
			EffectiveDays: retentionDays(min(store, r.Runs)),
		})
	}
	return out
}

// retentionDays converts a window to whole days, rounding down.
func retentionDays(d time.Duration) int {
	return int(d / (24 * time.Hour))
}
