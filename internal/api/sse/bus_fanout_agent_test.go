package sse

import (
	"testing"
)

// TestBusFanout_RoutesAgentSession proves a notice reaches only the stream of
// its session, never the brood-wide key.
func TestBusFanout_RoutesAgentSession(t *testing.T) {
	bf := NewBusFanout(nil)
	mine := bf.Subscribe(AgentSessionKeyPrefix + "s-1")
	other := bf.Subscribe(AgentSessionKeyPrefix + "s-2")
	all := bf.SubscribeAll()

	bf.deliverAgentSessionEvent("agentsessions.core._none",
		[]byte(`{"session_id":"s-1","kind":"assistant","state":"running","seq":4}`))

	select {
	case rec := <-mine:
		if rec.Event != "agentsession" {
			t.Errorf("event = %q, want agentsession", rec.Event)
		}
	default:
		t.Fatal("the session stream got no notice")
	}
	select {
	case <-other:
		t.Error("another session stream got the notice")
	default:
	}
	select {
	case <-all:
		t.Error("the brood-wide key got the notice")
	default:
	}
}

// TestBusFanout_DropsMalformedAgentSession proves a body with no session id,
// or one that does not parse, reaches no stream.
func TestBusFanout_DropsMalformedAgentSession(t *testing.T) {
	bf := NewBusFanout(nil)
	ch := bf.Subscribe(AgentSessionKeyPrefix + "")
	bf.deliverAgentSessionEvent("agentsessions.core._none", []byte(`{"state":"running"}`))
	bf.deliverAgentSessionEvent("agentsessions.core._none", []byte(`not json`))
	select {
	case <-ch:
		t.Error("a malformed notice was delivered")
	default:
	}
}
