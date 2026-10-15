package sharding

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ShardSet is the thread-safe set of shards that this replica holds. It also
// counts the work that runs on each held shard. A release waits for that work
// before it gives up the lease, so two replicas never write for one shard at
// the same time.
type ShardSet struct {
	mu   sync.Mutex
	held map[int]*shardWork
}

// shardWork counts the work that entered one holding of one shard. A shard
// that is dropped and held again gets a new shardWork.
type shardWork struct {
	mu       sync.Mutex
	inflight int
	// idle is closed while inflight is zero.
	idle chan struct{}
	// cancels holds the cancel of each work context in flight. cancelAll
	// calls them before it returns, so a check of a work context after a
	// release sees the cancel.
	cancels   map[uint64]context.CancelFunc
	next      uint64
	cancelled bool
	// confirmedAt is the start of the last lease write that confirmed this
	// holding. ShardSet.mu guards it. The value must carry a monotonic
	// reading, so that a jump of the wall clock cannot move a revoke.
	confirmedAt time.Time
}

func newShardWork() *shardWork {
	idle := make(chan struct{})
	close(idle)
	return &shardWork{idle: idle, cancels: make(map[uint64]context.CancelFunc)}
}

// enter registers one work context and returns its key.
func (w *shardWork) enter(cancel context.CancelFunc) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inflight == 0 {
		w.idle = make(chan struct{})
	}
	w.inflight++
	w.next++
	w.cancels[w.next] = cancel
	if w.cancelled {
		cancel()
	}
	return w.next
}

func (w *shardWork) exit(key uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.cancels, key)
	w.inflight--
	if w.inflight == 0 {
		close(w.idle)
	}
}

// cancelAll cancels every work context in flight before it returns.
func (w *shardWork) cancelAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = true
	for _, cancel := range w.cancels {
		cancel()
	}
}

func (w *shardWork) idleChan() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.idle
}

func (w *shardWork) busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inflight > 0
}

// NewShardSet creates an empty shard set.
func NewShardSet() *ShardSet {
	return &ShardSet{held: make(map[int]*shardWork)}
}

// Add marks a shard as held. Add of a held shard changes nothing.
func (s *ShardSet) Add(shard int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[shard]; !ok {
		s.held[shard] = newShardWork()
	}
}

// Remove drops a shard. New work cannot enter it. Work that entered it before
// keeps running and keeps its context.
func (s *ShardSet) Remove(shard int) {
	s.detach(shard)
}

// Revoke drops a shard and cancels the context of the work that entered it.
// Use it when another replica can already hold the lease. It returns false
// when the shard was not held, for example after a stale sweep dropped it.
func (s *ShardSet) Revoke(shard int) bool {
	w := s.detach(shard)
	if w == nil {
		return false
	}
	w.cancelAll()
	return true
}

func (s *ShardSet) detach(shard int) *shardWork {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.held[shard]
	delete(s.held, shard)
	return w
}

// Confirm records at as the start of a lease write that confirmed the shard.
// Take at before the request goes out, because the lease counts from that
// time. Confirm of an unheld shard changes nothing, and Confirm never moves
// the time back.
func (s *ShardSet) Confirm(shard int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.held[shard]; w != nil && at.After(w.confirmedAt) {
		w.confirmedAt = at
	}
}

// RevokeStale revokes every held shard whose last confirmation is maxAge or
// more before now. It cancels the work on those shards before it returns, and
// it returns the revoked shards sorted. A shard with no confirmation stays
// held.
func (s *ShardSet) RevokeStale(now time.Time, maxAge time.Duration) []int {
	s.mu.Lock()
	stale := make(map[int]*shardWork)
	for shard, w := range s.held {
		if !w.confirmedAt.IsZero() && now.Sub(w.confirmedAt) >= maxAge {
			stale[shard] = w
			delete(s.held, shard)
		}
	}
	s.mu.Unlock()

	out := make([]int, 0, len(stale))
	for shard, w := range stale {
		w.cancelAll()
		out = append(out, shard)
	}
	slices.Sort(out)
	return out
}

// Held returns true if the shard is currently held.
func (s *ShardSet) Held(shard int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.held[shard]
	return ok
}

// Owns returns true if the given key maps to a held shard.
func (s *ShardSet) Owns(ring *Ring, key string) bool {
	return s.Held(ring.ShardFor(key))
}

// OwnsController is a convenience that checks ownership by namespace+name.
func (s *ShardSet) OwnsController(ring *Ring, ns, name string) bool {
	return s.Owns(ring, fmt.Sprintf("%s/%s", ns, name))
}

// Count returns the number of held shards.
func (s *ShardSet) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

// Shards returns a copy of all held shard numbers.
func (s *ShardSet) Shards() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.held))
	for shard := range s.held {
		out = append(out, shard)
	}
	return out
}

// Enter registers work on a held shard. It returns false, and registers
// nothing, when the shard is not held. On success the caller must call leave
// when the work ends. The returned context ends when ctx ends or when a
// release cancels the work on the shard. Every write for the shard must use
// that context.
func (s *ShardSet) Enter(ctx context.Context, shard int) (context.Context, func(), bool) {
	s.mu.Lock()
	w := s.held[shard]
	if w == nil {
		s.mu.Unlock()
		return ctx, func() {}, false
	}
	// Enter under s.mu, so a Drain that detaches the shard sees this work.
	wctx, cancel := context.WithCancel(ctx)
	key := w.enter(cancel)
	s.mu.Unlock()

	var once sync.Once
	leave := func() {
		once.Do(func() {
			cancel()
			w.exit(key)
		})
	}
	return wctx, leave, true
}

// EnterController is Enter for the shard of namespace/name.
func (s *ShardSet) EnterController(ctx context.Context, ring *Ring, ns, name string) (context.Context, func(), bool) {
	return s.Enter(ctx, ring.ShardFor(fmt.Sprintf("%s/%s", ns, name)))
}

// Drain drops the shards and waits for the work that entered them to end. It
// waits at most wait. It then cancels the work that still runs and waits at
// most cancelWait more. It returns the sorted shards whose work still runs.
// The caller must not release the lease of such a shard. That lease must
// expire, because the work can still write.
func (s *ShardSet) Drain(shards []int, wait, cancelWait time.Duration) []int {
	_, busy := s.DrainHeld(shards, wait, cancelWait)
	return busy
}

// DrainHeld is Drain that also returns the sorted shards that it dropped.
// A shard that was not held when the drain started is in neither list.
func (s *ShardSet) DrainHeld(shards []int, wait, cancelWait time.Duration) (dropped, busy []int) {
	works := make(map[int]*shardWork, len(shards))
	for _, shard := range shards {
		if w := s.detach(shard); w != nil {
			works[shard] = w
		}
	}
	dropped = make([]int, 0, len(works))
	for shard := range works {
		dropped = append(dropped, shard)
	}
	slices.Sort(dropped)
	still := waitIdle(works, wait)
	for _, w := range still {
		w.cancelAll()
	}
	still = waitIdle(still, cancelWait)
	busy = make([]int, 0, len(still))
	for shard := range still {
		busy = append(busy, shard)
	}
	slices.Sort(busy)
	return dropped, busy
}

// waitIdle waits at most d for all works to become idle. It returns the works
// that are still busy.
func waitIdle(works map[int]*shardWork, d time.Duration) map[int]*shardWork {
	timer := time.NewTimer(d)
	defer timer.Stop()
	expired := false
	busy := make(map[int]*shardWork)
	for shard, w := range works {
		if !expired {
			select {
			case <-w.idleChan():
			case <-timer.C:
				expired = true
			}
		}
		if w.busy() {
			busy[shard] = w
		}
	}
	return busy
}
