package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// meshResumeTarget is the live instance every test in this file
// dispatches its resume to.
const meshResumeTarget = "pod-live"

// readHeartbeatStamps reads one run's two liveness columns.
func readHeartbeatStamps(t *testing.T, db *sql.DB, runID uuid.UUID) (last, received sql.NullTime) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT last_heartbeat_at, heartbeat_received_at FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&last, &received); err != nil {
		t.Fatalf("read the heartbeat columns: %v", err)
	}
	return last, received
}

// seedDispatchedRun claims one RUNNING run owned by the confirmed-gone
// instance, then runs a real takeover round. The round moves the run's
// owner to meshResumeTarget and writes the dispatched resume record, so
// a test starts from the state the mesh itself produces.
func seedDispatchedRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB) uuid.UUID {
	t.Helper()
	runID := seedMeshRunningRun(t, pool, db)
	seedMeshFleetLock(t, db, runID)
	seedMeshPlacement(t, db, meshResumeTarget, meshResumeTarget)
	conn := newTestActionConn(t)
	collectActions(t, conn)
	if got := runTakeover(t, pool, conn, nil, nil); got != 1 {
		t.Fatalf("published = %d, want 1 dispatched resume", got)
	}
	return runID
}

// applyHeartbeatFrom applies one run.heartbeat envelope for runID that
// names instance as its sender.
func applyHeartbeatFrom(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, instance string) {
	t.Helper()
	var intentKey string
	if err := pool.QueryRow(context.Background(),
		`SELECT intent_key FROM varroa.run_intents WHERE run_id = $1`, runID).Scan(&intentKey); err != nil {
		t.Fatalf("read the intent key: %v", err)
	}
	beat := newTestEnvelope(t, runID, intentKey, KindRunHeartbeat, map[string]any{})
	beat.CellInstance = instance
	if _, err := NewIngester(pool, nil, nil).Apply(context.Background(), beat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}
}

// TestLateHeartbeatFromTheDeadOwnerLeavesTheRunMarkable is this fix's
// proof. The dead instance can still deliver a heartbeat that was in
// flight when the mesh moved the run to another target. That heartbeat
// proves nothing, so it must raise neither liveness column: a raised
// column stands above the resume's own request time forever, and both
// the resume guard and the marking write then refuse the run for good.
func TestLateHeartbeatFromTheDeadOwnerLeavesTheRunMarkable(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedDispatchedRun(t, pool, db)

	applyHeartbeatFrom(t, pool, runID, meshDeadInstance)

	last, received := readHeartbeatStamps(t, db, runID)
	if last.Valid || received.Valid {
		t.Fatalf("last_heartbeat_at = %v, heartbeat_received_at = %v, want both null", last, received)
	}
	if _, _, instance := readMeshResume(t, db, runID); instance != meshResumeTarget {
		t.Fatalf("mesh.resume.instance = %q, want %q", instance, meshResumeTarget)
	}

	past := time.Now().UTC().Add(resumeDeadline + time.Minute)
	c := fetchRunningCandidate(t, pool, runID)
	reason, ok, decided := evaluateResumeGuard(past, c)
	if !decided || !ok || reason != ReasonResumeTimeout {
		t.Fatalf("resume guard = (%q, %v, %v), want (%q, true, true)",
			reason, ok, decided, ReasonResumeTimeout)
	}
	changed, err := markInterrupted(context.Background(), pool, c, reason, past)
	if err != nil {
		t.Fatalf("markInterrupted: %v", err)
	}
	if !changed {
		t.Fatal("the marking write refused the run, so the run stays RUNNING for good")
	}
}

// TestHeartbeatFromTheResumeTargetRaisesTheStamps proves the target's
// own heartbeat still counts. That heartbeat is the only proof a
// resumed build gives, so it raises both columns and clears the resume
// record.
func TestHeartbeatFromTheResumeTargetRaisesTheStamps(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedDispatchedRun(t, pool, db)

	applyHeartbeatFrom(t, pool, runID, meshResumeTarget)

	last, received := readHeartbeatStamps(t, db, runID)
	if !last.Valid || !received.Valid {
		t.Fatalf("last_heartbeat_at = %v, heartbeat_received_at = %v, want both set", last, received)
	}
	if _, ok := readResumeInstance(t, db, runID); ok {
		t.Fatal("the target's heartbeat left the resume record in place")
	}
}

// TestHeartbeatFromTheOwnerRaisesTheStamps proves the ordinary case is
// unchanged: a run with no resume record, whose own owner heartbeats,
// raises both columns.
func TestHeartbeatFromTheOwnerRaisesTheStamps(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRunningRun(t, pool, newTestClaimRequest(intentKey, 0))

	applyHeartbeatFrom(t, pool, runID, "pod-a")

	last, received := readHeartbeatStamps(t, db, runID)
	if !last.Valid || !received.Valid {
		t.Fatalf("last_heartbeat_at = %v, heartbeat_received_at = %v, want both set", last, received)
	}
}

// TestHeartbeatWithNoInstanceRaisesTheStamps proves an envelope that
// names no sender still counts. The owner test needs a name on both
// sides, so an unnamed heartbeat keeps the earlier behaviour.
func TestHeartbeatWithNoInstanceRaisesTheStamps(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedRunningRun(t, pool, newTestClaimRequest(intentKey, 0))

	applyHeartbeatFrom(t, pool, runID, "")

	last, received := readHeartbeatStamps(t, db, runID)
	if !last.Valid || !received.Valid {
		t.Fatalf("last_heartbeat_at = %v, heartbeat_received_at = %v, want both set", last, received)
	}
}
