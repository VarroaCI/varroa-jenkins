package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// stageStaleRun seeds a completed run whose wait anchor is two hours old.
// A non-nil logPayload is applied as a run.log before the completion.
func stageStaleRun(t *testing.T, pool *pgxpool.Pool, db *sql.DB, intentKey string, logPayload map[string]any) uuid.UUID {
	t.Helper()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	if logPayload != nil {
		if _, err := g.Apply(context.Background(), newTestEnvelope(t, runID, intentKey, KindRunLog, logPayload)); err != nil {
			t.Fatalf("apply run.log: %v", err)
		}
	}
	if _, err := g.Apply(context.Background(), newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{})); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, time.Now().UTC().Add(-2*time.Hour), runID); err != nil {
		t.Fatalf("backdate ended_at: %v", err)
	}
	return runID
}

// writeIndexedLog writes n log parts and their index lines, the way the
// gateway writes an unbroken log. final marks the last index line final.
func writeIndexedLog(t *testing.T, store blob.Store, runID uuid.UUID, n int64, final bool) {
	t.Helper()
	var offset int64
	for seq := int64(0); seq < n; seq++ {
		data := []byte(fmt.Sprintf("line %d\n", seq))
		appendLogPart(t, store, runID.String(), seq, data)
		line, err := json.Marshal(map[string]any{
			"seq": seq, "offset": offset, "len": len(data), "final": final && seq == n-1,
		})
		if err != nil {
			t.Fatalf("marshal index line: %v", err)
		}
		writeIndexPart(t, store, runID, seq, append(line, '\n'))
		offset += int64(len(data))
	}
}

// writeIndexPart writes the raw bytes of one index part at seq.
func writeIndexPart(t *testing.T, store blob.Store, runID uuid.UUID, seq int64, line []byte) {
	t.Helper()
	if err := store.Append(context.Background(), blob.LogIndexKey(runID.String()), seq, line,
		logSeqTestOptions("application/x-ndjson")); err != nil {
		t.Fatalf("append index part %d: %v", seq, err)
	}
}

// logSummaryFields is the stored log summary fields the sweep writes. A
// nil field is absent from the summary.
type logSummaryFields struct {
	finalized  *bool
	incomplete *bool
	reason     *string
	chunkCount *int64
}

func readLogSummaryFields(t *testing.T, db *sql.DB, runID uuid.UUID) logSummaryFields {
	t.Helper()
	var f logSummaryFields
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'finalized')::bool, (payload->'log_summary'->>'incomplete')::bool,
		       payload->'log_summary'->>'incomplete_reason', (payload->'log_summary'->>'chunk_count')::bigint
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&f.finalized, &f.incomplete, &f.reason, &f.chunkCount); err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	return f
}

func (f logSummaryFields) String() string {
	show := func(v any) string {
		switch p := v.(type) {
		case *bool:
			if p != nil {
				return fmt.Sprint(*p)
			}
		case *string:
			if p != nil {
				return fmt.Sprintf("%q", *p)
			}
		case *int64:
			if p != nil {
				return fmt.Sprint(*p)
			}
		}
		return "absent"
	}
	return fmt.Sprintf("finalized=%s incomplete=%s reason=%s chunk_count=%s",
		show(f.finalized), show(f.incomplete), show(f.reason), show(f.chunkCount))
}

// isFinalizedClean reports whether the summary is finalized with no
// incomplete mark of its own.
func (f logSummaryFields) isFinalizedClean() bool {
	return f.finalized != nil && *f.finalized && f.incomplete == nil && f.reason == nil
}

// isNotFinalized reports whether the summary carries the sweep's
// not-finalized mark.
func (f logSummaryFields) isNotFinalized() bool {
	return f.finalized != nil && *f.finalized && f.incomplete != nil && *f.incomplete &&
		f.reason != nil && *f.reason == logReasonNotFinalized
}

// failingIndexReader fails each read of failKey, or of every key when
// failKey is empty, and serves every other read from store. A failLimit
// above 0 fails only that many reads. It counts every read.
type failingIndexReader struct {
	store     blob.Store
	failKey   string
	failLimit int
	failed    int
	reads     int
}

func (r *failingIndexReader) NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	r.reads++
	if (r.failKey == "" || key == r.failKey) && (r.failLimit == 0 || r.failed < r.failLimit) {
		r.failed++
		return nil, errors.New("blob tier unavailable")
	}
	return r.store.NewRangeReader(ctx, key, offset, length)
}

// readIndexReadFailedAt reads the run's stored first failed index read,
// in milliseconds since the epoch. It is nil when the run holds none.
func readIndexReadFailedAt(t *testing.T, db *sql.DB, runID uuid.UUID) *int64 {
	t.Helper()
	var ms *int64
	if err := db.QueryRow(`SELECT (payload->>'log_index_read_failed_unix_ms')::bigint FROM varroa.runs WHERE run_id = $1`,
		runID).Scan(&ms); err != nil {
		t.Fatalf("read failed-read time: %v", err)
	}
	return ms
}

// TestFinalizeStaleLogsNeverMarksAnOldRunOnOneReadError asserts that one
// transient read error does not mark a run whose wait anchor is already
// 48 hours old. The error starts the run's own give-up clock. The next
// round reads the intact index, finalizes the run with no incomplete
// mark, and removes the clock.
func TestFinalizeStaleLogsNeverMarksAnOldRunOnOneReadError(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	store := blob.NewPGBlobStore(pool)
	runID := stageStaleRun(t, pool, db, "manual:finalize-one-error-1", nil)
	if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, time.Now().UTC().Add(-48*time.Hour), runID); err != nil {
		t.Fatalf("backdate ended_at: %v", err)
	}
	writeIndexedLog(t, store, runID, 2, true)

	now := time.Now().UTC()
	reader := &failingIndexReader{store: store, failKey: blob.LogIndexKey(runID.String()), failLimit: 1}
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, now)
	if err != nil || n != 0 {
		t.Fatalf("round with one read error: n=%d err=%v, want no run finalized", n, err)
	}
	if found, _ := readLogSummaryFinalized(t, db, runID); found {
		t.Fatalf("one transient read error marked a run whose index is intact")
	}
	if got := readIndexReadFailedAt(t, db, runID); got == nil || *got != now.UnixMilli() {
		t.Fatalf("failed-read time = %v, want %d", got, now.UnixMilli())
	}

	n, err = finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, now.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("round with a good read: n=%d err=%v, want 1 run finalized", n, err)
	}
	if got := readLogSummaryFields(t, db, runID); !got.isFinalizedClean() {
		t.Fatalf("%s, want finalized true with no incomplete mark", got)
	}
	if got := readIndexReadFailedAt(t, db, runID); got != nil {
		t.Fatalf("failed-read time = %d, want it removed by the final write", *got)
	}
}

// TestFinalizeStaleLogsFinalizesAStoredFinalEntry asserts a run whose
// stored index holds the final entry, with no finalized marker in its
// record, is finalized with no incomplete mark on both blob tiers. The
// summary gets the chunk count that the final entry proves. A stored
// reason stays, as the gateway's own marker keeps it, and a late marker
// changes nothing.
func TestFinalizeStaleLogsFinalizesAStoredFinalEntry(t *testing.T) {
	t.Parallel()

	tiers := []struct {
		name string
		open func(t *testing.T, pool *pgxpool.Pool) blob.Store
	}{
		{name: "postgres", open: func(_ *testing.T, pool *pgxpool.Pool) blob.Store { return blob.NewPGBlobStore(pool) }},
		{name: "object store", open: func(t *testing.T, _ *pgxpool.Pool) blob.Store { return newLogSeqTestStore(t) }},
	}
	summaries := []struct {
		name       string
		logPayload map[string]any
		wantReason string
	}{
		{name: "no summary"},
		{name: "summary reports finalized false", logPayload: map[string]any{"finalized": false, "chunk_count": 3}},
		{name: "summary holds a loss reason", logPayload: map[string]any{"incomplete": true, "incomplete_reason": "gateway-gap"},
			wantReason: "gateway-gap"},
	}
	for _, tier := range tiers {
		for _, sc := range summaries {
			t.Run(tier.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				pool, db := setupLedgerPool(t)
				store := tier.open(t, pool)
				const intentKey = "manual:finalize-final-entry-1"
				runID := stageStaleRun(t, pool, db, intentKey, sc.logPayload)
				writeIndexedLog(t, store, runID, 3, true)

				n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, time.Now().UTC())
				if err != nil || n != 1 {
					t.Fatalf("finalizeStaleLogs: n=%d err=%v, want 1 run finalized", n, err)
				}
				check := func(when string) {
					t.Helper()
					got := readLogSummaryFields(t, db, runID)
					if got.chunkCount == nil || *got.chunkCount != 3 {
						t.Fatalf("%s: %s, want chunk_count 3", when, got)
					}
					if sc.wantReason == "" {
						if !got.isFinalizedClean() {
							t.Fatalf("%s: %s, want finalized true with no incomplete mark", when, got)
						}
						return
					}
					if got.finalized == nil || !*got.finalized || got.incomplete == nil || !*got.incomplete ||
						got.reason == nil || *got.reason != sc.wantReason {
						t.Fatalf("%s: %s, want finalized true and the stored reason %q", when, got, sc.wantReason)
					}
				}
				check("after the sweep")

				g := NewIngester(pool, nil, nil)
				marker := newTestEnvelope(t, runID, intentKey, KindRunLog, map[string]any{"finalized": true})
				if _, err := g.Apply(context.Background(), marker); err != nil {
					t.Fatalf("apply late marker: %v", err)
				}
				check("after a late marker")
			})
		}
	}
}

// TestFinalizeStaleLogsMarksALogWithNoFinalEntry asserts the sweep still
// marks a run not-finalized when the stored index holds no usable final
// entry: no index at all, a real short log, or an index that does not
// parse.
func TestFinalizeStaleLogsMarksALogWithNoFinalEntry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		write func(t *testing.T, store blob.Store, runID uuid.UUID)
	}{
		{name: "no stored index", write: func(*testing.T, blob.Store, uuid.UUID) {}},
		{name: "a real short log", write: func(t *testing.T, store blob.Store, runID uuid.UUID) {
			writeIndexedLog(t, store, runID, 3, false)
		}},
		{name: "an index that does not parse", write: func(t *testing.T, store blob.Store, runID uuid.UUID) {
			writeIndexPart(t, store, runID, 0, []byte("not json\n"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, db := setupLedgerPool(t)
			store := blob.NewPGBlobStore(pool)
			runID := stageStaleRun(t, pool, db, "manual:finalize-no-final-1", map[string]any{"finalized": false})
			tc.write(t, store, runID)

			n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, time.Now().UTC())
			if err != nil || n != 1 {
				t.Fatalf("finalizeStaleLogs: n=%d err=%v, want 1 run finalized", n, err)
			}
			if got := readLogSummaryFields(t, db, runID); !got.isNotFinalized() {
				t.Fatalf("%s, want finalized true and incomplete true with reason %q", got, logReasonNotFinalized)
			}
		})
	}
}

// TestFinalizeStaleLogsLeavesARunOnAnIndexReadError asserts a failed
// index read leaves that run's log summary untouched for the round, never
// marked not-finalized, and stores its failed-read time, while the round
// still finalizes the other runs. The next round with a healthy store
// finalizes the run from its index and removes the time.
func TestFinalizeStaleLogsLeavesARunOnAnIndexReadError(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	store := blob.NewPGBlobStore(pool)
	failed := stageStaleRun(t, pool, db, "manual:finalize-read-error-1", nil)
	writeIndexedLog(t, store, failed, 2, true)
	healthy := stageStaleRun(t, pool, db, "manual:finalize-read-error-2", nil)
	writeIndexedLog(t, store, healthy, 2, true)

	reader := &failingIndexReader{store: store, failKey: blob.LogIndexKey(failed.String())}
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("finalizeStaleLogs: n=%d err=%v, want only the healthy run finalized", n, err)
	}
	if found, _ := readLogSummaryFinalized(t, db, failed); found {
		t.Fatalf("the sweep wrote a summary for a run whose index read failed")
	}
	if readIndexReadFailedAt(t, db, failed) == nil {
		t.Fatalf("the failed read stored no failed-read time")
	}
	if got := readLogSummaryFields(t, db, healthy); !got.isFinalizedClean() {
		t.Fatalf("healthy run: %s, want finalized true with no incomplete mark", got)
	}

	n, err = finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, time.Now().UTC())
	if err != nil || n != 1 {
		t.Fatalf("second round: n=%d err=%v, want the left run finalized", n, err)
	}
	if got := readLogSummaryFields(t, db, failed); !got.isFinalizedClean() {
		t.Fatalf("left run: %s, want finalized true with no incomplete mark", got)
	}
	if got := readIndexReadFailedAt(t, db, failed); got != nil {
		t.Fatalf("failed-read time = %d, want it removed by the final write", *got)
	}
}

// TestFinalizeStaleLogsEndsTheRoundWhenTheBlobTierIsDown asserts that a
// failed read whose probe also fails ends the round after one index read
// and one probe read. No run gets a mark or a failed-read time, even a
// run whose wait anchor is past the give-up age, because a tier that
// does not answer is no evidence of a short log.
func TestFinalizeStaleLogsEndsTheRoundWhenTheBlobTierIsDown(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	old := time.Now().UTC().Add(-finalizeIndexGiveUpAge - time.Hour)
	var runs []uuid.UUID
	for i := 0; i < 4; i++ {
		runID := stageStaleRun(t, pool, db, fmt.Sprintf("manual:finalize-store-down-%d", i), nil)
		if _, err := db.Exec(`UPDATE varroa.runs SET ended_at = $1 WHERE run_id = $2`, old, runID); err != nil {
			t.Fatalf("backdate ended_at: %v", err)
		}
		runs = append(runs, runID)
	}

	reader := &failingIndexReader{store: blob.NewPGBlobStore(pool)}
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, time.Now().UTC())
	if err != nil || n != 0 {
		t.Fatalf("finalizeStaleLogs: n=%d err=%v, want no run finalized", n, err)
	}
	if reader.reads != 2 {
		t.Fatalf("blob reads = %d, want 2: one index read and one probe read", reader.reads)
	}
	for _, runID := range runs {
		if found, _ := readLogSummaryFinalized(t, db, runID); found {
			t.Fatalf("run %s got a summary while the blob tier was down", runID)
		}
		if readIndexReadFailedAt(t, db, runID) != nil {
			t.Fatalf("run %s got a failed-read time while the blob tier was down", runID)
		}
	}
}

// TestFinalizeLogSummaryKeepsAStoredChunkCount asserts the final-entry
// write keeps a chunk_count that the summary already holds.
func TestFinalizeLogSummaryKeepsAStoredChunkCount(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := stageStaleRun(t, pool, db, "manual:finalize-keep-count-1", map[string]any{"finalized": false, "chunk_count": 5})
	changed, err := finalizeLogSummary(context.Background(), pool, runID.String(), nil, time.Now().UTC().Add(-time.Minute), 3)
	if err != nil || !changed {
		t.Fatalf("finalizeLogSummary: changed=%v err=%v, want the row changed", changed, err)
	}
	if got := readLogSummaryFields(t, db, runID); !got.isFinalizedClean() || got.chunkCount == nil || *got.chunkCount != 5 {
		t.Fatalf("%s, want finalized true with no incomplete mark and the stored chunk_count 5", got)
	}
}

// TestFinalizeStaleLogsSpendsTheReadBudget asserts a round reads at most
// its budget of stored indexes, that a read that finds no index does not
// count, and that a run past the budget waits for the next round.
func TestFinalizeStaleLogsSpendsTheReadBudget(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	store := blob.NewPGBlobStore(pool)
	noIndex := stageStaleRun(t, pool, db, "manual:finalize-budget-1", nil)
	first := stageStaleRun(t, pool, db, "manual:finalize-budget-2", nil)
	writeIndexedLog(t, store, first, 2, true)
	second := stageStaleRun(t, pool, db, "manual:finalize-budget-3", nil)
	writeIndexedLog(t, store, second, 2, true)

	budget := finalizeReadBudget{first: 1, deferred: finalizeIndexDeferredReadBudget}
	n, err := finalizeStaleLogsWithin(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, time.Now().UTC(), budget)
	if err != nil || n != 2 {
		t.Fatalf("first round: n=%d err=%v, want 2 runs finalized", n, err)
	}
	if got := readLogSummaryFields(t, db, noIndex); !got.isNotFinalized() {
		t.Fatalf("run with no index: %s, want the not-finalized mark", got)
	}
	if got := readLogSummaryFields(t, db, first); !got.isFinalizedClean() {
		t.Fatalf("first indexed run: %s, want finalized true with no incomplete mark", got)
	}
	if found, _ := readLogSummaryFinalized(t, db, second); found {
		t.Fatalf("the round read past its budget")
	}

	n, err = finalizeStaleLogsWithin(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, time.Now().UTC(), budget)
	if err != nil || n != 1 {
		t.Fatalf("second round: n=%d err=%v, want the waiting run finalized", n, err)
	}
	if got := readLogSummaryFields(t, db, second); !got.isFinalizedClean() {
		t.Fatalf("second indexed run: %s, want finalized true with no incomplete mark", got)
	}
}

// poisonIndex stores an object under the index prefix of runID whose
// name is not a part sequence. Every bucket-tier read of that index then
// fails, while every other key reads normally.
func poisonIndex(t *testing.T, store blob.Store, runID uuid.UUID) {
	t.Helper()
	if err := store.Put(context.Background(), blob.LogIndexKey(runID.String())+"/junk", strings.NewReader("x"),
		logSeqTestOptions("application/octet-stream")); err != nil {
		t.Fatalf("store poison object: %v", err)
	}
	if _, err := store.NewRangeReader(context.Background(), blob.LogIndexKey(runID.String()), 0, -1); err == nil {
		t.Fatalf("the poisoned index of %s still reads", runID)
	}
}

// TestFinalizeStaleLogsReadsPastRunsWhoseIndexAlwaysFails asserts that
// three runs at the head of the scan, whose index read always fails on a
// healthy blob tier, never stop the round: a healthy run behind them is
// finalized in the first round. The failing runs get no mark inside the
// give-up age from their first failed read, and the not-finalized mark
// in the first round past it.
func TestFinalizeStaleLogsReadsPastRunsWhoseIndexAlwaysFails(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	store := newLogSeqTestStore(t)
	var poisoned []uuid.UUID
	for i := 0; i < 3; i++ {
		runID := stageStaleRun(t, pool, db, fmt.Sprintf("manual:finalize-poisoned-%d", i), nil)
		writeIndexedLog(t, store, runID, 2, true)
		poisonIndex(t, store, runID)
		poisoned = append(poisoned, runID)
	}
	healthy := stageStaleRun(t, pool, db, "manual:finalize-behind-poison-1", nil)
	writeIndexedLog(t, store, healthy, 3, true)

	start := time.Now().UTC()
	for round, offset := range []time.Duration{0, finalizeIndexGiveUpAge - time.Hour} {
		want := map[int]int{0: 1, 1: 0}[round]
		n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, start.Add(offset))
		if err != nil || n != want {
			t.Fatalf("round %d: n=%d err=%v, want %d runs finalized", round+1, n, err, want)
		}
		if got := readLogSummaryFields(t, db, healthy); !got.isFinalizedClean() {
			t.Fatalf("round %d: healthy run: %s, want finalized true with no incomplete mark", round+1, got)
		}
		for _, runID := range poisoned {
			if found, _ := readLogSummaryFinalized(t, db, runID); found {
				t.Fatalf("round %d: a run inside the give-up age got a summary", round+1)
			}
			if got := readIndexReadFailedAt(t, db, runID); got == nil || *got != start.UnixMilli() {
				t.Fatalf("round %d: failed-read time = %v, want the first failure at %d", round+1, got, start.UnixMilli())
			}
		}
	}

	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, start.Add(finalizeIndexGiveUpAge))
	if err != nil || n != len(poisoned) {
		t.Fatalf("round past the give-up age: n=%d err=%v, want %d runs finalized", n, err, len(poisoned))
	}
	for _, runID := range poisoned {
		if got := readLogSummaryFields(t, db, runID); !got.isNotFinalized() {
			t.Fatalf("run past the give-up age: %s, want the not-finalized mark", got)
		}
		if got := readIndexReadFailedAt(t, db, runID); got != nil {
			t.Fatalf("failed-read time = %d, want it removed by the final write", *got)
		}
	}
}

// TestFinalizeStaleLogsKeepsUnreadableRunsOffTheFirstBudget asserts that
// unreadable indexes at the head of the scan hold back a readable run for
// at most the round in which they first fail. In that round each first
// failure spends the first budget once. In every later round the failing
// runs are deferred: they read under the deferred budget only, a run past
// that budget gets no read, and the readable run behind them is
// finalized.
func TestFinalizeStaleLogsKeepsUnreadableRunsOffTheFirstBudget(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	store := newLogSeqTestStore(t)
	var poisoned []uuid.UUID
	for i := 0; i < 3; i++ {
		runID := stageStaleRun(t, pool, db, fmt.Sprintf("manual:finalize-deferred-%d", i), nil)
		writeIndexedLog(t, store, runID, 2, true)
		poisonIndex(t, store, runID)
		poisoned = append(poisoned, runID)
	}
	healthy := stageStaleRun(t, pool, db, "manual:finalize-behind-deferred-1", nil)
	writeIndexedLog(t, store, healthy, 2, true)
	budget := finalizeReadBudget{first: 3, deferred: 2}

	n, err := finalizeStaleLogsWithin(context.Background(), pool, HeartbeatLiveness{}, store, time.Minute, time.Now().UTC(), budget)
	if err != nil || n != 0 {
		t.Fatalf("round 1: n=%d err=%v, want no run finalized: three first failures spend the first budget", n, err)
	}
	if found, _ := readLogSummaryFinalized(t, db, healthy); found {
		t.Fatalf("round 1 read past its first budget")
	}

	for round := 2; round <= 3; round++ {
		// The fail key names no stored key, so the reader only counts.
		reader := &failingIndexReader{store: store, failKey: "-"}
		n, err := finalizeStaleLogsWithin(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, time.Now().UTC(), budget)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if want := map[int]int{2: 1, 3: 0}[round]; n != want {
			t.Fatalf("round %d: n=%d, want %d runs finalized", round, n, want)
		}
		// Two deferred reads, a probe after each, and one first read in
		// round 2. Round 3 reads the two deferred runs again and nothing
		// else, because the healthy run is finalized.
		if want := map[int]int{2: 5, 3: 4}[round]; reader.reads != want {
			t.Fatalf("round %d: blob reads = %d, want %d", round, reader.reads, want)
		}
		if got := readLogSummaryFields(t, db, healthy); !got.isFinalizedClean() {
			t.Fatalf("round %d: healthy run: %s, want finalized true with no incomplete mark", round, got)
		}
		for _, runID := range poisoned {
			if found, _ := readLogSummaryFinalized(t, db, runID); found {
				t.Fatalf("round %d: a run inside the give-up age got a summary", round)
			}
		}
	}
}

// TestFinalizeStaleLogsKeepsTheFirstFailureTime asserts a later failed
// read never moves a run's give-up clock, and that a read that fails just
// inside the give-up age from the first failure marks nothing.
func TestFinalizeStaleLogsKeepsTheFirstFailureTime(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	store := blob.NewPGBlobStore(pool)
	runID := stageStaleRun(t, pool, db, "manual:finalize-first-failure-1", nil)
	writeIndexedLog(t, store, runID, 2, true)
	reader := &failingIndexReader{store: store, failKey: blob.LogIndexKey(runID.String())}

	start := time.Now().UTC()
	for _, at := range []time.Time{start, start.Add(time.Hour), start.Add(finalizeIndexGiveUpAge - time.Minute)} {
		n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, at)
		if err != nil || n != 0 {
			t.Fatalf("round at %s: n=%d err=%v, want no run finalized", at.Sub(start), n, err)
		}
		if got := readIndexReadFailedAt(t, db, runID); got == nil || *got != start.UnixMilli() {
			t.Fatalf("round at %s: failed-read time = %v, want %d", at.Sub(start), got, start.UnixMilli())
		}
	}
	n, err := finalizeStaleLogs(context.Background(), pool, HeartbeatLiveness{}, reader, time.Minute, start.Add(finalizeIndexGiveUpAge))
	if err != nil || n != 1 {
		t.Fatalf("round at the give-up age: n=%d err=%v, want 1 run finalized", n, err)
	}
	if got := readLogSummaryFields(t, db, runID); !got.isNotFinalized() {
		t.Fatalf("%s, want the not-finalized mark", got)
	}
}
