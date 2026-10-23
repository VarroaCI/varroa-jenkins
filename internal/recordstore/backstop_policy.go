package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The backstop-scheduler capability's defaults, for a tenant with no
// tenant_placements row: a single-tenant controller.
const (
	defaultCronBackstop = true
	defaultCronCatchUp  = "one"
)

// defaultCronGrace is Tenant.spec.pipelines.cronGrace's default.
const defaultCronGrace = 5 * time.Minute

// cronCatchUpNone is the only Tenant.spec.pipelines.cronCatchUp value
// besides defaultCronCatchUp ("one").
const cronCatchUpNone = "none"

// tenantSchedulePolicy is the backstop round's per-tenant policy: the
// three backstop settings the announce's policy field carries.
type tenantSchedulePolicy struct {
	cronBackstop bool
	cronCatchUp  string
	cronGrace    time.Duration
}

// defaultTenantSchedulePolicy is the backstop-scheduler capability's
// answer for a tenant with no tenant_placements row.
func defaultTenantSchedulePolicy() tenantSchedulePolicy {
	return tenantSchedulePolicy{cronBackstop: defaultCronBackstop, cronCatchUp: defaultCronCatchUp, cronGrace: defaultCronGrace}
}

// schedulePolicyWire is the subset of tenant_placements.policy the
// backstop round reads. cronBackstop is a plain bool, not a pointer:
// the operator's announce already resolved Tenant.spec.pipelines'
// tri-state CronBackstop to a concrete value before it wrote the wire
// policy, so every stored row's own key is always present.
type schedulePolicyWire struct {
	CronBackstop *bool  `json:"cronBackstop"`
	CronCatchUp  string `json:"cronCatchUp"`
	CronGrace    string `json:"cronGrace"`
}

// decodeSchedulePolicy decodes raw against schedulePolicyWire, falling
// back to the backstop-scheduler capability's defaults for a field the
// JSON omits or a raw the decoder rejects.
func decodeSchedulePolicy(raw json.RawMessage) tenantSchedulePolicy {
	out := defaultTenantSchedulePolicy()
	if len(raw) == 0 {
		return out
	}
	var w schedulePolicyWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return out
	}
	if w.CronBackstop != nil {
		out.cronBackstop = *w.CronBackstop
	}
	if w.CronCatchUp != "" {
		out.cronCatchUp = w.CronCatchUp
	}
	if grace, err := time.ParseDuration(w.CronGrace); err == nil && grace > 0 {
		out.cronGrace = grace
	}
	return out
}

// schedulePolicyQuerier is the one method readTenantSchedulePolicy
// needs. A pool and a transaction both satisfy it.
type schedulePolicyQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readTenantSchedulePolicy reads cluster/tenant's tenant_placements
// policy. A missing row answers defaultTenantSchedulePolicy: a
// single-tenant controller.
func readTenantSchedulePolicy(ctx context.Context, q schedulePolicyQuerier, cluster, tenant string) (tenantSchedulePolicy, error) {
	var policyRaw json.RawMessage
	err := q.QueryRow(ctx, `
		SELECT policy FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant).Scan(&policyRaw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return defaultTenantSchedulePolicy(), nil
	case err != nil:
		return tenantSchedulePolicy{}, fmt.Errorf("read tenant schedule policy: %w", err)
	}
	return decodeSchedulePolicy(policyRaw), nil
}

// schedulePair is one distinct (cluster, tenant) pair job_schedules
// holds.
type schedulePair struct {
	cluster string
	tenant  string
}

// listJobSchedulePairs lists every distinct (cluster, tenant) pair
// job_schedules holds, so the backstop round can read each tenant's
// policy exactly once.
func listJobSchedulePairs(ctx context.Context, pool *pgxpool.Pool) ([]schedulePair, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT cluster, tenant FROM varroa.job_schedules ORDER BY cluster, tenant`)
	if err != nil {
		return nil, fmt.Errorf("list job schedule pairs: %w", err)
	}
	defer rows.Close()

	var out []schedulePair
	for rows.Next() {
		var p schedulePair
		if err := rows.Scan(&p.cluster, &p.tenant); err != nil {
			return nil, fmt.Errorf("scan job schedule pair: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list job schedule pairs: %w", err)
	}
	return out, nil
}
