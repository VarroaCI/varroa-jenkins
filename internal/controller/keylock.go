package controller

import (
	"context"
	"sync"
)

// keyLock is a set of mutexes, one for each key. A key holds an entry only
// while a caller holds or waits for its lock, so the map does not grow with
// every key that it ever saw.
type keyLock struct {
	mu      sync.Mutex
	entries map[string]*keyLockEntry
}

type keyLockEntry struct {
	// sem has room for one token. The holder of the token holds the lock.
	sem  chan struct{}
	refs int
}

func newKeyLock() *keyLock {
	return &keyLock{entries: make(map[string]*keyLockEntry)}
}

// lock waits for the lock of key until ctx ends. It returns ctx.Err() when
// ctx ends first, and the caller then does not hold the lock. On success the
// caller must call unlock with the same key.
func (l *keyLock) lock(ctx context.Context, key string) error {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		e = &keyLockEntry{sem: make(chan struct{}, 1)}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()

	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		l.release(key, e)
		return ctx.Err()
	}
}

// unlock releases the lock of key that the caller holds.
func (l *keyLock) unlock(key string) {
	l.mu.Lock()
	e := l.entries[key]
	l.mu.Unlock()
	if e == nil {
		panic("keyLock: unlock of a key that is not locked: " + key)
	}
	<-e.sem
	l.release(key, e)
}

// release drops one reference to e and removes the entry with the last one.
func (l *keyLock) release(key string, e *keyLockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(l.entries, key)
	}
}

// size returns the number of keys with an entry.
func (l *keyLock) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
