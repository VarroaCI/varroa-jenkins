// Package sharding provides consistent-hash-based shard assignment for
// distributing Controller CRs across operator replicas. Shard ownership is
// determined by per-shard Kubernetes Leases claimed by ShardManager, with
// presence leases (varroa-replica-<pod>.varroa.dev) used for replica discovery.
// The ShardManager runs on every replica (NeedLeaderElection=false) and
// rebalances shard ownership every 10s.
package sharding

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
)

const (
	// DefaultShards is the recommended number of virtual shards.
	DefaultShards = 256
)

// Ring is a fixed-size consistent hash ring. Each shard is a virtual node
// that can be claimed by an operator replica via a Kubernetes Lease.
type Ring struct {
	shards int
}

// NewRing creates a ring with the given number of shards.
func NewRing(shards int) *Ring {
	if shards < 1 {
		shards = DefaultShards
	}
	return &Ring{shards: shards}
}

// Shards returns the total number of shards on the ring.
func (r *Ring) Shards() int { return r.shards }

// ShardFor returns the shard that owns the given key. The key is typically
// "namespace/controllerName". The result is deterministic and stable.
func (r *Ring) ShardFor(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()) % r.shards
}

// Assignment describes which shards each replica owns based on the current
// set of replicas in the cluster. It distributes shards evenly.
type Assignment struct {
	Replicas []string         // sorted replica IDs
	Shards   map[string][]int // replica ID → shard list
}

// Assign distributes shards across replicas. Replicas are sorted so the
// assignment is deterministic. Every shard is assigned to exactly one replica.
func (r *Ring) Assign(replicas []string) *Assignment {
	if len(replicas) == 0 {
		return &Assignment{Shards: map[string][]int{}}
	}

	sorted := make([]string, len(replicas))
	copy(sorted, replicas)
	sort.Strings(sorted)

	a := &Assignment{
		Replicas: sorted,
		Shards:   make(map[string][]int),
	}
	for _, rep := range sorted {
		a.Shards[rep] = nil
	}

	// Distribute shards round-robin across sorted replicas.
	for s := 0; s < r.shards; s++ {
		rep := sorted[s%len(sorted)]
		a.Shards[rep] = append(a.Shards[rep], s)
	}
	return a
}

// ShardSet is a thread-safe set of held shard numbers.
type ShardSet struct {
	mu   sync.RWMutex
	held map[int]bool
}

// NewShardSet creates an empty shard set.
func NewShardSet() *ShardSet {
	return &ShardSet{held: make(map[int]bool)}
}

// Add marks a shard as held.
func (s *ShardSet) Add(shard int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[shard] = true
}

// Remove marks a shard as released.
func (s *ShardSet) Remove(shard int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, shard)
}

// Held returns true if the shard is currently held.
func (s *ShardSet) Held(shard int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.held[shard]
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.held)
}

// Shards returns a copy of all held shard numbers.
func (s *ShardSet) Shards() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]int, 0, len(s.held))
	for shard := range s.held {
		out = append(out, shard)
	}
	return out
}
