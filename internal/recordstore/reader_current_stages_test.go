package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// currentStageRow is one varroa.run_stages row a CurrentStages test
// seeds.
type currentStageRow struct {
	stageID  string
	name     string
	nodeType string
	status   string
	// parentID names the node the plugin nested this row under, which is
	// a branch or an outer stage. A top-level stage carries none, and the
	// INSERT then writes NULL.
	parentID  *string
	createdAt time.Time
	startedAt *time.Time
}

// topLevelStage builds a row for a top-level stage: node_type 'stage'
// and no parent.
func topLevelStage(stageID, name, status string, createdAt time.Time, startedAt *time.Time) currentStageRow {
	return currentStageRow{
		stageID: stageID, name: name, nodeType: "stage", status: status,
		createdAt: createdAt, startedAt: startedAt,
	}
}

// nestedRow builds a row for a node inside parentID: a parallel branch
// carries node_type 'parallel', and a stage inside a branch carries
// 'stage'.
func nestedRow(stageID, name, nodeType, status, parentID string, createdAt time.Time, startedAt *time.Time) currentStageRow {
	return currentStageRow{
		stageID: stageID, name: name, nodeType: nodeType, status: status, parentID: &parentID,
		createdAt: createdAt, startedAt: startedAt,
	}
}

// seedCurrentStagesRow inserts one varroa.run_stages row directly, so a
// test can build a stage fixture without going through the ingester.
func seedCurrentStagesRow(t *testing.T, db *sql.DB, runID uuid.UUID, row currentStageRow) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_stages (
			run_id, stage_id, created_at, parent_id, name, node_type, status, started_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, runID, row.stageID, row.createdAt, row.parentID, row.name, row.nodeType, row.status, row.startedAt); err != nil {
		t.Fatalf("seed stage %s for run %s: %v", row.name, runID, err)
	}
}

// TestCurrentStagesAnswersOneStagePerRun asserts the call answers the
// stage in progress for one run, and no entry for a run whose only stage
// has already finished.
func TestCurrentStagesAnswersOneStagePerRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	startedAt := base.Add(2 * time.Minute)
	inProgress := uuid.New()
	finished := uuid.New()
	seedCurrentStagesRow(t, db, inProgress, topLevelStage("stage-1", "Build", "IN_PROGRESS", base, &startedAt))
	seedCurrentStagesRow(t, db, finished, topLevelStage("stage-1", "Build", "SUCCESS", base, &startedAt))

	got, err := reader.CurrentStages(ctx, []string{inProgress.String(), finished.String()})
	if err != nil {
		t.Fatalf("CurrentStages: %v", err)
	}
	if len(got) != 1 || got[inProgress.String()] != "Build" {
		t.Fatalf("CurrentStages = %v, want one entry for %s naming Build", got, inProgress)
	}
}

// TestCurrentStagesAnswersNoEntryForAnUnknownRun asserts a run id the
// store holds no in-progress stage for carries no map entry.
func TestCurrentStagesAnswersNoEntryForAnUnknownRun(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)

	got, err := reader.CurrentStages(context.Background(), []string{uuid.New().String()})
	if err != nil {
		t.Fatalf("CurrentStages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CurrentStages = %v, want no entry", got)
	}
}

// TestCurrentStagesAnswersAnEmptyListWithoutAQuery asserts an empty run
// list answers an empty map.
func TestCurrentStagesAnswersAnEmptyListWithoutAQuery(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)

	got, err := reader.CurrentStages(context.Background(), nil)
	if err != nil {
		t.Fatalf("CurrentStages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CurrentStages = %v, want an empty map", got)
	}
}

// TestCurrentStagesPicksTheLaterStartedStage asserts a run that holds two
// top-level stages in progress answers the one that started later.
func TestCurrentStagesPicksTheLaterStartedStage(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	base := time.Now().UTC().Add(-time.Hour)
	early := base.Add(time.Minute)
	late := base.Add(5 * time.Minute)
	runID := uuid.New()
	seedCurrentStagesRow(t, db, runID, topLevelStage("stage-a", "Checkout", "IN_PROGRESS", base, &early))
	seedCurrentStagesRow(t, db, runID, topLevelStage("stage-b", "Test", "IN_PROGRESS", base.Add(time.Minute), &late))

	got, err := reader.CurrentStages(context.Background(), []string{runID.String()})
	if err != nil {
		t.Fatalf("CurrentStages: %v", err)
	}
	if got[runID.String()] != "Test" {
		t.Fatalf("CurrentStages = %v, want Test for %s", got, runID)
	}
}

// TestCurrentStagesPrefersAStartedStage asserts a stage that has started
// wins over a later-created stage in progress that carries no started_at
// yet, such as a sibling of a parallel branch.
func TestCurrentStagesPrefersAStartedStage(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	base := time.Now().UTC().Add(-time.Hour)
	started := base.Add(time.Minute)
	runID := uuid.New()
	seedCurrentStagesRow(t, db, runID, topLevelStage("stage-a", "Build", "IN_PROGRESS", base, &started))
	seedCurrentStagesRow(t, db, runID, topLevelStage("stage-b", "Test", "IN_PROGRESS", base.Add(time.Hour), nil))

	got, err := reader.CurrentStages(context.Background(), []string{runID.String()})
	if err != nil {
		t.Fatalf("CurrentStages: %v", err)
	}
	if got[runID.String()] != "Build" {
		t.Fatalf("CurrentStages = %v, want Build for %s", got, runID)
	}
}

// TestCurrentStagesAnswersTheTopLevelStageOfAFanOut asserts a run whose
// two parallel branches and one nested stage are in progress answers the
// top-level stage, never a branch and never the nested stage. The plugin
// writes one created_at for every stage row of a run, so started_at alone
// orders these rows.
func TestCurrentStagesAnswersTheTopLevelStageOfAFanOut(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)

	base := time.Now().UTC().Add(-time.Hour)
	stageStart := base
	branchStart := base.Add(time.Minute)
	nestedStart := base.Add(2 * time.Minute)
	runID := uuid.New()
	seedCurrentStagesRow(t, db, runID, topLevelStage("stage-test", "Test", "IN_PROGRESS", base, &stageStart))
	seedCurrentStagesRow(t, db, runID, nestedRow("branch-unit", "unit", "parallel", "IN_PROGRESS", "stage-test", base, &branchStart))
	seedCurrentStagesRow(t, db, runID, nestedRow("branch-integration", "integration", "parallel", "IN_PROGRESS", "stage-test", base, &branchStart))
	seedCurrentStagesRow(t, db, runID, nestedRow("stage-unit-checkout", "unit-checkout", "stage", "IN_PROGRESS", "branch-unit", base, &nestedStart))

	got, err := reader.CurrentStages(context.Background(), []string{runID.String()})
	if err != nil {
		t.Fatalf("CurrentStages: %v", err)
	}
	if got[runID.String()] != "Test" {
		t.Fatalf("CurrentStages = %v, want Test, the top-level stage of %s", got, runID)
	}
}

// TestCurrentStagesObeysASchemaTooOld asserts the call refuses a store
// whose schema version is below this reader's minimum.
func TestCurrentStagesObeysASchemaTooOld(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool)
	ctx := context.Background()

	if _, err := db.Exec(`DELETE FROM varroa.goose_db_version`); err != nil {
		t.Fatalf("clear goose_db_version: %v", err)
	}
	if _, err := reader.CurrentStages(ctx, []string{uuid.New().String()}); !errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("CurrentStages error = %v, want ErrSchemaTooOld", err)
	}
}
