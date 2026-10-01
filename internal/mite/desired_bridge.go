package mite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// desiredSource opens the desired-state subscription for one mite. The
// production source subscribes on core NATS. Tests substitute a fake, so the
// retry and the reply rules are exercisable without a live server.
type desiredSource interface {
	SubscribeDesired(subject string) (desiredInbox, error)
}

// desiredInbox is one live desired-state subscription.
type desiredInbox interface {
	// Next waits up to timeout for the next request. It returns
	// nats.ErrTimeout when no request arrives in that time.
	Next(timeout time.Duration) (desiredRequest, error)
	Unsubscribe() error
}

// desiredRequest is one desired-state request from the operator.
type desiredRequest struct {
	Data []byte
	// Confirm answers the operator. Call it only after the command reached
	// the mite stream: the operator treats any answer as delivery.
	Confirm func() error
}

// natsDesiredSource is the production desiredSource over a bus connection.
type natsDesiredSource struct {
	conn *bus.Conn
}

func (s natsDesiredSource) SubscribeDesired(subject string) (desiredInbox, error) {
	nc := s.conn.NATSConn()
	if nc == nil {
		return nil, errors.New("nats connection unavailable")
	}
	sub, err := nc.SubscribeSync(subject)
	if err != nil {
		return nil, fmt.Errorf("desired subscribe: %w", err)
	}
	return natsDesiredInbox{sub: sub}, nil
}

type natsDesiredInbox struct {
	sub *nats.Subscription
}

func (i natsDesiredInbox) Next(timeout time.Duration) (desiredRequest, error) {
	msg, err := i.sub.NextMsg(timeout)
	if err != nil {
		return desiredRequest{}, err
	}
	return desiredRequest{
		Data:    msg.Data,
		Confirm: func() error { return msg.Respond(nil) },
	}, nil
}

func (i natsDesiredInbox) Unsubscribe() error { return i.sub.Unsubscribe() }

// subscribeDesired makes one subscription attempt for the mite. OnConnect
// calls it before it announces the connection, so the operator's push on the
// new epoch finds a responder. A failed attempt returns nil, and
// serveDesiredState retries it.
func (h *BusHandler) subscribeDesired(ns, name string) desiredInbox {
	if h.desired == nil {
		return nil
	}
	inbox, err := h.desired.SubscribeDesired(bus.MiteDesiredSubject(h.cluster, ns, name))
	if err != nil {
		h.log().Warn("desired-state subscribe failed; retrying", "controller", ns+"/"+name, "error", err)
		return nil
	}
	return inbox
}

// serveDesiredState forwards each desired-state request for the mite to the
// gRPC stream and answers the operator after each successful send. inbox is
// the subscription OnConnect opened, or nil when that attempt failed. The
// subscription is rebuilt for as long as the mite stays connected: a lost
// subscription otherwise starves the mite of every desired-state push.
func (h *BusHandler) serveDesiredState(ctx context.Context, ns, name string, send Sender, inbox desiredInbox) {
	if h.desired == nil {
		return
	}
	subject := bus.MiteDesiredSubject(h.cluster, ns, name)
	pacer := h.newRebuildPacer()
	for {
		if inbox == nil {
			established := h.retryEstablish(ctx, ns, name, "desired", func() error {
				var err error
				inbox, err = h.desired.SubscribeDesired(subject)
				return err
			})
			if !established {
				return // context cancelled: the mite disconnected
			}
		}
		start := time.Now()
		reconnect := h.pumpDesired(ctx, inbox, ns, name, send)
		_ = inbox.Unsubscribe()
		inbox = nil
		if !reconnect {
			return
		}
		h.markWatchDegraded(ctx, ns, name, "desired", "desired-state subscription lost; rebuilding")
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// pumpDesired forwards requests until ctx is cancelled (returns false) or
// the subscription dies (returns true). A failed send gets no answer. During
// a reconnect two gateway replicas can hold a subscription for one mite, and
// only the replica with the live stream may answer.
func (h *BusHandler) pumpDesired(ctx context.Context, inbox desiredInbox, ns, name string, send Sender) bool {
	key := ns + "/" + name
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		req, err := inbox.Next(500 * time.Millisecond)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			// Any other error rebuilds the subscription through the pacer.
			// A retry in place spins on an error that does not clear.
			h.log().Error("desired-state subscription lost", "controller", key, "error", err)
			return true
		}
		var op mitev1.OperatorMessage
		if err := json.Unmarshal(req.Data, &op); err != nil {
			h.log().Error("unmarshal desired failed", "controller", key, "error", err)
			continue
		}
		if ctx.Err() != nil {
			return false
		}
		if err := send(&op); err != nil {
			h.log().Error("send desired to mite failed", "controller", key, "error", err)
			continue
		}
		if req.Confirm != nil {
			if err := req.Confirm(); err != nil && !errors.Is(err, nats.ErrMsgNoReply) {
				h.log().Warn("desired-state confirm failed", "controller", key, "error", err)
			}
		}
	}
}
