package apikey

import (
	"sync"
	"time"
)

type lastUsedTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	flushed  map[string]time.Time
}

func newLastUsedTracker() *lastUsedTracker {
	return &lastUsedTracker{
		lastSeen: make(map[string]time.Time),
		flushed:  make(map[string]time.Time),
	}
}

func (t *lastUsedTracker) record(prefix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastSeen[prefix] = time.Now()
}

func (t *lastUsedTracker) forget(prefix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.lastSeen, prefix)
	delete(t.flushed, prefix)
}

func (t *lastUsedTracker) runFlushLoop(interval time.Duration, flush func(string, time.Time)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		t.flushDirty(flush)
	}
}

func (t *lastUsedTracker) flushDirty(flush func(string, time.Time)) {
	t.mu.Lock()
	entries := make(map[string]time.Time)
	for prefix, seen := range t.lastSeen {
		flushed := t.flushed[prefix]
		if seen.After(flushed) {
			entries[prefix] = seen
		}
	}
	t.mu.Unlock()

	for prefix, seen := range entries {
		flush(prefix, seen)
		t.mu.Lock()
		t.flushed[prefix] = seen
		t.mu.Unlock()
	}
}
