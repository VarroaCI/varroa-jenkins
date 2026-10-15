package mite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// --- 5.9: the live-chunk relay payload matches the index entry ---

// TestPublishLiveLogChunkMatchesIndexEntry proves the gateway publishes
// the live-chunk relay payload only once a chunk's own index entry is
// durable, and that the published message repeats that entry field for
// field plus the base64 console bytes (design.md D5, "The live-chunk
// payload"; log-live-tail).
//
// It sends the run's two chunks out of order, so sequence 1's index
// entry — and its relay message — must wait for sequence 0 to close the
// hole below it, and its offset can only be correct if the gateway
// computes it at that later moment rather than at arrival.
func TestPublishLiveLogChunkMatchesIndexEntry(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	const ns, name = "ns1", "cell1"
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, sender)
	h.conn = busConn

	runID := newTestRunID()
	owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: ns, Cell: name, Instance: "inst1"})
	lease := h.getOrCreateTailLease(runID, ns, name)
	lease.mu.Lock()
	lease.active = true
	lease.mu.Unlock()

	received := make(chan liveLogChunkPayload, 8)
	sub, err := busConn.SubscribeData(bus.LogSubject("core", ns, name, runID), func(data []byte) {
		var p liveLogChunkPayload
		if err := json.Unmarshal(data, &p); err != nil {
			t.Errorf("decode live log chunk: %v", err)
			return
		}
		received <- p
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	data0 := []byte("first chunk\n")
	data1 := []byte("second chunk\n")

	// Sequence 1 arrives first. Its log part is durable, but its own
	// index entry cannot flush yet — the hole at sequence 0 is still
	// open — so no relay message is published for it.
	h.OnLogChunk(name, ns, &mitev1.LogChunk{
		RunId: runID, JobPath: "j", BuildNumber: 1, Seq: 1, NodeId: "n1", StageId: "s1",
		FirstLineUnixMs: 2000, Data: data1, CellInstance: "inst1",
	})
	select {
	case p := <-received:
		t.Fatalf("expected no relay message before sequence 0 closes the hole, got %+v", p)
	case <-time.After(150 * time.Millisecond):
	}

	// Sequence 0 arrives and fills the hole: both entries flush, in
	// order, each with its own correct offset.
	h.OnLogChunk(name, ns, &mitev1.LogChunk{
		RunId: runID, JobPath: "j", BuildNumber: 1, Seq: 0, NodeId: "n0", StageId: "s0",
		FirstLineUnixMs: 1000, Data: data0, CellInstance: "inst1",
	})

	first := waitForLiveChunk(t, received)
	second := waitForLiveChunk(t, received)

	if first.Seq != 0 || second.Seq != 1 {
		t.Fatalf("expected relay order seq 0 then seq 1, got %d then %d", first.Seq, second.Seq)
	}

	entries, maxSeq, _, err := readLogIndex(context.Background(), h.logStore, blob.LogIndexKey(runID))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if maxSeq != 1 {
		t.Fatalf("maxSeq = %d, want 1", maxSeq)
	}

	for _, tc := range []struct {
		name string
		got  liveLogChunkPayload
		data []byte
	}{
		{"seq 0", first, data0},
		{"seq 1", second, data1},
	} {
		want, ok := entries[tc.got.Seq]
		if !ok {
			t.Fatalf("%s: no index entry for sequence %d", tc.name, tc.got.Seq)
		}
		if tc.got.RunID != runID {
			t.Errorf("%s: runId = %q, want %q", tc.name, tc.got.RunID, runID)
		}
		if tc.got.LogIndexEntry != want {
			t.Errorf("%s: index entry = %+v, want %+v", tc.name, tc.got.LogIndexEntry, want)
		}
		if string(tc.got.Data) != string(tc.data) {
			t.Errorf("%s: data = %q, want %q", tc.name, tc.got.Data, tc.data)
		}
	}
}

// waitForLiveChunk waits for the next relay message, failing the test
// if none arrives in time.
func waitForLiveChunk(t *testing.T, ch chan liveLogChunkPayload) liveLogChunkPayload {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a live log chunk")
	}
	return liveLogChunkPayload{}
}
