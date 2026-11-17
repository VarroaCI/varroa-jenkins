package recordstore

import (
	"context"
	"testing"
)

// TestApplyChildTableKindsInsertNoRunsRow asserts that run.stage,
// run.artifacts, and run.quality write only their own tables for a run
// with no runs row. Each write inserts no runs row. The intent row and
// the job's watermark stay unchanged.
func TestApplyChildTableKindsInsertNoRunsRow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		kind    string
		payload any
		tables  []string
	}{
		{
			name: "run.stage",
			kind: KindRunStage,
			payload: RunStagePayload{
				StageID: "node-1", Name: "Build", NodeType: "stage", Status: "IN_PROGRESS",
			},
			tables: []string{"run_stages"},
		},
		{
			name: "run.artifacts",
			kind: KindRunArtifacts,
			payload: RunArtifactsPayload{
				Path: "build/out.jar", SizeBytes: 1024, BlobKey: "artifacts/core/acme/cell-a/out.jar",
			},
			tables: []string{"run_artifacts"},
		},
		{
			name:    "run.quality",
			kind:    KindRunQuality,
			payload: qualitySeedPayload(),
			tables:  []string{"run_coverage", "run_warnings"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			intentKey := "manual:no-runs-row-" + tc.name
			runID := seedGrantedIntent(t, pool, intentKey)
			before := snapshotStartedRun(t, db, intentKey, runID.String())
			g := NewIngester(pool, nil, nil)

			env := newTestEnvelope(t, runID, intentKey, tc.kind, tc.payload)
			if _, err := g.Apply(context.Background(), env); err != nil {
				t.Fatalf("apply %s: %v", tc.kind, err)
			}

			// The kind's own rows prove that the write ran. Without them,
			// a write that failed in silence would also leave no runs row.
			for _, table := range tc.tables {
				if n := countRows(t, pool, table, runID); n != 1 {
					t.Fatalf("%s rows = %d, want 1", table, n)
				}
			}
			if n := countRows(t, pool, "runs", runID); n != 0 {
				t.Fatalf("runs rows = %d, want 0", n)
			}
			assertStartedRunUnchanged(t, db, intentKey, runID.String(), before)
		})
	}
}
