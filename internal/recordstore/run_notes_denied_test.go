package recordstore

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestRunNotePostResponder_RecoversAfterDeniedSubscribe pins the recorder's
// recovery from a note subscription that NATS denies at start, before an
// authorization reload adds the grant. The request is empty, so the handler
// answers before any database call and the test needs no Postgres.
func TestRunNotePostResponder_RecoversAfterDeniedSubscribe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	srv, err := server.NewServer(deniedRecorderOpts(port, []string{bus.RunNotePostSubject}))
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

	sub, err := RegisterRunNotePostResponder(recorder, nil, nil)
	if err != nil {
		t.Fatalf("a denied subscribe must not fail the registration: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if _, err := bff.Request(bus.RunNotePostSubject, []byte(`{}`), 200*time.Millisecond); err == nil {
		t.Fatal("a denied responder answered")
	}

	if err := srv.ReloadOptions(deniedRecorderOpts(port, nil)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := bff.Request(bus.RunNotePostSubject, []byte(`{}`), 200*time.Millisecond)
		if err == nil {
			var resp RunNotePostResponse
			if err := json.Unmarshal(msg.Data, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error != RunNoteErrInvalidRequest {
				t.Fatalf("reply = %+v, want %s", resp, RunNoteErrInvalidRequest)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the responder did not answer within 10s after the reload: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
