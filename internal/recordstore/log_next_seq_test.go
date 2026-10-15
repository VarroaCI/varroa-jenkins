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
	line, err := json.Marshal(map[string]int64{"seq": seq, "offset": offset, "len": length})
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

func (failingLogReader) Parts(context.Context, string, int64) ([]blob.Part, error) {
	return nil, errors.New("blob tier unavailable")
}

func (failingLogReader) PartStamp(context.Context, string, int64) (blob.Stamp, error) {
	return blob.Stamp{}, errors.New("blob tier unavailable")
}

// partsFailingStore reads the index but fails every part listing.
type partsFailingStore struct{ blob.Store }

func (partsFailingStore) Parts(context.Context, string, int64) ([]blob.Part, error) {
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

// TestStoredLogNextSeqCountsUnindexedTrailingBytes asserts a log part
// past the last index line counts as one more sequence. The gateway
// writes a recovered index line for it, so it drops a chunk at that
// sequence.
func TestStoredLogNextSeqCountsUnindexedTrailingBytes(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 2)
	appendLogPart(t, store, logSeqTestRunID, 2, []byte("no index line\n"))
	assertLogNextSeq(t, store, 3)
}

// TestStoredLogNextSeqCountsEachUnindexedPart asserts that each readable
// part past the last index line is its own sequence. The gateway writes
// one recovered entry for each of them.
func TestStoredLogNextSeqCountsEachUnindexedPart(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 2)
	appendLogPart(t, store, logSeqTestRunID, 2, []byte("two\n"))
	appendLogPart(t, store, logSeqTestRunID, 3, []byte("three\n"))
	assertLogNextSeq(t, store, 4)
}

// TestStoredLogNextSeqStopsAtAMissingPart asserts that a part above a
// missing part is not a sequence. The gateway's recovery stops at the
// hole.
func TestStoredLogNextSeqStopsAtAMissingPart(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 2)
	appendLogPart(t, store, logSeqTestRunID, 2, []byte("two\n"))
	appendLogPart(t, store, logSeqTestRunID, 4, []byte("four\n"))
	assertLogNextSeq(t, store, 3)
}

// TestStoredLogNextSeqStopsAtAnEmptyPart asserts that an empty part is
// not a sequence. The gateway's recovery stops at an empty part, because
// a blank pass writes empty parts above a hole.
func TestStoredLogNextSeqStopsAtAnEmptyPart(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 2)
	appendLogPart(t, store, logSeqTestRunID, 2, []byte("two\n"))
	appendLogPart(t, store, logSeqTestRunID, 3, []byte{})
	appendLogPart(t, store, logSeqTestRunID, 4, []byte("four\n"))
	assertLogNextSeq(t, store, 3)
}

// TestStoredLogNextSeqStopsAtAnotherWritersPart asserts that a part of
// another writer than the highest index entry's is not a sequence. A
// lost cell's write can land after a takeover, and the gateway's
// recovery stops at it.
func TestStoredLogNextSeqStopsAtAnotherWritersPart(t *testing.T) {
	store := newLogSeqTestStore(t)
	ctx := context.Background()
	for seq, w := range []string{"ns/b/i2", "ns/b/i2", "ns/b/i2", "ns/a/i1"} {
		opts := logSeqTestOptions("text/plain; charset=utf-8")
		opts.Writer = w
		if err := store.Append(ctx, blob.LogKey(logSeqTestRunID), int64(seq), []byte("x\n"), opts); err != nil {
			t.Fatalf("append log part %d: %v", seq, err)
		}
	}
	for seq := int64(0); seq < 2; seq++ {
		line, err := json.Marshal(map[string]any{"seq": seq, "offset": seq * 2, "len": 2, "writer": "ns/b/i2"})
		if err != nil {
			t.Fatalf("marshal index line: %v", err)
		}
		line = append(line, '\n')
		if err := store.Append(ctx, blob.LogIndexKey(logSeqTestRunID), seq, line,
			logSeqTestOptions("application/x-ndjson")); err != nil {
			t.Fatalf("append index line %d: %v", seq, err)
		}
	}
	assertLogNextSeq(t, store, 3)
}

// TestStoredLogNextSeqSkipsAReservationAndCountsItsWritersParts asserts
// that a reservation line is no index entry, and that the next sequence
// counts the reserving writer's parts above the index, up to a part of
// another writer.
func TestStoredLogNextSeqSkipsAReservationAndCountsItsWritersParts(t *testing.T) {
	store := newLogSeqTestStore(t)
	ctx := context.Background()
	for seq, w := range []string{"ns/a/i1", "ns/a/i1", "ns/b/i2", "ns/a/i1"} {
		opts := logSeqTestOptions("text/plain; charset=utf-8")
		opts.Writer = w
		if err := store.Append(ctx, blob.LogKey(logSeqTestRunID), int64(seq), []byte("x\n"), opts); err != nil {
			t.Fatalf("append log part %d: %v", seq, err)
		}
	}
	lines := []map[string]any{
		{"seq": 0, "offset": 0, "len": 2, "writer": "ns/a/i1"},
		{"seq": 1, "offset": 2, "len": 2, "writer": "ns/a/i1"},
		{"seq": 2, "offset": 0, "len": 0, "writer": "ns/b/i2", "reserved": true},
	}
	for seq, l := range lines {
		line, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal index line: %v", err)
		}
		opts := logSeqTestOptions("application/x-ndjson")
		opts.Writer = l["writer"].(string)
		if err := store.Append(ctx, blob.LogIndexKey(logSeqTestRunID), int64(seq), append(line, '\n'), opts); err != nil {
			t.Fatalf("append index line %d: %v", seq, err)
		}
	}
	assertLogNextSeq(t, store, 3)
}

// TestStoredLogNextSeqPartListFailureIsAnError asserts that a failed part
// listing is an error, not a guess.
func TestStoredLogNextSeqPartListFailureIsAnError(t *testing.T) {
	store := newLogSeqTestStore(t)
	writeStoredLog(t, store, logSeqTestRunID, 2)
	if _, err := StoredLogNextSeq(context.Background(), partsFailingStore{store}, logSeqTestRunID); err == nil {
		t.Fatal("want an error from a failing part listing")
	}
}

// assertLogNextSeq asserts StoredLogNextSeq answers want.
func assertLogNextSeq(t *testing.T, store blob.Store, want int64) {
	t.Helper()
	got, err := StoredLogNextSeq(context.Background(), store, logSeqTestRunID)
	if err != nil {
		t.Fatalf("StoredLogNextSeq: %v", err)
	}
	if got != want {
		t.Fatalf("next = %d, want %d", got, want)
	}
}

func TestStoredLogNextSeqReadFailureIsAnError(t *testing.T) {
	if _, err := StoredLogNextSeq(context.Background(), failingLogReader{}, logSeqTestRunID); err == nil {
		t.Fatal("want an error from a failing reader")
	}
}

// TestStoredLogNextSeqMeasuresTrailingBytesFromTheHighestEntry asserts
// the part listing starts above the highest index entry, as the
// gateway's recovery does. Readable parts below an index hole are then
// not more sequences, and the gateway accepts the missing one.
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
