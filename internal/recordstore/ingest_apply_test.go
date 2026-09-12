package recordstore

import (
	"context"
	"errors"
	"testing"
)

// TestApplyUnknownKindIsRejected asserts Apply returns ErrUnknownKind for
// a kind outside the eight-kind set, with no database write.
func TestApplyUnknownKindIsRejected(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:unknown-kind-1")
	g := NewIngester(pool, nil, nil)

	env := newTestEnvelope(t, runID, "manual:unknown-kind-1", "run.frobnicated", map[string]any{})
	if _, err := g.Apply(context.Background(), env); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("err = %v, want ErrUnknownKind", err)
	}
}

// TestApplyIntentMissingIsRejected asserts Apply returns ErrIntentMissing
// for a run_id with no run_intents row.
func TestApplyIntentMissingIsRejected(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := newTestUUIDv7String()
	env := IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunHeartbeat,
		RunID:         runID,
		IntentKey:     "manual:missing",
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          "cell-a",
		CellInstance:  "pod-a",
		Tenant:        testTenant,
		JobPath:       testJobPath,
	}
	if _, err := g.Apply(context.Background(), env); !errors.Is(err, ErrIntentMissing) {
		t.Fatalf("err = %v, want ErrIntentMissing", err)
	}
}

// TestApplyBuildNumberConflictStoresNullAndFlagsConflict asserts a
// run.started write that collides with another intent's already-claimed
// build number commits with a null build_number, moves the intent to
// CONSUMED with a null build number, and reports ApplyResult.Conflict.
func TestApplyBuildNumberConflictStoresNullAndFlagsConflict(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runA := seedGrantedIntent(t, pool, "manual:conflict-a") // reserves build_number 1
	_ = seedGrantedIntent(t, pool, "manual:conflict-b")     // reserves build_number 2
	g := NewIngester(pool, nil, nil)

	colliding := int64(2)
	env := newTestEnvelope(t, runA, "manual:conflict-a", KindRunStarted, RunStartedPayload{
		BuildNumber: &colliding,
	})

	result, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply run.started with colliding build number: %v", err)
	}
	if !result.Conflict {
		t.Fatal("ApplyResult.Conflict = false, want true")
	}

	var buildNumber *int64
	var state string
	if err := db.QueryRow(`SELECT build_number, state FROM varroa.run_intents WHERE run_id = $1`, runA).
		Scan(&buildNumber, &state); err != nil {
		t.Fatalf("read run_intents: %v", err)
	}
	if buildNumber != nil {
		t.Fatalf("run_intents.build_number = %v, want nil", *buildNumber)
	}
	if state != stateConsumed {
		t.Fatalf("state = %q, want CONSUMED", state)
	}

	var runsBuildNumber *int64
	if err := db.QueryRow(`SELECT build_number FROM varroa.runs WHERE run_id = $1`, runA).Scan(&runsBuildNumber); err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if runsBuildNumber != nil {
		t.Fatalf("runs.build_number = %v, want nil", *runsBuildNumber)
	}
}
