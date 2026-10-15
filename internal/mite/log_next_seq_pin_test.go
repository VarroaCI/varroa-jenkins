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
// Each trailing case writes log parts with no index line, the state a
// gateway crash before or inside the index flush leaves. The gateway
// recovers each readable, non-empty part as one more sequence, so the
// recorder must count the same parts.
func TestStoredLogNextSeqMatchesGatewayRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trailing map[int64]string
		// foreign names the trailing parts that another cell instance
		// wrote, as a lost cell's late write after a takeover does.
		foreign map[int64]bool
		// moved names the trailing parts that the moved build's cell
		// instance wrote. reserved puts that instance's index reservation
		// at sequence 2, as a gateway replica that stopped after its
		// reservation leaves it.
		moved    map[int64]bool
		reserved bool
		want     int64
	}{
		{name: "indexed log", want: 2},
		{name: "one unindexed part", trailing: map[int64]string{2: "unindexed trailing line\n"}, want: 3},
		{name: "two unindexed parts", trailing: map[int64]string{2: "two\n", 3: "three\n"}, want: 4},
		{name: "a hole above the index", trailing: map[int64]string{2: "two\n", 4: "four\n"}, want: 3},
		{name: "an empty part above the index", trailing: map[int64]string{2: "two\n", 3: "", 4: "four\n"}, want: 3},
		{name: "another writer's part above the index", trailing: map[int64]string{2: "two\n", 3: "late\n"}, foreign: map[int64]bool{3: true}, want: 3},
		{name: "another writer's part at the index end", trailing: map[int64]string{2: "late\n", 3: "three\n"}, foreign: map[int64]bool{2: true}, want: 2},
		{name: "a reservation over the reserving writer's part", trailing: map[int64]string{2: "moved\n", 3: "late\n"}, moved: map[int64]bool{2: true}, reserved: true, want: 3},
		{name: "a reservation over the old writer's part", trailing: map[int64]string{2: "old\n"}, reserved: true, want: 2},
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
			for seq, data := range tc.trailing {
				opts := blob.ObjectOptions{ObjectClass: "log", ContentType: "text/plain; charset=utf-8",
					CreatedAt: runCreatedAtFromUUIDv7(runID), Writer: "ns1/cell1/inst1"}
				if tc.foreign[seq] {
					opts.Writer = "ns1/cell1/inst9"
				}
				if tc.moved[seq] {
					opts.Writer = "ns1/cell1/inst2"
				}
				if err := store.Append(ctx, blob.LogKey(runID), seq, []byte(data), opts); err != nil {
					t.Fatalf("append unindexed part %d: %v", seq, err)
				}
			}

			if tc.reserved {
				const moved = "ns1/cell1/inst2"
				if err := store.Append(ctx, blob.LogIndexKey(runID), 2, reservationLine(2, moved),
					indexOptions(runCreatedAtFromUUIDv7(runID), moved)); err != nil {
					t.Fatalf("append the reservation: %v", err)
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
			// serves the moved build does. The moved build is another cell
			// instance, so the gateway empties the old parts above the hole.
			owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell1", Instance: "inst2"})
			newSender := &recordingSender{}
			newGateway := newLogTestHandlerOnStore(t, owner, newSender, store)
			newGateway.OnLogChunk("cell1", "ns1", &mitev1.LogChunk{
				RunId: runID, Seq: next, Data: []byte("Resuming build\n"), Final: true, CellInstance: "inst2",
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
			for seq := int64(0); seq < next; seq++ {
				if _, ok := entries[seq]; !ok {
					t.Fatalf("index has no entry at %d below the next sequence %d", seq, next)
				}
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
