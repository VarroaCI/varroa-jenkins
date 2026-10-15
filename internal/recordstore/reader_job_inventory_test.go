package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// jiMembersTwoActive is a placement with two active slots, cell-a in slot 0
// and cell-b in slot 1.
const jiMembersTwoActive = `[
	{"cell":"cell-a","namespace":"jenkins","role":"active","slot":0,"instance":"pod-a"},
	{"cell":"cell-b","namespace":"jenkins","role":"active","slot":1,"instance":"pod-b"}
]`

// jiMembersActiveAndStandby is one active slot, cell-a, and a standby cell-s.
const jiMembersActiveAndStandby = `[
	{"cell":"cell-a","namespace":"jenkins","role":"active","slot":0,"instance":"pod-a"},
	{"cell":"cell-s","namespace":"jenkins","role":"standby","instance":"pod-s"}
]`

// seedJobInventoryPlacement inserts the tenant placement of the inventory
// tests. The members list is given as JSON.
func seedJobInventoryPlacement(t *testing.T, db *sql.DB, membersJSON string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance, members)
		VALUES ($1, 'acme', 'jenkins', 'cell-a', 1, 'pod-a', 'pod-a', $2::jsonb)
	`, jsTestCluster, membersJSON); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
}

// applyInventorySchedules applies one job.schedules set from cell. The set
// holds acme/app, or nothing when empty is true.
func applyInventorySchedules(t *testing.T, g *Ingester, cell string, at time.Time, truncated bool) {
	t.Helper()
	env := newJobScheduleEnvelope(t, cell, at, JobSchedulesPayload{
		Hash:      "h-" + at.String(),
		Truncated: truncated,
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", PlacementGeneration: 1, JobPath: "acme/app", TriggerType: "none"},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply job.schedules: %v", err)
	}
}

// applyInventoryBranches applies one job.branches set from cell. The set
// holds the acme/app project.
func applyInventoryBranches(t *testing.T, g *Ingester, cell string, at time.Time, truncated bool) {
	t.Helper()
	env := newJobBranchesEnvelope(t, cell, at, JobBranchesPayload{
		Hash:      "b-" + at.String(),
		Truncated: truncated,
		Projects:  []JobBranchProjectInput{oneBranchProject("acme/app")},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply job.branches: %v", err)
	}
}

// readInventory reads the inventory of the acme tenant and fails the test
// on error.
func readInventory(t *testing.T, reader Reader) JobInventory {
	t.Helper()
	inv, err := reader.JobInventory(context.Background(), jsTestCluster, "acme")
	if err != nil {
		t.Fatalf("JobInventory: %v", err)
	}
	return inv
}

func TestJobInventoryNoPlacementIsUnknown(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	applyInventorySchedules(t, g, jsTestCell, time.Now().UTC(), false)
	applyInventoryBranches(t, g, jsTestCell, time.Now().UTC(), false)

	inv := readInventory(t, NewReader(pool))
	if inv.JobsKnown || inv.ProjectsKnown {
		t.Fatalf("no placement must be unknown, got JobsKnown=%v ProjectsKnown=%v", inv.JobsKnown, inv.ProjectsKnown)
	}
	if inv.Jobs != nil || inv.Projects != nil {
		t.Fatalf("no placement must hold no paths, got jobs=%v projects=%v", inv.Jobs, inv.Projects)
	}
}

func TestJobInventoryReportedCellIsKnown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersActiveAndStandby)
	g := NewIngester(pool, nil, nil)
	at := time.Now().UTC()
	applyInventorySchedules(t, g, jsTestCell, at, false)
	applyInventoryBranches(t, g, jsTestCell, at, false)

	inv := readInventory(t, NewReader(pool))
	if !inv.JobsKnown || !inv.ProjectsKnown {
		t.Fatalf("a complete report from the one active cell must be known, got JobsKnown=%v ProjectsKnown=%v",
			inv.JobsKnown, inv.ProjectsKnown)
	}
	if _, ok := inv.Jobs["acme/app"]; !ok {
		t.Fatalf("Jobs = %v, want acme/app", inv.Jobs)
	}
	if _, ok := inv.Projects["acme/app"]; !ok {
		t.Fatalf("Projects = %v, want acme/app", inv.Projects)
	}
}

func TestJobInventoryTruncatedCellIsUnknown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersActiveAndStandby)
	g := NewIngester(pool, nil, nil)
	at := time.Now().UTC()
	applyInventorySchedules(t, g, jsTestCell, at, true)
	applyInventoryBranches(t, g, jsTestCell, at, false)

	inv := readInventory(t, NewReader(pool))
	if inv.JobsKnown {
		t.Fatal("a truncated job.schedules set must make the jobs unknown")
	}
	if !inv.ProjectsKnown {
		t.Fatal("a complete job.branches set must keep the projects known")
	}
}

func TestJobInventoryUnreportedMemberIsUnknown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersTwoActive)
	g := NewIngester(pool, nil, nil)
	at := time.Now().UTC()
	applyInventorySchedules(t, g, jsTestCell, at, false)
	applyInventoryBranches(t, g, jsTestCell, at, false)

	inv := readInventory(t, NewReader(pool))
	if inv.JobsKnown || inv.ProjectsKnown {
		t.Fatalf("an active member without a watermark must be unknown, got JobsKnown=%v ProjectsKnown=%v",
			inv.JobsKnown, inv.ProjectsKnown)
	}

	applyInventorySchedules(t, g, "cell-b", at, false)
	applyInventoryBranches(t, g, "cell-b", at, false)
	inv = readInventory(t, NewReader(pool))
	if !inv.JobsKnown || !inv.ProjectsKnown {
		t.Fatalf("both active members reported, want known, got JobsKnown=%v ProjectsKnown=%v",
			inv.JobsKnown, inv.ProjectsKnown)
	}
}

func TestJobInventoryIgnoresStandby(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersActiveAndStandby)
	g := NewIngester(pool, nil, nil)
	at := time.Now().UTC()
	applyInventorySchedules(t, g, jsTestCell, at, false)
	applyInventoryBranches(t, g, jsTestCell, at, false)

	// cell-s has no watermark row at all: a standby must not count.
	inv := readInventory(t, NewReader(pool))
	if !inv.JobsKnown || !inv.ProjectsKnown {
		t.Fatalf("a standby without a watermark must not make the inventory unknown, got JobsKnown=%v ProjectsKnown=%v",
			inv.JobsKnown, inv.ProjectsKnown)
	}
}

func TestJobInventoryProjectsKnownSeparately(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersActiveAndStandby)
	g := NewIngester(pool, nil, nil)
	applyInventorySchedules(t, g, jsTestCell, time.Now().UTC(), false)

	inv := readInventory(t, NewReader(pool))
	if !inv.JobsKnown {
		t.Fatal("a complete job.schedules set must make the jobs known")
	}
	if inv.ProjectsKnown {
		t.Fatal("a cell that never reported job.branches must leave the projects unknown")
	}
}

// TestApplyJobSchedulesRecordsTruncation proves the watermark keeps the
// truncation flag of the last applied set, and a later complete set clears it.
func TestApplyJobSchedulesRecordsTruncation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := time.Now().UTC()

	applyInventorySchedules(t, g, jsTestCell, t0, true)
	if got := truncationFlag(t, db, "schedules_truncated"); !got {
		t.Fatal("schedules_truncated = false after a truncated set, want true")
	}

	applyInventorySchedules(t, g, jsTestCell, t0.Add(time.Second), false)
	if got := truncationFlag(t, db, "schedules_truncated"); got {
		t.Fatal("schedules_truncated = true after a complete set, want false")
	}
}

// TestApplyJobBranchesRecordsTruncation proves the branch watermark keeps
// its own truncation flag, separate from the schedule flag.
func TestApplyJobBranchesRecordsTruncation(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := time.Now().UTC()

	applyInventoryBranches(t, g, jsTestCell, t0, true)
	if got := truncationFlag(t, db, "branches_truncated"); !got {
		t.Fatal("branches_truncated = false after a truncated set, want true")
	}
	if got := truncationFlag(t, db, "schedules_truncated"); got {
		t.Fatal("a branches set must not set schedules_truncated")
	}

	applyInventoryBranches(t, g, jsTestCell, t0.Add(time.Second), false)
	if got := truncationFlag(t, db, "branches_truncated"); got {
		t.Fatal("branches_truncated = true after a complete set, want false")
	}
}

// truncationFlag reads one truncation column of the test cell's watermark.
func truncationFlag(t *testing.T, db *sql.DB, column string) bool {
	t.Helper()
	var flag bool
	if err := db.QueryRow(`SELECT `+column+` FROM varroa.job_schedule_cells WHERE cluster = $1 AND cell = $2`,
		jsTestCluster, jsTestCell).Scan(&flag); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	return flag
}

// TestJobInventoryExcludesOtherTenants proves the inventory holds only the
// asked tenant's paths. Another tenant of the same cluster reports its own
// job and project, and neither appears in acme's sets.
func TestJobInventoryExcludesOtherTenants(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersActiveAndStandby)
	g := NewIngester(pool, nil, nil)
	at := time.Now().UTC()
	applyInventorySchedules(t, g, jsTestCell, at, false)
	applyInventoryBranches(t, g, jsTestCell, at, false)

	other := newJobScheduleEnvelope(t, jsTestCell, at.Add(time.Second), JobSchedulesPayload{
		// A cell envelope replaces the whole cell's set, so it carries
		// acme's row as well as the other tenant's row.
		Hash: "other-schedules",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", PlacementGeneration: 1, JobPath: "acme/app", TriggerType: "none"},
			{Tenant: "other", PlacementGeneration: 1, JobPath: "other/app", TriggerType: "none"},
		},
	})
	if _, err := g.Apply(context.Background(), other); err != nil {
		t.Fatalf("apply other tenant job.schedules: %v", err)
	}
	otherProject := oneBranchProject("other/app")
	otherProject.Tenant = "other"
	otherBranches := newJobBranchesEnvelope(t, jsTestCell, at.Add(time.Second), JobBranchesPayload{
		Hash:     "other-branches",
		Projects: []JobBranchProjectInput{oneBranchProject("acme/app"), otherProject},
	})
	if _, err := g.Apply(context.Background(), otherBranches); err != nil {
		t.Fatalf("apply other tenant job.branches: %v", err)
	}

	inv := readInventory(t, NewReader(pool))
	if _, ok := inv.Jobs["other/app"]; ok {
		t.Fatalf("Jobs holds another tenant's path: %v", inv.Jobs)
	}
	if _, ok := inv.Projects["other/app"]; ok {
		t.Fatalf("Projects holds another tenant's path: %v", inv.Projects)
	}
	if _, ok := inv.Jobs["acme/app"]; !ok {
		t.Fatalf("Jobs = %v, want acme/app", inv.Jobs)
	}
	if _, ok := inv.Projects["acme/app"]; !ok {
		t.Fatalf("Projects = %v, want acme/app", inv.Projects)
	}
}

// TestJobScheduleCellTruncationDefaults proves a new watermark row starts
// with both truncation flags false, so a cell reads complete once it
// reports a complete set. A row that migration 00035 backfilled holds
// true until its cell reports again.
func TestJobScheduleCellTruncationDefaults(t *testing.T) {
	t.Parallel()

	_, db := setupLedgerPool(t)
	for _, column := range []string{"schedules_truncated", "branches_truncated"} {
		var def string
		if err := db.QueryRow(`
			SELECT column_default FROM information_schema.columns
			WHERE table_schema = 'varroa' AND table_name = 'job_schedule_cells' AND column_name = $1
		`, column).Scan(&def); err != nil {
			t.Fatalf("read default of %s: %v", column, err)
		}
		if def != "false" {
			t.Fatalf("default of %s = %q, want false", column, def)
		}
	}

	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedule_cells (cluster, namespace, cell, observed_at)
		VALUES ($1, $2, $3, now())
	`, jsTestCluster, jsTestNS, jsTestCell); err != nil {
		t.Fatalf("insert watermark row: %v", err)
	}
	if got := truncationFlag(t, db, "schedules_truncated"); got {
		t.Fatal("new row schedules_truncated = true, want false")
	}
	if got := truncationFlag(t, db, "branches_truncated"); got {
		t.Fatal("new row branches_truncated = true, want false")
	}
}

// TestJobInventoryMigratedTruncatedRowIsUnknown simulates a row that
// migration 00035 left truncated: its cell reported a set, and the flag
// is true. The cell reads unknown until a complete set from it applies.
func TestJobInventoryMigratedTruncatedRowIsUnknown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedJobInventoryPlacement(t, db, jiMembersActiveAndStandby)
	base := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedule_cells
			(cluster, namespace, cell, observed_at, schedules_truncated, branches_truncated)
		VALUES ($1, $2, $3, $4, true, true)
	`, jsTestCluster, jsTestNS, jsTestCell, base); err != nil {
		t.Fatalf("insert migrated watermark row: %v", err)
	}

	inv := readInventory(t, NewReader(pool))
	if inv.JobsKnown {
		t.Fatal("a truncated migrated row must leave the jobs unknown")
	}

	g := NewIngester(pool, nil, nil)
	applyInventorySchedules(t, g, jsTestCell, base.Add(time.Second), false)
	inv = readInventory(t, NewReader(pool))
	if !inv.JobsKnown {
		t.Fatal("a complete set from the cell must make the jobs known")
	}
}
