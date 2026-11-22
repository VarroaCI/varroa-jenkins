package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// foreignInstanceEnvelope returns newTestEnvelope's envelope as a cell
// instance that does not own the run sends it. The intent that
// newTestClaimRequest grants names pod-a.
func foreignInstanceEnvelope(t *testing.T, runID uuid.UUID, intentKey, kind string, payload any) IngestEnvelope {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, kind, payload)
	env.Cell = "cell-b"
	env.CellInstance = "pod-b"
	return env
}

// readStoredLogSummary reads runs.payload.log_summary of runID as a map.
func readStoredLogSummary(t *testing.T, db *sql.DB, runID uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT payload->'log_summary' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	summary := map[string]any{}
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatalf("decode log summary %s: %v", raw, err)
	}
	return summary
}

// applyEnvelopes applies each envelope in order and fails the test on
// the first error.
func applyEnvelopes(t *testing.T, g *Ingester, envs ...IngestEnvelope) {
	t.Helper()
	for _, env := range envs {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply %s from %s: %v", env.Kind, env.CellInstance, err)
		}
	}
}

// TestApplyRunLogHoldsNoOwnerCheck asserts that applyRunLog merges the
// summary of a cell instance that does not own the run as it merges the
// owner's. The rank and finalized rules do not bound that summary, so
// the gateway withholds a non-owner's summary and sends only its own
// log facts (BusHandler.withholdSenderLogSummary).
func TestApplyRunLogHoldsNoOwnerCheck(t *testing.T) {
	t.Parallel()
	result := "SUCCESS"

	t.Run("a loss reason of a non-owner stays for good", func(t *testing.T) {
		t.Parallel()
		pool, db := setupLedgerPool(t)
		intentKey := "manual:" + newTestUUIDv7String()
		runID := startRunForFinalize(t, pool, intentKey)
		g := NewIngester(pool, nil, nil)
		owner := map[string]any{"chunk_count": 4, "dropped_bytes": 0, "incomplete": false, "finalized": true}
		applyEnvelopes(t, g,
			newTestEnvelope(t, runID, intentKey, KindRunLog, owner),
			newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result}),
			foreignInstanceEnvelope(t, runID, intentKey, KindRunLog, map[string]any{
				"chunk_count": 9, "dropped_bytes": 3, "incomplete": true, "incomplete_reason": "jenkins-restart", "finalized": false,
			}),
			foreignInstanceEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result}),
			newTestEnvelope(t, runID, intentKey, KindRunLog, owner),
		)

		got := readStoredLogSummary(t, db, runID)
		want := map[string]any{
			"chunk_count": float64(4), "dropped_bytes": float64(0),
			"incomplete": true, "incomplete_reason": "jenkins-restart", "finalized": true,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("log_summary = %v, want %v: the non-owner's loss survives the owner's complete log", got, want)
		}
	})

	t.Run("a complete claim of a non-owner clears a generic marker", func(t *testing.T) {
		t.Parallel()
		pool, db := setupLedgerPool(t)
		intentKey := "manual:" + newTestUUIDv7String()
		runID := startRunForFinalize(t, pool, intentKey)
		g := NewIngester(pool, nil, nil)
		applyEnvelopes(t, g,
			newTestEnvelope(t, runID, intentKey, KindRunLog, map[string]any{
				"chunk_count": 4, "dropped_bytes": 0, "incomplete": false, "finalized": false,
			}),
			newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result}),
			newTestEnvelope(t, runID, intentKey, KindRunLog, incompleteWith("finalize-deadline")),
			foreignInstanceEnvelope(t, runID, intentKey, KindRunLog, map[string]any{
				"chunk_count": 2, "dropped_bytes": 0, "incomplete": false, "finalized": false,
			}),
		)

		got := readStoredLogSummary(t, db, runID)
		want := map[string]any{"chunk_count": float64(2), "dropped_bytes": float64(0), "incomplete": false, "finalized": false}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("log_summary = %v, want %v: the non-owner's claim replaces the owner's marker", got, want)
		}
	})
}

// TestApplyRunLogGatewayFactsOfANonOwnerKeepTheOwnersSummary asserts that
// the payload the gateway sends for a non-owner's completion changes no
// fact of the owner's summary. finalized true and an incomplete marker
// with a gateway reason describe the stored log, so they merge. The
// owner's chunk count, dropped bytes, and complete flag stay.
func TestApplyRunLogGatewayFactsOfANonOwnerKeepTheOwnersSummary(t *testing.T) {
	t.Parallel()
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	result := "SUCCESS"
	applyEnvelopes(t, g,
		newTestEnvelope(t, runID, intentKey, KindRunLog, map[string]any{
			"chunk_count": 4, "dropped_bytes": 1, "incomplete": false, "finalized": false,
		}),
		newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result}),
		foreignInstanceEnvelope(t, runID, intentKey, KindRunLog, map[string]any{"finalized": true}),
		foreignInstanceEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result}),
	)

	got := readStoredLogSummary(t, db, runID)
	want := map[string]any{"chunk_count": float64(4), "dropped_bytes": float64(1), "incomplete": false, "finalized": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log_summary after the finalized fact = %v, want %v", got, want)
	}

	applyEnvelopes(t, g, foreignInstanceEnvelope(t, runID, intentKey, KindRunLog, incompleteWith("gateway-gap")))
	got = readStoredLogSummary(t, db, runID)
	want = map[string]any{
		"chunk_count": float64(4), "dropped_bytes": float64(1),
		"incomplete": true, "incomplete_reason": "gateway-gap", "finalized": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log_summary after the gap fact = %v, want %v", got, want)
	}
}
