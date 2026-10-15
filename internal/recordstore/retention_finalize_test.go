package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeLiveness answers CellGone with a fixed value, or an error when err
// is set.
type fakeLiveness struct {
	gone bool
	err  error
}

func (f fakeLiveness) CellGone(context.Context, RunLiveness) (bool, error) {
	return f.gone, f.err
}

// startRunForFinalize seeds a granted intent and applies a run.started
// envelope, so the resulting runs row has the columns the finalize sweep
// reads.
func startRunForFinalize(t *testing.T, pool *pgxpool.Pool, intentKey string) uuid.UUID {
	t.Helper()
	runID := seedGrantedIntent(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	return runID
}

// readLogSummaryFinalized reads runs.payload.log_summary.finalized for
// runID. found is false when the run has no log summary at all.
func readLogSummaryFinalized(t *testing.T, db *sql.DB, runID uuid.UUID) (found bool, finalized bool) {
	t.Helper()
	var raw *bool
	err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'finalized')::bool FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&raw)
	if err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	if raw == nil {
		return false, false
	}
	return true, *raw
}

// TestFinalizeStaleLogsClosesTerminalRunWithNoSummary asserts a terminal
// run with no log summary at all gets one, marked finalized and
// incomplete.
func TestFinalizeStaleLogsClosesTerminalRunWithNoSummary(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-no-summary-1")

	completed := newTestEnvelope(t, runID, "manual:finalize-no-summary-1", KindRunCompleted, RunCompletedPayload{})
	g := NewIngester(pool, nil, nil)
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	// Backdate the anchor so it is unambiguously past the wait: no
	// column here holds a timestamp on its own within the wait window.
	oldEnded := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, oldEnded, runID); err != nil {
		t.Fatalf("backdate ended_at: %v", err)
	}

	now := time.Now().UTC()
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, now)
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalized %d runs, want 1", n)
	}

	found, finalized := readLogSummaryFinalized(t, db, runID)
	if !found || !finalized {
		t.Fatalf("found=%v finalized=%v, want a summary with finalized=true", found, finalized)
	}

	var incomplete bool
	var reason string
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'incomplete')::bool, payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&incomplete, &reason); err != nil {
		t.Fatalf("read incomplete fields: %v", err)
	}
	if !incomplete || reason != "not-finalized" {
		t.Fatalf("incomplete=%v reason=%q, want true/\"not-finalized\"", incomplete, reason)
	}
}

// TestFinalizeStaleLogsKeepsAStoredReason asserts the sweep finalizes a
// run whose summary already holds a reason, and keeps that reason.
func TestFinalizeStaleLogsKeepsAStoredReason(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-keep-reason-1")
	g := NewIngester(pool, nil, nil)
	for _, env := range []IngestEnvelope{
		newTestEnvelope(t, runID, "manual:finalize-keep-reason-1", KindRunLog, map[string]any{
			"incomplete": true, "incomplete_reason": "jenkins-restart",
		}),
		newTestEnvelope(t, runID, "manual:finalize-keep-reason-1", KindRunCompleted, RunCompletedPayload{}),
	} {
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply %s: %v", env.Kind, err)
		}
	}
	oldEnded := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, oldEnded, runID); err != nil {
		t.Fatalf("backdate ended_at: %v", err)
	}

	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("finalizeStaleLogs: n=%d err=%v, want 1 run finalized", n, err)
	}

	var finalized, incomplete bool
	var reason string
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'finalized')::bool, (payload->'log_summary'->>'incomplete')::bool,
		       payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&finalized, &incomplete, &reason); err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	if !finalized || !incomplete || reason != "jenkins-restart" {
		t.Fatalf("finalized=%v incomplete=%v reason=%q, want true/true/jenkins-restart", finalized, incomplete, reason)
	}
}

// TestFinalizeStaleLogsLeavesRunInsideTheWait asserts a terminal run
// whose heartbeat is inside C3's finalize wait is left alone.
func TestFinalizeStaleLogsLeavesRunInsideTheWait(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-inside-wait-1")

	completed := newTestEnvelope(t, runID, "manual:finalize-inside-wait-1", KindRunCompleted, RunCompletedPayload{})
	g := NewIngester(pool, nil, nil)
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	heartbeat := newTestEnvelope(t, runID, "manual:finalize-inside-wait-1", KindRunHeartbeat, map[string]any{})
	if _, err := g.Apply(context.Background(), heartbeat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}

	// The run's wait anchor falls back to heartbeat_received_at, stamped at
	// "now" by the heartbeat envelope, so a one-hour wait is not yet
	// past.
	now := time.Now().UTC()
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Hour, now)
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 0 {
		t.Fatalf("finalized %d runs, want 0 (still inside the wait)", n)
	}

	found, _ := readLogSummaryFinalized(t, db, runID)
	if found {
		t.Fatalf("a log summary was created for a run still inside the wait")
	}
}

// TestFinalizeStaleLogsNeverLowersFinalizedTrue asserts a run already
// marked finalized true is left unchanged.
func TestFinalizeStaleLogsNeverLowersFinalizedTrue(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-already-true-1")

	completed := newTestEnvelope(t, runID, "manual:finalize-already-true-1", KindRunCompleted, RunCompletedPayload{})
	g := NewIngester(pool, nil, nil)
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	logEnv := newTestEnvelope(t, runID, "manual:finalize-already-true-1", KindRunLog, map[string]any{"finalized": true})
	if _, err := g.Apply(context.Background(), logEnv); err != nil {
		t.Fatalf("apply run.log: %v", err)
	}

	now := time.Now().UTC()
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, now)
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 0 {
		t.Fatalf("finalized %d runs, want 0 (already finalized)", n)
	}

	var incomplete *bool
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'incomplete')::bool FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&incomplete); err != nil {
		t.Fatalf("read incomplete field: %v", err)
	}
	if incomplete != nil {
		t.Fatalf("incomplete = %v, want the sweep to have written nothing", *incomplete)
	}
}

// TestFinalizeStaleLogsUsesHeartbeatForARunningRun asserts a RUNNING run
// whose heartbeat is 20 minutes old is finalized by the heartbeat-only
// Liveness this change ships, while a fresh heartbeat is left alone.
func TestFinalizeStaleLogsUsesHeartbeatForARunningRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-stale-heartbeat-1")

	staleHeartbeat := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, staleHeartbeat, runID); err != nil {
		t.Fatalf("stamp stale heartbeat: %v", err)
	}

	now := time.Now().UTC()
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, now)
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalized %d runs, want 1 (20-minute-old heartbeat means the cell is gone)", n)
	}
	found, finalized := readLogSummaryFinalized(t, db, runID)
	if !found || !finalized {
		t.Fatalf("found=%v finalized=%v, want a closed summary", found, finalized)
	}

	// A second RUNNING run whose heartbeat is fresh is left alone.
	runID2 := startRunForFinalize(t, pool, "manual:finalize-fresh-heartbeat-1")
	freshHeartbeat := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, freshHeartbeat, runID2); err != nil {
		t.Fatalf("stamp fresh heartbeat: %v", err)
	}
	if _, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, now); err != nil {
		t.Fatalf("finalizeStaleLogs (second call): %v", err)
	}
	found2, _ := readLogSummaryFinalized(t, db, runID2)
	if found2 {
		t.Fatalf("a fresh-heartbeat RUNNING run was finalized")
	}
}

// TestFinalizeStaleLogsSkipsOnLivenessError asserts a Liveness error
// makes the sweep skip that run for the round.
func TestFinalizeStaleLogsSkipsOnLivenessError(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-liveness-error-1")

	staleHeartbeat := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, staleHeartbeat, runID); err != nil {
		t.Fatalf("stamp stale heartbeat: %v", err)
	}

	now := time.Now().UTC()
	liveness := fakeLiveness{err: errors.New("liveness backend unavailable")}
	n, err := finalizeStaleLogs(context.Background(), pool, liveness, time.Minute, now)
	if err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}
	if n != 0 {
		t.Fatalf("finalized %d runs, want 0 (a Liveness error must skip the run)", n)
	}
	found, _ := readLogSummaryFinalized(t, db, runID)
	if found {
		t.Fatalf("a run was finalized despite a Liveness error")
	}
}

// recordingLiveness records every RunLiveness it is called with, so a
// test can assert the finalize sweep passes the run's owning cell
// identity through the seam instead of empty strings.
type recordingLiveness struct {
	calls []RunLiveness
	gone  bool
}

func (r *recordingLiveness) CellGone(_ context.Context, run RunLiveness) (bool, error) {
	r.calls = append(r.calls, run)
	return r.gone, nil
}

// TestFinalizeStaleLogsPassesCellIdentityToLiveness asserts the sweep
// gives Liveness the run's cluster, namespace, and cell, so a
// placement-aware Liveness can look up the right cell without an
// undocumented extra lookup.
func TestFinalizeStaleLogsPassesCellIdentityToLiveness(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-cell-identity-1")

	staleHeartbeat := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, staleHeartbeat, runID); err != nil {
		t.Fatalf("stamp stale heartbeat: %v", err)
	}

	liveness := &recordingLiveness{gone: true}
	now := time.Now().UTC()
	if _, err := finalizeStaleLogs(context.Background(), pool, liveness, time.Minute, now); err != nil {
		t.Fatalf("finalizeStaleLogs: %v", err)
	}

	if len(liveness.calls) != 1 {
		t.Fatalf("Liveness.CellGone called %d times, want 1", len(liveness.calls))
	}
	got := liveness.calls[0]
	if got.RunID != runID.String() {
		t.Fatalf("RunID = %q, want %q", got.RunID, runID.String())
	}
	if got.Cluster != testCluster || got.Namespace != testNamespace || got.Cell != "cell-a" {
		t.Fatalf("cell identity = %+v, want cluster=%q namespace=%q cell=%q", got, testCluster, testNamespace, "cell-a")
	}
}

// TestFinalizeStaleLogsSkipsWhenHeartbeatRacesTheUpdate asserts a
// concurrent run.log that stamps a fresher heartbeat between the
// candidate select and the finalize update leaves the log summary
// un-finalized.
func TestFinalizeStaleLogsSkipsWhenHeartbeatRacesTheUpdate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-race-1")
	g := NewIngester(pool, nil, nil)

	oldMs := time.Now().UTC().Add(-2 * time.Hour).UnixMilli()
	oldHeartbeat := newTestEnvelope(t, runID, "manual:finalize-race-1", KindRunLog, map[string]any{"writer_heartbeat_unix_ms": oldMs})
	if _, err := g.Apply(context.Background(), oldHeartbeat); err != nil {
		t.Fatalf("apply old run.log: %v", err)
	}
	completed := newTestEnvelope(t, runID, "manual:finalize-race-1", KindRunCompleted, RunCompletedPayload{})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	cutoff := time.Now().UTC().Add(-time.Minute)
	candidates, err := selectFinalizeCandidates(context.Background(), pool, cutoff, nil)
	if err != nil {
		t.Fatalf("selectFinalizeCandidates: %v", err)
	}
	var candidate *finalizeCandidate
	for i := range candidates {
		if candidates[i].runID == runID.String() {
			candidate = &candidates[i]
		}
	}
	if candidate == nil {
		t.Fatalf("run %s did not appear as a finalize candidate", runID)
	}

	// A concurrent run.log stamps a fresher heartbeat after the select
	// above already read the old one.
	freshMs := time.Now().UTC().UnixMilli()
	freshHeartbeat := newTestEnvelope(t, runID, "manual:finalize-race-1", KindRunLog, map[string]any{"writer_heartbeat_unix_ms": freshMs})
	if _, err := g.Apply(context.Background(), freshHeartbeat); err != nil {
		t.Fatalf("apply fresh run.log: %v", err)
	}

	changed, err := finalizeLogSummary(context.Background(), pool, candidate.runID, candidate.writerHeartbeatRaw, cutoff)
	if err != nil {
		t.Fatalf("finalizeLogSummary: %v", err)
	}
	if changed {
		t.Fatalf("finalizeLogSummary reported a change for a row its own guard should have skipped")
	}

	found, finalized := readLogSummaryFinalized(t, db, runID)
	if found {
		t.Fatalf("finalizeLogSummary closed a log whose heartbeat moved after the select, found=%v finalized=%v", found, finalized)
	}
}

// TestFinalizeStaleLogsIsolatesMalformedHeartbeat asserts a run whose
// stored writer_heartbeat_unix_ms is not a safe integer never poisons
// the sweep: it falls back to waitAnchor's other fields, and a
// qualifying companion run still gets finalized in the same round. It
// covers every malformed shape the safe cast must reject: a non-numeric
// string, a fractional JSON number, and a JSON integer wider than
// bigint.
func TestFinalizeStaleLogsIsolatesMalformedHeartbeat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value any
	}{
		{name: "non-numeric string", value: "bad"},
		{name: "fractional number", value: 1.5},
		{name: "integer wider than bigint", value: json.Number("123456789012345678901234567890")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			g := NewIngester(pool, nil, nil)

			malformedRunID := startRunForFinalize(t, pool, "manual:finalize-malformed-1")
			malformedLog := newTestEnvelope(t, malformedRunID, "manual:finalize-malformed-1", KindRunLog, map[string]any{"writer_heartbeat_unix_ms": tc.value})
			if _, err := g.Apply(context.Background(), malformedLog); err != nil {
				t.Fatalf("apply malformed run.log: %v", err)
			}
			malformedCompleted := newTestEnvelope(t, malformedRunID, "manual:finalize-malformed-1", KindRunCompleted, RunCompletedPayload{})
			if _, err := g.Apply(context.Background(), malformedCompleted); err != nil {
				t.Fatalf("apply run.completed for the malformed run: %v", err)
			}
			oldEnded := time.Now().UTC().Add(-2 * time.Hour)
			if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, oldEnded, malformedRunID); err != nil {
				t.Fatalf("backdate ended_at for the malformed run: %v", err)
			}

			otherRunID := startRunForFinalize(t, pool, "manual:finalize-malformed-other-1")
			otherCompleted := newTestEnvelope(t, otherRunID, "manual:finalize-malformed-other-1", KindRunCompleted, RunCompletedPayload{})
			if _, err := g.Apply(context.Background(), otherCompleted); err != nil {
				t.Fatalf("apply run.completed for the other run: %v", err)
			}
			if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, oldEnded, otherRunID); err != nil {
				t.Fatalf("backdate ended_at for the other run: %v", err)
			}

			now := time.Now().UTC()
			n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, time.Minute, now)
			if err != nil {
				t.Fatalf("finalizeStaleLogs: %v", err)
			}
			if n != 2 {
				t.Fatalf("finalized %d runs, want 2 (the malformed run through its ended_at fallback, and the other run)", n)
			}

			for _, runID := range []uuid.UUID{malformedRunID, otherRunID} {
				found, finalized := readLogSummaryFinalized(t, db, runID)
				if !found || !finalized {
					t.Fatalf("run %s: found=%v finalized=%v, want a closed summary", runID, found, finalized)
				}
			}
		})
	}
}

// TestFinalizeStaleLogsProtectsFallbackAnchorFromRace asserts a
// concurrent run.heartbeat that refreshes a run's fallback anchor
// between the candidate select and the finalize update leaves the log
// open, even though the run carries no writer heartbeat at all and the
// writer-heartbeat guard alone cannot see the change.
func TestFinalizeStaleLogsProtectsFallbackAnchorFromRace(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := startRunForFinalize(t, pool, "manual:finalize-fallback-race-1")

	staleHeartbeat := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, staleHeartbeat, runID); err != nil {
		t.Fatalf("stamp stale heartbeat: %v", err)
	}

	cutoff := time.Now().UTC().Add(-time.Minute)
	candidates, err := selectFinalizeCandidates(context.Background(), pool, cutoff, nil)
	if err != nil {
		t.Fatalf("selectFinalizeCandidates: %v", err)
	}
	var candidate *finalizeCandidate
	for i := range candidates {
		if candidates[i].runID == runID.String() {
			candidate = &candidates[i]
		}
	}
	if candidate == nil {
		t.Fatalf("run %s did not appear as a finalize candidate", runID)
	}
	if candidate.writerHeartbeatRaw != nil {
		t.Fatalf("writerHeartbeatRaw = %q, want nil for a run with no writer heartbeat", *candidate.writerHeartbeatRaw)
	}

	// A concurrent run.heartbeat raises heartbeat_received_at, the run's
	// fallback anchor, after the select above already read the stale
	// value.
	g := NewIngester(pool, nil, nil)
	heartbeat := newTestEnvelope(t, runID, "manual:finalize-fallback-race-1", KindRunHeartbeat, map[string]any{})
	if _, err := g.Apply(context.Background(), heartbeat); err != nil {
		t.Fatalf("apply concurrent run.heartbeat: %v", err)
	}

	changed, err := finalizeLogSummary(context.Background(), pool, candidate.runID, candidate.writerHeartbeatRaw, cutoff)
	if err != nil {
		t.Fatalf("finalizeLogSummary: %v", err)
	}
	if changed {
		t.Fatalf("finalizeLogSummary closed a log whose fallback anchor moved after the select")
	}

	found, _ := readLogSummaryFinalized(t, db, runID)
	if found {
		t.Fatalf("a log summary was created for a run whose fallback anchor moved after the select")
	}
}

// TestWaitAnchorPrecedence asserts waitAnchor picks the writer's own
// heartbeat first, then ended_at, then heartbeat_received_at, then
// started_at, then queued_at, in that order.
func TestWaitAnchorPrecedence(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	writerMs := base.UnixMilli()
	at := func(offset time.Duration) *time.Time {
		t := base.Add(offset)
		return &t
	}

	tests := []struct {
		name string
		c    finalizeCandidate
	}{
		{
			name: "writer heartbeat wins over every fallback",
			c: finalizeCandidate{
				writerHeartbeatUnixMs: &writerMs,
				endedAt:               at(time.Hour),
				heartbeatReceivedAt:   at(2 * time.Hour),
				startedAt:             at(3 * time.Hour),
				queuedAt:              at(4 * time.Hour),
			},
		},
		{
			name: "ended_at wins when the writer heartbeat is absent",
			c: finalizeCandidate{
				endedAt:             at(0),
				heartbeatReceivedAt: at(time.Hour),
				startedAt:           at(2 * time.Hour),
				queuedAt:            at(3 * time.Hour),
			},
		},
		{
			name: "heartbeat_received_at wins when the writer heartbeat and ended_at are absent",
			c: finalizeCandidate{
				heartbeatReceivedAt: at(0),
				startedAt:           at(time.Hour),
				queuedAt:            at(2 * time.Hour),
			},
		},
		{
			name: "started_at wins when only started_at and queued_at remain",
			c: finalizeCandidate{
				startedAt: at(0),
				queuedAt:  at(time.Hour),
			},
		},
		{
			name: "queued_at is the last fallback",
			c: finalizeCandidate{
				queuedAt: at(0),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := waitAnchor(tc.c)
			if got == nil || !got.Equal(base) {
				t.Fatalf("waitAnchor = %v, want %v", got, base)
			}
		})
	}
}

// TestWaitAnchorNilWhenEveryFieldIsAbsent asserts waitAnchor returns nil
// for a candidate with no anchor field set at all.
func TestWaitAnchorNilWhenEveryFieldIsAbsent(t *testing.T) {
	if got := waitAnchor(finalizeCandidate{}); got != nil {
		t.Fatalf("waitAnchor = %v, want nil", got)
	}
}
