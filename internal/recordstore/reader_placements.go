package recordstore

import (
	"context"
	"fmt"
)

// TenantPlacement is the slot-0 coordinate of one tenant_placements row.
type TenantPlacement struct {
	Cluster   string
	Namespace string
	Cell      string
}

// ListTenantPlacements answers the slot-0 coordinate of every placement
// row of tenant, one row for each cluster, ordered by cluster.
func (r *pgReader) ListTenantPlacements(ctx context.Context, tenant string) ([]TenantPlacement, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT cluster, namespace, cell
		FROM varroa.tenant_placements
		WHERE tenant = $1
		ORDER BY cluster
	`, tenant)
	if err != nil {
		return nil, fmt.Errorf("list tenant placements: %w", err)
	}
	defer rows.Close()

	var out []TenantPlacement
	for rows.Next() {
		var p TenantPlacement
		if err := rows.Scan(&p.Cluster, &p.Namespace, &p.Cell); err != nil {
			return nil, fmt.Errorf("scan tenant placement: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant placements: %w", err)
	}
	return out, nil
}
