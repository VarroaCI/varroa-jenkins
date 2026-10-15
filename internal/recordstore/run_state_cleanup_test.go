package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// seedReplicatedRun grants and starts one run, then stores one mesh file
// entry per path, each at meshPusherEpoch and generation 1.
func seedReplicatedRun(t *testing.T, pool *pgxpool.Pool, ingester *Ingester, paths ...string) (uuid.UUID, string) {
	t.Helper()
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	started := newTestEnvelope(t, runID, intentKey, KindRunStarted, RunStartedPayload{})
	if _, err := ingester.Apply(context.Background(), started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	for _, p := range paths {
		applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
			RelPath: p, Bytes: 100, Seq: 1, StateGen: 1, PusherEpoch: meshPusherEpoch,
		})
	}
	return runID, intentKey
}

// TestTerminalWriteKeepsTheBuildRecord asserts a run's real terminal
// write deletes every object its record names by key, except the
// build.xml, and deletes no prefix.
func TestTerminalWriteKeepsTheBuildRecord(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	fake := &prefixRecordingStore{}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetStateDeleter(fake)

	runID, intentKey := seedReplicatedRun(t, pool, ingester, meshProgramPath, meshBuildRecordPath)
	fake.keys, fake.prefixes = nil, nil

	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result, Seq: 1})
	if _, err := ingester.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	if len(fake.prefixes) != 0 {
		t.Fatalf("deleted prefixes = %v, want none: the run keeps its build.xml", fake.prefixes)
	}
	want := []string{blob.RunStateKey(runID.String(), meshPusherEpoch, 1, meshProgramPath)}
	if !reflect.DeepEqual(fake.keys, want) {
		t.Fatalf("deleted keys = %v, want %v", fake.keys, want)
	}
}

// TestTerminalWriteWithNoBuildRecordDeletesThePrefix asserts a run whose
// record names no build.xml loses its whole prefix at the terminal write.
func TestTerminalWriteWithNoBuildRecordDeletesThePrefix(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	fake := &prefixRecordingStore{}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetStateDeleter(fake)

	runID, intentKey := seedReplicatedRun(t, pool, ingester, meshProgramPath)
	fake.keys, fake.prefixes = nil, nil

	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result, Seq: 1})
	if _, err := ingester.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	want := []string{blob.RunStatePrefix(runID.String())}
	if !reflect.DeepEqual(fake.prefixes, want) {
		t.Fatalf("deleted prefixes = %v, want %v", fake.prefixes, want)
	}
	if len(fake.keys) != 0 {
		t.Fatalf("deleted keys = %v, want none beside the prefix", fake.keys)
	}
}

// TestInterruptionCleanupKeepsTheBuildRecord asserts the recorder's own
// interruption marking keeps the build.xml of the marked run.
func TestInterruptionCleanupKeepsTheBuildRecord(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ingester := NewIngester(pool, nil, nil)
	runID, _ := seedReplicatedRun(t, pool, ingester, meshProgramPath, meshBuildRecordPath)

	var createdAt time.Time
	if err := db.QueryRow(`SELECT created_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	fake := &prefixRecordingStore{}
	deleteRunState(context.Background(), pool, fake, runInterruptCandidate{runID: runID, createdAt: createdAt}, slog.Default())

	if len(fake.prefixes) != 0 {
		t.Fatalf("deleted prefixes = %v, want none: the marked run keeps its build.xml", fake.prefixes)
	}
	want := []string{blob.RunStateKey(runID.String(), meshPusherEpoch, 1, meshProgramPath)}
	if !reflect.DeepEqual(fake.keys, want) {
		t.Fatalf("deleted keys = %v, want %v", fake.keys, want)
	}
}

// TestBackstopKeepsTheBuildRecord asserts the hourly backstop deletes a
// terminal run's objects by key and keeps its build.xml.
func TestBackstopKeepsTheBuildRecord(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ingester := NewIngester(pool, nil, nil)
	runID, _ := seedReplicatedRun(t, pool, ingester, meshProgramPath, meshBuildRecordPath)
	now := time.Now().UTC()
	if _, err := db.Exec(`
		UPDATE varroa.runs SET status = 'COMPLETED', result = 'SUCCESS', ended_at = $2 WHERE run_id = $1
	`, runID, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("mark run terminal: %v", err)
	}

	fake := &prefixRecordingStore{}
	if _, err := deleteExpiredRunState(context.Background(), pool, fake, now); err != nil {
		t.Fatalf("deleteExpiredRunState: %v", err)
	}
	if len(fake.prefixes) != 0 {
		t.Fatalf("deleted prefixes = %v, want none: the run keeps its build.xml", fake.prefixes)
	}
	want := []string{blob.RunStateKey(runID.String(), meshPusherEpoch, 1, meshProgramPath)}
	if !reflect.DeepEqual(fake.keys, want) {
		t.Fatalf("deleted keys = %v, want %v", fake.keys, want)
	}
}

// failingDeleteStore fails every Delete of one key and records every call.
type failingDeleteStore struct {
	prefixRecordingStore
	failKey string
}

// errStoreUnavailable is the error failingDeleteStore answers.
var errStoreUnavailable = errors.New("store unavailable")

func (f *failingDeleteStore) Delete(ctx context.Context, key string) error {
	_ = f.prefixRecordingStore.Delete(ctx, key)
	if key == f.failKey {
		return errStoreUnavailable
	}
	return nil
}

// TestDeleteRunStateExceptScriptContinuesPastAFailedKey asserts one failed
// key delete still lets every other key delete run, and that the helper
// answers one error after the loop that names the failed key, the failure
// count, and the store's own error.
func TestDeleteRunStateExceptScriptContinuesPastAFailedKey(t *testing.T) {
	const runID = "0199aa11-2222-7333-8444-555566667777"
	files := []runStateFile{
		{RelPath: "a/program.dat", StateGen: 1, Epoch: 5},
		{RelPath: "a/build.xml", StateGen: 1, Epoch: 5},
		{RelPath: "a/workflow/1.xml", StateGen: 1, Epoch: 5},
	}
	store := &failingDeleteStore{failKey: blob.RunStateKey(runID, 5, 1, "a/program.dat")}
	err := deleteRunStateExceptScript(context.Background(), store, runID, files)
	if err == nil {
		t.Fatal("want the failed key's error")
	}
	if !errors.Is(err, errStoreUnavailable) {
		t.Fatalf("err = %v, want it to wrap the store's error", err)
	}
	failedKey := blob.RunStateKey(runID, 5, 1, "a/program.dat")
	if !strings.Contains(err.Error(), failedKey) || !strings.Contains(err.Error(), "(1 of 2 keys failed)") {
		t.Fatalf("err = %q, want the failed key %s and the count (1 of 2 keys failed)", err, failedKey)
	}
	want := []string{
		blob.RunStateKey(runID, 5, 1, "a/program.dat"),
		blob.RunStateKey(runID, 5, 1, "a/workflow/1.xml"),
	}
	if !reflect.DeepEqual(store.keys, want) {
		t.Fatalf("deleted keys = %v, want %v", store.keys, want)
	}

	nilStore := &prefixRecordingStore{}
	if err := deleteRunStateExceptScript(context.Background(), nilStore, runID, nil); err != nil {
		t.Fatalf("nil list: %v", err)
	}
	if !reflect.DeepEqual(nilStore.prefixes, []string{blob.RunStatePrefix(runID)}) || len(nilStore.keys) != 0 {
		t.Fatalf("nil list: prefixes = %v, keys = %v, want the whole prefix alone", nilStore.prefixes, nilStore.keys)
	}
}

// rowCheckingStore records, for each run-state prefix delete, whether the
// run's row still existed when the delete ran.
type rowCheckingStore struct {
	prefixRecordingStore
	db         *sql.DB
	rowPresent map[string]bool
}

func (r *rowCheckingStore) DeletePrefix(ctx context.Context, prefix string) error {
	_ = r.prefixRecordingStore.DeletePrefix(ctx, prefix)
	var n int
	// The round also deletes the restart-copy prefix of the same run.
	runID := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(prefix, "runstate/"), "restartsource/"), "/")
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM varroa.runs WHERE run_id = $1`, runID).Scan(&n); err != nil {
		return err
	}
	r.rowPresent[prefix] = n > 0
	return nil
}

// TestRetentionRoundDeletesTheRetainedScriptBeforeThePartitionDrop
// asserts the maintenance round deletes the whole run-state prefix of a
// run past the record retention cutoff, and does so while the run's row
// still exists, before its partition drops. A run inside the cutoff
// keeps its prefix.
func TestRetentionRoundDeletesTheRetainedScriptBeforeThePartitionDrop(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	now := time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC)
	jan1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	feb1 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mar1 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	apr1 := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)

	seed := func(suffix string, from, to, runAt time.Time) uuid.UUID {
		if err := createPartition(ctx, pool, "runs", suffix, from, to); err != nil {
			t.Fatalf("create runs partition %s: %v", suffix, err)
		}
		runID := uuid.New()
		if _, err := db.Exec(`
			INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			                         status)
			VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED')
		`, runID, runAt, "manual:retained-"+suffix, testCluster, testNamespace, testTenant, testJobPath); err != nil {
			t.Fatalf("seed run for %s: %v", suffix, err)
		}
		return runID
	}
	expired := seed("202401", jan1, feb1, jan1.AddDate(0, 0, 14))
	kept := seed("202404", apr1, may1, apr1.AddDate(0, 0, 14))

	// Neither run carries ended_at, so the hourly backstop names neither
	// one, and every prefix delete this round makes is the retention step's.
	store := &rowCheckingStore{db: db, rowPresent: map[string]bool{}}
	cfg := MaintenanceConfig{
		RunRetention:      now.Sub(mar1),
		LogRetention:      now.Sub(mar1),
		ArtifactRetention: now.Sub(mar1),
		LogStore:          store,
		FinalizeWait:      time.Minute,
		Now:               func() time.Time { return now },
	}
	if _, err := RunMaintenanceRound(ctx, pool, cfg); err != nil {
		t.Fatalf("RunMaintenanceRound: %v", err)
	}

	expiredPrefix := blob.RunStatePrefix(expired.String())
	wantPrefixes := []string{expiredPrefix, blob.RestartSourcePrefix(expired.String())}
	if !reflect.DeepEqual(store.prefixes, wantPrefixes) {
		t.Fatalf("deleted prefixes = %v, want %v", store.prefixes, wantPrefixes)
	}
	present := store.rowPresent[expiredPrefix]
	if !present {
		t.Fatal("the expired run's prefix was deleted after its row left, want the delete before the partition drop")
	}
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, expired).Scan(&remaining); err != nil {
		t.Fatalf("count expired run: %v", err)
	}
	if remaining != 0 {
		t.Fatal("the expired run's partition survived the round")
	}
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE run_id = $1`, kept).Scan(&remaining); err != nil {
		t.Fatalf("count kept run: %v", err)
	}
	if remaining != 1 {
		t.Fatal("the run inside the cutoff lost its row")
	}
}

// TestDeleteRetainedRunScriptsTestsCreatedAtAlone asserts the retention
// step deletes the prefix of a run created before the cutoff and leaves a
// run created after it.
func TestDeleteRetainedRunScriptsTestsCreatedAtAlone(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	runID := seedTerminalRunAt(t, pool, db, "manual:"+newTestUUIDv7String(), time.Now().UTC())
	var createdAt time.Time
	if err := db.QueryRow(`SELECT created_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	before := &prefixRecordingStore{}
	if n, err := deleteRetainedRunScripts(ctx, pool, before, createdAt.Add(-time.Second)); err != nil || n != 0 {
		t.Fatalf("cutoff before the run: n = %d, err = %v, want 0 and no error", n, err)
	}
	if len(before.prefixes) != 0 {
		t.Fatalf("deleted prefixes = %v, want none for a run inside the cutoff", before.prefixes)
	}

	after := &prefixRecordingStore{}
	if n, err := deleteRetainedRunScripts(ctx, pool, after, createdAt.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("cutoff after the run: n = %d, err = %v, want 1 and no error", n, err)
	}
	wantPrefixes := []string{blob.RunStatePrefix(runID), blob.RestartSourcePrefix(runID)}
	if !reflect.DeepEqual(after.prefixes, wantPrefixes) {
		t.Fatalf("deleted prefixes = %v, want %v", after.prefixes, wantPrefixes)
	}
}
