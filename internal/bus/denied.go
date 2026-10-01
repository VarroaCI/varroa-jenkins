package bus

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// ErrSubscribeDenied reports that the server rejected a subscription.
// nats.go returns no error from Subscribe for that case: the server sends
// the rejection later, as an asynchronous permission violation.
var ErrSubscribeDenied = errors.New("bus: the server denied the subscription")

// subscribeDeniedRe takes the subject out of the server's violation text,
// `Permissions Violation for Subscription to "<subject>"`.
var subscribeDeniedRe = regexp.MustCompile(`Subscription to "(\S+)"`)

// subscribeConfirmWait bounds how long ConfirmSubscription waits for the
// asynchronous error callback after the flush. The server sends the
// rejection before the flush answer, but nats.go runs the callback on its
// own goroutine, so the signal can arrive just after the flush returns.
const subscribeConfirmWait = 100 * time.Millisecond

// subscribeConfirmFlush bounds the round trip that orders the server's
// answer to the subscription before ConfirmSubscription reads it.
const subscribeConfirmFlush = 2 * time.Second

// deniedWatchers fans a subscription denial out to every watcher of the
// denied subject.
type deniedWatchers struct {
	mu        sync.Mutex
	next      uint64
	bySubject map[string]map[uint64]chan struct{}
}

func (w *deniedWatchers) add(subject string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.bySubject == nil {
		w.bySubject = make(map[string]map[uint64]chan struct{})
	}
	id := w.next
	w.next++
	if w.bySubject[subject] == nil {
		w.bySubject[subject] = make(map[uint64]chan struct{})
	}
	w.bySubject[subject][id] = ch
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.bySubject[subject], id)
		if len(w.bySubject[subject]) == 0 {
			delete(w.bySubject, subject)
		}
	}
}

// notify signals every watcher of the subject that err names. The send
// never blocks: one pending signal is enough to make a watcher rebuild.
func (w *deniedWatchers) notify(err error) {
	m := subscribeDeniedRe.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.bySubject[m[1]] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WatchSubscribeDenied returns a channel that receives a signal each time
// the server rejects a subscription to subject on this connection, and a
// func that releases the watcher. Register the watcher before the
// subscription, so no rejection arrives unwatched.
func (c *Conn) WatchSubscribeDenied(subject string) (<-chan struct{}, func()) {
	return c.denied.add(subject)
}

// ConfirmSubscription waits until the server has answered every earlier
// subscription on this connection, then reports ErrSubscribeDenied when
// denied holds a rejection. A rejection that arrives after it returns
// still reaches denied, so the holder must keep watching it.
func (c *Conn) ConfirmSubscription(denied <-chan struct{}) error {
	if err := c.nc.FlushTimeout(subscribeConfirmFlush); err != nil {
		return fmt.Errorf("confirm subscription: %w", err)
	}
	select {
	case <-denied:
		return ErrSubscribeDenied
	case <-time.After(subscribeConfirmWait):
		return nil
	}
}

// isSubscribeDenial reports whether err is a permission violation on a
// subscription, as opposed to one on a publish.
func isSubscribeDenial(err error) bool {
	return errors.Is(err, nats.ErrPermissionViolation) && subscribeDeniedRe.MatchString(err.Error())
}
