package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	restartSourceBuildXML = "jobs/team-a/jobs/app/builds/7/build.xml"
	restartSourceLibJar   = "jobs/team-a/jobs/app/builds/7/libs/a.jar"
)

// restartCopyPayload builds the payload of one file of a restart copy.
func restartCopyPayload(relpath string, bytes, epoch, copyFiles, copyBytes int64) RunStatePayload {
	return RunStatePayload{
		RelPath: relpath, Bytes: bytes, StateGen: 1, PusherEpoch: epoch,
		RestartSource: true, CopyFiles: copyFiles, CopyBytes: copyBytes,
	}
}

// readRestartManifest returns runID's restart_source manifest, or nil when
// the run holds none.
func readRestartManifest(t *testing.T, db *sql.DB, runID uuid.UUID) *restartSourceRecord {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT payload->'restart_source' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read restart_source: %v", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var rec restartSourceRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode restart_source: %v", err)
	}
	return &rec
}

func TestApplyRestartSource_CompleteWhenEveryFileArrives(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 2, 9))
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceLibJar, 5, meshPusherEpoch, 2, 9))

	rec := readRestartManifest(t, db, runID)
	if rec == nil || !rec.Complete || len(rec.Files) != 2 {
		t.Fatalf("manifest = %+v, want a complete copy of two files", rec)
	}
	run := Run{Payload: mustPayload(t, db, runID)}
	files, ok := RestartSourceOf(run)
	if !ok || len(files) != 2 {
		t.Fatalf("RestartSourceOf = %v, %v; want the two files", files, ok)
	}
	if files[0].Bytes+files[1].Bytes != 9 {
		t.Fatalf("file bytes = %+v, want the declared total 9", files)
	}
	var meshSet bool
	if err := db.QueryRow(`SELECT payload ? 'mesh' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&meshSet); err != nil {
		t.Fatalf("read mesh key: %v", err)
	}
	if meshSet {
		t.Fatal("a restart copy wrote the run's mesh state")
	}
}

func TestApplyRestartSource_PartialStaysIncomplete(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 2, 9))

	rec := readRestartManifest(t, db, runID)
	if rec == nil || rec.Complete || len(rec.Files) != 1 {
		t.Fatalf("manifest = %+v, want one file and not complete", rec)
	}
	if _, ok := RestartSourceOf(Run{Payload: mustPayload(t, db, runID)}); ok {
		t.Fatal("RestartSourceOf answered a partial copy")
	}
}

func TestApplyRestartSource_RedeliveryIsIdempotent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	payload := restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 2, 9)
	applyMeshRunState(t, ingester, runID, intentKey, payload)
	applyMeshRunState(t, ingester, runID, intentKey, payload)

	if rec := readRestartManifest(t, db, runID); rec == nil || len(rec.Files) != 1 {
		t.Fatalf("manifest after a redelivery = %+v, want one file", rec)
	}
}

func TestApplyRestartSource_NewerEpochResetsRecord(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 2, 9))
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceLibJar, 5, meshPusherEpoch+1, 1, 5))

	rec := readRestartManifest(t, db, runID)
	if rec == nil || rec.Epoch != meshPusherEpoch+1 || len(rec.Files) != 1 || rec.Files[0].RelPath != restartSourceLibJar {
		t.Fatalf("manifest after a newer epoch = %+v, want the new epoch's one file", rec)
	}
	if !rec.Complete {
		t.Fatal("the new epoch's copy of one file is complete")
	}
}

func TestApplyRestartSource_OlderEpochIgnored(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch+1, 2, 9))
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceLibJar, 5, meshPusherEpoch, 2, 9))

	rec := readRestartManifest(t, db, runID)
	if rec == nil || rec.Epoch != meshPusherEpoch+1 || len(rec.Files) != 1 {
		t.Fatalf("manifest after an older epoch = %+v, want the newer epoch unchanged", rec)
	}
}

func TestApplyRestartSource_MismatchedExpectedCountsIgnored(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 2, 9))
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceLibJar, 5, meshPusherEpoch, 3, 9))

	rec := readRestartManifest(t, db, runID)
	if rec == nil || len(rec.Files) != 1 || rec.ExpectedFiles != 2 {
		t.Fatalf("manifest after a mismatched count = %+v, want the first declaration kept", rec)
	}
}

func TestApplyRestartSource_OverDeclaredBytesNotRecorded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 5, meshPusherEpoch, 1, 4))

	if rec := readRestartManifest(t, db, runID); rec != nil {
		t.Fatalf("a file past the declared bytes was recorded: %+v", rec)
	}
}

// TestApplyRestartSource_RunCompletedCleanupKeepsRestartSource asserts the
// terminal cleanup deletes no restart-copy key and no restart-copy prefix.
func TestApplyRestartSource_RunCompletedCleanupKeepsRestartSource(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	fake := &prefixRecordingStore{}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetStateDeleter(fake)

	runID, intentKey := seedReplicatedRun(t, pool, ingester, meshProgramPath)
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 1, 4))
	fake.keys, fake.prefixes = nil, nil

	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result, Seq: 1})
	if _, err := ingester.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	for _, k := range append(append([]string{}, fake.keys...), fake.prefixes...) {
		if strings.HasPrefix(k, "restartsource/") {
			t.Fatalf("the terminal cleanup deleted restart-copy object %q", k)
		}
	}
}

// mustPayload reads runID's raw payload.
func mustPayload(t *testing.T, db *sql.DB, runID uuid.UUID) json.RawMessage {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT COALESCE(payload, '{}'::jsonb) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return raw
}

// TestRestartSourceOf_AnswersCompleteOnly is a pure check of the reader.
func TestRestartSourceOf_AnswersCompleteOnly(t *testing.T) {
	t.Parallel()

	complete := `{"restart_source":{"epoch":1,"expected_files":2,"expected_bytes":9,"complete":true,
		"files":[{"relpath":"a","bytes":4},{"relpath":"b","bytes":5}]}}`
	if files, ok := RestartSourceOf(Run{Payload: json.RawMessage(complete)}); !ok || len(files) != 2 {
		t.Fatalf("complete copy: got %v, %v", files, ok)
	}
	partial := `{"restart_source":{"epoch":1,"expected_files":2,"expected_bytes":9,"complete":false,
		"files":[{"relpath":"a","bytes":4}]}}`
	if _, ok := RestartSourceOf(Run{Payload: json.RawMessage(partial)}); ok {
		t.Fatal("a partial copy answered ok")
	}
	bytesOff := `{"restart_source":{"epoch":1,"expected_files":1,"expected_bytes":9,"complete":true,
		"files":[{"relpath":"a","bytes":4}]}}`
	if _, ok := RestartSourceOf(Run{Payload: json.RawMessage(bytesOff)}); ok {
		t.Fatal("a copy whose byte sum differs answered ok")
	}
	if _, ok := RestartSourceOf(Run{}); ok {
		t.Fatal("a run with no payload answered ok")
	}
}
