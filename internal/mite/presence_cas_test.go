package mite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// presenceReplicas builds two BusHandlers over one NATS server and one
// presence bucket: two gateway replicas of one install.
func presenceReplicas(t *testing.T, ttl time.Duration) (*BusHandler, *BusHandler, *bus.KV) {
	t.Helper()
	natsSrv := startNATS(t)
	replica := func() (*BusHandler, *bus.KV) {
		busConn, err := bus.Connect(natsSrv.ClientURL())
		if err != nil {
			t.Fatalf("bus connect: %v", err)
		}
		t.Cleanup(busConn.Close)
		snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
		if err != nil {
			t.Fatalf("snapshot kv: %v", err)
		}
		presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, ttl)
		if err != nil {
			t.Fatalf("presence kv: %v", err)
		}
		h := mustNewBusHandler(t, busConn, snapshotKV, presenceKV)
		t.Cleanup(h.Close)
		return h, presenceKV
	}
	a, kv := replica()
	b, _ := replica()
	return a, b, kv
}

// putForeignPresence writes a record of epoch for the mite "ctl" with a
// blind Put, as another gateway replica of an older build would.
func putForeignPresence(t *testing.T, kv *bus.KV, epoch int64) {
	t.Helper()
	data, err := json.Marshal(bus.Presence{Version: "other", LastHeartbeat: time.Now(), Epoch: epoch, StreamOpen: true})
	if err != nil {
		t.Fatalf("marshal presence: %v", err)
	}
	if err := kv.Put(bus.PresenceKey(bus.DefaultCluster, "ns", "ctl"), data); err != nil {
		t.Fatalf("put presence: %v", err)
	}
}

// TestPresenceStaleReplicaTimerStops is the probe of two replicas. Replica
// A keeps a half-open old stream and runs its timer. Replica B accepts the
// newer stream and receives its heartbeats. After B's connect write, no
// read returns A's epoch, and A's timer stops for good. A's token stays
// live in A, so only the record of B can stop A's timer.
func TestPresenceStaleReplicaTimerStops(t *testing.T) {
	a, b, kv := presenceReplicas(t, bus.PresenceTTL)
	a.presenceRefreshEvery = 5 * time.Millisecond

	tokA := a.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { a.OnDisconnect("ctl", "ns", tokA) })
	_, connectRev, found := readPresenceRecord(t, kv, "ctl")
	if !found {
		t.Fatal("A wrote no presence record at connect")
	}
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		a.KeepPresence(context.Background(), "ctl", "ns", tokA, func() (string, time.Time) {
			return readLoopRecv, time.Now()
		})
	}()
	waitFor(t, func() bool {
		p, rev, ok := readPresenceRecord(t, kv, "ctl")
		return ok && p.Epoch == tokA && rev > connectRev
	}, 10*time.Second, "A's timer did not write the record again")

	tokB := b.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { b.OnDisconnect("ctl", "ns", tokB) })
	if tokB <= tokA {
		t.Fatalf("epoch of B %d is not newer than epoch of A %d", tokB, tokA)
	}
	stop := make(chan struct{})
	beats := make(chan struct{})
	go func() {
		defer close(beats)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				b.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-beats })

	reads, stale := 0, 0
	// readUntil reads the record until done closes or the deadline passes,
	// and reports whether done closed.
	readUntil := func(done <-chan struct{}, deadline <-chan time.Time) bool {
		for {
			if p, _, ok := readPresenceRecord(t, kv, "ctl"); ok {
				reads++
				if p.Epoch == tokA {
					stale++
				}
			}
			select {
			case <-done:
				return true
			case <-deadline:
				return false
			default:
			}
		}
	}
	stopped := readUntil(aDone, time.After(10*time.Second))
	// Read on for many more of A's intervals, so a timer that kept running
	// shows its writes.
	readUntil(nil, time.After(100*a.presenceRefreshEvery))

	if stale != 0 {
		t.Errorf("%d of %d reads after B's connect returned A's epoch", stale, reads)
	}
	if reads == 0 {
		t.Error("no read after B's connect")
	}
	if !stopped {
		t.Fatal("A's timer did not stop after B's connect")
	}

	// A heartbeat that the old stream still dispatches yields too.
	a.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "old"})
	if p, _, _ := readPresenceRecord(t, kv, "ctl"); p.Epoch != tokB {
		t.Fatalf("record epoch = %d after A's late heartbeat, want B's %d", p.Epoch, tokB)
	}
}

// TestRefreshPresenceRewritesALostRecord covers the incident: the bus lost
// the record of an open stream. The timer writes it again, after a delete,
// after a purge, and after the bucket TTL expired the key.
func TestRefreshPresenceRewritesALostRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		lose func(t *testing.T, kv *bus.KV)
	}{
		{"deleted", bus.PresenceTTL, func(t *testing.T, kv *bus.KV) {
			if err := kv.Delete(bus.PresenceKey(bus.DefaultCluster, "ns", "ctl")); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}},
		{"purged", bus.PresenceTTL, func(t *testing.T, kv *bus.KV) {
			if err := kv.KeyValue().Purge(bus.PresenceKey(bus.DefaultCluster, "ns", "ctl")); err != nil {
				t.Fatalf("purge: %v", err)
			}
		}},
		{"expired", time.Second, func(t *testing.T, kv *bus.KV) {
			waitFor(t, func() bool {
				_, _, found := readPresenceRecord(t, kv, "ctl")
				return !found
			}, 30*time.Second, "the bucket TTL did not expire the record")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, kv := presenceReplicas(t, tc.ttl)
			token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
			t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })
			tc.lose(t, kv)

			if _, ok := h.refreshPresence("ctl", "ns", token, 0); !ok {
				t.Fatal("refreshPresence stopped after the record was lost")
			}
			p, _, found := readPresenceRecord(t, kv, "ctl")
			if !found || p.Epoch != token || !p.StreamOpen {
				t.Fatalf("record = %+v found=%v, want the record of epoch %d again", p, found, token)
			}
		})
	}
}

// TestPresenceGuardReplacesAnOlderRecord covers a record of an older epoch
// that another replica wrote after this connection. The guarded write
// reads it once and replaces it.
func TestPresenceGuardReplacesAnOlderRecord(t *testing.T) {
	h, _, kv := presenceReplicas(t, bus.PresenceTTL)
	token := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { h.OnDisconnect("ctl", "ns", token) })
	putForeignPresence(t, kv, token-1)

	h.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})
	if p, _, _ := readPresenceRecord(t, kv, "ctl"); p.Epoch != token {
		t.Fatalf("record epoch = %d, want %d: the older record stayed", p.Epoch, token)
	}
}

// TestPresenceMisorderedEpochsRecover covers clock skew between replicas.
// The clock of replica A leads, so its old stream holds a larger epoch than
// the newer stream on B. A's record wins while A's stream lives. When it
// ends, A deletes its own record, and B's next heartbeat writes B's record.
func TestPresenceMisorderedEpochsRecover(t *testing.T) {
	a, b, kv := presenceReplicas(t, bus.PresenceTTL)
	tokA := a.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	skewed := tokA + int64(time.Hour)
	a.mu.Lock()
	a.connectEpoch["ns/ctl"] = skewed
	a.mu.Unlock()

	tokB := b.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { b.OnDisconnect("ctl", "ns", tokB) })
	if p, _, _ := readPresenceRecord(t, kv, "ctl"); p.Epoch != tokB {
		t.Fatalf("record epoch = %d after B's connect, want %d: the connect write claims the key", p.Epoch, tokB)
	}

	if _, ok := a.refreshPresence("ctl", "ns", skewed, 0); !ok {
		t.Fatal("A's timer stopped at a record of an older epoch")
	}
	b.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})
	if p, _, _ := readPresenceRecord(t, kv, "ctl"); p.Epoch != skewed {
		t.Fatalf("record epoch = %d, want A's skewed %d while A's stream lives", p.Epoch, skewed)
	}

	a.OnDisconnect("ctl", "ns", skewed)
	if _, _, found := readPresenceRecord(t, kv, "ctl"); found {
		t.Fatal("A's disconnect left its own record")
	}
	b.OnHeartbeat("ctl", "ns", &mitev1.Heartbeat{Version: "v1"})
	if p, _, found := readPresenceRecord(t, kv, "ctl"); !found || p.Epoch != tokB {
		t.Fatalf("record = %+v found=%v, want B's epoch %d after A's stream ended", p, found, tokB)
	}
}

// TestOnDisconnectLeavesANewerRecord covers the teardown of the old stream
// on replica A after replica B wrote the record of the newer stream.
func TestOnDisconnectLeavesANewerRecord(t *testing.T) {
	a, b, kv := presenceReplicas(t, bus.PresenceTTL)
	tokA := a.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	tokB := b.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	t.Cleanup(func() { b.OnDisconnect("ctl", "ns", tokB) })

	a.OnDisconnect("ctl", "ns", tokA)
	if p, _, found := readPresenceRecord(t, kv, "ctl"); !found || p.Epoch != tokB {
		t.Fatalf("record = %+v found=%v, want B's record of epoch %d", p, found, tokB)
	}
}

// TestOnConnectClearsWriteMarks covers a supersede on one replica: the
// write marks of the old connection do not carry over to the new one.
func TestOnConnectClearsWriteMarks(t *testing.T) {
	h, kv := presenceTestHandler(t)
	first := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	h.mu.Lock()
	_, marked := h.presenceRev["ns/ctl"]
	h.mu.Unlock()
	if !marked {
		t.Fatal("the connect write recorded no revision")
	}

	h.presenceKV = nil // the connect write of the next connection fails
	second := h.OnConnect("ctl", "ns", "v1", time.Now().Add(time.Hour), noSend)
	h.mu.Lock()
	_, rev := h.presenceRev["ns/ctl"]
	_, at := h.presenceWrittenAt["ns/ctl"]
	h.mu.Unlock()
	if rev || at {
		t.Errorf("write marks of epoch %d kept for epoch %d (rev=%v at=%v)", first, second, rev, at)
	}
	h.presenceKV = kv
	h.OnDisconnect("ctl", "ns", second)
}

// conflictStore answers every write with a conflict and every read with a
// record of epoch, and counts the calls.
type conflictStore struct {
	epoch   int64
	updates int
	creates int
	reads   int
	missing bool
	fail    error
}

func (s *conflictStore) Entry(string) ([]byte, uint64, error) {
	s.reads++
	if s.missing {
		return nil, 0, nil
	}
	data, _ := json.Marshal(bus.Presence{Epoch: s.epoch})
	return data, uint64(10 + s.reads), nil
}

func (s *conflictStore) Update(string, []byte, uint64) (uint64, error) {
	s.updates++
	if s.fail != nil {
		return 0, s.fail
	}
	return 0, bus.ErrKVRevisionMismatch
}

func (s *conflictStore) CreateRevision(string, []byte) (uint64, error) {
	s.creates++
	return 0, bus.ErrKVKeyExists
}

func TestCasPresenceBoundsTheConflictPath(t *testing.T) {
	const ep = int64(100)
	t.Run("conflicts give up after two reads", func(t *testing.T) {
		s := &conflictStore{epoch: ep - 1}
		_, outcome, err := casPresence(s, "k", []byte("{}"), ep, 7)
		if outcome != presenceNotWritten || err == nil {
			t.Fatalf("outcome = %v err = %v, want presenceNotWritten with an error", outcome, err)
		}
		if s.updates != 1+presenceCASReads || s.reads != presenceCASReads {
			t.Errorf("updates=%d reads=%d, want %d and %d", s.updates, s.reads, 1+presenceCASReads, presenceCASReads)
		}
	})
	t.Run("a missing key that another writer creates first gives up too", func(t *testing.T) {
		s := &conflictStore{missing: true}
		_, outcome, _ := casPresence(s, "k", []byte("{}"), ep, 0)
		if outcome != presenceNotWritten || s.creates != presenceCASReads || s.updates != 0 {
			t.Errorf("outcome=%v creates=%d updates=%d, want presenceNotWritten after %d creates", outcome, s.creates, s.updates, presenceCASReads)
		}
	})
	t.Run("a newer epoch yields without a write", func(t *testing.T) {
		s := &conflictStore{epoch: ep + 1}
		_, outcome, err := casPresence(s, "k", []byte("{}"), ep, 7)
		if outcome != presenceNewer || err != nil || s.updates != 1 || s.reads != 1 {
			t.Errorf("outcome=%v err=%v updates=%d reads=%d, want presenceNewer after one update and one read",
				outcome, err, s.updates, s.reads)
		}
	})
	t.Run("a bus failure stops at once", func(t *testing.T) {
		s := &conflictStore{epoch: ep - 1, fail: errors.New("no responders")}
		_, outcome, err := casPresence(s, "k", []byte("{}"), ep, 7)
		if outcome != presenceNotWritten || err == nil || s.updates != 1 || s.reads != 0 {
			t.Errorf("outcome=%v err=%v updates=%d reads=%d, want one failed update", outcome, err, s.updates, s.reads)
		}
	})
}
