package mite

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Bounds and timeouts for the run-ownership cache (design.md D4).
const (
	defaultOwnerCacheSize = 50000
	ownerMatchTTL         = 30 * time.Minute
	ownerRefuseTTL        = 60 * time.Second
	ownerLookupTimeout    = 5 * time.Second
)

// ownerTuple is the ledger's answer to "who owns this run": the
// authenticated (cluster, namespace, cell) triple plus the cell instance
// the ledger recorded at claim time, and the run's current status and
// finalize-wait anchor.
type ownerTuple struct {
	cluster, namespace, cell, instance string
	status                             string
	anchorUnixMs                       int64
}

// ownerCacheEntry is one decision the cache holds for a run_id. found
// false means the ledger carries no intent row for the run — a policy
// failure, cached the same as a foreign owner.
type ownerCacheEntry struct {
	found   bool
	tuple   ownerTuple
	expires time.Time
}

// ownerCacheItem is the container list.Element.Value holds, so the LRU
// order and the map lookup share one allocation per entry.
type ownerCacheItem struct {
	runID string
	entry ownerCacheEntry
}

// ownerLookupRequest is the request body of a varroa.ledger.owner call.
type ownerLookupRequest struct {
	RunID string `json:"runId"`
}

// ownerLookupResponse is the reply body of a varroa.ledger.owner call.
// Field names mirror internal/recordstore's ownerLookupResponse exactly.
// Error is non-empty only when the recorder's lookup itself failed (a
// database error), never for a genuine miss.
type ownerLookupResponse struct {
	Found        bool   `json:"found"`
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	Cell         string `json:"cell"`
	Instance     string `json:"instance"`
	Status       string `json:"status"`
	AnchorUnixMs int64  `json:"anchorUnixMs"`
	Error        string `json:"error"`
}

// ownerLookupResult is what a coalesced lookup delivers to every waiter.
type ownerLookupResult struct {
	entry ownerCacheEntry
	err   error
}

// ownerCache is the gateway's bounded, least-recently-used cache of run
// ownership decisions. A match expires after 30 minutes; a refusal
// expires after 60 seconds, so a legitimate ownership transfer converges
// within one minute. Concurrent lookups for the same run_id coalesce
// into one request-reply call.
type ownerCache struct {
	mu      sync.Mutex
	maxSize int
	entries map[string]*list.Element
	order   *list.List // front = most recently used

	inflightMu sync.Mutex
	inflight   map[string][]chan ownerLookupResult
}

// newOwnerCache returns a cache bounded at maxSize entries. maxSize <= 0
// means defaultOwnerCacheSize.
func newOwnerCache(maxSize int) *ownerCache {
	if maxSize <= 0 {
		maxSize = defaultOwnerCacheSize
	}
	return &ownerCache{
		maxSize:  maxSize,
		entries:  make(map[string]*list.Element),
		order:    list.New(),
		inflight: make(map[string][]chan ownerLookupResult),
	}
}

// get returns the cached decision for runID, or ok=false on a miss or an
// expired entry.
func (c *ownerCache) get(runID string) (ownerCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[runID]
	if !ok {
		return ownerCacheEntry{}, false
	}
	item := el.Value.(*ownerCacheItem)
	if time.Now().After(item.entry.expires) {
		c.order.Remove(el)
		delete(c.entries, runID)
		return ownerCacheEntry{}, false
	}
	c.order.MoveToFront(el)
	return item.entry, true
}

// set stores or replaces the decision for runID, evicting the least
// recently used entry when the cache is at its bound.
func (c *ownerCache) set(runID string, entry ownerCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[runID]; ok {
		el.Value.(*ownerCacheItem).entry = entry
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&ownerCacheItem{runID: runID, entry: entry})
	c.entries[runID] = el
	if c.order.Len() > c.maxSize {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.entries, oldest.Value.(*ownerCacheItem).runID)
		}
	}
}

// observeGrant records a matched owner directly from a relayed granted
// ClaimResponse, at no lookup cost. A later grant for the same run_id
// replaces the entry unconditionally, so an ownership transfer
// invalidates a stale match immediately rather than waiting on its TTL.
func (c *ownerCache) observeGrant(runID, cluster, namespace, cell, instance string) {
	c.set(runID, ownerCacheEntry{
		found:   true,
		tuple:   ownerTuple{cluster: cluster, namespace: namespace, cell: cell, instance: instance},
		expires: time.Now().Add(ownerMatchTTL),
	})
}

// lookup performs the request-reply call to bus.LedgerOwnerSubject,
// coalescing concurrent callers for the same run_id into one call. cluster,
// namespace, cell, and instance name the caller whose admission decision
// this lookup answers: the TTL the entry receives depends on whether the
// ledger's tuple matches this caller, per design.md D4's table.
func (c *ownerCache) lookup(ctx context.Context, requester ledgerRequester, runID, cluster, namespace, cell, instance string) (ownerCacheEntry, error) {
	ch := make(chan ownerLookupResult, 1)
	c.inflightMu.Lock()
	waiters, already := c.inflight[runID]
	c.inflight[runID] = append(waiters, ch)
	c.inflightMu.Unlock()
	if already {
		res := <-ch
		return res.entry, res.err
	}

	entry, err := c.doLookup(ctx, requester, runID, cluster, namespace, cell, instance)

	c.inflightMu.Lock()
	waiters = c.inflight[runID]
	delete(c.inflight, runID)
	c.inflightMu.Unlock()
	for _, w := range waiters {
		w <- ownerLookupResult{entry: entry, err: err}
	}
	return entry, err
}

// doLookup makes one request-reply call and caches the answer. It never
// caches a lookup failure: a timeout, an unreachable recorder, an
// unparsable reply, or a recorder-reported database error must be
// retried, not remembered as a refusal.
func (c *ownerCache) doLookup(ctx context.Context, requester ledgerRequester, runID, cluster, namespace, cell, instance string) (ownerCacheEntry, error) {
	body, err := json.Marshal(ownerLookupRequest{RunID: runID})
	if err != nil {
		return ownerCacheEntry{}, err
	}
	msg, err := requester.RequestWithContext(ctx, bus.LedgerOwnerSubject, body, ownerLookupTimeout)
	if err != nil {
		return ownerCacheEntry{}, err
	}
	var resp ownerLookupResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return ownerCacheEntry{}, err
	}
	if resp.Error != "" {
		return ownerCacheEntry{}, fmt.Errorf("recorder owner lookup failed: %s", resp.Error)
	}

	if !resp.Found {
		entry := ownerCacheEntry{found: false, expires: time.Now().Add(ownerRefuseTTL)}
		c.set(runID, entry)
		return entry, nil
	}

	tuple := ownerTuple{
		cluster: resp.Cluster, namespace: resp.Namespace, cell: resp.Cell, instance: resp.Instance,
		status: resp.Status, anchorUnixMs: resp.AnchorUnixMs,
	}
	matched := tuple.cluster == cluster && tuple.namespace == namespace && tuple.cell == cell && tuple.instance == instance
	ttl := ownerRefuseTTL
	if matched {
		ttl = ownerMatchTTL
	}
	entry := ownerCacheEntry{found: true, tuple: tuple, expires: time.Now().Add(ttl)}
	c.set(runID, entry)
	return entry, nil
}

// ownerDecision is the outcome of comparing a chunk's authenticated
// stream identity against the ledger's owner for its run_id.
type ownerDecision int

const (
	// ownerUnknown means the lookup failed or timed out. The gateway
	// fails closed: it admits nothing on ownerUnknown.
	ownerUnknown ownerDecision = iota
	ownerMatched
	ownerForeign
)

// decide compares entry against the caller's identity.
func decideOwner(entry ownerCacheEntry, cluster, namespace, cell, instance string) ownerDecision {
	if !entry.found {
		return ownerForeign
	}
	if entry.tuple.cluster == cluster && entry.tuple.namespace == namespace &&
		entry.tuple.cell == cell && entry.tuple.instance == instance {
		return ownerMatched
	}
	return ownerForeign
}

// checkOwner answers whether (cluster, namespace, cell, instance) owns
// runID, consulting the cache first and issuing one coalesced lookup on a
// miss. It returns the ledger's tuple alongside the decision so a caller
// can read the run's status and finalize-wait anchor from a match.
func (h *BusHandler) checkOwner(ctx context.Context, cluster, namespace, cell, instance, runID string) (ownerDecision, ownerTuple) {
	if h.ownerCache == nil {
		// No cache configured (e.g. a hand-built BusHandler in a test
		// that never calls NewBusHandler or SetLogStore). Fail closed:
		// the mTLS identity alone never admits a chunk.
		return ownerUnknown, ownerTuple{}
	}
	if entry, ok := h.ownerCache.get(runID); ok {
		return decideOwner(entry, cluster, namespace, cell, instance), entry.tuple
	}
	entry, err := h.ownerCache.lookup(ctx, h.ledger, runID, cluster, namespace, cell, instance)
	if err != nil {
		return ownerUnknown, ownerTuple{}
	}
	return decideOwner(entry, cluster, namespace, cell, instance), entry.tuple
}
