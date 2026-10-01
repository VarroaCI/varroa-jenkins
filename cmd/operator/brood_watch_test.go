package main

import (
	"log/slog"
	"testing"
)

// fakeBroodTrigger records every reconcile call the brood subscriber
// makes. calls holds routed triggers, and owned holds owner-only ones.
type fakeBroodTrigger struct {
	calls []string
	owned []string
}

func (f *fakeBroodTrigger) TriggerReconcile(cluster, name, namespace string) {
	f.calls = append(f.calls, cluster+"/"+namespace+"/"+name)
}

func (f *fakeBroodTrigger) TriggerOwnedReconcile(cluster, name, namespace string) {
	f.owned = append(f.owned, cluster+"/"+namespace+"/"+name)
}

// TestHandleBroodEvent asserts the subscriber's rules: a disconnected
// event for this cluster triggers one routed reconcile and one wake; a
// connected event triggers one owner-only reconcile and no wake; a
// foreign cluster triggers nothing; and every other event triggers
// nothing.
func TestHandleBroodEvent(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantCalls   int
		wantOwned   int
		wantWake    bool
		wantTrigger string
	}{
		{
			name:        "disconnected for this cluster",
			payload:     `{"event":"disconnected","name":"cell-a","namespace":"ci","cluster":"core"}`,
			wantCalls:   1,
			wantWake:    true,
			wantTrigger: "core/ci/cell-a",
		},
		{
			name:      "foreign cluster",
			payload:   `{"event":"disconnected","name":"cell-a","namespace":"ci","cluster":"member-c"}`,
			wantCalls: 0,
			wantWake:  false,
		},
		{
			name:      "connected event",
			payload:   `{"event":"connected","name":"cell-a","namespace":"ci","cluster":"core"}`,
			wantCalls: 0,
			wantOwned: 1,
			wantWake:  false,
		},
		{
			name:      "connected for a foreign cluster",
			payload:   `{"event":"connected","name":"cell-a","namespace":"ci","cluster":"member-c"}`,
			wantCalls: 0,
			wantWake:  false,
		},
		{
			name:      "heartbeat event",
			payload:   `{"event":"heartbeat","name":"cell-a","namespace":"ci","cluster":"core"}`,
			wantCalls: 0,
			wantWake:  false,
		},
		{
			name:      "malformed payload",
			payload:   `not json`,
			wantCalls: 0,
			wantWake:  false,
		},
		{
			name:      "no controller name",
			payload:   `{"event":"disconnected","name":"","namespace":"ci","cluster":"core"}`,
			wantCalls: 0,
			wantWake:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trigger := &fakeBroodTrigger{}
			wake := make(chan struct{}, 1)

			handleBroodEvent([]byte(tc.payload), "core", trigger, wake, slog.Default())

			if len(trigger.calls) != tc.wantCalls {
				t.Fatalf("reconcile calls = %v, want %d", trigger.calls, tc.wantCalls)
			}
			if len(trigger.owned) != tc.wantOwned {
				t.Fatalf("owned reconcile calls = %v, want %d", trigger.owned, tc.wantOwned)
			}
			if tc.wantOwned > 0 && trigger.owned[0] != "core/ci/cell-a" {
				t.Fatalf("owned reconcile call = %q, want core/ci/cell-a", trigger.owned[0])
			}
			if tc.wantTrigger != "" && trigger.calls[0] != tc.wantTrigger {
				t.Fatalf("reconcile call = %q, want %q", trigger.calls[0], tc.wantTrigger)
			}
			gotWake := len(wake) == 1
			if gotWake != tc.wantWake {
				t.Fatalf("wake delivered = %v, want %v", gotWake, tc.wantWake)
			}
		})
	}
}

// TestHandleBroodEventFullWakeChannel asserts that a full wake channel
// drops the wake without blocking. The pending token already covers the
// tick the dropped one would have run.
func TestHandleBroodEventFullWakeChannel(t *testing.T) {
	trigger := &fakeBroodTrigger{}
	wake := make(chan struct{}, 1)
	wake <- struct{}{}

	payload := []byte(`{"event":"disconnected","name":"cell-a","namespace":"ci","cluster":"core"}`)
	done := make(chan struct{})
	go func() {
		handleBroodEvent(payload, "core", trigger, wake, slog.Default())
		close(done)
	}()
	<-done

	if len(trigger.calls) != 1 {
		t.Fatalf("reconcile calls = %v, want 1", trigger.calls)
	}
	if len(wake) != 1 {
		t.Fatalf("wake channel holds %d tokens, want 1", len(wake))
	}
}
