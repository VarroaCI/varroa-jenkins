package recordstore

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// TestDropExpiredPartitionsSeedsFourMonths seeds four monthly "runs"
// partitions and asserts a cutoff at the fourth month's start drops
// exactly the first three.
func TestDropExpiredPartitionsSeedsFourMonths(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	ctx := context.Background()

	months := []time.Time{
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	names := make([]string, 0, len(months))
	for _, m := range months {
		suffix := m.Format("200601")
		if err := createPartition(ctx, pool, "runs", suffix, m, m.AddDate(0, 1, 0)); err != nil {
			t.Fatalf("create partition %s: %v", suffix, err)
		}
		names = append(names, "runs_p"+suffix)
	}

	cutoff := time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC)
	dropped, err := dropExpiredPartitions(ctx, pool, "runs", cutoff)
	if err != nil {
		t.Fatalf("dropExpiredPartitions: %v", err)
	}

	wantDropped := append([]string(nil), names[:3]...)
	sort.Strings(dropped)
	sort.Strings(wantDropped)
	if !reflect.DeepEqual(dropped, wantDropped) {
		t.Fatalf("dropped = %v, want %v", dropped, wantDropped)
	}

	remaining, err := listPartitions(ctx, pool, "runs")
	if err != nil {
		t.Fatalf("listPartitions: %v", err)
	}
	for _, want := range names[:3] {
		for _, p := range remaining {
			if p.name == want {
				t.Fatalf("partition %s still exists after drop", want)
			}
		}
	}
	found := false
	for _, p := range remaining {
		if p.name == names[3] {
			found = true
		}
	}
	if !found {
		t.Fatalf("partition %s was dropped, want it to survive", names[3])
	}
}

// TestDeleteExpiredLogObjectsDeletesBothKeys writes both log keys for one
// run, expires it, and asserts both keys are gone.
func TestDeleteExpiredLogObjectsDeletesBothKeys(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	ctx := context.Background()
	store := blob.NewPGBlobStore(pool)

	runID := seedGrantedIntent(t, pool, "manual:log-objects-1")
	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, "manual:log-objects-1", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(ctx, started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	logKey := blob.LogKey(runID.String())
	indexKey := blob.LogIndexKey(runID.String())
	writeAt := time.Now().UTC()
	if err := store.Put(ctx, logKey, strings.NewReader("hello"), blob.ObjectOptions{
		ObjectClass: "log", ContentType: "text/plain", CreatedAt: writeAt,
	}); err != nil {
		t.Fatalf("put log object: %v", err)
	}
	if err := store.Put(ctx, indexKey, strings.NewReader("index"), blob.ObjectOptions{
		ObjectClass: "log", ContentType: "application/json", CreatedAt: writeAt,
	}); err != nil {
		t.Fatalf("put log index object: %v", err)
	}

	cutoff := time.Now().UTC().Add(time.Hour)
	if _, err := deleteExpiredLogObjects(ctx, pool, store, cutoff); err != nil {
		t.Fatalf("deleteExpiredLogObjects: %v", err)
	}

	if _, err := store.NewRangeReader(ctx, logKey, 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Fatalf("log object still readable, err = %v, want ErrObjectNotFound", err)
	}
	if _, err := store.NewRangeReader(ctx, indexKey, 0, -1); !errors.Is(err, blob.ErrObjectNotFound) {
		t.Fatalf("log index object still readable, err = %v, want ErrObjectNotFound", err)
	}
}

// recordingBlobStore is a blob.Store fake that records every Delete call
// and fails the test if DeletePrefix is ever called. It satisfies
// ObjectDeleter through blob.Store's superset method set.
type recordingBlobStore struct {
	t       *testing.T
	deleted []string
}

func (r *recordingBlobStore) Put(context.Context, string, io.Reader, blob.ObjectOptions) error {
	return nil
}

func (r *recordingBlobStore) Append(context.Context, string, int64, []byte, blob.ObjectOptions) error {
	return nil
}

func (r *recordingBlobStore) NewRangeReader(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, blob.ErrObjectNotFound
}

func (r *recordingBlobStore) Delete(_ context.Context, key string) error {
	r.deleted = append(r.deleted, key)
	return nil
}

func (r *recordingBlobStore) DeletePrefix(_ context.Context, prefix string) error {
	r.t.Fatalf("DeletePrefix called with prefix %q; retention must never call it on an artifact key", prefix)
	return nil
}

func (r *recordingBlobStore) NewObjectReader(context.Context, string, int64, int64) (io.ReadCloser, int64, error) {
	return nil, 0, blob.ErrObjectNotFound
}

// TestDeleteExpiredArtifactsNeverCallsDeletePrefix asserts the round
// deletes an expired artifact through Delete on its stored blob_key
// verbatim, and never calls DeletePrefix.
func TestDeleteExpiredArtifactsNeverCallsDeletePrefix(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	runID := seedGrantedIntent(t, pool, "manual:artifact-1")
	g := NewIngester(pool, nil, nil)
	started := newTestEnvelope(t, runID, "manual:artifact-1", KindRunStarted, RunStartedPayload{})
	if _, err := g.Apply(ctx, started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	blobKey := "artifacts/" + testCluster + "/" + testNamespace + "/cell-a/" + runID.String() + "/build.log"
	oldCreatedAt := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := db.Exec(`
		INSERT INTO varroa.run_artifacts (run_id, path, created_at, size_bytes, digest, content_type, blob_key)
		VALUES ($1, 'build.log', $2, 100, NULL, 'text/plain', $3)
	`, runID, oldCreatedAt, blobKey); err != nil {
		t.Fatalf("seed run_artifacts row: %v", err)
	}

	fake := &recordingBlobStore{t: t}
	cutoff := time.Now().UTC()
	n, err := deleteExpiredArtifacts(ctx, pool, fake, cutoff)
	if err != nil {
		t.Fatalf("deleteExpiredArtifacts: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d artifacts, want 1", n)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != blobKey {
		t.Fatalf("deleted keys = %v, want [%s]; retention must delete run_artifacts.blob_key verbatim", fake.deleted, blobKey)
	}

	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_artifacts WHERE run_id = $1`, runID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining run_artifacts rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("run_artifacts row still exists after delete")
	}
}

// TestMaintenanceRoundAppliesRetentionAcrossFourMonths seeds a run, its
// child rows, and its artifact in each of four monthly partitions, runs
// one full maintenance round, and asserts the exact partition drop set,
// the child-row deletion, and the dual-retention log_chunks rule: a
// partition past retention.logs but not past retention.artifacts
// survives.
func TestMaintenanceRoundAppliesRetentionAcrossFourMonths(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	now := time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC)
	jan1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	feb1 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	mar1 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	apr1 := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)

	months := []struct {
		suffix string
		from   time.Time
		to     time.Time
		runAt  time.Time
	}{
		{"202401", jan1, feb1, jan1.AddDate(0, 0, 14)},
		{"202402", feb1, mar1, feb1.AddDate(0, 0, 14)},
		{"202403", mar1, apr1, mar1.AddDate(0, 0, 14)},
		{"202404", apr1, may1, apr1.AddDate(0, 0, 14)},
	}

	runIDs := map[string]uuid.UUID{}
	for _, m := range months {
		if err := createPartition(ctx, pool, "runs", m.suffix, m.from, m.to); err != nil {
			t.Fatalf("create runs partition %s: %v", m.suffix, err)
		}
		if err := createPartition(ctx, pool, "log_chunks", m.suffix, m.from, m.to); err != nil {
			t.Fatalf("create log_chunks partition %s: %v", m.suffix, err)
		}

		runID := uuid.New()
		runIDs[m.suffix] = runID
		if _, err := db.Exec(`
			INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
			VALUES ($1, $2, $3, $4, $5, 'cell-a', $6, $7, 'COMPLETED')
		`, runID, m.runAt, "manual:retention-"+m.suffix, testCluster, testNamespace, testTenant, testJobPath); err != nil {
			t.Fatalf("seed run for %s: %v", m.suffix, err)
		}
		if _, err := db.Exec(`
			INSERT INTO varroa.run_stages (run_id, stage_id, created_at, name, node_type, status)
			VALUES ($1, 'stage-1', $2, 'Build', 'stage', 'SUCCESS')
		`, runID, m.runAt); err != nil {
			t.Fatalf("seed run_stages for %s: %v", m.suffix, err)
		}
		if _, err := db.Exec(`
			INSERT INTO varroa.run_tests (run_id, suite, created_at, total, failed, skipped)
			VALUES ($1, 'suite-1', $2, 1, 0, 0)
		`, runID, m.runAt); err != nil {
			t.Fatalf("seed run_tests for %s: %v", m.suffix, err)
		}
		if _, err := db.Exec(`
			INSERT INTO varroa.run_artifacts (run_id, path, created_at, size_bytes, blob_key)
			VALUES ($1, 'build.log', $2, 10, $3)
		`, runID, m.runAt, "artifacts/"+m.suffix+"/build.log"); err != nil {
			t.Fatalf("seed run_artifacts for %s: %v", m.suffix, err)
		}
	}

	cfg := MaintenanceConfig{
		RunRetention:      now.Sub(mar1),
		LogRetention:      now.Sub(apr1),
		ArtifactRetention: now.Sub(feb1),
		LogStore:          blob.NewPGBlobStore(pool),
		FinalizeWait:      time.Minute,
		Now:               func() time.Time { return now },
	}

	// 202402's upper bound must sit past retention.logs but not past
	// retention.artifacts, or its survival below would not exercise the
	// dual-retention rule at all.
	logsCutoff := now.Add(-cfg.LogRetention)
	artifactsCutoff := now.Add(-cfg.ArtifactRetention)
	if months[1].to.After(logsCutoff) {
		t.Fatalf("test setup: 202402's upper bound %v must not be after the logs cutoff %v", months[1].to, logsCutoff)
	}
	if !months[1].to.After(artifactsCutoff) {
		t.Fatalf("test setup: 202402's upper bound %v must be after the artifacts cutoff %v", months[1].to, artifactsCutoff)
	}

	if _, err := RunMaintenanceRound(ctx, pool, cfg); err != nil {
		t.Fatalf("RunMaintenanceRound: %v", err)
	}

	runsPartitions, err := listPartitions(ctx, pool, "runs")
	if err != nil {
		t.Fatalf("listPartitions runs: %v", err)
	}
	gotRuns := map[string]bool{}
	for _, p := range runsPartitions {
		gotRuns[p.name] = true
	}
	if gotRuns["runs_p202401"] || gotRuns["runs_p202402"] {
		t.Fatalf("runs partitions = %v, want 202401 and 202402 dropped", gotRuns)
	}
	if !gotRuns["runs_p202403"] || !gotRuns["runs_p202404"] {
		t.Fatalf("runs partitions = %v, want 202403 and 202404 to survive", gotRuns)
	}

	logChunksPartitions, err := listPartitions(ctx, pool, "log_chunks")
	if err != nil {
		t.Fatalf("listPartitions log_chunks: %v", err)
	}
	gotLogChunks := map[string]bool{}
	for _, p := range logChunksPartitions {
		gotLogChunks[p.name] = true
	}
	if gotLogChunks["log_chunks_p202401"] {
		t.Fatalf("log_chunks partitions = %v, want 202401 dropped (past both retention.logs and retention.artifacts)", gotLogChunks)
	}
	for _, name := range []string{"log_chunks_p202402", "log_chunks_p202403", "log_chunks_p202404"} {
		if !gotLogChunks[name] {
			t.Fatalf("log_chunks partitions = %v, want %s to survive", gotLogChunks, name)
		}
	}

	var stagesRemaining, testsRemaining int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_stages`).Scan(&stagesRemaining); err != nil {
		t.Fatalf("count run_stages: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_tests`).Scan(&testsRemaining); err != nil {
		t.Fatalf("count run_tests: %v", err)
	}
	if stagesRemaining != 2 || testsRemaining != 2 {
		t.Fatalf("run_stages=%d run_tests=%d remaining, want 2 each (March and April only)", stagesRemaining, testsRemaining)
	}
	for _, suffix := range []string{"202403", "202404"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM varroa.run_stages WHERE run_id = $1`, runIDs[suffix]).Scan(&count); err != nil {
			t.Fatalf("count run_stages for %s: %v", suffix, err)
		}
		if count != 1 {
			t.Fatalf("run_stages for %s = %d, want 1 (must survive retention.runs)", suffix, count)
		}
	}

	var artifactsRemaining int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_artifacts`).Scan(&artifactsRemaining); err != nil {
		t.Fatalf("count run_artifacts: %v", err)
	}
	if artifactsRemaining != 3 {
		t.Fatalf("run_artifacts remaining = %d, want 3 (only January's artifact is past retention.artifacts)", artifactsRemaining)
	}
	var januaryArtifactCount int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_artifacts WHERE run_id = $1`, runIDs["202401"]).Scan(&januaryArtifactCount); err != nil {
		t.Fatalf("count January run_artifacts: %v", err)
	}
	if januaryArtifactCount != 0 {
		t.Fatalf("January's run_artifacts row survived, want it deleted")
	}
}
