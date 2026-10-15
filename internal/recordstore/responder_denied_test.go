package recordstore

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// deniedRecorderOpts lets the "recorder" user subscribe to every subject
// except deny. The "bff" user has no limits.
func deniedRecorderOpts(port int, deny []string) *server.Options {
	return &server.Options{
		Host: "127.0.0.1",
		Port: port,
		Users: []*server.User{
			{Username: "recorder", Password: "r", Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: []string{">"}},
				Subscribe: &server.SubjectPermission{Allow: []string{">"}, Deny: deny},
			}},
			{Username: "bff", Password: "b"},
		},
	}
}

// TestFleetLockReleaseResponder_RecoversAfterDeniedSubscribe pins the
// recorder's recovery from a responder subscription that NATS denies at
// start, before an authorization reload adds the grant. The responder
// answers after the reload, without a restart and without a reconnect.
// The request carries no reason, so the handler answers before any
// database call and the test needs no Postgres.
func TestFleetLockReleaseResponder_RecoversAfterDeniedSubscribe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	srv, err := server.NewServer(deniedRecorderOpts(port, []string{bus.FleetLockReleaseSubject}))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	connect := func(user, pass string) *bus.Conn {
		c, err := bus.Connect(srv.ClientURL(), bus.Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("connect %s: %v", user, err)
		}
		t.Cleanup(c.Close)
		return c
	}
	recorder := connect("recorder", "r")
	bff := connect("bff", "b")

	sub, err := RegisterFleetLockReleaseResponder(recorder, nil, nil, nil)
	if err != nil {
		t.Fatalf("a denied subscribe must not fail the registration: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	req, _ := json.Marshal(FleetLockForceReleaseRequest{Name: "lock", RunID: "run-1", Actor: "op"})
	if _, err := bff.Request(bus.FleetLockReleaseSubject, req, 200*time.Millisecond); err == nil {
		t.Fatal("a denied responder answered")
	}

	if err := srv.ReloadOptions(deniedRecorderOpts(port, nil)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := bff.Request(bus.FleetLockReleaseSubject, req, 200*time.Millisecond)
		if err == nil {
			var resp FleetLockForceReleaseResponse
			if err := json.Unmarshal(msg.Data, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error == "" {
				t.Fatalf("a request with no reason = %+v, want an error", resp)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the responder did not answer within 10s after the reload: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
