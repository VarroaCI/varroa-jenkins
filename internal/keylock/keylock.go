// Package keylock provides a set of mutexes, one for each key, whose waits
// end with a context, and a non-blocking try for each key. An entry exists
// only while a caller holds or waits for its key, so the set does not grow
// with every key it ever saw.
package keylock

import (
	"context"
	"sync"
)

// KeyLock is a set of mutexes, one for each key. The zero value is not usable;
// create one with New.
type KeyLock struct {
	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	// sem has room for one token. The holder of the token holds the lock.
	sem  chan struct{}
	refs int
}

// New returns an empty KeyLock.
func New() *KeyLock {
	return &KeyLock{entries: make(map[string]*entry)}
}

// Lock waits for the lock of key until ctx ends. It returns ctx.Err() when
// ctx ends first, and the caller then does not hold the lock. On success the
// caller must call Unlock with the same key.
func (l *KeyLock) Lock(ctx context.Context, key string) error {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		e = &entry{sem: make(chan struct{}, 1)}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()

	// A free lock wins even when ctx has already ended. The caller then gets
	// the lock and runs its own deadline check.
	select {
	case e.sem <- struct{}{}:
		return nil
	default:
	}

	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		l.release(key, e)
		return ctx.Err()
	}
}

// TryLock takes the lock of key without waiting. It returns true when the
// caller gets the lock, and the caller must then call Unlock with the same
// key. It returns false when another caller holds the lock. The caller then
// holds nothing, and an entry that no one else uses is removed.
func (l *KeyLock) TryLock(key string) bool {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		e = &entry{sem: make(chan struct{}, 1)}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()

	select {
	case e.sem <- struct{}{}:
		return true
	default:
		l.release(key, e)
		return false
	}
}

// Unlock releases the lock of key that the caller holds.
func (l *KeyLock) Unlock(key string) {
	l.mu.Lock()
	e := l.entries[key]
	l.mu.Unlock()
	if e == nil {
		panic("keylock: unlock of a key that is not locked: " + key)
	}
	<-e.sem
	l.release(key, e)
}

// release drops one reference to e and removes the entry with the last one.
func (l *KeyLock) release(key string, e *entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(l.entries, key)
	}
}

// Size returns the number of keys with an entry.
func (l *KeyLock) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
