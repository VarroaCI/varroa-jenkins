package recordstore

import (
	"context"
	"fmt"
)

// TenantPlacement is the slot-0 coordinate of one tenant_placements row.
// Generation is 0 for a row whose tenant a teardown ended: the row stays
// for the interrupted-run sweep, but it names no live tenant.
type TenantPlacement struct {
	Cluster    string
	Tenant     string
	Namespace  string
	Cell       string
	Generation int64
}

// ListTenantPlacements answers the slot-0 coordinate of every placement
// row of tenant, one row for each cluster, ordered by cluster.
func (r *pgReader) ListTenantPlacements(ctx context.Context, tenant string) ([]TenantPlacement, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	return r.queryTenantPlacements(ctx, `
		SELECT cluster, tenant, namespace, cell, generation
		FROM varroa.tenant_placements
		WHERE tenant = $1
		ORDER BY cluster
	`, tenant)
}

// ListAllTenantPlacements answers the slot-0 coordinate of every placement
// row of every tenant, ordered by tenant, then by cluster.
func (r *pgReader) ListAllTenantPlacements(ctx context.Context) ([]TenantPlacement, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	return r.queryTenantPlacements(ctx, `
		SELECT cluster, tenant, namespace, cell, generation
		FROM varroa.tenant_placements
		ORDER BY tenant, cluster
	`)
}

// queryTenantPlacements runs one placement query. Every query must select
// the five columns in TenantPlacement's field order.
func (r *pgReader) queryTenantPlacements(ctx context.Context, query string, args ...any) ([]TenantPlacement, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tenant placements: %w", err)
	}
	defer rows.Close()

	var out []TenantPlacement
	for rows.Next() {
		var p TenantPlacement
		if err := rows.Scan(&p.Cluster, &p.Tenant, &p.Namespace, &p.Cell, &p.Generation); err != nil {
			return nil, fmt.Errorf("scan tenant placement: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant placements: %w", err)
	}
	return out, nil
}
