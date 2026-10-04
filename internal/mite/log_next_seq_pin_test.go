package mite

import (
	"context"
	"strings"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// TestStoredLogNextSeqMatchesGatewayRecovery pins the recorder's next log
// sequence to the gateway's own restart recovery. A moved build starts
// its log at the recorder's value. A lower value names a sequence the
// gateway drops, and a higher value leaves a hole it never fills.
//
// The trailing case writes a log part with no index line, the state a
// gateway crash between the two appends leaves. The gateway recovers
// that part as one more sequence, so the recorder must count it too.
func TestStoredLogNextSeqMatchesGatewayRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trailing bool
		want     int64
	}{
		{name: "indexed log", trailing: false, want: 2},
		{name: "unindexed trailing bytes", trailing: true, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := blob.Open(ctx, "file://"+t.TempDir(), nil)
			if err != nil {
				t.Fatalf("open blob store: %v", err)
			}
			runID := newTestRunID()
			owner := newFakeOwnerRequester()
			owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst1"})

			oldSender := &recordingSender{}
			oldGateway := newLogTestHandlerOnStore(t, owner, oldSender, store)
			for seq := int64(0); seq < 2; seq++ {
				oldGateway.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
					RunId: runID, Seq: seq, Data: []byte("old line\n"), CellInstance: "inst1",
				})
				waitForLogAcks(t, oldSender, int(seq)+1)
			}
			if tc.trailing {
				opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8",
					CreatedAt: runCreatedAtFromUUIDv7(runID)}
				if err := store.Append(ctx, blob.LogKey(runID), 2, []byte("unindexed trailing line\n"), opts); err != nil {
					t.Fatalf("append unindexed part: %v", err)
				}
			}

			next, err := recordstore.StoredLogNextSeq(ctx, store, runID)
			if err != nil {
				t.Fatalf("StoredLogNextSeq: %v", err)
			}
			if next != tc.want {
				t.Fatalf("next = %d, want %d", next, tc.want)
			}

			// A fresh gateway recovers from the store, as the gateway that
			// serves the moved build does.
			newSender := &recordingSender{}
			newGateway := newLogTestHandlerOnStore(t, owner, newSender, store)
			newGateway.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
				RunId: runID, Seq: next, Data: []byte("Resuming build\n"), Final: true, CellInstance: "inst1",
			})
			acks := waitForLogAcks(t, newSender, 1)
			if acks[0].Status != logAckStatusStored || acks[0].AckSeq != next {
				t.Fatalf("ack = %+v, want STORED ack_seq=%d", acks[0], next)
			}

			entries, _, finalSeen, err := readLogIndex(ctx, store, blob.LogIndexKey(runID))
			if err != nil {
				t.Fatalf("read index: %v", err)
			}
			if e, ok := entries[next]; !ok || e.Len != int64(len("Resuming build\n")) || !finalSeen {
				t.Fatalf("index entry at %d = %+v (present %v, final %v), want the new chunk", next, e, ok, finalSeen)
			}
			if !waitForLogSuffix(t, store, runID, "Resuming build\n") {
				t.Fatal("the stored log does not end with the moved build's line")
			}
		})
	}
}

// waitForLogSuffix polls until the readable log of runID ends with
// suffix, or answers false after a bounded wait.
func waitForLogSuffix(t *testing.T, store blob.Store, runID, suffix string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := store.NewRangeReader(context.Background(), blob.LogKey(runID), 0, -1)
		if err == nil {
			var b strings.Builder
			buf := make([]byte, 4096)
			for {
				n, rerr := r.Read(buf)
				b.Write(buf[:n])
				if rerr != nil {
					break
				}
			}
			_ = r.Close()
			if strings.HasSuffix(b.String(), suffix) {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
