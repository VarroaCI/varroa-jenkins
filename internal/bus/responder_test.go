package bus

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const (
	responderTestSubject = "varroa.test.responder"
	responderTestOther   = "varroa.test.other"
	responderTestBackoff = 20 * time.Millisecond
)

// responderServer runs an embedded nats-server whose "responder" user may
// not subscribe to the subjects in deny. setDeny reloads the authorization
// the way the chart's config reloader does: the client keeps its
// connection, and nats.go sends no denied subscription again.
type responderServer struct {
	t    *testing.T
	srv  *server.Server
	port int
}

func (d *responderServer) opts(deny []string) *server.Options {
	perms := &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: []string{">"}},
		Subscribe: &server.SubjectPermission{Allow: []string{">"}, Deny: deny},
	}
	return &server.Options{
		Host: "127.0.0.1",
		Port: d.port,
		Users: []*server.User{
			{Username: "responder", Password: "r", Permissions: perms},
			{Username: "requester", Password: "q"},
		},
	}
}

func startResponderServer(t *testing.T, deny ...string) *responderServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	d := &responderServer{t: t, port: port}
	srv, err := server.NewServer(d.opts(deny))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	d.srv = srv
	return d
}

func (d *responderServer) setDeny(deny ...string) {
	d.t.Helper()
	if err := d.srv.ReloadOptions(d.opts(deny)); err != nil {
		d.t.Fatalf("reload: %v", err)
	}
}

func (d *responderServer) connect(user, pass string) *Conn {
	d.t.Helper()
	c, err := Connect(d.srv.ClientURL(), Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
	if err != nil {
		d.t.Fatalf("connect %s: %v", user, err)
	}
	d.t.Cleanup(c.Close)
	return c
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
	rc := d.connect("responder", "r")
	qc := d.connect("requester", "q")

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
	rc := d.connect("responder", "r")
	qc := d.connect("requester", "q")

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
	rc := d.connect("responder", "r")
	qc := d.connect("requester", "q")

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
	rc := d.connect("responder", "r")
	qc := d.connect("requester", "q")

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
