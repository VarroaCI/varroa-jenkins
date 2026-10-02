package bus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// responderBaseBackoff and responderMaxBackoff pace the subscribe attempts
// after a denial. They are the gateway's watch backoff values, so the
// gateway, the operator, and the recorder recover on the same schedule.
const (
	responderBaseBackoff = 1 * time.Second
	responderMaxBackoff  = 30 * time.Second
)

// Responder is a request-reply subscription that subscribes again each time
// the server denies it. A denial can arrive at the first subscribe, before
// an authorization reload reaches the server, or later, when a reload
// removes the grant. The server does not restore a denied subscription when
// the grant comes back, so only a new subscribe recovers it.
type Responder struct {
	conn    *Conn
	subject string
	queue   string
	handler RequestHandler
	backoff time.Duration

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// sub, denied, and release belong to the run goroutine after
	// ServeRequests returns. The Responder holds exactly one watcher at a
	// time, and run releases it before each rebuild and at stop.
	sub     *Sub
	denied  <-chan struct{}
	release func()
}

// ServeRequests subscribes handler to subject with request-reply semantics,
// as SubscribeRequest does, and keeps the subscription alive for as long as
// the Responder lives. The first attempt runs before ServeRequests returns.
// A denial of that attempt returns no error: the Responder logs it and
// subscribes again in the background. Only a subscribe call that fails
// locally, for example on a closed connection, returns an error.
// Call Unsubscribe to stop the Responder and release its watcher.
func (c *Conn) ServeRequests(subject, queue string, handler RequestHandler) (*Responder, error) {
	return c.serveRequests(subject, queue, handler, responderBaseBackoff)
}

func (c *Conn) serveRequests(subject, queue string, handler RequestHandler, backoff time.Duration) (*Responder, error) {
	r := &Responder{
		conn:    c,
		subject: subject,
		queue:   queue,
		handler: handler,
		backoff: backoff,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	confirmErr, err := r.subscribe()
	if err != nil {
		close(r.done)
		return nil, err
	}
	wasDenied := false
	if confirmErr != nil {
		r.logDenied(false, confirmErr)
		wasDenied = true
	}
	go r.run(wasDenied)
	return r, nil
}

// subscribe makes one attempt. err reports a local subscribe failure, and
// leaves nothing held. confirmErr reports a denial or a failed confirm, and
// leaves the subscription and its watcher held, so a later denial still
// reaches the watcher.
func (r *Responder) subscribe() (confirmErr, err error) {
	// The watcher goes first: the server rejects a subscription only
	// after the subscribe call returns, and only to the error handler.
	denied, release := r.conn.WatchSubscribeDenied(r.subject)
	sub, err := r.conn.SubscribeRequest(r.subject, r.queue, r.handler)
	if err != nil {
		release()
		return nil, err
	}
	r.sub, r.denied, r.release = sub, denied, release
	return r.conn.ConfirmSubscription(denied), nil
}

// drop unsubscribes and releases the watcher of the current attempt.
func (r *Responder) drop() {
	if r.sub != nil {
		_ = r.sub.Unsubscribe()
	}
	if r.release != nil {
		r.release()
	}
	r.sub, r.denied, r.release = nil, nil, nil
}

// run subscribes again after each denial until stop closes. wasDenied
// tells run that the first attempt failed its confirm.
func (r *Responder) run(wasDenied bool) {
	defer close(r.done)
	defer r.drop()
	delay := r.backoff
	pending := wasDenied
	for {
		if !pending {
			select {
			case <-r.stop:
				return
			case <-r.denied:
				r.logDenied(wasDenied, ErrSubscribeDenied)
				wasDenied = true
			}
		}
		pending = false
		r.drop()
		select {
		case <-r.stop:
			return
		case <-time.After(delay):
		}
		confirmErr, err := r.subscribe()
		switch {
		case err != nil:
			pending = true
		case confirmErr != nil:
			r.logDenied(true, confirmErr)
			pending = true
		default:
			if wasDenied {
				r.log(slog.LevelInfo, "request responder subscription accepted again")
			}
			wasDenied = false
			delay = r.backoff
			continue
		}
		if delay *= 2; delay > responderMaxBackoff {
			delay = responderMaxBackoff
		}
		if err != nil {
			r.log(slog.LevelError, "request responder subscribe failed; retrying", "error", err, "retryIn", delay)
		}
	}
}

// logDenied logs the first denial at Error and each later one at Debug.
// A confirm that fails for another reason, for example a flush timeout
// while the connection reconnects, logs at Warn.
func (r *Responder) logDenied(repeat bool, err error) {
	switch {
	case !errors.Is(err, ErrSubscribeDenied):
		r.log(slog.LevelWarn, "request responder subscription unconfirmed; subscribing again", "error", err)
	case repeat:
		r.log(slog.LevelDebug, "request responder subscription still denied; subscribing again")
	default:
		r.log(slog.LevelError, "request responder subscription denied; subscribing again")
	}
}

func (r *Responder) log(level slog.Level, msg string, args ...any) {
	l := r.conn.logger
	if l == nil {
		return
	}
	args = append([]any{"subject", r.subject, "queue", r.queue}, args...)
	l.Log(context.Background(), level, msg, args...)
}

// Unsubscribe stops the Responder. It waits until the background goroutine
// has exited, so no subscribe attempt follows its return, and it releases
// the denial watcher.
func (r *Responder) Unsubscribe() error {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	return nil
}
