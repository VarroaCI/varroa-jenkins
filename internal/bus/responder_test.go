package bus

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

const (
	responderTestSubject = "varroa.test.responder"
	responderTestOther   = "varroa.test.other"
	responderTestBackoff = 20 * time.Millisecond
)

// responderServer wraps a bustest.DenyServer whose restricted user may not
// subscribe to the subjects of its deny list.
type responderServer struct {
	*bustest.DenyServer
	t *testing.T
}

func startResponderServer(t *testing.T, deny ...string) *responderServer {
	t.Helper()
	return &responderServer{DenyServer: bustest.StartDenyServer(t, deny...), t: t}
}

func (d *responderServer) setDeny(deny ...string) { d.SetDeny(deny...) }

func (d *responderServer) connect(user, pass string) *Conn {
	d.t.Helper()
	c, err := Connect(d.URL(), Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
	if err != nil {
		d.t.Fatalf("connect %s: %v", user, err)
	}
	d.t.Cleanup(c.Close)
	return c
}

// restricted connects as the user the deny list applies to.
func (d *responderServer) restricted() *Conn {
	return d.connect(bustest.RestrictedUser, bustest.RestrictedPassword)
}

// client connects as the user with every grant.
func (d *responderServer) client() *Conn {
	return d.connect(bustest.ClientUser, bustest.ClientPassword)
}

func echoHandler(data []byte) []byte { return append([]byte("echo:"), data...) }

// answered reports whether one request on the test subject gets the echo
// answer.
func answered(c *Conn) bool {
	msg, err := c.Request(responderTestSubject, []byte("x"), 200*time.Millisecond)
	return err == nil && string(msg.Data) == "echo:x"
}

// waitAnswered polls until a request on the test subject gets an answer.
func waitAnswered(t *testing.T, c *Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if answered(c) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no answer on %s within 5s", responderTestSubject)
}

// waitUnanswered polls until a request on the test subject finds no
// responder.
func waitUnanswered(t *testing.T, c *Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := c.Request(responderTestSubject, []byte("x"), 200*time.Millisecond)
		if errors.Is(err, nats.ErrNoResponders) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the responder on %s still answers", responderTestSubject)
}

func watcherCount(c *Conn, subject string) int {
	c.denied.mu.Lock()
	defer c.denied.mu.Unlock()
	return len(c.denied.bySubject[subject])
}

// TestServeRequests_RecoversAfterDeniedSubscribe pins the recovery from a
// subscription that NATS denies at the first subscribe, for example before
// an authorization reload reaches the server. ServeRequests returns no
// error, and the responder answers once the reload allows the subject,
// without a reconnect.
func TestServeRequests_RecoversAfterDeniedSubscribe(t *testing.T) {
	d := startResponderServer(t, responderTestSubject)
	rc := d.restricted()
	qc := d.client()

	r, err := rc.serveRequests(responderTestSubject, "recorder", echoHandler, responderTestBackoff)
	if err != nil {
		t.Fatalf("a denied first subscribe must not return an error: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()
	if answered(qc) {
		t.Fatal("a denied responder answered")
	}

	d.setDeny()
	waitAnswered(t, qc)
	if got := watcherCount(rc, responderTestSubject); got != 1 {
		t.Fatalf("watchers after recovery = %d, want 1", got)
	}
}

// TestServeRequests_RecoversAfterGrantRemovedAndRestored pins the recovery
// from a reload that removes the grant of a live subscription and a later
// reload that restores it. The server removes the subscription and does
// not restore it with the grant, so only a new subscribe recovers it.
func TestServeRequests_RecoversAfterGrantRemovedAndRestored(t *testing.T) {
	d := startResponderServer(t)
	rc := d.restricted()
	qc := d.client()

	r, err := rc.serveRequests(responderTestSubject, "recorder", echoHandler, responderTestBackoff)
	if err != nil {
		t.Fatalf("ServeRequests: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()
	waitAnswered(t, qc)

	d.setDeny(responderTestSubject)
	waitUnanswered(t, qc)

	d.setDeny()
	waitAnswered(t, qc)
}

// TestServeRequests_IgnoresDenialOfOtherSubject pins that a denial of
// another subject does not rebuild an accepted responder.
func TestServeRequests_IgnoresDenialOfOtherSubject(t *testing.T) {
	d := startResponderServer(t, responderTestOther)
	rc := d.restricted()
	qc := d.client()

	var mu sync.Mutex
	calls := 0
	handler := func(data []byte) []byte {
		mu.Lock()
		calls++
		mu.Unlock()
		return echoHandler(data)
	}
	r, err := rc.serveRequests(responderTestSubject, "recorder", handler, responderTestBackoff)
	if err != nil {
		t.Fatalf("ServeRequests: %v", err)
	}
	defer func() { _ = r.Unsubscribe() }()
	waitAnswered(t, qc)
	first := r.sub

	other, err := rc.SubscribeData(responderTestOther, func([]byte) {})
	if err != nil {
		t.Fatalf("subscribe other: %v", err)
	}
	defer func() { _ = other.Unsubscribe() }()
	time.Sleep(10 * responderTestBackoff) // A wrong rebuild has time to happen.

	if !answered(qc) {
		t.Fatal("the responder stopped answering after a denial of another subject")
	}
	if r.sub != first {
		t.Fatal("a denial of another subject rebuilt the responder")
	}
}

// TestServeRequests_UnsubscribeReleasesWatcher pins that Unsubscribe stops
// the background goroutine, releases the denial watcher, and leaves no
// subscribe attempt behind it.
func TestServeRequests_UnsubscribeReleasesWatcher(t *testing.T) {
	d := startResponderServer(t, responderTestSubject)
	rc := d.restricted()
	qc := d.client()

	r, err := rc.serveRequests(responderTestSubject, "recorder", echoHandler, responderTestBackoff)
	if err != nil {
		t.Fatalf("ServeRequests: %v", err)
	}
	if err := r.Unsubscribe(); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	select {
	case <-r.done:
	default:
		t.Fatal("Unsubscribe returned before the goroutine exited")
	}
	if got := watcherCount(rc, responderTestSubject); got != 0 {
		t.Fatalf("watchers after Unsubscribe = %d, want 0", got)
	}
	if err := r.Unsubscribe(); err != nil {
		t.Fatalf("a second Unsubscribe: %v", err)
	}

	d.setDeny()
	time.Sleep(10 * responderTestBackoff) // A stray rebuild has time to happen.
	if answered(qc) {
		t.Fatal("a subscribe attempt followed Unsubscribe")
	}
}

// TestServeRequests_ReturnsBeforeConfirm pins that ServeRequests returns
// after one subscribe call and leaves the confirm to the background loop.
// A confirm waits subscribeConfirmWait for an accepted subscription, so a
// serial confirm of every responder would delay the last one by the sum.
func TestServeRequests_ReturnsBeforeConfirm(t *testing.T) {
	d := startResponderServer(t)
	rc := d.restricted()

	const n = 20
	start := time.Now()
	for i := 0; i < n; i++ {
		r, err := rc.serveRequests(responderTestSubject, "recorder", echoHandler, responderTestBackoff)
		if err != nil {
			t.Fatalf("ServeRequests: %v", err)
		}
		defer func() { _ = r.Unsubscribe() }()
	}
	if elapsed := time.Since(start); elapsed >= n*subscribeConfirmWait/2 {
		t.Fatalf("%d ServeRequests calls took %s; the confirm must not block the call", n, elapsed)
	}
}
