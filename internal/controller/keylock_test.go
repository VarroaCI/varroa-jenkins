package controller

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
	l := newKeyLock()
	if err := l.lock(context.Background(), "ns/a"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.lock(context.Background(), "ns/a"); err != nil {
			t.Errorf("second lock: %v", err)
			return
		}
		got.Store(true)
		l.unlock("ns/a")
	}()
	time.Sleep(30 * time.Millisecond)
	if got.Load() {
		t.Fatal("the second caller took the lock while the first held it")
	}
	l.unlock("ns/a")
	<-done
	if !got.Load() {
		t.Fatal("the second caller did not take the lock after the unlock")
	}
}

// TestKeyLock_DifferentKeysRunTogether pins that the locks of two keys are
// independent.
func TestKeyLock_DifferentKeysRunTogether(t *testing.T) {
	l := newKeyLock()
	if err := l.lock(context.Background(), "ns/a"); err != nil {
		t.Fatalf("lock a: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.lock(ctx, "ns/b"); err != nil {
		t.Fatalf("lock b while a is held: %v", err)
	}
	l.unlock("ns/b")
	l.unlock("ns/a")
}

// TestKeyLock_ContextTimeout pins that a waiter gives up when its context
// ends, and that the holder still holds the lock.
func TestKeyLock_ContextTimeout(t *testing.T) {
	l := newKeyLock()
	if err := l.lock(context.Background(), "ns/a"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.lock(ctx, "ns/a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock err = %v, want DeadlineExceeded", err)
	}
	if n := l.size(); n != 1 {
		t.Fatalf("entries = %d, want 1 for the holder", n)
	}
	l.unlock("ns/a")
}

// TestKeyLock_UnlockRemovesTheEntry pins that the map holds no entry for a
// key that nobody holds or waits for.
func TestKeyLock_UnlockRemovesTheEntry(t *testing.T) {
	l := newKeyLock()
	for i := 0; i < 3; i++ {
		if err := l.lock(context.Background(), "ns/a"); err != nil {
			t.Fatalf("lock: %v", err)
		}
		l.unlock("ns/a")
	}
	if n := l.size(); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}
