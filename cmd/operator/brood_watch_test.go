package main

import (
	"log/slog"
	"testing"
)

// fakeBroodTrigger records every TriggerReconcile call the brood
// subscriber makes.
type fakeBroodTrigger struct {
	calls []string
}

func (f *fakeBroodTrigger) TriggerReconcile(cluster, name, namespace string) {
	f.calls = append(f.calls, cluster+"/"+namespace+"/"+name)
}

// TestHandleBroodEvent asserts the subscriber's three rules: a
// disconnected event for this cluster triggers one reconcile and one
// wake; a foreign cluster triggers neither; and every event but
// disconnected triggers neither.
func TestHandleBroodEvent(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantCalls   int
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
