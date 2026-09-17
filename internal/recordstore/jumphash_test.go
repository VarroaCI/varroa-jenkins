package recordstore

import (
	"fmt"
	"testing"
)

// TestJumpHashPublishedVectors asserts the Lamping and Veach reference
// vectors. The home map is stored, so a change to this function would
// re-home every job of every tenant on its next recompute.
func TestJumpHashPublishedVectors(t *testing.T) {
	cases := []struct {
		key     uint64
		buckets int32
		want    int32
	}{
		{1, 1, 0},
		{42, 57, 43},
		{0xDEAD10CC, 1, 0},
		{0xDEAD10CC, 666, 361},
		{256, 1024, 520},
	}
	for _, c := range cases {
		if got := jumpHash(c.key, c.buckets); got != c.want {
			t.Errorf("jumpHash(%d, %d) = %d, want %d", c.key, c.buckets, got, c.want)
		}
	}
}

// TestJumpHashStaysInRange asserts every answer is a valid bucket.
func TestJumpHashStaysInRange(t *testing.T) {
	for buckets := int32(1); buckets <= 16; buckets++ {
		for key := uint64(0); key < 500; key++ {
			got := jumpHash(key, buckets)
			if got < 0 || got >= buckets {
				t.Fatalf("jumpHash(%d, %d) = %d, outside [0, %d)", key, buckets, got, buckets)
			}
		}
	}
}

// TestJobHomeSlotGrowMovesOnlyOntoTheNewSlot asserts the property this
// design needs: a growth from N to N+1 moves about 1/(N+1) of the paths,
// and every path that moves lands on the new slot. No path moves between
// two old slots.
func TestJobHomeSlotGrowMovesOnlyOntoTheNewSlot(t *testing.T) {
	const paths = 2000
	for n := int32(1); n <= 6; n++ {
		moved := 0
		for i := 0; i < paths; i++ {
			path := fmt.Sprintf("acme/job-%d", i)
			before := jobHomeSlot(path, n)
			after := jobHomeSlot(path, n+1)
			if before == after {
				continue
			}
			moved++
			if after != n {
				t.Fatalf("%s moved from slot %d to slot %d, and the new slot is %d", path, before, after, n)
			}
		}
		want := float64(paths) / float64(n+1)
		if float64(moved) < want*0.7 || float64(moved) > want*1.3 {
			t.Errorf("a growth from %d to %d moved %d of %d paths, want about %.0f", n, n+1, moved, paths, want)
		}
	}
}

// TestJobHomeSlotShrinkRehomesOnlyTheRemovedSlot asserts a scale-down
// re-homes only the paths of the slots it removes.
func TestJobHomeSlotShrinkRehomesOnlyTheRemovedSlot(t *testing.T) {
	const paths = 2000
	for i := 0; i < paths; i++ {
		path := fmt.Sprintf("acme/job-%d", i)
		before := jobHomeSlot(path, 3)
		after := jobHomeSlot(path, 2)
		if before < 2 && after != before {
			t.Fatalf("%s was homed on slot %d and moved to slot %d on a shrink", path, before, after)
		}
		if after < 0 || after > 1 {
			t.Fatalf("%s landed on slot %d, outside the surviving range", path, after)
		}
	}
}

// TestJobHomeSlotOneBucketIsAlwaysZero asserts a single-slot tenant homes
// every job on slot 0, which is the behaviour before slots existed.
func TestJobHomeSlotOneBucketIsAlwaysZero(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := jobHomeSlot(fmt.Sprintf("acme/job-%d", i), 1); got != 0 {
			t.Fatalf("a one-slot tenant homed a job on slot %d", got)
		}
	}
	if got := jobHomeSlot("acme/api", 0); got != 0 {
		t.Fatalf("a zero bucket count answered slot %d, want 0", got)
	}
}

// TestComputeJobHomesIsDeterministic asserts two runs answer the same map
// and that an empty path is dropped.
func TestComputeJobHomesIsDeterministic(t *testing.T) {
	members := twoSlotRow().members
	paths := []string{"acme/api", "acme/web", "", "acme/api"}
	first := computeJobHomes(paths, members)
	second := computeJobHomes(paths, members)
	if !jobHomesEqual(first, second) {
		t.Fatalf("two runs answered different maps: %v and %v", first, second)
	}
	if _, ok := first[""]; ok {
		t.Fatal("an empty job path holds a home entry")
	}
	if len(first) != 2 {
		t.Fatalf("map size: want 2, got %d: %v", len(first), first)
	}
}

// TestMovedJobPaths asserts the moved set holds a changed path, a path
// present in one map alone, and nothing else.
func TestMovedJobPaths(t *testing.T) {
	before := map[string]int32{"a": 0, "b": 1, "c": 0}
	after := map[string]int32{"a": 0, "b": 0, "d": 1}
	got := movedJobPaths(before, after)
	want := []string{"b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("moved paths: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("moved paths: want %v, got %v", want, got)
		}
	}
	if len(movedJobPaths(before, before)) != 0 {
		t.Fatal("an unchanged map reported a move")
	}
}
