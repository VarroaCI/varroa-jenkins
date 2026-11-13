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

// desiredRequest is one request from the operator on the desired-state
// subject: a desired-state command or a token grant.
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
	// The watcher goes first: the server rejects a subscription only
	// after Subscribe returns, and only to the connection's error handler.
	denied, release := s.conn.WatchSubscribeDenied(subject)
	sub, err := nc.SubscribeSync(subject)
	if err != nil {
		release()
		return nil, fmt.Errorf("desired subscribe: %w", err)
	}
	if err := s.conn.ConfirmSubscription(denied); err != nil {
		_ = sub.Unsubscribe()
		release()
		return nil, fmt.Errorf("desired subscribe %s: %w", subject, err)
	}
	return natsDesiredInbox{sub: sub, denied: denied, release: release}, nil
}

type natsDesiredInbox struct {
	sub     *nats.Subscription
	denied  <-chan struct{}
	release func()
}

func (i natsDesiredInbox) Next(timeout time.Duration) (desiredRequest, error) {
	select {
	case <-i.denied:
		return desiredRequest{}, bus.ErrSubscribeDenied
	default:
	}
	msg, err := i.sub.NextMsg(timeout)
	if err != nil {
		return desiredRequest{}, err
	}
	return desiredRequest{
		Data:    msg.Data,
		Confirm: func() error { return msg.Respond(nil) },
	}, nil
}

func (i natsDesiredInbox) Unsubscribe() error {
	i.release()
	return i.sub.Unsubscribe()
}

// directMessageKind names the operator messages that the desired-state
// subject carries. Both carry credentials, so neither may ride a stream. Any
// other message returns "": an imperative on this path would skip the
// in-flight limit and the result bookkeeping of the imperative pump.
func directMessageKind(op *mitev1.OperatorMessage) string {
	switch {
	case op.GetDesiredState() != nil:
		return "desired-state"
	case op.GetTokenGrant() != nil:
		return "token-grant"
	default:
		return ""
	}
}

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

// serveDesiredState forwards each request for the mite on the desired-state
// subject to the gRPC stream and answers the operator after each successful
// send. Each request carries a desired-state command or a token grant. inbox is
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
		cause := h.pumpDesired(ctx, inbox, ns, name, send)
		_ = inbox.Unsubscribe()
		inbox = nil
		if cause == nil {
			return
		}
		h.markWatchDegraded(ctx, ns, name, "desired", "desired-state subscription lost; rebuilding: "+cause.Error())
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// pumpDesired forwards requests until ctx is cancelled (returns nil) or the
// subscription dies (returns the cause). Each request carries a desired-state
// command or a token grant. A server denial of the
// subscription is such a death. A failed send gets no answer. During
// a reconnect two gateway replicas can hold a subscription for one mite, and
// only the replica with the live stream may answer.
func (h *BusHandler) pumpDesired(ctx context.Context, inbox desiredInbox, ns, name string, send Sender) error {
	key := ns + "/" + name
	for {
		select {
		case <-ctx.Done():
			return nil
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
			return err
		}
		var op mitev1.OperatorMessage
		if err := json.Unmarshal(req.Data, &op); err != nil {
			h.log().Error("unmarshal desired failed", "controller", key, "error", err)
			continue
		}
		kind := directMessageKind(&op)
		if kind == "" {
			h.log().Error("desired-state subject carried an unsupported message, dropping it", "controller", key)
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := send(&op); err != nil {
			h.log().Error("send to mite failed", "controller", key, "kind", kind, "error", err)
			continue
		}
		if req.Confirm != nil {
			if err := req.Confirm(); err != nil && !errors.Is(err, nats.ErrMsgNoReply) {
				h.log().Warn("confirm failed", "controller", key, "kind", kind, "error", err)
			}
		}
	}
}
