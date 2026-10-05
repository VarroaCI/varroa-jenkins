package sharding

import (
	"context"
	"testing"
	"time"
)

// TestShardSet_EnterRefusesAnUnheldShard pins that work cannot enter a shard
// that the set does not hold.
func TestShardSet_EnterRefusesAnUnheldShard(t *testing.T) {
	ss := NewShardSet()
	ctx, leave, ok := ss.Enter(context.Background(), 3)
	defer leave()
	if ok {
		t.Fatal("Enter of an unheld shard succeeded")
	}
	if ctx.Err() != nil {
		t.Fatalf("refused Enter returned an ended context: %v", ctx.Err())
	}
}

// TestShardSet_EnterAfterRemoveIsRefused pins that a dropped shard takes no
// new work.
func TestShardSet_EnterAfterRemoveIsRefused(t *testing.T) {
	ss := NewShardSet()
	ss.Add(3)
	ss.Remove(3)
	if _, leave, ok := ss.Enter(context.Background(), 3); ok {
		leave()
		t.Fatal("Enter of a removed shard succeeded")
	}
}

// TestShardSet_DrainWaitsForInFlightWork pins that Drain returns only after
// the work on the shard ends, and that it cancels nothing that ends in time.
func TestShardSet_DrainWaitsForInFlightWork(t *testing.T) {
	ss := NewShardSet()
	ss.Add(3)
	ctx, leave, ok := ss.Enter(context.Background(), 3)
	if !ok {
		t.Fatal("Enter of a held shard failed")
	}

	done := make(chan []int, 1)
	go func() { done <- ss.Drain([]int{3}, 5*time.Second, time.Second) }()

	select {
	case <-done:
		t.Fatal("Drain returned while work ran on the shard")
	case <-time.After(50 * time.Millisecond):
	}
	if ss.Held(3) {
		t.Fatal("the shard is still held during the drain")
	}
	if ctx.Err() != nil {
		t.Fatalf("Drain cancelled work before its wait ended: %v", ctx.Err())
	}

	leave()
	select {
	case busy := <-done:
		if len(busy) != 0 {
			t.Fatalf("busy shards = %v, want none", busy)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the work ended")
	}
}

// TestShardSet_DrainCancelsWorkPastTheWait pins that Drain cancels the work
// that runs past the wait, and reports no busy shard when that work then ends.
func TestShardSet_DrainCancelsWorkPastTheWait(t *testing.T) {
	ss := NewShardSet()
	ss.Add(3)
	ctx, leave, _ := ss.Enter(context.Background(), 3)
	go func() {
		<-ctx.Done()
		leave()
	}()

	busy := ss.Drain([]int{3}, 10*time.Millisecond, 2*time.Second)
	if len(busy) != 0 {
		t.Fatalf("busy shards = %v, want none", busy)
	}
	if ctx.Err() == nil {
		t.Fatal("the work context was not cancelled")
	}
}

// TestShardSet_DrainReportsWorkThatIgnoresTheCancel pins that Drain reports a
// shard whose work still runs after the cancel wait.
func TestShardSet_DrainReportsWorkThatIgnoresTheCancel(t *testing.T) {
	ss := NewShardSet()
	ss.Add(3)
	ss.Add(4)
	_, leave, _ := ss.Enter(context.Background(), 3)
	defer leave()

	busy := ss.Drain([]int{3, 4}, 10*time.Millisecond, 10*time.Millisecond)
	if len(busy) != 1 || busy[0] != 3 {
		t.Fatalf("busy shards = %v, want [3]", busy)
	}
}

// TestShardSet_RevokeCancelsInFlightWork pins that Revoke drops the shard and
// cancels the work on it at once.
func TestShardSet_RevokeCancelsInFlightWork(t *testing.T) {
	ss := NewShardSet()
	ss.Add(3)
	ctx, leave, _ := ss.Enter(context.Background(), 3)
	defer leave()

	ss.Revoke(3)
	if ss.Held(3) {
		t.Fatal("a revoked shard is still held")
	}
	if ctx.Err() == nil {
		t.Fatal("Revoke returned before it cancelled the work context")
	}
}

// TestShardSet_AddAfterDrainStartsANewHolding pins that a shard held again
// does not inherit the cancel of the earlier holding.
func TestShardSet_AddAfterDrainStartsANewHolding(t *testing.T) {
	ss := NewShardSet()
	ss.Add(3)
	ss.Revoke(3)
	ss.Add(3)
	ctx, leave, ok := ss.Enter(context.Background(), 3)
	defer leave()
	if !ok || ctx.Err() != nil {
		t.Fatalf("Enter after a new Add: ok=%v err=%v, want ok and a live context", ok, ctx.Err())
	}
}
