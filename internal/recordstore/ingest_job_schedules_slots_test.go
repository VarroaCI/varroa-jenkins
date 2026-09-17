package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// seedScheduleMembers binds tenant to two active slots and one standby,
// with the given home map. The cells are the ones the schedule tests use.
func seedScheduleMembers(t *testing.T, db *sql.DB, tenant string, jobHomes map[string]int32) {
	t.Helper()
	members := []member{
		{Cell: "cell-a", Namespace: jsTestNS, Role: MemberRoleActive, Slot: slotPtr(0)},
		{Cell: "cell-b", Namespace: jsTestNS, Role: MemberRoleActive, Slot: slotPtr(1)},
		{Cell: "cell-c", Namespace: jsTestNS, Role: MemberRoleStandby},
	}
	membersJSON, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	homesJSON, err := json.Marshal(nonNilJobHomes(jobHomes))
	if err != nil {
		t.Fatalf("marshal job homes: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, members, job_homes)
		VALUES ($1, $2, $3, 'cell-a', 4, $4::jsonb, $5::jsonb)
	`, jsTestCluster, tenant, jsTestNS, string(membersJSON), string(homesJSON)); err != nil {
		t.Fatalf("seed schedule members: %v", err)
	}
}

// readScheduleCell reads the stored cell of one job's schedule row. It
// answers an empty string when no row exists.
func readScheduleCell(t *testing.T, db *sql.DB, tenant, jobPath string) string {
	t.Helper()
	var cell string
	err := db.QueryRow(`
		SELECT cell FROM varroa.job_schedules
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, jsTestCluster, tenant, jobPath).Scan(&cell)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read schedule cell: %v", err)
	}
	return cell
}

// TestApplyJobSchedules_StandbyExportIsDropped asserts a standby's export
// of a slot-0 job writes no row, and that the stored row still names slot
// 0's cell.
func TestApplyJobSchedules_StandbyExportIsDropped(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	seedScheduleMembers(t, db, "acme", map[string]int32{"acme/app": 0})

	now := time.Now().UTC()
	homeEnv := newJobScheduleEnvelope(t, "cell-a", now.Add(-time.Minute), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "H * * * *"}},
	})
	if _, err := g.Apply(context.Background(), homeEnv); err != nil {
		t.Fatalf("apply the home envelope: %v", err)
	}

	standbyEnv := newJobScheduleEnvelope(t, "cell-c", now, JobSchedulesPayload{
		Hash: "h2",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "H * * * *"}},
	})
	if _, err := g.Apply(context.Background(), standbyEnv); err != nil {
		t.Fatalf("apply the standby envelope: %v", err)
	}

	if got := readScheduleCell(t, db, "acme", "acme/app"); got != "cell-a" {
		t.Fatalf("stored cell = %q, want cell-a: a standby never owns a job's row", got)
	}
}

// TestApplyJobSchedules_TheHomeStoresTheRow asserts slot 1's holder owns
// the row of a job homed on slot 1.
func TestApplyJobSchedules_TheHomeStoresTheRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	seedScheduleMembers(t, db, "acme", map[string]int32{"acme/api": 1})

	env := newJobScheduleEnvelope(t, "cell-b", time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/api", TriggerType: "cron", Spec: "H * * * *"}},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got := readScheduleCell(t, db, "acme", "acme/api"); got != "cell-b" {
		t.Fatalf("stored cell = %q, want cell-b", got)
	}
}

// TestApplyJobSchedules_MixedEnvelopeStoresItsHomeRows asserts one
// dropped row fails no envelope: the home rows are stored and the cell's
// watermark still advances.
func TestApplyJobSchedules_MixedEnvelopeStoresItsHomeRows(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	seedScheduleMembers(t, db, "acme", map[string]int32{"acme/web": 0, "acme/api": 1})

	occurred := time.Now().UTC().Truncate(time.Microsecond)
	env := newJobScheduleEnvelope(t, "cell-a", occurred, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/web", TriggerType: "cron", Spec: "H * * * *"},
			{Tenant: "acme", JobPath: "acme/api", TriggerType: "cron", Spec: "H * * * *"},
		},
	})
	result, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.ScheduleStale || result.Truncated {
		t.Fatalf("result = %+v, want a clean apply", result)
	}

	if got := readScheduleCell(t, db, "acme", "acme/web"); got != "cell-a" {
		t.Fatalf("the home row stored cell = %q, want cell-a", got)
	}
	if got := readScheduleCell(t, db, "acme", "acme/api"); got != "" {
		t.Fatalf("the non-home row stored cell = %q, want no row", got)
	}

	var watermark time.Time
	if err := db.QueryRow(`
		SELECT observed_at FROM varroa.job_schedule_cells
		WHERE cluster = $1 AND namespace = $2 AND cell = 'cell-a'
	`, jsTestCluster, jsTestNS).Scan(&watermark); err != nil {
		t.Fatalf("read the watermark: %v", err)
	}
	if !watermark.Equal(occurred) {
		t.Fatalf("watermark = %v, want %v: a dropped row still advances it", watermark, occurred)
	}
}

// TestApplyJobSchedules_SingleMemberTenantStoresEveryRow asserts a tenant
// with one active slot, and a tenant with no placement row at all, both
// store every row they export.
func TestApplyJobSchedules_SingleMemberTenantStoresEveryRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell, generation)
		VALUES ($1, 'solo', $2, 'cell-a', 1)
	`, jsTestCluster, jsTestNS); err != nil {
		t.Fatalf("seed the single-member placement: %v", err)
	}

	env := newJobScheduleEnvelope(t, "cell-a", time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "solo", JobPath: "solo/app", TriggerType: "cron", Spec: "H * * * *"},
			{Tenant: "unannounced", JobPath: "unannounced/app", TriggerType: "cron", Spec: "H * * * *"},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got := readScheduleCell(t, db, "solo", "solo/app"); got != "cell-a" {
		t.Fatalf("the single-member row stored cell = %q, want cell-a", got)
	}
	if got := readScheduleCell(t, db, "unannounced", "unannounced/app"); got != "cell-a" {
		t.Fatalf("the unannounced tenant's row stored cell = %q, want cell-a", got)
	}
}
