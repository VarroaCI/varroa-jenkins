package sse

import (
	"testing"
	"time"
)

// --------------------------------------------------------------------------
// Tests for per-subscriber filtered delivery (design §2.3)
// --------------------------------------------------------------------------

func TestSubscribeFiltered_DeliversOnlyMatchingEvents(t *testing.T) {
	bf := NewBusFanout(nil)

	// Create a filtered subscriber that admits only nsA/ctrlA.
	filter := func(rec Record) bool {
		ns, _ := rec.Data.(map[string]interface{})["namespace"].(string)
		ctrl, _ := rec.Data.(map[string]interface{})["controller"].(string)
		return ns == "nsA" && ctrl == "ctrlA"
	}

	ch := bf.SubscribeFiltered("nsA/ctrlA", filter)
	defer bf.Unsubscribe("nsA/ctrlA", ch)

	// Deliver an event for A (should pass filter).
	bf.deliver("nsA/ctrlA", Record{
		Event: "test",
		Data: map[string]interface{}{
			"namespace":  "nsA",
			"controller": "ctrlA",
		},
	})

	// Deliver an event for B (should be filtered out).
	bf.deliver("nsA/ctrlA", Record{
		Event: "test",
		Data: map[string]interface{}{
			"namespace":  "nsB",
			"controller": "ctrlB",
		},
	})

	// We should receive exactly one event.
	select {
	case rec := <-ch:
		data := rec.Data.(map[string]interface{})
		if data["namespace"] != "nsA" || data["controller"] != "ctrlA" {
			t.Errorf("expected nsA/ctrlA event, got %s/%s", data["namespace"], data["controller"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event A")
	}

	// The B event should NOT be delivered.
	select {
	case <-ch:
		t.Error("unexpected event B was delivered through filter")
	case <-time.After(50 * time.Millisecond):
		// Expected — filter blocked it.
	}
}

func TestSubscribeFiltered_NilFilterReceivesAll(t *testing.T) {
	bf := NewBusFanout(nil)

	// Subscribe with nil filter (back-compat).
	ch := bf.SubscribeFiltered("test-key", nil)
	defer bf.Unsubscribe("test-key", ch)

	// Deliver two events of different keys on the same subscriber key.
	bf.deliver("test-key", Record{
		Event: "event1",
		Data:  "payload1",
	})
	bf.deliver("test-key", Record{
		Event: "event2",
		Data:  "payload2",
	})

	// Both should arrive.
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
			// Expected
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i+1)
		}
	}
}

func TestSubscribe_BackCompatReceivesAll(t *testing.T) {
	bf := NewBusFanout(nil)

	// Old-style Subscribe (no filter).
	ch := bf.Subscribe("test-key")
	defer bf.Unsubscribe("test-key", ch)

	bf.deliver("test-key", Record{Event: "e1", Data: "d1"})
	bf.deliver("test-key", Record{Event: "e2", Data: "d2"})

	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i+1)
		}
	}
}

func TestSubscribeAll_BackCompat(t *testing.T) {
	bf := NewBusFanout(nil)

	ch := bf.SubscribeAll()
	defer bf.Unsubscribe("*", ch)

	bf.deliver("*", Record{Event: "e1", Data: "d1"})

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSubscribeFiltered_FilterBlockedNeverDelivers(t *testing.T) {
	bf := NewBusFanout(nil)

	// Filter that rejects everything.
	ch := bf.SubscribeFiltered("block-key", func(rec Record) bool { return false })
	defer bf.Unsubscribe("block-key", ch)

	bf.deliver("block-key", Record{Event: "e1", Data: "payload"})

	select {
	case <-ch:
		t.Error("expected filtered event to be blocked")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestUnsubscribe_MultipleSubscribers(t *testing.T) {
	bf := NewBusFanout(nil)

	// Add two subscribers: one filtered, one unfiltered.
	filter := func(rec Record) bool { return true }
	ch1 := bf.SubscribeFiltered("multi-key", filter)
	ch2 := bf.Subscribe("multi-key")

	// Deliver an event — both should get it.
	bf.deliver("multi-key", Record{Event: "e1", Data: "d1"})

	select {
	case <-ch1:
	case <-time.After(time.Second):
		t.Fatal("ch1 timed out")
	}
	select {
	case <-ch2:
	case <-time.After(time.Second):
		t.Fatal("ch2 timed out")
	}

	// Unsubscribe ch1 and verify ch2 still works.
	bf.Unsubscribe("multi-key", ch1)

	bf.deliver("multi-key", Record{Event: "e2", Data: "d2"})

	select {
	case <-ch2:
	case <-time.After(time.Second):
		t.Fatal("ch2 timed out after ch1 unsubscribed")
	}
}

func TestSubjectToKey(t *testing.T) {
	tests := []struct {
		subject string
		want    string
	}{
		{subject: "activity.core.team-a.foo", want: "core/team-a/foo"},
		{subject: "activity.core._global", want: "_global"},
		{subject: "activity.team-a.foo", want: ""},
		{subject: "activity.", want: ""},
		{subject: "", want: ""},
		{subject: "activity.core", want: ""},
		{subject: "activity.core.onlytwo", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			got := subjectToKey(tt.subject)
			if got != tt.want {
				t.Errorf("subjectToKey(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}

// TestDeliverActivityEvent_MalformedSubject verifies that a malformed/legacy
// activity subject (one that produces an empty key) is dropped entirely —
// it must NOT reach any subscriber, not even the "*" wildcard subscriber.
func TestDeliverActivityEvent_MalformedSubject(t *testing.T) {
	bf := NewBusFanout(nil)

	// Subscribe to a specific key and to "*".
	keyedCh := bf.Subscribe("core/team-a/foo")
	defer bf.Unsubscribe("core/team-a/foo", keyedCh)
	wildCh := bf.Subscribe("*")
	defer bf.Unsubscribe("*", wildCh)

	// Deliver a malformed activity event (legacy 3-token subject: no cluster).
	bf.deliverActivityEvent("activity.team-a.foo", []byte(`{"type":"test","namespace":"team-a","controller":"foo","cluster":"core","message":"test"}`))

	// Neither subscriber should receive anything.
	select {
	case <-keyedCh:
		t.Fatal("malformed event delivered to keyed subscriber")
	case <-wildCh:
		t.Fatal("malformed event delivered to wildcard subscriber")
	case <-time.After(100 * time.Millisecond):
		// Expected — event was dropped.
	}
}
