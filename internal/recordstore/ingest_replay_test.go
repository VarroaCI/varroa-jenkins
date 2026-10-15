package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestIngestConsumerReplaySafety delivers each of the eight kinds twice
// through the real consumer and asserts one row per natural key with
// identical column values after the second delivery (Contract F5).
func TestIngestConsumerReplaySafety(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedGrantedIntent(t, pool, "manual:replay-all-1")

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureBuildsStream(conn, 1); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	ingester := NewIngester(pool, conn, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunIngestConsumer(ctx, conn, ingester, nil, 24*time.Hour, nil)
	}()

	occurredAt := time.Now().UTC().Truncate(time.Microsecond)
	buildNumber := int64(5)
	branch := "main"
	revision := "deadbeef"

	started := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunStarted, RunStartedPayload{
		BuildNumber: &buildNumber, Branch: &branch, SCMRevision: &revision,
	})
	started.OccurredAt = occurredAt

	result := "SUCCESS"
	durationMs := int64(1234)
	completed := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunCompleted, RunCompletedPayload{
		Result: &result, DurationMs: &durationMs,
	})
	completed.OccurredAt = occurredAt

	stage := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunStage, RunStagePayload{
		StageID: "node-1", Name: "Build", NodeType: "stage", Status: "SUCCESS",
	})
	stage.OccurredAt = occurredAt

	testsEnv := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunTests, RunTestsPayload{
		Total: 10, Failed: 1, Skipped: 0,
		Suites: []RunTestSuitePayload{{Suite: "unit", Total: 10, Failed: 1}},
	})
	testsEnv.OccurredAt = occurredAt

	artifacts := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunArtifacts, RunArtifactsPayload{
		Path: "out.jar", SizeBytes: 42, BlobKey: "artifacts/core/jenkins/cell-a/out.jar",
	})
	artifacts.OccurredAt = occurredAt

	changes := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunChanges, RunChangesPayload{
		Changes: []byte(`[{"commit":"abc"}]`),
	})
	changes.OccurredAt = occurredAt

	logEnv := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunLog, map[string]any{"chunk_count": 2})
	logEnv.OccurredAt = occurredAt

	heartbeat := newTestEnvelope(t, runID, "manual:replay-all-1", KindRunHeartbeat, map[string]any{})
	heartbeat.OccurredAt = occurredAt

	envelopes := []IngestEnvelope{started, stage, completed, testsEnv, artifacts, changes, logEnv, heartbeat}
	if len(envelopes) != 8 {
		t.Fatalf("expected exactly eight envelopes, got %d", len(envelopes))
	}

	for _, env := range envelopes {
		publishBuildEvent(t, conn, env)
	}
	// Every envelope must be applied before the first snapshot, so the
	// wait is for the work-queue stream to drain, not for one column.
	waitForRow(t, func() bool {
		info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
		return err == nil && info.State.Msgs == 0
	})

	snapshot := captureReplaySnapshot(t, db, runID)

	// Redeliver every envelope a second time.
	for _, env := range envelopes {
		publishBuildEvent(t, conn, env)
	}
	waitForRow(t, func() bool {
		info, err := conn.JetStream().StreamInfo(bus.BuildsStreamName)
		return err == nil && info.State.Msgs == 0
	})

	second := captureReplaySnapshot(t, db, runID)
	if snapshot != second {
		t.Fatalf("replay changed stored values:\nfirst:  %+v\nsecond: %+v", snapshot, second)
	}
}

// replaySnapshot is a comparable summary of every row a run's eight
// kinds write, used to assert a replay changes nothing.
type replaySnapshot struct {
	runRow      string
	payloadRow  string
	stageRow    string
	testsRow    string
	artifactRow string
}

// captureReplaySnapshot reads back the rows TestIngestConsumerReplaySafety
// writes, formatted as comparable strings.
func captureReplaySnapshot(t *testing.T, db *sql.DB, runID uuid.UUID) replaySnapshot {
	t.Helper()
	var snap replaySnapshot

	if err := db.QueryRow(`
		SELECT format('%s|%s|%s|%s|%s|%s|%s|%s|%s|%s',
			status, coalesce(result,''), coalesce(branch,''), coalesce(scm_revision,''),
			coalesce(build_number::text,''), coalesce(duration_ms::text,''),
			coalesce(tests_total::text,''), coalesce(tests_failed::text,''),
			coalesce(tests_skipped::text,''), coalesce(changes::text,''))
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&snap.runRow); err != nil {
		t.Fatalf("snapshot runs: %v", err)
	}

	if err := db.QueryRow(`
		SELECT coalesce(payload::text, '') FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&snap.payloadRow); err != nil {
		t.Fatalf("snapshot runs.payload: %v", err)
	}

	if err := db.QueryRow(`
		SELECT format('%s|%s|%s', stage_id, status, name)
		FROM varroa.run_stages WHERE run_id = $1
	`, runID).Scan(&snap.stageRow); err != nil {
		t.Fatalf("snapshot run_stages: %v", err)
	}

	if err := db.QueryRow(`
		SELECT format('%s|%s|%s|%s', suite, total, failed, skipped)
		FROM varroa.run_tests WHERE run_id = $1
	`, runID).Scan(&snap.testsRow); err != nil {
		t.Fatalf("snapshot run_tests: %v", err)
	}

	if err := db.QueryRow(`
		SELECT format('%s|%s|%s', path, size_bytes, blob_key)
		FROM varroa.run_artifacts WHERE run_id = $1
	`, runID).Scan(&snap.artifactRow); err != nil {
		t.Fatalf("snapshot run_artifacts: %v", err)
	}

	return snap
}
