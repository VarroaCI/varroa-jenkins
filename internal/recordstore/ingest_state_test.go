package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// applyMeshRunState builds a run.state envelope for runID/intentKey and
// applies it through ingester, failing the test on error.
func applyMeshRunState(t *testing.T, ingester *Ingester, runID uuid.UUID, intentKey string, payload RunStatePayload) {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, KindRunState, payload)
	if _, err := ingester.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.state: %v", err)
	}
}

// readMeshFiles reads runID's mesh.files array.
func readMeshFiles(t *testing.T, db *sql.DB, runID uuid.UUID) []runStateFile {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`SELECT payload->'mesh'->'files' FROM varroa.runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read mesh.files: %v", err)
	}
	var files []runStateFile
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatalf("decode mesh.files: %v", err)
	}
	return files
}

// TestApplyRunStateStoresFileAndSetsAnchor asserts a stored file sets
// mesh.state_at and adds one mesh.files entry carrying the pushed
// bytes, seq, and state_gen.
func TestApplyRunStateStoresFileAndSetsAnchor(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshProgramPath, Bytes: 4096, Seq: 1, StateGen: 3,
	})

	var stateAt sql.NullTime
	if err := db.QueryRow(`SELECT (payload->'mesh'->>'state_at')::timestamptz FROM varroa.runs WHERE run_id = $1`,
		runID).Scan(&stateAt); err != nil {
		t.Fatalf("read state_at: %v", err)
	}
	if !stateAt.Valid {
		t.Fatal("want mesh.state_at set")
	}

	files := readMeshFiles(t, db, runID)
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if files[0].RelPath != meshProgramPath || files[0].Bytes != 4096 || files[0].Seq != 1 || files[0].StateGen != 3 {
		t.Fatalf("files[0] = %+v, want the stored payload", files[0])
	}
}

// TestApplyRunStateCapWarningLeavesAnchorUnchanged asserts a
// state-cap rejection sets mesh.cap_warned_at and leaves mesh.state_at
// unchanged: the gateway's own counter is advisory and must not end a
// run's replication.
func TestApplyRunStateCapWarningLeavesAnchorUnchanged(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshBuildRecordPath, Bytes: 100, Seq: 1, StateGen: 1,
	})
	var firstStateAt time.Time
	if err := db.QueryRow(`SELECT (payload->'mesh'->>'state_at')::timestamptz FROM varroa.runs WHERE run_id = $1`,
		runID).Scan(&firstStateAt); err != nil {
		t.Fatalf("read state_at after the stored file: %v", err)
	}

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{RejectedReason: runStateCapReason})

	var stateAt time.Time
	var capWarnedAt sql.NullTime
	if err := db.QueryRow(`
		SELECT (payload->'mesh'->>'state_at')::timestamptz, (payload->'mesh'->>'cap_warned_at')::timestamptz
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&stateAt, &capWarnedAt); err != nil {
		t.Fatalf("read run after the cap warning: %v", err)
	}
	if !stateAt.Equal(firstStateAt) {
		t.Fatalf("state_at changed to %v, want it left at %v", stateAt, firstStateAt)
	}
	if !capWarnedAt.Valid {
		t.Fatal("want mesh.cap_warned_at set")
	}
}

// TestApplyRunStateUpsertsByRelPath asserts a repeated relpath leaves
// one mesh.files entry, carrying the newer bytes, seq, and state_gen.
func TestApplyRunStateUpsertsByRelPath(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshProgramPath, Bytes: 1000, Seq: 1, StateGen: 1,
	})
	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshProgramPath, Bytes: 2000, Seq: 2, StateGen: 2,
	})

	files := readMeshFiles(t, db, runID)
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1: a repeated relpath must upsert", len(files))
	}
	if files[0].Bytes != 2000 || files[0].Seq != 2 || files[0].StateGen != 2 {
		t.Fatalf("files[0] = %+v, want the newer bytes/seq/state_gen", files[0])
	}
}

// TestApplyRunStateCapDeletesStateAndClearsResume asserts the durable
// cap: once the recorded file sizes pass 64 MiB, the recorder clears
// mesh.state_at to JSON null, sets mesh.rejected_reason, clears
// mesh.resume, and reports the run for a prefix delete.
func TestApplyRunStateCapDeletesStateAndClearsResume(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)

	fake := &prefixRecordingStore{}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetStateDeleter(fake)

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: "small.txt", Bytes: 1, Seq: 1, StateGen: 1,
	})

	// Seed a live arm, matching a run the mesh has already claimed: the
	// cap must clear it, because a run whose state is gone can never
	// resume.
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('resume',
		           jsonb_build_object('armed_at', now(), 'arm_expires', now() + interval '10 minutes')))
		 WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("seed an existing arm: %v", err)
	}

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshBuildRecordPath, Bytes: 70 * 1024 * 1024, Seq: 1, StateGen: 1,
	})

	var stateAtText, rejectedReason string
	var hasResume bool
	if err := db.QueryRow(`
		SELECT (payload->'mesh'->'state_at')::text, COALESCE(payload->'mesh'->>'rejected_reason', ''),
		       (payload->'mesh' ? 'resume')
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&stateAtText, &rejectedReason, &hasResume); err != nil {
		t.Fatalf("read run after the cap: %v", err)
	}
	if stateAtText != "null" {
		t.Fatalf("mesh.state_at = %s, want JSON null", stateAtText)
	}
	if rejectedReason != runStateCapReason {
		t.Fatalf("rejected_reason = %q, want %q", rejectedReason, runStateCapReason)
	}
	if hasResume {
		t.Fatal("want mesh.resume cleared at the cap")
	}

	want := blob.RunStatePrefix(runID.String())
	if len(fake.prefixes) != 1 || fake.prefixes[0] != want {
		t.Fatalf("deleted prefixes = %v, want [%s]", fake.prefixes, want)
	}
}

// --- Pre-flight round 1: a proved resume is cleared ---

// seedDispatchedResume stamps runID with a mesh record whose resume
// names target, as the takeover's dispatch writes it.
func seedDispatchedResume(t *testing.T, db *sql.DB, runID uuid.UUID, target string) {
	t.Helper()
	seedDispatchedResumeAt(t, db, runID, target, time.Now().UTC())
}

// seedDispatchedResumeAt stamps a dispatched resume whose request time
// is requestedAt, so a test can place that moment either side of the
// recorder's own receive clock.
func seedDispatchedResumeAt(t *testing.T, db *sql.DB, runID uuid.UUID, target string, requestedAt time.Time) {
	t.Helper()
	now := requestedAt
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh', jsonb_build_object(
		         'state_at', $2::timestamptz,
		         'resume', jsonb_build_object(
		             'armed_at', $2::timestamptz, 'arm_expires', $3::timestamptz,
		             'instance', $4::text, 'requested_at', $2::timestamptz, 'deadline', $3::timestamptz)))
		 WHERE run_id = $1
	`, runID, now, now.Add(6*time.Minute), target); err != nil {
		t.Fatalf("seed a dispatched resume: %v", err)
	}
}

// readResumeInstance reads runID's mesh.resume.instance, reporting
// ok=false when the resume record is absent.
func readResumeInstance(t *testing.T, db *sql.DB, runID uuid.UUID) (string, bool) {
	t.Helper()
	var instance sql.NullString
	if err := db.QueryRow(
		`SELECT payload->'mesh'->'resume'->>'instance' FROM varroa.runs WHERE run_id = $1`,
		runID).Scan(&instance); err != nil {
		t.Fatalf("read mesh.resume.instance: %v", err)
	}
	return instance.String, instance.Valid
}

// TestHeartbeatFromTheTargetClearsTheResume proves the named target's
// own heartbeat clears mesh.resume. A resumed build emits no
// run.started, so the heartbeat is the only proof it took the run over,
// and a record left in place would shield the run from every
// interruption test for the rest of its life.
func TestHeartbeatFromTheTargetClearsTheResume(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	if _, err := ingester.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	seedDispatchedResume(t, db, runID, "pod-live")

	beat := newTestEnvelope(t, runID, intentKey, KindRunHeartbeat, map[string]any{})
	beat.CellInstance = "pod-live"
	if _, err := ingester.Apply(context.Background(), beat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}

	if _, ok := readResumeInstance(t, db, runID); ok {
		t.Fatal("the target's heartbeat left the resume record in place")
	}
}

// TestHeartbeatFromTheDeadInstanceKeepsTheResume proves a heartbeat the
// dead instance had in flight clears nothing. Only the named target
// proves the resume.
func TestHeartbeatFromTheDeadInstanceKeepsTheResume(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	if _, err := ingester.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	seedDispatchedResume(t, db, runID, "pod-live")

	beat := newTestEnvelope(t, runID, intentKey, KindRunHeartbeat, map[string]any{})
	beat.CellInstance = "pod-dead"
	if _, err := ingester.Apply(context.Background(), beat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}

	instance, ok := readResumeInstance(t, db, runID)
	if !ok || instance != "pod-live" {
		t.Fatalf("mesh.resume.instance = %q, ok = %v, want pod-live", instance, ok)
	}
}

// TestHeartbeatBeforeTheRequestKeepsTheResume proves a heartbeat that
// reaches the recorder before the dispatch clears nothing. Such a
// heartbeat proves the target was alive, never that it restored the
// run, and clearing on it would drop the run's protection before the
// target had taken the run over.
//
// The resume is seeded with a request time in the future, because the
// clear compares against the recorder's own receive clock and never the
// pod's.
func TestHeartbeatBeforeTheRequestKeepsTheResume(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	ingester := NewIngester(pool, nil, nil)

	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	if _, err := ingester.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	seedDispatchedResumeAt(t, db, runID, "pod-live", time.Now().UTC().Add(time.Hour))

	beat := newTestEnvelope(t, runID, intentKey, KindRunHeartbeat, map[string]any{})
	beat.CellInstance = "pod-live"
	if _, err := ingester.Apply(context.Background(), beat); err != nil {
		t.Fatalf("apply run.heartbeat: %v", err)
	}

	instance, ok := readResumeInstance(t, db, runID)
	if !ok || instance != "pod-live" {
		t.Fatalf("mesh.resume.instance = %q, ok = %v, want pod-live", instance, ok)
	}
}
