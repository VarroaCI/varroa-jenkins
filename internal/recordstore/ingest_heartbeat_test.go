package recordstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applySummaryHeartbeat applies one run.heartbeat envelope for runID
// whose payload carries payload.
func applySummaryHeartbeat(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, intentKey string, payload map[string]any) {
	t.Helper()
	beat := newTestEnvelope(t, runID, intentKey, KindRunHeartbeat, payload)
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), beat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}
}

// readRunSummary reads one run's summary text and its payload's
// summary_seq key. Each value is invalid when the row holds neither.
func readRunSummary(t *testing.T, db *sql.DB, runID uuid.UUID) (text, seq sql.NullString) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT summary, payload->>'summary_seq' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&text, &seq); err != nil {
		t.Fatalf("read the summary columns: %v", err)
	}
	return text, seq
}

// startRunForSummary writes the run.started envelope that gives the run
// its row, so a later heartbeat takes the update path.
func startRunForSummary(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, intentKey string) {
	t.Helper()
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
}

// TestHeartbeatRaisesTheSummaryAndItsWatermark proves a heartbeat whose
// payload carries a summary value writes both the summary column and the
// payload's summary_seq key.
func TestHeartbeatRaisesTheSummaryAndItsWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:heartbeat-summary-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	startRunForSummary(t, pool, runID, intentKey)

	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"summary": "deployed canary to 10%", "seq": int64(5)})

	text, seq := readRunSummary(t, db, runID)
	if !text.Valid || text.String != "deployed canary to 10%" {
		t.Fatalf("summary = %v, want the envelope's text", text)
	}
	if !seq.Valid || seq.String != "5" {
		t.Fatalf("summary_seq = %v, want 5", seq)
	}
}

// TestStaleHeartbeatLeavesTheSummaryAlone proves a heartbeat whose seq
// sits below the stored summary_seq changes neither the summary column
// nor the watermark. A redelivered or reordered heartbeat therefore
// cannot restore an older line of text.
func TestStaleHeartbeatLeavesTheSummaryAlone(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:heartbeat-summary-stale-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	startRunForSummary(t, pool, runID, intentKey)

	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"summary": "newer line", "seq": int64(9)})
	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"summary": "older line", "seq": int64(2)})

	text, seq := readRunSummary(t, db, runID)
	if !text.Valid || text.String != "newer line" {
		t.Fatalf("summary = %v, want the newer line", text)
	}
	if !seq.Valid || seq.String != "9" {
		t.Fatalf("summary_seq = %v, want 9", seq)
	}
}

// TestHeartbeatWithoutASummaryChangesNothing proves a heartbeat whose
// payload carries no summary key writes neither the summary column nor
// the summary_seq key, whatever its seq. A run that never calls
// varroa.summary therefore keeps both values null for its whole life.
func TestHeartbeatWithoutASummaryChangesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:heartbeat-summary-absent-1"
	runID := seedGrantedIntent(t, pool, intentKey)
	startRunForSummary(t, pool, runID, intentKey)

	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"seq": int64(4)})

	text, seq := readRunSummary(t, db, runID)
	if text.Valid {
		t.Fatalf("summary = %v, want null", text)
	}
	if seq.Valid {
		t.Fatalf("summary_seq = %v, want null", seq)
	}
}

// TestHeartbeatSummaryOnANullPayloadRowStillRaises proves the summary
// raise reaches a row whose payload is still null. A first heartbeat
// inserts the row and lists no payload column, so the stored payload is
// null and the concatenation that writes summary_seq reads a null left
// operand.
func TestHeartbeatSummaryOnANullPayloadRowStillRaises(t *testing.T) {
	pool, db := setupLedgerPool(t)
	const intentKey = "manual:heartbeat-summary-null-payload-1"
	runID := seedGrantedIntent(t, pool, intentKey)

	// The first heartbeat inserts the placeholder row and writes no
	// payload.
	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"seq": int64(0)})
	var payloadIsNull bool
	if err := db.QueryRow(`SELECT payload IS NULL FROM varroa.runs WHERE run_id = $1`, runID).Scan(&payloadIsNull); err != nil {
		t.Fatalf("read the payload: %v", err)
	}
	if !payloadIsNull {
		t.Fatal("the first heartbeat wrote a payload, so the case proves nothing")
	}

	applySummaryHeartbeat(t, pool, runID, intentKey, map[string]any{"summary": "first line", "seq": int64(1)})

	text, seq := readRunSummary(t, db, runID)
	if !text.Valid || text.String != "first line" {
		t.Fatalf("summary = %v, want the envelope's text", text)
	}
	if !seq.Valid || seq.String != "1" {
		t.Fatalf("summary_seq = %v, want 1", seq)
	}
}
