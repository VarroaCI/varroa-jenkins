package mite

import (
	"context"
	"time"
)

// imperativeInFlightLimitDefault is the most imperative commands one connection
// can send to its mite without a result. The mite ends its stream when its
// queue of imperatives is full, and that queue can hold every sent command
// while the worker applies desired state. This value must not exceed the mite
// queue cap of 16.
const imperativeInFlightLimitDefault = 8

// imperativeSlotRecheckDefault is how often a pump at the in-flight limit
// checks again for a result.
const imperativeSlotRecheckDefault = 100 * time.Millisecond

// imperativeInFlightLimit returns the configured in-flight limit, or
// imperativeInFlightLimitDefault.
func (h *BusHandler) imperativeInFlightLimit() int {
	if h.imperativeInFlightMax > 0 {
		return h.imperativeInFlightMax
	}
	return imperativeInFlightLimitDefault
}

// imperativeSlotRecheckInterval returns the configured recheck interval, or
// imperativeSlotRecheckDefault.
func (h *BusHandler) imperativeSlotRecheckInterval() time.Duration {
	if h.imperativeSlotRecheck > 0 {
		return h.imperativeSlotRecheck
	}
	return imperativeSlotRecheckDefault
}

// imperativesInFlight counts the imperative commands of one connection that
// have no result yet. A zero token counts every entry, which matches the token
// rule of releasePendingImperatives.
func (h *BusHandler) imperativesInFlight(ns, name string, token int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, pending := range h.pendingAcks[ns+"/"+name] {
		if token == 0 || pending.token == token {
			n++
		}
	}
	return n
}

// waitImperativeSlot blocks until this connection has room to send one more
// imperative. It returns false when ctx ends first.
//
// The mite ends its stream on a full queue, so a fetch past the limit ends
// every new session while a backlog lasts. Commands over the limit stay in
// JetStream in publish order.
func (h *BusHandler) waitImperativeSlot(ctx context.Context, ns, name string, token int64) bool {
	limit := h.imperativeInFlightLimit()
	logged := false
	for h.imperativesInFlight(ns, name, token) >= limit {
		if !logged {
			h.log().Debug("imperative in-flight limit reached, waiting for a result",
				"controller", ns+"/"+name, "limit", limit)
			logged = true
		}
		t := time.NewTimer(h.imperativeSlotRecheckInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
	return ctx.Err() == nil
}
