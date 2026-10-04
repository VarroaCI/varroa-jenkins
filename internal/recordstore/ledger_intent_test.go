package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAVetoWritesNoRow asserts a vetoed claim leaves the key usable: no
// run_intents row exists, and a later claim with the same key can still
// be granted.
func TestAVetoWritesNoRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, denyingFence{reasonCode: ReasonNotReady})

	req := newTestClaimRequest("manual:x-veto", 0)
	vetoed := ledger.Claim(context.Background(), req)
	if vetoed.Decision != DecisionVetoed {
		t.Fatalf("want vetoed, got %+v", vetoed)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)

	allowed := NewLedger(pool, nil)
	granted := allowed.Claim(context.Background(), req)
	if granted.Decision != DecisionGranted {
		t.Fatalf("want the same key granted once the veto clears, got %+v", granted)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// TestRunStartedConsumesIntent asserts a run.started event moves a
// granted intent to CONSUMED and sets consumed_at.
func TestRunStartedConsumesIntent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:x-consume", 0)
	granted := mustGrant(t, ledger, req)
	runID := uuid.MustParse(granted.RunID)

	consumedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := consumeIntent(context.Background(), pool, runID, granted.BuildNumber, consumedAt); err != nil {
		t.Fatalf("consume intent: %v", err)
	}

	var state string
	var storedConsumedAt time.Time
	var storedBuildNumber int64
	if err := db.QueryRow(`
		SELECT state, consumed_at, build_number FROM varroa.run_intents WHERE run_id = $1
	`, runID).Scan(&state, &storedConsumedAt, &storedBuildNumber); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if state != stateConsumed {
		t.Fatalf("want state CONSUMED, got %q", state)
	}
	if !storedConsumedAt.Equal(consumedAt) {
		t.Fatalf("want consumed_at %s, got %s", consumedAt, storedConsumedAt)
	}
	if storedBuildNumber != granted.BuildNumber {
		t.Fatalf("want build_number %d, got %d", granted.BuildNumber, storedBuildNumber)
	}
}
