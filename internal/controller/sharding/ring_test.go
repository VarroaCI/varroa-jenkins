package sharding

import (
	"fmt"
	"testing"
)

func TestRing_ShardFor_Deterministic(t *testing.T) {
	r := NewRing(DefaultShards)

	// Same key always maps to same shard.
	s1 := r.ShardFor("myns/myctrl")
	s2 := r.ShardFor("myns/myctrl")
	if s1 != s2 {
		t.Fatalf("expected deterministic, got %d != %d", s1, s2)
	}
}

func TestRing_ShardFor_Range(t *testing.T) {
	r := NewRing(64)

	for i := 0; i < 1000; i++ {
		key := "ns/ctrl-" + string(rune('0'+i%10))
		s := r.ShardFor(key)
		if s < 0 || s >= 64 {
			t.Fatalf("shard out of range: %d for key %s", s, key)
		}
	}
}

func TestRing_ShardFor_Distribution(t *testing.T) {
	r := NewRing(256)

	// Use realistic controller names to verify shard distribution.
	namespaces := []string{"team-a", "team-b", "platform", "data-eng", "ml-ops",
		"prod", "staging", "dev", "qa", "sandbox"}
	controllers := []string{"jenkins-primary", "ci-pipeline", "deploy-bot",
		"nightly-build", "pr-validator", "release-train", "security-scan",
		"perf-test", "integration-suite", "docs-builder"}

	hits := make([]int, 256)
	for _, ns := range namespaces {
		for _, ctrl := range controllers {
			key := fmt.Sprintf("%s/%s", ns, ctrl)
			s := r.ShardFor(key)
			hits[s]++
		}
	}

	// All 100 keys should hit a shard.
	total := 0
	for _, c := range hits {
		total += c
	}
	if total != 100 {
		t.Fatalf("expected 100 total hits, got %d", total)
	}
}

func TestRing_Assign(t *testing.T) {
	r := NewRing(8)

	t.Run("single replica owns all", func(t *testing.T) {
		a := r.Assign([]string{"op-0"})
		if len(a.Shards["op-0"]) != 8 {
			t.Fatalf("expected 8 shards, got %d", len(a.Shards["op-0"]))
		}
	})

	t.Run("two replicas split evenly", func(t *testing.T) {
		a := r.Assign([]string{"op-0", "op-1"})
		if len(a.Shards["op-0"]) != 4 || len(a.Shards["op-1"]) != 4 {
			t.Fatalf("expected 4+4 shards, got %d+%d", len(a.Shards["op-0"]), len(a.Shards["op-1"]))
		}
	})

	t.Run("three replicas distribute", func(t *testing.T) {
		a := r.Assign([]string{"op-0", "op-1", "op-2"})
		total := 0
		for _, shards := range a.Shards {
			total += len(shards)
		}
		if total != 8 {
			t.Fatalf("expected 8 total shards, got %d", total)
		}
		// op-0 should have 3, op-1 should have 3, op-2 should have 2 (8/3 round-robin)
		if len(a.Shards["op-2"]) < 2 {
			t.Fatalf("unexpected distribution: %v", a.Shards)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		replicas := []string{"op-2", "op-0", "op-1"}
		a1 := r.Assign(replicas)
		a2 := r.Assign(replicas)
		if len(a1.Shards) != len(a2.Shards) {
			t.Fatal("assignments should be deterministic")
		}
		for rep, shards := range a1.Shards {
			if len(a2.Shards[rep]) != len(shards) {
				t.Fatal("shard counts should match")
			}
		}
	})

	t.Run("zero replicas", func(t *testing.T) {
		a := r.Assign(nil)
		if len(a.Shards) != 0 {
			t.Fatal("expected empty assignment for zero replicas")
		}
	})
}

func TestShardSet(t *testing.T) {
	ring := NewRing(256)
	ss := NewShardSet()

	// Initially empty.
	if ss.Held(0) {
		t.Fatal("expected shard 0 not held")
	}
	if ss.Count() != 0 {
		t.Fatal("expected count 0")
	}

	// Add and check.
	ss.Add(42)
	if !ss.Held(42) {
		t.Fatal("expected shard 42 held")
	}
	if ss.Count() != 1 {
		t.Fatalf("expected count 1, got %d", ss.Count())
	}

	// Remove and check.
	ss.Remove(42)
	if ss.Held(42) {
		t.Fatal("expected shard 42 not held after remove")
	}
	if ss.Count() != 0 {
		t.Fatalf("expected count 0 after remove, got %d", ss.Count())
	}

	// Owns checks ring + shard set.
	shard := ring.ShardFor("ns/ctrl")
	ss.Add(shard)
	if !ss.OwnsController(ring, "ns", "ctrl") {
		t.Fatal("expected to own ns/ctrl")
	}
	if ss.OwnsController(ring, "other", "controller") {
		t.Fatal("expected NOT to own other/controller")
	}
}

func TestRing_NewRing_Boundary(t *testing.T) {
	// Negative or zero shards should default.
	r := NewRing(0)
	if r.Shards() != DefaultShards {
		t.Fatalf("expected %d shards, got %d", DefaultShards, r.Shards())
	}
	r2 := NewRing(-1)
	if r2.Shards() != DefaultShards {
		t.Fatalf("expected %d shards, got %d", DefaultShards, r2.Shards())
	}
}
