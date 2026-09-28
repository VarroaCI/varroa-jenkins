package recordstore

import (
	"context"
	"testing"
)

// TestListTenantPlacementsAnswersEveryCluster asserts one row for each
// cluster that holds a placement of the tenant, ordered by cluster, and no
// row of another tenant.
func TestListTenantPlacementsAnswersEveryCluster(t *testing.T) {
	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	for _, row := range []struct{ cluster, tenant, namespace, cell string }{
		{"member-b", "shop", "cells", "cell-b"},
		{"member-a", "shop", "cells", "cell-a"},
		{"member-a", "other", "cells", "cell-a"},
	} {
		if _, err := db.Exec(`
			INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell)
			VALUES ($1, $2, $3, $4)
		`, row.cluster, row.tenant, row.namespace, row.cell); err != nil {
			t.Fatalf("seed placement %+v: %v", row, err)
		}
	}

	got, err := reader.ListTenantPlacements(ctx, "shop")
	if err != nil {
		t.Fatalf("ListTenantPlacements: %v", err)
	}
	want := []TenantPlacement{
		{Cluster: "member-a", Namespace: "cells", Cell: "cell-a"},
		{Cluster: "member-b", Namespace: "cells", Cell: "cell-b"},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	none, err := reader.ListTenantPlacements(ctx, "absent")
	if err != nil {
		t.Fatalf("ListTenantPlacements(absent): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("absent tenant rows = %+v, want none", none)
	}
}
