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

// TestShardSet_RevokeStaleCancelsUnconfirmedWork pins that RevokeStale drops
// a shard at its deadline, cancels the work on it, and keeps a fresher shard.
func TestShardSet_RevokeStaleCancelsUnconfirmedWork(t *testing.T) {
	start := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ss := NewShardSet()
	ss.Add(1)
	ss.Add(2)
	ss.Confirm(1, start)
	ss.Confirm(2, start.Add(10*time.Second))
	ctx, leave, ok := ss.Enter(context.Background(), 1)
	if !ok {
		t.Fatal("Enter of a held shard failed")
	}
	defer leave()

	if got := ss.RevokeStale(start.Add(24*time.Second), 25*time.Second); len(got) != 0 {
		t.Fatalf("RevokeStale before the deadline = %v, want none", got)
	}
	got := ss.RevokeStale(start.Add(25*time.Second), 25*time.Second)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("RevokeStale at the deadline = %v, want [1]", got)
	}
	if ss.Held(1) || !ss.Held(2) {
		t.Fatalf("held after the revoke: 1=%v 2=%v, want 1=false 2=true", ss.Held(1), ss.Held(2))
	}
	if ctx.Err() == nil {
		t.Fatal("the work on the revoked shard was not cancelled")
	}
}

// TestShardSet_ConfirmIgnoresUnheldShard pins that Confirm cannot hold a
// shard, and that a later Add starts without a confirmation.
func TestShardSet_ConfirmIgnoresUnheldShard(t *testing.T) {
	start := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ss := NewShardSet()
	ss.Confirm(4, start)
	if ss.Held(4) {
		t.Fatal("Confirm of an unheld shard held it")
	}
	ss.Add(4)
	if got := ss.RevokeStale(start.Add(time.Hour), time.Second); len(got) != 0 {
		t.Fatalf("RevokeStale = %v, want none for a shard with no confirmation", got)
	}
}

// TestShardSet_RevokeStaleSkipsZeroStamp pins that a shard with no
// confirmation stays held, and that Confirm never moves the time back.
func TestShardSet_RevokeStaleSkipsZeroStamp(t *testing.T) {
	start := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ss := NewShardSet()
	ss.Add(5)
	if got := ss.RevokeStale(start, 0); len(got) != 0 {
		t.Fatalf("RevokeStale = %v, want none for a shard with no confirmation", got)
	}
	ss.Confirm(5, start.Add(20*time.Second))
	ss.Confirm(5, start)
	if got := ss.RevokeStale(start.Add(30*time.Second), 25*time.Second); len(got) != 0 {
		t.Fatalf("RevokeStale = %v, want none: Confirm moved the time back", got)
	}
}

// TestShardSet_RevokeReportsAHeldShard pins that Revoke reports a drop only
// once, so a pass does not report a shard that the sweep already dropped.
func TestShardSet_RevokeReportsAHeldShard(t *testing.T) {
	ss := NewShardSet()
	ss.Add(6)
	if !ss.Revoke(6) {
		t.Fatal("Revoke of a held shard returned false")
	}
	if ss.Revoke(6) {
		t.Fatal("Revoke of an unheld shard returned true")
	}
}

// TestShardSet_DrainHeldSkipsAnUnheldShard pins that DrainHeld reports only
// the shards that it dropped itself.
func TestShardSet_DrainHeldSkipsAnUnheldShard(t *testing.T) {
	ss := NewShardSet()
	ss.Add(1)
	ss.Add(2)
	ss.Revoke(2)
	dropped, busy := ss.DrainHeld([]int{1, 2}, time.Millisecond, time.Millisecond)
	if len(dropped) != 1 || dropped[0] != 1 {
		t.Fatalf("dropped = %v, want [1]", dropped)
	}
	if len(busy) != 0 {
		t.Fatalf("busy = %v, want none", busy)
	}
}
