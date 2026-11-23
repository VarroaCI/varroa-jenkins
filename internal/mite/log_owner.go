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

	// ownerRefusedMax is the most callers that one entry marks as
	// refused. ownerRefusedRereads is the most reads of an aged mark that
	// one entry allows. A mark is aged ownerRefusedRereadAfter after the
	// read that set it.
	ownerRefusedMax         = 4
	ownerRefusedRereads     = 3
	ownerRefusedRereadAfter = 5 * time.Second
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
//
// refused maps the ownerCallerKey of each caller that a ledger read behind
// this entry refused to the time of that read. It holds at most
// ownerRefusedMax callers. rereads counts the reads of an aged mark. Both
// live until refusedUntil, ownerRefuseTTL after the last read that marked
// a caller, and they do not change expires. A match entry can therefore
// hold marks and keep its match lifetime. A fresh answer that names the
// same owner tuple keeps live marks. A match read, a grant, a miss, a new
// owner tuple, and the end of refusedUntil start them again. The map is
// never modified after the entry is stored.
type ownerCacheEntry struct {
	found        bool
	tuple        ownerTuple
	expires      time.Time
	refused      map[string]time.Time
	rereads      int
	refusedUntil time.Time
}

// needsRead reports whether a cached foreign decision for the caller key
// must read the ledger before it refuses the caller. A move with no claim
// can leave the entry stale. A caller with no mark reads while the set has
// room. A marked caller reads again only when its mark is aged and the
// entry still allows a re-read, because the ledger can name that caller
// after the read that refused it. Every other foreign decision is final.
func (e ownerCacheEntry) needsRead(key string, now time.Time) bool {
	if !now.Before(e.refusedUntil) {
		return true
	}
	at, marked := e.refused[key]
	if !marked {
		return len(e.refused) < ownerRefusedMax
	}
	return e.rereads < ownerRefusedRereads && now.Sub(at) >= ownerRefusedRereadAfter
}

// refusalEntry returns the entry for a fresh answer that names tuple and
// refuses caller. found is false for a missing intent row. When prev names
// the same tuple, the entry keeps the later expiry of prev, so a foreign
// caller's read never shortens the owner's match. It also keeps the live
// marks and the re-read count of prev. A caller that prev marked counts
// one re-read. A new caller gets a mark only while the set holds fewer
// than ownerRefusedMax callers. A caller past that bound gets prev back
// unchanged, so its read does not extend the marks.
func refusalEntry(prev ownerCacheEntry, hadPrev, found bool, tuple ownerTuple, caller string, now time.Time) ownerCacheEntry {
	entry := ownerCacheEntry{
		found: found, tuple: tuple, expires: now.Add(ownerRefuseTTL),
		refused: make(map[string]time.Time, ownerRefusedMax), refusedUntil: now.Add(ownerRefuseTTL),
	}
	if hadPrev && prev.found == found && prev.tuple == tuple {
		if prev.expires.After(entry.expires) {
			entry.expires = prev.expires
		}
		if now.Before(prev.refusedUntil) {
			for k, at := range prev.refused {
				entry.refused[k] = at
			}
			entry.rereads = prev.rereads
		}
	}
	if _, marked := entry.refused[caller]; marked {
		entry.rereads++
	} else if len(entry.refused) >= ownerRefusedMax {
		return prev
	}
	entry.refused[caller] = now
	return entry
}

// ownerCallerKey names the caller of an owner check by the stream
// identity that the gateway authenticated. It holds no cell instance: the
// sender stamps the instance on each chunk, so a sender that changes it
// must not get a new mark, and a new ledger read, for each value.
func ownerCallerKey(cluster, namespace, cell string) string {
	return cluster + "/" + namespace + "/" + cell
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

// peek returns the cached decision for runID like get, but it does not
// move the entry to the front of the LRU order. The idle sweep reads the
// cache through peek, so its rounds never keep an idle run's entry in
// the cache ahead of a live run's entry.
func (c *ownerCache) peek(runID string) (ownerCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[runID]
	if !ok {
		return ownerCacheEntry{}, false
	}
	entry := el.Value.(*ownerCacheItem).entry
	if time.Now().After(entry.expires) {
		return ownerCacheEntry{}, false
	}
	return entry, true
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

// invalidate drops runID's cached decision, so the next check reads the
// ledger again. A resume moves a run's owner with no claim behind it,
// so observeGrant never replaces the entry for such a run.
func (c *ownerCache) invalidate(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[runID]
	if !ok {
		return
	}
	c.order.Remove(el)
	delete(c.entries, runID)
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

	caller := ownerCallerKey(cluster, namespace, cell)
	prev, hadPrev := c.peek(runID)
	now := time.Now()
	if !resp.Found {
		entry := refusalEntry(prev, hadPrev, false, ownerTuple{}, caller, now)
		c.set(runID, entry)
		return entry, nil
	}

	tuple := ownerTuple{
		cluster: resp.Cluster, namespace: resp.Namespace, cell: resp.Cell, instance: resp.Instance,
		status: resp.Status, anchorUnixMs: resp.AnchorUnixMs,
	}
	matched := tuple.cluster == cluster && tuple.namespace == namespace && tuple.cell == cell && tuple.instance == instance
	entry := ownerCacheEntry{found: true, tuple: tuple, expires: now.Add(ownerMatchTTL)}
	if !matched {
		entry = refusalEntry(prev, hadPrev, true, tuple, caller, now)
	}
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
//
// A cached match admits the caller. A cached foreign decision takes the
// coalesced lookup first when ownerCacheEntry.needsRead says so: a resume
// or a takeover moves the owner with no claim, so a match of the old owner
// or a refusal read for another caller can name an owner that the ledger
// no longer holds. The lookup marks its own caller and never shortens a
// match. One set of marks therefore costs at most ownerRefusedMax lookups
// for new marks and ownerRefusedRereads lookups for aged marks, and a
// lookup in flight is shared. The lookup runs on the caller's goroutine,
// the stream read loop for a chunk, and lasts at most ownerLookupTimeout.
// A failed lookup writes no entry and answers ownerUnknown, never
// ownerForeign.
func (h *BusHandler) checkOwner(ctx context.Context, cluster, namespace, cell, instance, runID string) (ownerDecision, ownerTuple) {
	if h.ownerCache == nil {
		// No cache configured (e.g. a hand-built BusHandler in a test
		// that never calls NewBusHandler or SetLogStore). Fail closed:
		// the mTLS identity alone never admits a chunk.
		return ownerUnknown, ownerTuple{}
	}
	if entry, ok := h.ownerCache.get(runID); ok {
		decision := decideOwner(entry, cluster, namespace, cell, instance)
		if decision == ownerMatched || !entry.needsRead(ownerCallerKey(cluster, namespace, cell), time.Now()) {
			return decision, entry.tuple
		}
	}
	entry, err := h.ownerCache.lookup(ctx, h.ledger, runID, cluster, namespace, cell, instance)
	if err != nil {
		return ownerUnknown, ownerTuple{}
	}
	return decideOwner(entry, cluster, namespace, cell, instance), entry.tuple
}

// checkOwnerForCompletion answers whether the caller owns runID, for a
// COMPLETED event. The answer comes from a ledger request that this call
// sends. It never comes from a cached entry, and it never joins a lookup
// already in flight, which can predate the event. A move with no claim,
// such as a resume, leaves the old owner's cached match in place for up
// to ownerMatchTTL, and the old owner is the sender most likely to send a
// stale completion. The answer replaces the cached entry with the
// lifetime doLookup gives it, so the chunk path keeps a fresh match for
// the whole ownerMatchTTL with no new request.
//
// fresh is false when the request failed. The decision is then
// ownerMatched when a cached entry matches the caller, the answer the
// chunk path gives in the same state, and ownerUnknown otherwise. A
// failed request never answers ownerForeign. A call costs one ledger
// request, bounded by ownerLookupTimeout.
func (h *BusHandler) checkOwnerForCompletion(ctx context.Context, cluster, namespace, cell, instance, runID string) (decision ownerDecision, tuple ownerTuple, fresh bool) {
	if h.ownerCache == nil {
		return ownerUnknown, ownerTuple{}, false
	}
	cached, hasCached := h.ownerCache.get(runID)
	entry, err := h.ownerCache.doLookup(ctx, h.ledger, runID, cluster, namespace, cell, instance)
	if err != nil {
		if hasCached && decideOwner(cached, cluster, namespace, cell, instance) == ownerMatched {
			return ownerMatched, cached.tuple, false
		}
		return ownerUnknown, ownerTuple{}, false
	}
	return decideOwner(entry, cluster, namespace, cell, instance), entry.tuple, true
}
