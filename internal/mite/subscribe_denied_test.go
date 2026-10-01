package mite

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// deniedServer runs an embedded nats-server whose gateway user may not
// subscribe to deniedSubject until allow is called. allow reloads the
// authorization the way the chart's config reloader does: the client keeps
// its connection, and nats.go does not send the denied subscription again.
type deniedServer struct {
	t       *testing.T
	srv     *server.Server
	port    int
	deniedS string
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (d *deniedServer) opts(deny bool) *server.Options {
	gwPerms := &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: []string{">"}},
		Subscribe: &server.SubjectPermission{Allow: []string{">"}},
	}
	if deny {
		gwPerms.Subscribe.Deny = []string{d.deniedS}
	}
	return &server.Options{
		Host: "127.0.0.1",
		Port: d.port,
		Users: []*server.User{
			{Username: "gateway", Password: "gw", Permissions: gwPerms},
			{Username: "operator", Password: "op"},
		},
	}
}

func startDeniedServer(t *testing.T, deniedSubject string) *deniedServer {
	t.Helper()
	d := &deniedServer{t: t, port: freePort(t), deniedS: deniedSubject}
	srv, err := server.NewServer(d.opts(true))
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

func (d *deniedServer) allow() {
	d.t.Helper()
	if err := d.srv.ReloadOptions(d.opts(false)); err != nil {
		d.t.Fatalf("reload: %v", err)
	}
}

func (d *deniedServer) connect(user, pass string) *bus.Conn {
	d.t.Helper()
	c, err := bus.Connect(d.srv.ClientURL(), bus.Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
	if err != nil {
		d.t.Fatalf("connect %s: %v", user, err)
	}
	d.t.Cleanup(c.Close)
	return c
}

func newDeniedTestHandler(t *testing.T, gw *bus.Conn) *BusHandler {
	t.Helper()
	h := NewBusHandler(bus.DefaultCluster, gw, nil, nil, nil)
	h.watchBackoff = 20 * time.Millisecond
	t.Cleanup(h.Close)
	return h
}

// TestServeDesiredState_RecoversAfterDeniedSubscribe pins the recovery from
// a desired-state subscription that NATS rejects, for example before an
// authorization reload reaches the server. The gateway marks the controller
// degraded and names the denial, subscribes again with backoff while the
// mite stays connected, and answers requests once NATS accepts the
// subscription. No reconnect of the NATS connection takes place.
func TestServeDesiredState_RecoversAfterDeniedSubscribe(t *testing.T) {
	ns, name := "varroa-system", "cell-a"
	subject := bus.MiteDesiredSubject(bus.DefaultCluster, ns, name)
	d := startDeniedServer(t, subject)
	gw := d.connect("gateway", "gw")
	op := d.connect("operator", "op")
	h := newDeniedTestHandler(t, gw)

	delivered := make(chan *mitev1.OperatorMessage, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.serveDesiredState(ctx, ns, name, func(m *mitev1.OperatorMessage) error {
		delivered <- m
		return nil
	}, nil)

	waitFor(t, func() bool {
		reason, degraded := h.streamDegraded(ns + "/" + name)
		return degraded && strings.Contains(reason, "denied")
	}, 3*time.Second, "a denied desired-state subscription was not marked degraded")
	if _, err := op.Request(subject, []byte(`{}`), 200*time.Millisecond); err == nil {
		t.Fatal("a denied subscription answered a request")
	}

	d.allow()

	payload, _ := json.Marshal(&mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_DesiredState{
		DesiredState: &mitev1.DesiredStateCommand{CommandId: "after-reload"},
	}})
	waitFor(t, func() bool {
		_, err := op.Request(subject, payload, 200*time.Millisecond)
		return err == nil
	}, 5*time.Second, "the desired-state subscription did not recover after the reload")
	select {
	case m := <-delivered:
		if m.GetDesiredState().CommandId != "after-reload" {
			t.Fatalf("delivered %+v, want after-reload", m)
		}
	case <-time.After(time.Second):
		t.Fatal("the recovered subscription did not forward the request")
	}
	waitFor(t, func() bool {
		_, degraded := h.streamDegraded(ns + "/" + name)
		return !degraded
	}, 3*time.Second, "the degraded mark stayed after the subscription recovered")
	if s := gw.NATSConn().Stats(); s.Reconnects != 0 {
		t.Fatalf("the gateway connection reconnected %d times; recovery must not need it", s.Reconnects)
	}
}

// TestWatchContent_RecoversAfterDeniedSubscribe pins the same recovery for
// the content-fetch subscription.
func TestWatchContent_RecoversAfterDeniedSubscribe(t *testing.T) {
	ns, name := "varroa-system", "cell-b"
	subject := bus.MiteContentSubject(bus.DefaultCluster, ns, name)
	d := startDeniedServer(t, subject)
	gw := d.connect("gateway", "gw")
	op := d.connect("operator", "op")
	h := newDeniedTestHandler(t, gw)

	asked := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchContent(ctx, ns, name, func(m *mitev1.OperatorMessage) error {
		if r := m.GetContentRequest(); r != nil {
			asked <- r.RequestId
		}
		return nil
	})

	waitFor(t, func() bool {
		reason, degraded := h.streamDegraded(ns + "/" + name)
		return degraded && strings.Contains(reason, "denied")
	}, 3*time.Second, "a denied content subscription was not marked degraded")

	d.allow()

	payload, _ := json.Marshal(&mitev1.ContentRequest{RequestId: "req-1"})
	waitFor(t, func() bool {
		if err := op.NATSConn().PublishRequest(subject, nats.NewInbox(), payload); err != nil {
			return false
		}
		select {
		case id := <-asked:
			return id == "req-1"
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}, 5*time.Second, "the content subscription did not recover after the reload")
	waitFor(t, func() bool {
		_, degraded := h.streamDegraded(ns + "/" + name)
		return !degraded
	}, 3*time.Second, "the degraded mark stayed after the content subscription recovered")
}

// TestWatchTail_RecoversAfterDeniedSubscribe pins the same recovery for the
// tail subscription, a callback subscription that nats.go gives no error
// channel.
func TestWatchTail_RecoversAfterDeniedSubscribe(t *testing.T) {
	ns, name := "varroa-system", "cell-c"
	subject := bus.TailSubject(bus.DefaultCluster, ns, name)
	d := startDeniedServer(t, subject)
	gw := d.connect("gateway", "gw")
	op := d.connect("operator", "op")
	h := newDeniedTestHandler(t, gw)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.watchTail(ctx, ns, name, func(*mitev1.OperatorMessage) error { return nil })

	waitFor(t, func() bool {
		reason, degraded := h.streamDegraded(ns + "/" + name)
		return degraded && strings.Contains(reason, "denied")
	}, 3*time.Second, "a denied tail subscription was not marked degraded")

	d.allow()

	waitFor(t, func() bool {
		_, err := op.Request(subject, []byte(`{}`), 200*time.Millisecond)
		return err == nil
	}, 5*time.Second, "the tail subscription did not recover after the reload")
	waitFor(t, func() bool {
		_, degraded := h.streamDegraded(ns + "/" + name)
		return !degraded
	}, 3*time.Second, "the degraded mark stayed after the tail subscription recovered")
}
