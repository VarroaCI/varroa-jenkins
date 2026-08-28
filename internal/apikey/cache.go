package apikey

import (
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

type verifiedCache struct {
	mu   sync.RWMutex
	ttl  time.Duration
	data map[string]*cacheEntry
}

type cacheEntry struct {
	// secretHash is the stored Hash of the key's secret. A cache hit must
	// re-verify the presented secret against it: the cache is keyed by the
	// public prefix, so without this check any well-formed token sharing a
	// cached prefix would authenticate while the entry is live.
	secretHash string
	claims     *auth.Claims
	expiresAt  time.Time
}

func newVerifiedCache(ttl time.Duration) *verifiedCache {
	return &verifiedCache{
		ttl:  ttl,
		data: make(map[string]*cacheEntry),
	}
}

func (c *verifiedCache) get(prefix, secret string) *auth.Claims {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.data[prefix]
	if !ok {
		return nil
	}
	// Treat an expired entry as a miss. We must not mutate the map here:
	// delete under the read lock would be a concurrent map write. The entry
	// is reclaimed lazily by the next set() or evict() for this prefix.
	if time.Now().After(entry.expiresAt) {
		return nil
	}
	// A wrong secret is a miss, not a rejection: the caller falls through to
	// the full verify path, which fails against the stored hash.
	if !Verify(secret, entry.secretHash) {
		return nil
	}
	return entry.claims
}

func (c *verifiedCache) set(prefix, secretHash string, claims *auth.Claims) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[prefix] = &cacheEntry{
		secretHash: secretHash,
		claims:     claims,
		expiresAt:  time.Now().Add(c.ttl),
	}
}

func (c *verifiedCache) evict(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, prefix)
}
