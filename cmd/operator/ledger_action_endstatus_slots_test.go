package main

import (
	"context"
	"testing"
	"time"
)

// TestHandleLedgerAction_EndCommitStatusDropsWhenSlotsFull checks that a full
// set of forge slots drops the write without a call and without blocking.
func TestHandleLedgerAction_EndCommitStatusDropsWhenSlotsFull(t *testing.T) {
	for i := 0; i < cap(endStatusSlots); i++ {
		endStatusSlots <- struct{}{}
	}
	defer func() {
		for len(endStatusSlots) > 0 {
			<-endStatusSlots
		}
	}()
	ender := &fakeCommitStatusEnder{}
	done := make(chan struct{})
	go func() {
		handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
			"varroa.ledger.actions.core", "core", &fakeLedgerActionRunner{}, ender, endCommitStatusAction(t))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler blocked on a full forge slot set")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(ender.calls()); n != 0 {
		t.Fatalf("forge calls = %d, want 0 when dropped", n)
	}
}
