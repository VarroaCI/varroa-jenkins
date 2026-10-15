package bus

import (
	"time"

	"github.com/nats-io/nats.go"
)

// SubjectHandler is a subscription callback that receives the subject of
// each message with its payload. A wildcard subscription reads the
// concrete subject from it.
type SubjectHandler func(subject string, data []byte)

// Listener is a plain subscription that subscribes again each time the
// server denies it. It holds one subscription and one denial watcher.
type Listener struct {
	*resubscriber
}

// Listen subscribes handler to subject and keeps the subscription alive
// for as long as the Listener lives. A non-empty queue selects queue-group
// delivery. Listen registers the denial watcher before the subscribe call
// and returns after that call and one flush. A background loop then
// confirms the subscription and subscribes again after each denial, at
// start or later.
// A denial returns no error. Only a subscribe call that fails locally, for
// example on a closed connection, returns an error. Call Unsubscribe to
// stop the Listener and release its subscription and its watcher.
func (c *Conn) Listen(subject, queue string, handler SubjectHandler) (*Listener, error) {
	return c.listen(subject, queue, handler, responderBaseBackoff)
}

func (c *Conn) listen(subject, queue string, handler SubjectHandler, backoff time.Duration) (*Listener, error) {
	cb := func(msg *nats.Msg) { handler(msg.Subject, msg.Data) }
	r, err := c.keepSubscribed("listener", subject, queue, backoff, func() (*Sub, error) {
		var (
			natsSub *nats.Subscription
			err     error
		)
		if queue != "" {
			natsSub, err = c.nc.QueueSubscribe(subject, queue, cb)
		} else {
			natsSub, err = c.nc.Subscribe(subject, cb)
		}
		if err != nil {
			return nil, err
		}
		return &Sub{natsSub: natsSub}, nil
	})
	if err != nil {
		return nil, err
	}
	return &Listener{r}, nil
}
