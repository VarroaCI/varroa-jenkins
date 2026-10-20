package keylock

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestKeyLock_SameKeyExcludes pins that a second caller of the same key
// waits until the first caller unlocks.
func TestKeyLock_SameKeyExcludes(t *testing.T) {
	l := New()
	if err := l.Lock(context.Background(), "ns/a"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.Lock(context.Background(), "ns/a"); err != nil {
			t.Errorf("second lock: %v", err)
			return
		}
		got.Store(true)
		l.Unlock("ns/a")
	}()
	time.Sleep(30 * time.Millisecond)
	if got.Load() {
		t.Fatal("the second caller took the lock while the first held it")
	}
	l.Unlock("ns/a")
	<-done
	if !got.Load() {
		t.Fatal("the second caller did not take the lock after the unlock")
	}
}

// TestKeyLock_DifferentKeysRunTogether pins that the locks of two keys are
// independent.
func TestKeyLock_DifferentKeysRunTogether(t *testing.T) {
	l := New()
	if err := l.Lock(context.Background(), "ns/a"); err != nil {
		t.Fatalf("lock a: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.Lock(ctx, "ns/b"); err != nil {
		t.Fatalf("lock b while a is held: %v", err)
	}
	l.Unlock("ns/b")
	l.Unlock("ns/a")
}

// TestKeyLock_ContextTimeout pins that a waiter gives up when its context
// ends, and that the holder still holds the lock.
func TestKeyLock_ContextTimeout(t *testing.T) {
	l := New()
	if err := l.Lock(context.Background(), "ns/a"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Lock(ctx, "ns/a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock err = %v, want DeadlineExceeded", err)
	}
	if n := l.Size(); n != 1 {
		t.Fatalf("entries = %d, want 1 for the holder", n)
	}
	l.Unlock("ns/a")
}

// TestKeyLock_UnlockRemovesTheEntry pins that the map holds no entry for a
// key that nobody holds or waits for.
func TestKeyLock_UnlockRemovesTheEntry(t *testing.T) {
	l := New()
	for i := 0; i < 3; i++ {
		if err := l.Lock(context.Background(), "ns/a"); err != nil {
			t.Fatalf("lock: %v", err)
		}
		l.Unlock("ns/a")
	}
	if n := l.Size(); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}
