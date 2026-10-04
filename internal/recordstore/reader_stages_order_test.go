package recordstore

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestListStagesAnswersStageStartOrder asserts the stage list of one run
// is in stage-start order. Ingest writes the run's created_at on every
// stage row, so the fixture gives every row the same created_at. A row
// with no started_at comes last. Two rows with the same started_at come
// in numeric order of their ids, so id 4 comes before id 12.
func TestListStagesAnswersStageStartOrder(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	reader := NewReader(pool).(*pgReader)

	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	at := func(d time.Duration) *time.Time {
		v := created.Add(d)
		return &v
	}
	runID := uuid.New()
	rows := []currentStageRow{
		topLevelStage("30", "Queued", "NOT_EXECUTED", created, nil),
		nestedRow("12", "Branch: B", "parallel", "SUCCESS", "3", created, at(2*time.Minute)),
		topLevelStage("3", "CI", "FAILURE", created, at(time.Minute)),
		nestedRow("4", "Branch: A", "parallel", "FAILURE", "3", created, at(2*time.Minute)),
		topLevelStage("2", "Checkout", "SUCCESS", created, at(0)),
	}
	for _, row := range rows {
		seedCurrentStagesRow(t, db, runID, row)
	}

	stages, err := reader.listStages(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("listStages: %v", err)
	}
	got := make([]string, 0, len(stages))
	for _, s := range stages {
		got = append(got, s.StageID)
	}
	want := []string{"2", "3", "4", "12", "30"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stage order = %v, want %v", got, want)
	}
}
