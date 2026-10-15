package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// seedMemberPlacement binds tenant to every member cell the list names.
// The scalar columns describe slot 0, as the placement pass writes them.
func seedMemberPlacement(t *testing.T, db *sql.DB, tenant string, members []member) {
	t.Helper()
	membersJSON, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	slotZero := member{}
	for _, m := range members {
		if m.Role == MemberRoleActive && m.Slot != nil && *m.Slot == 0 {
			slotZero = m
		}
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell, members)
		VALUES ($1, $2, $3, $4, $5::jsonb)
	`, testCluster, tenant, cellUsageNamespace, slotZero.Cell, string(membersJSON)); err != nil {
		t.Fatalf("seed member placement of %s: %v", tenant, err)
	}
}

// usageMemberSet builds the two-slot, one-standby member list the tests
// below share.
func usageMemberSet() []member {
	return []member{
		{Cell: "cell-a", Namespace: cellUsageNamespace, Role: MemberRoleActive, Slot: slotPtr(0)},
		{Cell: "cell-b", Namespace: cellUsageNamespace, Role: MemberRoleActive, Slot: slotPtr(1)},
		{Cell: "cell-c", Namespace: cellUsageNamespace, Role: MemberRoleStandby},
	}
}

// tenantRow finds one cell's row for a tenant.
func tenantRow(t *testing.T, usage map[string]CellUsage, cell string) CellTenantUsage {
	t.Helper()
	for _, row := range usage[cell].Tenants {
		if row.Tenant == cellUsageScheduleTenant {
			return row
		}
	}
	t.Fatalf("no row for the tenant on cell %s: %+v", cell, usage[cell].Tenants)
	return CellTenantUsage{}
}

// TestCellUsageCountsATwoSlotTenantOnBothCells asserts a run executed on
// slot 1's cell joins that cell's counts. Reading the scalar cell alone
// would under-report every member above slot 0.
func TestCellUsageCountsATwoSlotTenantOnBothCells(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	seedMemberPlacement(t, db, cellUsageScheduleTenant, usageMemberSet())

	now := time.Now().UTC()
	seedCellRun(t, db, cellUsageNamespace, "cell-a", cellUsageScheduleTenant, "acme/web", now.Add(-time.Hour), "RUNNING", nil)
	seedCellRun(t, db, cellUsageNamespace, "cell-b", cellUsageScheduleTenant, "acme/api", now.Add(-time.Hour), "RUNNING", nil)

	usage, err := reader.CellUsageMany(context.Background(), testCluster, cellUsageNamespace,
		[]string{"cell-a", "cell-b", "cell-c"})
	if err != nil {
		t.Fatalf("CellUsageMany: %v", err)
	}

	if got := tenantRow(t, usage, "cell-a").RunningNow; got != 1 {
		t.Fatalf("cell-a running = %d, want 1", got)
	}
	if got := tenantRow(t, usage, "cell-b").RunningNow; got != 1 {
		t.Fatalf("cell-b running = %d, want 1", got)
	}
}

// TestCellUsageNamesEachTenantsRole asserts an active member reads as
// "slot <n>" and a standby reads as "standby" with no running build.
func TestCellUsageNamesEachTenantsRole(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	seedMemberPlacement(t, db, cellUsageScheduleTenant, usageMemberSet())

	usage, err := reader.CellUsageMany(context.Background(), testCluster, cellUsageNamespace,
		[]string{"cell-a", "cell-b", "cell-c"})
	if err != nil {
		t.Fatalf("CellUsageMany: %v", err)
	}

	if got := tenantRow(t, usage, "cell-a").Role; got != "slot 0" {
		t.Fatalf("cell-a role = %q, want slot 0", got)
	}
	if got := tenantRow(t, usage, "cell-b").Role; got != "slot 1" {
		t.Fatalf("cell-b role = %q, want slot 1", got)
	}
	standby := tenantRow(t, usage, "cell-c")
	if standby.Role != MemberRoleStandby {
		t.Fatalf("cell-c role = %q, want standby", standby.Role)
	}
	if standby.RunningNow != 0 {
		t.Fatalf("a standby reports %d running builds, want 0", standby.RunningNow)
	}
}

// TestCellUsageAnEmptyMemberListReadsAsSlotZero asserts a placement row
// the operator has not re-announced since the upgrade still appears on
// its scalar cell, with the role "slot 0".
func TestCellUsageAnEmptyMemberListReadsAsSlotZero(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	seedCellPlacement(t, db, "cell-a", cellUsageScheduleTenant)

	now := time.Now().UTC()
	seedCellRun(t, db, cellUsageNamespace, "cell-a", cellUsageScheduleTenant, "acme/web", now.Add(-time.Hour), "RUNNING", nil)

	usage, err := reader.CellUsageMany(context.Background(), testCluster, cellUsageNamespace, []string{"cell-a"})
	if err != nil {
		t.Fatalf("CellUsageMany: %v", err)
	}
	row := tenantRow(t, usage, "cell-a")
	if row.Role != "slot 0" {
		t.Fatalf("role = %q, want slot 0", row.Role)
	}
	if row.RunningNow != 1 {
		t.Fatalf("running = %d, want 1", row.RunningNow)
	}
}

// TestCellTenantUsageReadsASecondSlot asserts the per-job read joins a
// tenant's own member cell, not only slot 0's.
func TestCellTenantUsageReadsASecondSlot(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	seedMemberPlacement(t, db, cellUsageScheduleTenant, usageMemberSet())

	now := time.Now().UTC()
	seedCellRun(t, db, cellUsageNamespace, "cell-b", cellUsageScheduleTenant, "acme/api", now.Add(-time.Hour), "RUNNING", nil)

	rows, err := reader.CellTenantUsage(context.Background(), testCluster, cellUsageNamespace,
		"cell-b", cellUsageScheduleTenant)
	if err != nil {
		t.Fatalf("CellTenantUsage: %v", err)
	}
	if len(rows) != 1 || rows[0].JobPath != "acme/api" || rows[0].RunningNow != 1 {
		t.Fatalf("rows = %+v, want one acme/api row with one running build", rows)
	}
}

// TestRenderMemberRole asserts the two rendered values, and that an
// active member with no slot reads as slot 0.
func TestRenderMemberRole(t *testing.T) {
	if got := renderMemberRole(MemberRoleActive, slotPtr(2)); got != "slot 2" {
		t.Fatalf("role = %q, want slot 2", got)
	}
	if got := renderMemberRole(MemberRoleStandby, nil); got != "standby" {
		t.Fatalf("role = %q, want standby", got)
	}
	if got := renderMemberRole(MemberRoleActive, nil); got != "slot 0" {
		t.Fatalf("role = %q, want slot 0", got)
	}
}
