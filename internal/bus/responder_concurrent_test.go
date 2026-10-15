package bus

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// blockingHandler holds every request whose payload is "hold" until
// release closes, and answers every other request at once.
type blockingHandler struct {
	release  chan struct{}
	inFlight atomic.Int32
	peak     atomic.Int32
	started  chan struct{}
}

func newBlockingHandler() *blockingHandler {
	return &blockingHandler{release: make(chan struct{}), started: make(chan struct{}, 16)}
}

func (h *blockingHandler) handle(data []byte) []byte {
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	for {
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if string(data) == "hold" {
		h.started <- struct{}{}
		<-h.release
	}
	return append([]byte("echo:"), data...)
}

// requestAsync sends one request in the background and answers its
// reply payload or its error on the returned channel.
func requestAsync(c *Conn, data string) <-chan string {
	out := make(chan string, 1)
	go func() {
		msg, err := c.Request(responderTestSubject, []byte(data), 5*time.Second)
		if err != nil {
			out <- "error:" + err.Error()
			return
		}
		out <- string(msg.Data)
	}()
	return out
}

func waitStarted(t *testing.T, h *blockingHandler) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the held request did not reach the handler")
	}
}

// TestServeRequestsConcurrently_SlowRequestDoesNotBlockOthers pins that a
// request that waits in the handler does not hold the next request on the
// same subscription.
func TestServeRequestsConcurrently_SlowRequestDoesNotBlockOthers(t *testing.T) {
	d := startResponderServer(t)
	rc := d.restricted()
	qc := d.client()
	h := newBlockingHandler()

	r, err := rc.serveRequestsConcurrently(responderTestSubject, "", 4, h.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()

	held := requestAsync(qc, "hold")
	waitStarted(t, h)

	msg, err := qc.Request(responderTestSubject, []byte("b"), time.Second)
	if err != nil {
		t.Fatalf("the second request waited behind the held one: %v", err)
	}
	if string(msg.Data) != "echo:b" {
		t.Fatalf("second reply = %q, want echo:b", msg.Data)
	}

	close(h.release)
	if got := <-held; got != "echo:hold" {
		t.Fatalf("held reply = %q, want echo:hold", got)
	}
}

// TestServeRequestsConcurrently_BoundsInFlightHandlers pins that no more
// than limit handler calls run at a time, and that a request over the
// limit runs after a slot is free.
func TestServeRequestsConcurrently_BoundsInFlightHandlers(t *testing.T) {
	d := startResponderServer(t)
	rc := d.restricted()
	qc := d.client()
	h := newBlockingHandler()

	r, err := rc.serveRequestsConcurrently(responderTestSubject, "", 2, h.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()

	replies := []<-chan string{
		requestAsync(qc, "hold"),
		requestAsync(qc, "hold"),
	}
	waitStarted(t, h)
	waitStarted(t, h)

	third := requestAsync(qc, "c")
	select {
	case got := <-third:
		t.Fatalf("a request over the limit ran while both slots were held: %q", got)
	case <-time.After(300 * time.Millisecond):
	}

	close(h.release)
	if got := <-third; got != "echo:c" {
		t.Fatalf("third reply = %q, want echo:c", got)
	}
	for _, ch := range replies {
		if got := <-ch; got != "echo:hold" {
			t.Fatalf("held reply = %q, want echo:hold", got)
		}
	}
	if got := h.peak.Load(); got != 2 {
		t.Fatalf("peak in-flight handler calls = %d, want 2", got)
	}
}

// TestServeRequestsConcurrently_NilReplySendsNothing pins that a nil
// handler result sends no reply, so the requester times out.
func TestServeRequestsConcurrently_NilReplySendsNothing(t *testing.T) {
	d := startResponderServer(t)
	rc := d.restricted()
	qc := d.client()

	r, err := rc.serveRequestsConcurrently(responderTestSubject, "", 2,
		func([]byte) []byte { return nil }, responderTestBackoff)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()

	_, err = qc.Request(responderTestSubject, []byte("x"), 200*time.Millisecond)
	if !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("request error = %v, want a timeout", err)
	}
}

// TestServeRequestsConcurrently_RecoversAfterDeniedSubscribe pins that the
// concurrent responder keeps the self-healing loop of ServeRequests.
func TestServeRequestsConcurrently_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := startResponderServer(t, responderTestSubject)
	rc := d.restricted()
	qc := d.client()

	r, err := rc.serveRequestsConcurrently(responderTestSubject, "", 2, echoHandler, responderTestBackoff)
	if err != nil {
		t.Fatalf("a denied first subscribe must not return an error: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()
	if answered(qc) {
		t.Fatal("a denied responder answered")
	}

	d.setDeny()
	waitAnswered(t, qc)
}

// TestServeRequestsConcurrently_LimitHoldsAcrossResubscribe pins that a
// subscription that the loop builds again after a denial shares the slots
// of the denied one. The calls of the denied subscription still run, so a
// new set of slots would let the calls in flight exceed the limit.
func TestServeRequestsConcurrently_LimitHoldsAcrossResubscribe(t *testing.T) {
	d := startResponderServer(t)
	rc := d.restricted()
	qc := d.client()
	h := newBlockingHandler()

	r, err := rc.serveRequestsConcurrently(responderTestSubject, "", 2, h.handle, responderTestBackoff)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()

	replies := []<-chan string{requestAsync(qc, "hold"), requestAsync(qc, "hold")}
	waitStarted(t, h)
	waitStarted(t, h)

	d.setDeny(responderTestSubject)
	waitUnanswered(t, qc)
	d.setDeny()

	// The new subscription exists when a request times out instead of
	// finding no responder. It must not run while both slots are held.
	deadline := time.Now().Add(5 * time.Second)
	for {
		msg, err := qc.Request(responderTestSubject, []byte("probe"), 200*time.Millisecond)
		if err == nil {
			t.Fatalf("a request ran over the limit after the resubscribe: %q", msg.Data)
		}
		if errors.Is(err, nats.ErrTimeout) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the responder did not subscribe again: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	close(h.release)
	for _, ch := range replies {
		if got := <-ch; got != "echo:hold" {
			t.Fatalf("held reply = %q, want echo:hold", got)
		}
	}
	if got := h.peak.Load(); got != 2 {
		t.Fatalf("peak in-flight handler calls = %d, want 2", got)
	}
}
