// Package bustest runs an embedded NATS server for tests that deny a
// subscription and then reload the authorization, the way the chart's
// config reloader does. It does not import package bus, so the tests of
// package bus can use it too.
package bustest

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// The restricted user may not subscribe to the subjects in the deny list.
// The client user has no restriction and publishes or requests.
const (
	RestrictedUser     = "restricted"
	RestrictedPassword = "r"
	ClientUser         = "client"
	ClientPassword     = "c"
)

// DenyServer is an embedded NATS server whose restricted user may not
// subscribe to the subjects of its current deny list. SetDeny reloads the
// authorization in place: each client keeps its connection, and nats.go
// does not send a denied subscription again.
type DenyServer struct {
	t    testing.TB
	srv  *server.Server
	port int
	mu   sync.Mutex
}

// StartDenyServer starts a DenyServer that denies the subjects in deny to
// the restricted user. The test cleanup shuts it down.
func StartDenyServer(t testing.TB, deny ...string) *DenyServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	d := &DenyServer{t: t, port: port}
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

func (d *DenyServer) opts(deny []string) *server.Options {
	perms := &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: []string{">"}},
		Subscribe: &server.SubjectPermission{Allow: []string{">"}, Deny: deny},
	}
	return &server.Options{
		Host: "127.0.0.1",
		Port: d.port,
		Users: []*server.User{
			{Username: RestrictedUser, Password: RestrictedPassword, Permissions: perms},
			{Username: ClientUser, Password: ClientPassword},
		},
	}
}

// URL returns the client URL of the server.
func (d *DenyServer) URL() string { return d.srv.ClientURL() }

// SetDeny replaces the deny list of the restricted user and reloads the
// authorization. An empty list restores every grant.
func (d *DenyServer) SetDeny(deny ...string) {
	d.t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.srv.ReloadOptions(d.opts(deny)); err != nil {
		d.t.Fatalf("reload: %v", err)
	}
}

// Eventually polls cond every 20 ms until it holds or 10 s pass, and fails
// the test with msg when it never holds. The window covers the first
// three rebuilds of the production backoff: 1 s, 2 s, and 4 s.
func Eventually(t testing.TB, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
