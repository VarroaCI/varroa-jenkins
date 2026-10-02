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
// gateway, the operator, the BFF, and the recorder recover on the same
// schedule.
const (
	responderBaseBackoff = 1 * time.Second
	responderMaxBackoff  = 30 * time.Second
)

// resubscriber holds one subscription and subscribes again each time the
// server denies it. A denial can arrive at the first subscribe, before an
// authorization reload reaches the server, or later, when a reload
// removes the grant. The server does not restore a denied subscription
// when the grant comes back, so only a new subscribe recovers it.
type resubscriber struct {
	conn    *Conn
	kind    string
	subject string
	queue   string
	attempt func() (*Sub, error)
	backoff time.Duration

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// sub, denied, and release belong to the run goroutine after the
	// constructor returns. The resubscriber holds exactly one watcher at
	// a time, and run releases it before each rebuild and at stop.
	sub     *Sub
	denied  <-chan struct{}
	release func()
}

// keepSubscribed makes the first subscribe call and starts the loop that
// confirms it and rebuilds it after each denial. Only a subscribe call
// that fails locally, for example on a closed connection, returns an
// error. One flush orders the subscription at the server before the
// call returns, so a request sent after the return finds it. The confirm
// runs in the background and never delays the return by its wait.
func (c *Conn) keepSubscribed(kind, subject, queue string, backoff time.Duration, attempt func() (*Sub, error)) (*resubscriber, error) {
	r := &resubscriber{
		conn:    c,
		kind:    kind,
		subject: subject,
		queue:   queue,
		attempt: attempt,
		backoff: backoff,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if err := r.subscribe(); err != nil {
		close(r.done)
		return nil, err
	}
	// A failed flush is not fatal: the loop's confirm reports it and
	// subscribes again.
	_ = c.nc.FlushTimeout(subscribeConfirmFlush)
	go r.run()
	return r, nil
}

// subscribe makes one attempt. An error reports a local subscribe failure
// and leaves nothing held. On success the subscription and its watcher
// stay held, so a denial that arrives later still reaches the watcher.
func (r *resubscriber) subscribe() error {
	// The watcher goes first: the server rejects a subscription only
	// after the subscribe call returns, and only to the error handler.
	denied, release := r.conn.WatchSubscribeDenied(r.subject)
	sub, err := r.attempt()
	if err != nil {
		release()
		return err
	}
	r.sub, r.denied, r.release = sub, denied, release
	return nil
}

// drop unsubscribes and releases the watcher of the current attempt.
func (r *resubscriber) drop() {
	if r.sub != nil {
		_ = r.sub.Unsubscribe()
	}
	if r.release != nil {
		r.release()
	}
	r.sub, r.denied, r.release = nil, nil, nil
}

// run confirms the attempt it holds, watches it for a later denial, and
// subscribes again after each denial until stop closes.
func (r *resubscriber) run() {
	defer close(r.done)
	defer r.drop()
	delay := r.backoff
	wasDenied := false
	for {
		if err := r.conn.ConfirmSubscription(r.denied); err != nil {
			r.logDenied(wasDenied, err)
			wasDenied = true
		} else {
			if wasDenied {
				r.log(slog.LevelInfo, r.kind+" subscription accepted again")
			}
			wasDenied = false
			delay = r.backoff
			select {
			case <-r.stop:
				return
			case <-r.denied:
				r.logDenied(false, ErrSubscribeDenied)
				wasDenied = true
			}
		}
		for {
			r.drop()
			select {
			case <-r.stop:
				return
			case <-time.After(delay):
			}
			err := r.subscribe()
			if delay *= 2; delay > responderMaxBackoff {
				delay = responderMaxBackoff
			}
			if err == nil {
				break
			}
			r.log(slog.LevelError, r.kind+" subscribe failed; retrying", "error", err, "retryIn", delay)
		}
	}
}

// logDenied logs the first denial at Error and each later one at Debug.
// A confirm that fails for another reason, for example a flush timeout
// while the connection reconnects, logs at Warn.
func (r *resubscriber) logDenied(repeat bool, err error) {
	switch {
	case !errors.Is(err, ErrSubscribeDenied):
		r.log(slog.LevelWarn, r.kind+" subscription unconfirmed; subscribing again", "error", err)
	case repeat:
		r.log(slog.LevelDebug, r.kind+" subscription still denied; subscribing again")
	default:
		r.log(slog.LevelError, r.kind+" subscription denied; subscribing again")
	}
}

func (r *resubscriber) log(level slog.Level, msg string, args ...any) {
	l := r.conn.logger
	if l == nil {
		return
	}
	args = append([]any{"subject", r.subject, "queue", r.queue}, args...)
	l.Log(context.Background(), level, msg, args...)
}

// Unsubscribe stops the loop. It waits until the background goroutine has
// exited, so no subscribe attempt follows its return, and it releases the
// denial watcher.
func (r *resubscriber) Unsubscribe() error {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	return nil
}

// Responder is a request-reply subscription that subscribes again each
// time the server denies it.
type Responder struct {
	*resubscriber
}

// ServeRequests subscribes handler to subject with request-reply semantics,
// as SubscribeRequest does, and keeps the subscription alive for as long as
// the Responder lives. It registers the denial watcher before the
// subscribe call and returns after that call and one flush. A background
// loop then confirms the subscription and subscribes again after each
// denial, at start or later. A denial returns no error. Only a subscribe call that
// fails locally, for example on a closed connection, returns an error.
// Call Unsubscribe to stop the Responder and release its watcher.
func (c *Conn) ServeRequests(subject, queue string, handler RequestHandler) (*Responder, error) {
	return c.serveRequests(subject, queue, handler, responderBaseBackoff)
}

func (c *Conn) serveRequests(subject, queue string, handler RequestHandler, backoff time.Duration) (*Responder, error) {
	r, err := c.keepSubscribed("request responder", subject, queue, backoff, func() (*Sub, error) {
		return c.SubscribeRequest(subject, queue, handler)
	})
	if err != nil {
		return nil, err
	}
	return &Responder{r}, nil
}
