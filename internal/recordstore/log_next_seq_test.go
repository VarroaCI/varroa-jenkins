package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

const logSeqTestRunID = "0190a5a0-0000-7000-8000-000000000001"

// newLogSeqTestStore opens a file-backed blob store in a temporary
// directory. It needs no database.
func newLogSeqTestStore(t *testing.T) blob.Store {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return store
}

// logSeqTestOptions returns the object options the gateway writes a log
// part and an index line with.
func logSeqTestOptions(contentType string) blob.ObjectOptions {
	return blob.ObjectOptions{ObjectClass: "log", ContentType: contentType, CreatedAt: time.Now().UTC()}
}

// appendLogPart writes one log part at seq.
func appendLogPart(t *testing.T, store blob.Store, runID string, seq int64, data []byte) {
	t.Helper()
	if err := store.Append(context.Background(), blob.LogKey(runID), seq, data,
		logSeqTestOptions("text/plain; charset=utf-8")); err != nil {
		t.Fatalf("append log part %d: %v", seq, err)
	}
}

// appendIndexLine writes one index line at indexSeq that names seq.
func appendIndexLine(t *testing.T, store blob.Store, runID string, indexSeq, seq, offset, length int64) {
	t.Helper()
	line, err := json.Marshal(storedLogIndexEntry{Seq: seq, Offset: offset, Len: length})
	if err != nil {
		t.Fatalf("marshal index line: %v", err)
	}
	line = append(line, '\n')
	if err := store.Append(context.Background(), blob.LogIndexKey(runID), indexSeq, line,
		logSeqTestOptions("application/x-ndjson")); err != nil {
		t.Fatalf("append index line %d: %v", seq, err)
	}
}

// writeStoredLog writes n log parts and their index lines, the way the
// gateway writes an unbroken log.
func writeStoredLog(t *testing.T, store blob.Store, runID string, n int64) {
	t.Helper()
	var offset int64
	for seq := int64(0); seq < n; seq++ {
		data := []byte(fmt.Sprintf("line %d\n", seq))
		appendLogPart(t, store, runID, seq, data)
		appendIndexLine(t, store, runID, seq, seq, offset, int64(len(data)))
		offset += int64(len(data))
	}
}

// failingLogReader answers an error to every read.
type failingLogReader struct{}

func (failingLogReader) NewRangeReader(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("blob tier unavailable")
}

func TestStoredLogNextSeqNoIndexAnswersZero(t *testing.T) {
	store := newLogSeqTestStore(t)
	got, err := StoredLogNextSeq(context.Background(), store, logSeqTestRunID)
	if err != nil {
		t.Fatalf("StoredLogNextSeq: %v", err)
	}
	if got != 0 {
		t.Fatalf("next = %d, want 0", got)
	}
}

func TestStoredLogNextSeqContiguousIndex(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 5)
	got, err := StoredLogNextSeq(context.Background(), store, logSeqTestRunID)
	if err != nil {
		t.Fatalf("StoredLogNextSeq: %v", err)
	}
	if got != 5 {
		t.Fatalf("next = %d, want 5", got)
	}
}

// TestStoredLogNextSeqStopsAtFirstHole asserts an index line above a
// missing sequence does not raise the answer. The gateway accepts the
// missing sequence next.
func TestStoredLogNextSeqStopsAtFirstHole(t *testing.T) {
	store := newLogSeqTestStore(t)
	appendLogPart(t, store, logSeqTestRunID, 0, []byte("a\n"))
	appendLogPart(t, store, logSeqTestRunID, 1, []byte("b\n"))
	appendIndexLine(t, store, logSeqTestRunID, 0, 0, 0, 2)
	appendIndexLine(t, store, logSeqTestRunID, 1, 1, 2, 2)
	appendIndexLine(t, store, logSeqTestRunID, 2, 3, 6, 2)
	got, err := StoredLogNextSeq(context.Background(), store, logSeqTestRunID)
	if err != nil {
		t.Fatalf("StoredLogNextSeq: %v", err)
	}
	if got != 2 {
		t.Fatalf("next = %d, want 2", got)
	}
}

// TestStoredLogNextSeqCountsUnindexedTrailingBytes asserts log bytes past
// the last index line count as one more sequence. The gateway writes a
// recovered index line for them, so it drops a chunk at that sequence.
func TestStoredLogNextSeqCountsUnindexedTrailingBytes(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 2)
	appendLogPart(t, store, logSeqTestRunID, 2, []byte("no index line\n"))
	got, err := StoredLogNextSeq(context.Background(), store, logSeqTestRunID)
	if err != nil {
		t.Fatalf("StoredLogNextSeq: %v", err)
	}
	if got != 3 {
		t.Fatalf("next = %d, want 3", got)
	}
}

func TestStoredLogNextSeqReadFailureIsAnError(t *testing.T) {
	if _, err := StoredLogNextSeq(context.Background(), failingLogReader{}, logSeqTestRunID); err == nil {
		t.Fatal("want an error from a failing reader")
	}
}

// TestStoredLogNextSeqMeasuresTrailingBytesFromTheHighestEntry asserts
// the trailing-bytes test starts at the end of the highest index entry,
// as the gateway's recovery does. Readable bytes above an index hole are
// then not one more sequence, and the gateway accepts the missing one.
func TestStoredLogNextSeqMeasuresTrailingBytesFromTheHighestEntry(t *testing.T) {
	store := newLogSeqTestStore(t)
	for seq := int64(0); seq < 3; seq++ {
		appendLogPart(t, store, logSeqTestRunID, seq, []byte("x\n"))
	}
	appendIndexLine(t, store, logSeqTestRunID, 0, 0, 0, 2)
	appendIndexLine(t, store, logSeqTestRunID, 1, 1, 2, 2)
	appendIndexLine(t, store, logSeqTestRunID, 2, 3, 6, 2)
	got, err := StoredLogNextSeq(context.Background(), store, logSeqTestRunID)
	if err != nil {
		t.Fatalf("StoredLogNextSeq: %v", err)
	}
	if got != 2 {
		t.Fatalf("next = %d, want 2", got)
	}
}
