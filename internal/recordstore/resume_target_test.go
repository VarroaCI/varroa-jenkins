package recordstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedResumeTargetPlacement inserts one tenant_placements row naming
// cell, cellInstance, and rehydratedInstance for resumeTargetForRun's
// three tests.
func seedResumeTargetPlacement(t *testing.T, db *sql.DB, cell, cellInstance, rehydratedInstance string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance)
		VALUES ($1, $2, $3, $4, 1, $5, $6)
	`, testCluster, testTenant, testNamespace, cell, cellInstance, rehydratedInstance); err != nil {
		t.Fatalf("seed tenant placement: %v", err)
	}
}

// withResumeTargetTx opens a read-only transaction over pool and calls
// resumeTargetForRun for run, then rolls the transaction back: the
// function only reads.
func withResumeTargetTx(t *testing.T, pool *pgxpool.Pool, run meshCandidate) (resumeMember, bool) {
	t.Helper()
	member, refusal := withResumeTargetRefusal(t, pool, run)
	return member, refusal == resumeRefusalNone
}

// withResumeTargetRefusal is withResumeTargetTx with the refusal reason
// the helper answered, for a test that tells two refusals apart.
func withResumeTargetRefusal(t *testing.T, pool *pgxpool.Pool, run meshCandidate) (resumeMember, resumeRefusal) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	member, refusal, err := resumeTargetForRun(ctx, tx, run)
	if err != nil {
		t.Fatalf("resumeTargetForRun: %v", err)
	}
	return member, refusal
}

// TestResumeTargetForRunThreeTests asserts design D9's three tests: an
// empty cell_instance, a cell_instance equal to the run's own stored
// owner, and a rehydrated_instance that differs from the live instance,
// each answer not-ok. All three holding answers ok with the member.
func TestResumeTargetForRunThreeTests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name               string
		cell               string
		cellInstance       string
		rehydratedInstance string
		wantOK             bool
	}{
		{
			name: "an empty cell_instance answers not-ok",
			cell: "cell-b", cellInstance: "", rehydratedInstance: "",
		},
		{
			name: "a cell_instance equal to the run's own stored owner answers not-ok",
			cell: "cell-b", cellInstance: "pod-dead", rehydratedInstance: "pod-dead",
		},
		{
			name: "a rehydrated_instance unequal to the live instance answers not-ok",
			cell: "cell-b", cellInstance: "pod-live", rehydratedInstance: "pod-other",
		},
		{
			name: "all three holding answers ok",
			cell: "cell-b", cellInstance: "pod-live", rehydratedInstance: "pod-live", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			seedResumeTargetPlacement(t, db, tc.cell, tc.cellInstance, tc.rehydratedInstance)

			run := meshCandidate{cluster: testCluster, tenant: testTenant, cellInstance: "pod-dead"}
			member, ok := withResumeTargetTx(t, pool, run)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK {
				if member.cell != tc.cell || member.instance != tc.cellInstance || member.namespace != testNamespace {
					t.Fatalf("member = %+v, want cell=%s instance=%s namespace=%s",
						member, tc.cell, tc.cellInstance, testNamespace)
				}
			}
		})
	}
}

// TestResumeTargetForRunNoPlacementRow asserts a tenant with no
// tenant_placements row answers not-ok, with no error.
func TestResumeTargetForRunNoPlacementRow(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	run := meshCandidate{cluster: testCluster, tenant: testTenant, cellInstance: "pod-dead"}
	_, ok := withResumeTargetTx(t, pool, run)
	if ok {
		t.Fatal("want not-ok for a tenant with no placement row")
	}
}
