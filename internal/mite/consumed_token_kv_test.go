package mite

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// fakeKV is an in-process stand-in for the shared consumed-tokens bucket.
type fakeKV struct {
	mu      sync.Mutex
	keys    map[string]struct{}
	calls   int
	failErr error // when set, Create returns it without recording
}

func newFakeKV() *fakeKV { return &fakeKV{keys: map[string]struct{}{}} }

func (f *fakeKV) Create(key string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failErr != nil {
		return f.failErr
	}
	if _, ok := f.keys[key]; ok {
		return bus.ErrKVKeyExists
	}
	f.keys[key] = struct{}{}
	return nil
}

func (f *fakeKV) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestKVConsumedStoreFirstUse(t *testing.T) {
	kv := newFakeKV()
	s := newKVConsumedStore(kv, nil)
	defer s.stop()

	// Constructing the store performs no KV writes: only Consume touches the
	// bucket, and Register only calls Consume after full token validation, so
	// an invalid token can never burn a jti in the shared bucket.
	if kv.callCount() != 0 {
		t.Fatalf("store construction wrote to KV: %d calls", kv.callCount())
	}

	exp := time.Now().Add(15 * time.Minute)
	if !s.Consume("jti-1", exp) {
		t.Fatal("first Consume rejected")
	}
	if _, ok := kv.keys["jti-1"]; !ok {
		t.Fatal("jti not recorded in KV")
	}
}

func TestKVConsumedStoreSameReplicaReplayShortCircuits(t *testing.T) {
	kv := newFakeKV()
	s := newKVConsumedStore(kv, nil)
	defer s.stop()

	exp := time.Now().Add(15 * time.Minute)
	if !s.Consume("jti-1", exp) {
		t.Fatal("first Consume rejected")
	}
	before := kv.callCount()
	if s.Consume("jti-1", exp) {
		t.Fatal("same-replica replay accepted")
	}
	if kv.callCount() != before {
		t.Fatalf("replay hit KV: %d extra calls", kv.callCount()-before)
	}
}

func TestKVConsumedStoreCrossReplicaReplayRejected(t *testing.T) {
	kv := newFakeKV()
	a := newKVConsumedStore(kv, nil)
	defer a.stop()
	b := newKVConsumedStore(kv, nil)
	defer b.stop()

	exp := time.Now().Add(15 * time.Minute)
	if !a.Consume("jti-1", exp) {
		t.Fatal("replica A first Consume rejected")
	}
	if b.Consume("jti-1", exp) {
		t.Fatal("replica B accepted a jti consumed by replica A")
	}
}

func TestKVConsumedStoreKVErrorFailsOpen(t *testing.T) {
	kv := newFakeKV()
	kv.failErr = errors.New("nats: connection closed")
	s := newKVConsumedStore(kv, nil)
	defer s.stop()

	exp := time.Now().Add(15 * time.Minute)
	if !s.Consume("jti-1", exp) {
		t.Fatal("KV error blocked registration (want fail-open)")
	}
	// The in-memory layer still enforces same-replica single-use.
	if s.Consume("jti-1", exp) {
		t.Fatal("same-replica replay accepted during KV outage")
	}
}

func TestKVConsumedStoreCrossReplicaEmbeddedNATS(t *testing.T) {
	s := startNATS(t)

	busConn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	// 1m TTL for test speed only — production uses 16m (>= the 15m token TTL).
	kv, err := busConn.EnsureKV(bus.KVConsumedTokensBucket, time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	a := NewKVConsumedStore(kv, nil)
	defer a.(interface{ stop() }).stop()
	b := NewKVConsumedStore(kv, nil)
	defer b.(interface{ stop() }).stop()

	exp := time.Now().Add(15 * time.Minute)
	if !a.Consume("jti-shared", exp) {
		t.Fatal("replica A first Consume rejected")
	}
	if b.Consume("jti-shared", exp) {
		t.Fatal("replica B accepted a jti consumed by replica A over the shared bucket")
	}
	if !b.Consume("jti-other", exp) {
		t.Fatal("replica B rejected an unrelated jti")
	}
}
