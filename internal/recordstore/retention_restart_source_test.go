package recordstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// restartSourceRecordingStore records each DeletePrefix call and can fail
// every call on demand.
type restartSourceRecordingStore struct {
	prefixRecordingStore
	fail bool
}

func (s *restartSourceRecordingStore) DeletePrefix(ctx context.Context, prefix string) error {
	if s.fail {
		return errors.New("store unavailable")
	}
	return s.prefixRecordingStore.DeletePrefix(ctx, prefix)
}

func TestDeleteExpiredRestartSources_DeletesPrefixAndClearsPayload(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ingester := NewIngester(pool, nil, nil)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 1, 4))

	fake := &restartSourceRecordingStore{}
	n, err := deleteExpiredRestartSources(context.Background(), pool, fake, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("deleteExpiredRestartSources: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d runs, want 1", n)
	}
	want := blob.RestartSourcePrefix(runID.String())
	if len(fake.prefixes) != 1 || fake.prefixes[0] != want {
		t.Fatalf("deleted prefixes = %v, want [%s]", fake.prefixes, want)
	}
	if rec := readRestartManifest(t, db, runID); rec != nil {
		t.Fatalf("the manifest survived the round: %+v", rec)
	}
}

func TestDeleteExpiredRestartSources_KeepsUnexpired(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ingester := NewIngester(pool, nil, nil)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 1, 4))

	fake := &restartSourceRecordingStore{}
	// The run was created a moment ago, so a cutoff an hour back keeps it.
	n, err := deleteExpiredRestartSources(context.Background(), pool, fake, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("deleteExpiredRestartSources: %v", err)
	}
	if n != 0 || len(fake.prefixes) != 0 {
		t.Fatalf("cleared %d runs, prefixes %v; want none", n, fake.prefixes)
	}
	if rec := readRestartManifest(t, db, runID); rec == nil {
		t.Fatal("an unexpired copy lost its manifest")
	}
}

func TestDeleteExpiredRestartSources_UsesCreatedAtWhenNotEnded(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ingester := NewIngester(pool, nil, nil)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 1, 4))

	var ended sql.NullTime
	if err := db.QueryRow(`SELECT ended_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&ended); err != nil {
		t.Fatalf("read ended_at: %v", err)
	}
	if ended.Valid {
		t.Fatal("the seeded run has an end time, so the created-at path is not under test")
	}
	var createdAt time.Time
	if err := db.QueryRow(`SELECT created_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	fake := &restartSourceRecordingStore{}
	// A cutoff at or before the creation time must not expire the copy.
	if _, err := deleteExpiredRestartSources(context.Background(), pool, fake, createdAt); err != nil {
		t.Fatalf("deleteExpiredRestartSources: %v", err)
	}
	if len(fake.prefixes) != 0 {
		t.Fatalf("a copy expired at its own creation time: %v", fake.prefixes)
	}
}

func TestDeleteExpiredRestartSources_FailedDeleteKeepsPayload(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ingester := NewIngester(pool, nil, nil)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)
	applyMeshRunState(t, ingester, runID, intentKey, restartCopyPayload(restartSourceBuildXML, 4, meshPusherEpoch, 1, 4))

	fake := &restartSourceRecordingStore{fail: true}
	if _, err := deleteExpiredRestartSources(context.Background(), pool, fake, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a failed prefix delete returned no error")
	}
	if rec := readRestartManifest(t, db, runID); rec == nil {
		t.Fatal("a failed delete cleared the manifest, so the next round would lose its retry")
	}
}

func TestMaintenanceConfig_RestartSourceRetentionDefaultsAndClamps(t *testing.T) {
	t.Parallel()

	const day = 24 * time.Hour
	if got := (MaintenanceConfig{}).restartSourceRetention(); got != 7*day {
		t.Fatalf("zero value = %v, want 7d", got)
	}
	if got := (MaintenanceConfig{RestartSourceRetention: 3 * day, RunRetention: 365 * day}).restartSourceRetention(); got != 3*day {
		t.Fatalf("3d under runs 365d = %v, want 3d", got)
	}
	if got := (MaintenanceConfig{RestartSourceRetention: 7 * day, RunRetention: 5 * day}).restartSourceRetention(); got != 5*day {
		t.Fatalf("7d under runs 5d = %v, want the runs window 5d", got)
	}
}
