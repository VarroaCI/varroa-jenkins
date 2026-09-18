package recordstore

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// TestApplyRunStateKeepsTheHigherGeneration asserts the same-epoch
// reorder a gateway rollout can produce. Two gateway replicas forward
// chunks of one Jenkins process, so an older generation's report can
// arrive after a newer one.
//
// The record must stay on the newer generation, and the older
// generation's own key must be reported for deletion. That key holds the
// object the late write stored, and no reader needs it.
func TestApplyRunStateKeepsTheHigherGeneration(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)

	fake := &prefixRecordingStore{}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetStateDeleter(fake)

	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshProgramPath, Bytes: 2000, Seq: 1, StateGen: 6, PusherEpoch: 100,
	})
	applyMeshRunState(t, ingester, runID, intentKey, RunStatePayload{
		RelPath: meshProgramPath, Bytes: 1000, Seq: 1, StateGen: 5, PusherEpoch: 100,
	})

	files := readMeshFiles(t, db, runID)
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if files[0].StateGen != 6 || files[0].Bytes != 2000 {
		t.Fatalf("files[0] = %+v, want the generation-6 entry", files[0])
	}

	want := blob.RunStateKey(runID.String(), 100, 5, meshProgramPath)
	if len(fake.keys) != 1 || fake.keys[0] != want {
		t.Fatalf("deleted keys = %v, want only %q", fake.keys, want)
	}
}

// TestApplyRunStateRedeliveredPushDeletesNothing asserts a repeat of the
// stored push deletes no object. A redelivery names the winner's own
// key, so a delete would remove the object the record keeps.
func TestApplyRunStateRedeliveredPushDeletesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := seedGrantedIntent(t, pool, intentKey)

	fake := &prefixRecordingStore{}
	ingester := NewIngester(pool, nil, nil)
	ingester.SetStateDeleter(fake)

	push := RunStatePayload{
		RelPath: meshProgramPath, Bytes: 1000, Seq: 1, StateGen: 3, PusherEpoch: 100,
	}
	applyMeshRunState(t, ingester, runID, intentKey, push)
	applyMeshRunState(t, ingester, runID, intentKey, push)

	files := readMeshFiles(t, db, runID)
	if len(files) != 1 || files[0].StateGen != 3 || files[0].Bytes != 1000 {
		t.Fatalf("files = %+v, want the one stored push", files)
	}
	if len(fake.keys) != 0 {
		t.Fatalf("deleted keys = %v, want none for a redelivery", fake.keys)
	}
}
