package mite

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Bounds and timeouts for the run-ownership cache (design.md D4).
const (
	defaultOwnerCacheSize = 50000
	ownerMatchTTL         = 30 * time.Minute
	ownerRefuseTTL        = 60 * time.Second
	ownerLookupTimeout    = 5 * time.Second

	// ownerLookupPause is how long the cache sends no ledger request
	// after a request timed out. A timed-out request holds the read loop
	// of its stream for ownerLookupTimeout. Without the pause, each check
	// in an outage adds one such stall.
	ownerLookupPause = 30 * time.Second

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

// errOwnerReadOvertaken is the error of a ledger read that a change to
// its run's entry overtook: an invalidation, a relayed grant, or the
// stored answer of another read. The answer can predate that change, so
// it decides nothing and the cache does not keep it.
var errOwnerReadOvertaken = errors.New("the cached owner of the run changed during the ledger read")

// errOwnerLookupPaused is the error of a check that sends no ledger
// request, because a request timed out less than ownerLookupPause ago.
var errOwnerLookupPaused = errors.New("owner lookups pause after a timed-out ledger request")

// ownerReads counts the ledger reads in flight for one run and the
// changes to the run's entry since the first of them began. A write and
// an invalidation are changes. The cache holds it only while a read is
// in flight.
type ownerReads struct {
	inFlight int
	changes  uint64
}

// ownerCache is the gateway's bounded, least-recently-used cache of run
// ownership decisions. A match expires after 30 minutes; a refusal
// expires after 60 seconds, so a legitimate ownership transfer converges
// within one minute. The recorder's owner move signal drops a moved run's
// entry at once (WatchOwnerMoves); these lifetimes bound a replica that
// misses the signal. Concurrent lookups for the same run_id coalesce
// into one request-reply call.
type ownerCache struct {
	mu      sync.Mutex
	maxSize int
	entries map[string]*list.Element
	order   *list.List // front = most recently used
	// reads holds the ledger reads in flight for each run. mu guards it.
	reads map[string]*ownerReads
	// pausedUntil ends the pause that a timed-out request starts. No
	// read begins before it. mu guards it.
	pausedUntil time.Time

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
		reads:    make(map[string]*ownerReads),
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
	c.setLocked(runID, entry)
}

// setUnlessChanged stores entry like set, unless the entry of runID
// changed after the ledger read that produced entry began. began is the
// count that beginRead returned for that read. It answers whether it
// stored entry. A stored entry is itself a change, so it overtakes each
// other read of the run in flight.
func (c *ownerCache) setUnlessChanged(runID string, began uint64, entry ownerCacheEntry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.reads[runID]; r != nil && r.changes != began {
		return false
	}
	c.setLocked(runID, entry)
	return true
}

// beginRead records one ledger read in flight for runID. It returns the
// change count that setUnlessChanged compares. Every call needs one
// endRead.
func (c *ownerCache) beginRead(runID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.reads[runID]
	if r == nil {
		r = &ownerReads{}
		c.reads[runID] = r
	}
	r.inFlight++
	return r.changes
}

// endRead ends one ledger read that beginRead recorded. The record of the
// run goes with its last read, so the map holds only runs with a read in
// flight.
func (c *ownerCache) endRead(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.reads[runID]
	if r == nil {
		return
	}
	if r.inFlight--; r.inFlight <= 0 {
		delete(c.reads, runID)
	}
}

// noteChangeLocked counts one change to runID's entry for each read of the
// run in flight. The caller holds c.mu.
func (c *ownerCache) noteChangeLocked(runID string) {
	if r := c.reads[runID]; r != nil {
		r.changes++
	}
}

// lookupsPaused reports whether a timed-out request paused the ledger
// requests at now.
func (c *ownerCache) lookupsPaused(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.pausedUntil)
}

// pauseLookups starts the pause after a request that timed out at now.
func (c *ownerCache) pauseLookups(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pausedUntil = now.Add(ownerLookupPause)
}

// resumeLookups ends a pause. A request that began before the pause and
// got a reply shows that the ledger answers again.
func (c *ownerCache) resumeLookups() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pausedUntil = time.Time{}
}

// ownerRequestTimedOut reports whether err is the end of the request's
// own wait. Only such a failure holds the caller for ownerLookupTimeout.
// A request that no responder takes fails at once.
func ownerRequestTimedOut(err error) bool {
	return errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// setLocked is set for a caller that holds c.mu. Each write is a change
// that overtakes the reads of the run in flight.
func (c *ownerCache) setLocked(runID string, entry ownerCacheEntry) {
	c.noteChangeLocked(runID)
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

// invalidate drops runID's cached decision, its refusal marks, and
// refusedUntil, so the next check reads the ledger again. A resume or a
// takeover moves a run's owner with no claim that this replica relays,
// so observeGrant never replaces the entry for such a run. A ledger read
// for runID that is in flight can answer the owner from before the move.
// invalidate therefore also makes each such read fail with
// errOwnerReadOvertaken, so its answer is neither cached nor used. A run
// with no entry and no read in flight is left as it is.
func (c *ownerCache) invalidate(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noteChangeLocked(runID)
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
// A ledger read of the run in flight can answer the owner from before the
// claim, so the grant overtakes it and the read stores nothing.
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
// ledger's tuple matches this caller, per design.md D4's table. The call
// is readOwner, so a waiter that joins after a change to the run's entry
// gets the answer of a read that began after that change.
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

	entry, err := c.readOwner(ctx, requester, runID, cluster, namespace, cell, instance)

	c.inflightMu.Lock()
	waiters = c.inflight[runID]
	delete(c.inflight, runID)
	c.inflightMu.Unlock()
	for _, w := range waiters {
		w <- ownerLookupResult{entry: entry, err: err}
	}
	return entry, err
}

// readOwner reads the run's owner through doLookup. When a change to the
// run's entry overtakes the read, it reads once more: the second read
// begins after that change, so its answer is fresh. A second overtaken
// read fails with errOwnerReadOvertaken. Only a read that got a reply can
// be overtaken, so the two reads hold the caller for less than twice
// ownerLookupTimeout.
func (c *ownerCache) readOwner(ctx context.Context, requester ledgerRequester, runID, cluster, namespace, cell, instance string) (ownerCacheEntry, error) {
	entry, err := c.doLookup(ctx, requester, runID, cluster, namespace, cell, instance)
	if errors.Is(err, errOwnerReadOvertaken) {
		entry, err = c.doLookup(ctx, requester, runID, cluster, namespace, cell, instance)
	}
	return entry, err
}

// doLookup makes one request-reply call and caches the answer. It never
// caches a lookup failure: a timeout, an unreachable recorder, an
// unparsable reply, or a recorder-reported database error must be
// retried, not remembered as a refusal. A change to the run's entry while
// the call is in flight is a failure too (errOwnerReadOvertaken): the
// answer can predate that change.
//
// A request that times out pauses the requests of the whole cache for
// ownerLookupPause, and a call in the pause fails at once with
// errOwnerLookupPaused. A recorder or bus outage holds every request, so
// one stall stands for all the checks of the pause. The end of the
// caller's own context starts no pause. A reply ends the pause.
func (c *ownerCache) doLookup(ctx context.Context, requester ledgerRequester, runID, cluster, namespace, cell, instance string) (ownerCacheEntry, error) {
	body, err := json.Marshal(ownerLookupRequest{RunID: runID})
	if err != nil {
		return ownerCacheEntry{}, err
	}
	if c.lookupsPaused(time.Now()) {
		return ownerCacheEntry{}, errOwnerLookupPaused
	}
	began := c.beginRead(runID)
	defer c.endRead(runID)
	msg, err := requester.RequestWithContext(ctx, bus.LedgerOwnerSubject, body, ownerLookupTimeout)
	if err != nil {
		if ctx.Err() == nil && ownerRequestTimedOut(err) {
			c.pauseLookups(time.Now())
		}
		return ownerCacheEntry{}, err
	}
	c.resumeLookups()
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
	var entry ownerCacheEntry
	if !resp.Found {
		entry = refusalEntry(prev, hadPrev, false, ownerTuple{}, caller, now)
	} else {
		tuple := ownerTuple{
			cluster: resp.Cluster, namespace: resp.Namespace, cell: resp.Cell, instance: resp.Instance,
			status: resp.Status, anchorUnixMs: resp.AnchorUnixMs,
		}
		matched := tuple.cluster == cluster && tuple.namespace == namespace && tuple.cell == cell && tuple.instance == instance
		entry = ownerCacheEntry{found: true, tuple: tuple, expires: now.Add(ownerMatchTTL)}
		if !matched {
			entry = refusalEntry(prev, hadPrev, true, tuple, caller, now)
		}
	}
	if !c.setUnlessChanged(runID, began, entry) {
		return ownerCacheEntry{}, errOwnerReadOvertaken
	}
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
// no longer holds. The recorder's owner move signal drops such an entry
// (onOwnerMove); the re-read covers a replica that missed the signal.
// The lookup marks its own caller and never shortens a match. One set of
// marks therefore costs at most ownerRefusedMax lookups for new marks and
// ownerRefusedRereads lookups for aged marks, and a lookup in flight is
// shared. The lookup runs on the caller's goroutine, the stream read loop
// for a chunk. It lasts at most ownerLookupTimeout, and less than twice
// that when a change to the entry overtakes its first read.
//
// A failed lookup writes no entry and answers ownerUnknown, never
// ownerForeign. In the pause after a timed-out request, a check that needs
// a lookup sends no request and answers ownerUnknown at once. A cached
// match still admits the caller in the pause.
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
// the whole ownerMatchTTL with no new request. A change to the entry that
// overtakes the request costs one more request (readOwner).
//
// fresh is false when no request answered: a failed request, or a call
// in the pause after a timed-out request, which sends none. The decision
// is then ownerMatched when the entry that the cache holds after the
// attempt matches the caller, the answer the chunk path gives in the same
// state. With no entry, or with an entry for another owner, it is
// ownerUnknown. A failed request never answers ownerForeign. A check whose
// two reads are both overtaken is the exception. It answers ownerUnknown
// even when the cache holds a match, because the entry changed during
// each read and the check does not trust it.
func (h *BusHandler) checkOwnerForCompletion(ctx context.Context, cluster, namespace, cell, instance, runID string) (decision ownerDecision, tuple ownerTuple, fresh bool) {
	if h.ownerCache == nil {
		return ownerUnknown, ownerTuple{}, false
	}
	entry, err := h.ownerCache.readOwner(ctx, h.ledger, runID, cluster, namespace, cell, instance)
	if err == nil {
		return decideOwner(entry, cluster, namespace, cell, instance), entry.tuple, true
	}
	if errors.Is(err, errOwnerReadOvertaken) {
		return ownerUnknown, ownerTuple{}, false
	}
	if cached, ok := h.ownerCache.get(runID); ok && decideOwner(cached, cluster, namespace, cell, instance) == ownerMatched {
		return ownerMatched, cached.tuple, false
	}
	return ownerUnknown, ownerTuple{}, false
}
