package mite

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"
)

// imperativeAckWaitOrDefault returns the configured imperative ack wait, or
// imperativeAckWaitDefault.
func (h *BusHandler) imperativeAckWaitOrDefault() time.Duration {
	if h.imperativeAckWait <= 0 {
		return imperativeAckWaitDefault
	}
	return h.imperativeAckWait
}

// imperativeProgressInterval is how often the gateway marks each pending
// imperative command in progress: a quarter of the ack wait, unless a test
// sets imperativeProgressEvery.
func (h *BusHandler) imperativeProgressInterval() time.Duration {
	if h.imperativeProgressEvery > 0 {
		return h.imperativeProgressEvery
	}
	return h.imperativeAckWaitOrDefault() / 4
}

// markImperativesInProgress marks each imperative command that this
// connection holds without a result as in progress, once per progress
// interval, until ctx ends. The mite runs commands one at a time, so a
// command can wait behind other work for longer than any one command
// deadline. Only this heartbeat keeps such a command from a repeat while
// the connection lives.
//
// The token scope follows releasePendingImperatives: a superseded
// connection marks only its own deliveries, and a zero token marks every
// entry. The heartbeat stops with the connection's context, so a crashed
// gateway leaves each command to its ack wait.
func (h *BusHandler) markImperativesInProgress(ctx context.Context, ns, name string, token int64) {
	ticker := time.NewTicker(h.imperativeProgressInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		h.markPendingInProgress(ctx, ns, name, token)
	}
}

// markPendingInProgress sends one in-progress acknowledgement for each
// pending imperative of this connection. The bookkeeping reads under h.mu,
// and the acknowledgements go out after the unlock, because h.mu is on
// every controller's heartbeat path.
func (h *BusHandler) markPendingInProgress(ctx context.Context, ns, name string, token int64) {
	key := ns + "/" + name
	var msgs []*nats.Msg
	h.mu.Lock()
	if ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	for _, pending := range h.pendingAcks[key] {
		if token != 0 && pending.token != token {
			continue
		}
		msgs = append(msgs, pending.msg)
	}
	h.mu.Unlock()

	for _, m := range msgs {
		if err := m.InProgress(); err != nil {
			h.log().Debug("imperative in-progress ack failed", "controller", key, "error", err)
		}
	}
}
