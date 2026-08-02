package mite

import (
	"sync"
	"time"
)

// ConsumedTokenStore records single-use bootstrap tokens by jti so a token cannot
// be replayed. Implementations must be safe for concurrent use.
type ConsumedTokenStore interface {
	// Consume atomically records jti with its expiry. It returns false if jti was
	// already present (i.e. the token is being replayed).
	Consume(jti string, expiry time.Time) bool
}

// inMemoryConsumedStore is the default ConsumedTokenStore. It is per-process, so
// single-use is enforced within one gateway replica; cross-replica single-use would
// require a shared backend injected via Server.SetConsumedTokenStore. The residual
// cross-replica replay window is bounded by the short bootstrap-token TTL, the random
// jti, and mTLS certificate pinning.
type inMemoryConsumedStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
	done chan struct{}
}

func newInMemoryConsumedStore() *inMemoryConsumedStore {
	s := &inMemoryConsumedStore{
		seen: make(map[string]time.Time),
		done: make(chan struct{}),
	}
	go s.sweep()
	return s
}

// Consume records jti under the lock, returning false if it was already seen.
func (s *inMemoryConsumedStore) Consume(jti string, expiry time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[jti]; ok {
		return false
	}
	s.seen[jti] = expiry
	return true
}

// sweep evicts expired entries every minute so the map stays bounded by
// (registration rate × TTL).
func (s *inMemoryConsumedStore) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.mu.Lock()
			for jti, exp := range s.seen {
				if now.After(exp) {
					delete(s.seen, jti)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *inMemoryConsumedStore) stop() { close(s.done) }
