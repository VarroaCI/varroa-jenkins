package transport

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// deniedInboundServer runs an embedded nats-server whose operator user may
// not subscribe to one mite inbound subject until allow is called. allow
// reloads the authorization the way the chart's config reloader does: the
// client keeps its connection, and nats.go does not send the denied
// subscription again.
type deniedInboundServer struct {
	t       *testing.T
	srv     *server.Server
	port    int
	subject string
}

func (d *deniedInboundServer) opts(deny bool) *server.Options {
	opPerms := &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: []string{">"}},
		Subscribe: &server.SubjectPermission{Allow: []string{">"}},
	}
	if deny {
		opPerms.Subscribe.Deny = []string{d.subject}
	}
	return &server.Options{
		Host: "127.0.0.1",
		Port: d.port,
		Users: []*server.User{
			{Username: "operator", Password: "op", Permissions: opPerms},
			{Username: "gateway", Password: "gw"},
		},
	}
}

func startDeniedInboundServer(t *testing.T, subject string) *deniedInboundServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	d := &deniedInboundServer{t: t, port: port, subject: subject}
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

func (d *deniedInboundServer) allow() {
	d.t.Helper()
	if err := d.srv.ReloadOptions(d.opts(false)); err != nil {
		d.t.Fatalf("reload: %v", err)
	}
}

func (d *deniedInboundServer) connect(user, pass string) *bus.Conn {
	d.t.Helper()
	c, err := bus.Connect(d.srv.ClientURL(), bus.Config{Username: user, Password: pass, StartupTimeout: 5 * time.Second})
	if err != nil {
		d.t.Fatalf("connect %s: %v", user, err)
	}
	d.t.Cleanup(c.Close)
	return c
}

// lockedBuffer is a log sink that the NATS callback goroutines and the
// test goroutine can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitUntil polls cond for up to three seconds.
func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// waitDenied blocks until the server rejects a subscription to the
// watched subject.
func waitDenied(t *testing.T, denied <-chan struct{}) {
	t.Helper()
	select {
	case <-denied:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not deny the inbound subscription")
	}
}

// settle makes sure the server answered every earlier subscription on op
// and that a denial callback had time to run.
func settle(t *testing.T, op *bus.Conn) {
	t.Helper()
	if err := op.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
}

func commandResult(t *testing.T, id string) []byte {
	t.Helper()
	data, err := json.Marshal(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_CommandResult{
		CommandResult: &mitev1.CommandResult{CommandId: id},
	}})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return data
}

// TestBusTransport_InboundSubscriptionRecoversAfterDenial pins the recovery
// from an inbound subscription that NATS rejects, for example before an
// authorization reload reaches the server. The operator subscribes again
// on a later call, logs the first denial at Error and later ones at Debug,
// and logs one Info line once the server accepts the subscription. Command
// results and token refresh requests then reach the operator without a
// reconnect of its NATS connection.
func TestBusTransport_InboundSubscriptionRecoversAfterDenial(t *testing.T) {
	ns, name := "varroa-system", "cell-a"
	subject := bus.MiteInSubject(bus.DefaultCluster, ns, name)
	d := startDeniedInboundServer(t, subject)
	op := d.connect("operator", "op")
	gw := d.connect("gateway", "gw")

	var logs lockedBuffer
	tr := NewBusTransport(bus.DefaultCluster, op, nil, nil)
	tr.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	refreshed := make(chan string, 4)
	tr.TokenGrantFunc = func(ns, name string) (string, int64, error) {
		refreshed <- ns + "/" + name
		return "token", 1, nil
	}

	watch, release := op.WatchSubscribeDenied(subject)
	defer release()

	tr.DrainResults(ns, name)
	waitDenied(t, watch)
	// The watchers of one subject get the signal one after another, so
	// the transport's own watcher can lag the test's watcher.
	waitUntil(t, func() bool {
		tr.DrainResults(ns, name)
		return strings.Contains(logs.String(), "mite inbound subscription denied; subscribing again")
	}, "the first denial was not logged")
	if got := strings.Count(logs.String(), "mite inbound subscription denied; subscribing again"); got != 1 {
		t.Fatalf("Error denial lines = %d, want 1; logs:\n%s", got, logs.String())
	}

	waitDenied(t, watch)
	waitUntil(t, func() bool {
		tr.DrainResults(ns, name)
		return strings.Contains(logs.String(), "mite inbound subscription still denied; subscribing again")
	}, "a repeated denial was not logged at Debug")
	if strings.Contains(logs.String(), "mite inbound subscription accepted again") {
		t.Fatalf("a call before the denial arrived logged the subscription as accepted; logs:\n%s", logs.String())
	}
	if got := strings.Count(logs.String(), "level=ERROR msg=\"mite inbound subscription denied"); got != 1 {
		t.Fatalf("a repeated denial logged at Error again; logs:\n%s", logs.String())
	}

	waitDenied(t, watch)
	d.allow()
	// This loop finds the last denial and subscribes again. The server
	// now accepts the subscription.
	debugLines := strings.Count(logs.String(), "still denied")
	waitUntil(t, func() bool {
		tr.DrainResults(ns, name)
		return strings.Count(logs.String(), "still denied") > debugLines
	}, "the last denial did not cause a rebuild")
	settle(t, op)
	tr.DrainResults(ns, name)
	if strings.Contains(logs.String(), "mite inbound subscription accepted again") {
		t.Fatalf("the accepted line came before the grace period; logs:\n%s", logs.String())
	}
	time.Sleep(inboundAcceptGrace)
	tr.DrainResults(ns, name)
	if got := strings.Count(logs.String(), "mite inbound subscription accepted again"); got != 1 {
		t.Fatalf("accepted lines = %d, want 1; logs:\n%s", got, logs.String())
	}
	tr.DrainResults(ns, name)
	if got := strings.Count(logs.String(), "mite inbound subscription accepted again"); got != 1 {
		t.Fatalf("the accepted line repeated; logs:\n%s", logs.String())
	}

	if err := gw.Publish(subject, commandResult(t, "after-reload")); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	var got []*mitev1.CommandResult
	waitUntil(t, func() bool {
		got = append(got, tr.DrainResults(ns, name)...)
		return len(got) > 0
	}, "the recovered subscription delivered no command result")
	if got[0].CommandId != "after-reload" {
		t.Fatalf("result = %q, want after-reload", got[0].CommandId)
	}

	refresh, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_TokenRefreshRequest{TokenRefreshRequest: &mitev1.TokenRefreshRequest{}},
	})
	if err != nil {
		t.Fatalf("marshal refresh: %v", err)
	}
	if err := gw.Publish(subject, refresh); err != nil {
		t.Fatalf("publish refresh: %v", err)
	}
	select {
	case who := <-refreshed:
		if who != ns+"/"+name {
			t.Fatalf("refresh for %q, want %s/%s", who, ns, name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the recovered subscription did not answer a token refresh request")
	}

	if s := op.NATSConn().Stats(); s.Reconnects != 0 {
		t.Fatalf("the operator connection reconnected %d times; recovery must not need it", s.Reconnects)
	}
}

// TestBusTransport_EnsureSubKeepsAcceptedSubscription pins that an accepted
// subscription is kept: repeated calls do not subscribe again.
func TestBusTransport_EnsureSubKeepsAcceptedSubscription(t *testing.T) {
	tr := newBusTransport(t, startNATS(t))
	ns, name := "ns", "ctrl"
	base := tr.conn.NATSConn().NumSubscriptions()

	tr.DrainResults(ns, name)
	tr.mu.Lock()
	first := tr.subs[ns+"/"+name]
	tr.mu.Unlock()
	if first == nil {
		t.Fatal("no inbound subscription after the first call")
	}
	settle(t, tr.conn)
	for range 5 {
		tr.DrainResults(ns, name)
	}
	tr.mu.Lock()
	last := tr.subs[ns+"/"+name]
	tr.mu.Unlock()
	if last != first {
		t.Fatal("an accepted inbound subscription was replaced")
	}
	if n := tr.conn.NATSConn().NumSubscriptions() - base; n != 1 {
		t.Fatalf("new NATS subscriptions = %d, want 1", n)
	}
}

// TestBusTransport_EnsureSubConcurrentAfterDenial pins that concurrent
// callers rebuild a denied subscription once: one live subscription stays,
// and a later result arrives once.
func TestBusTransport_EnsureSubConcurrentAfterDenial(t *testing.T) {
	ns, name := "varroa-system", "cell-b"
	subject := bus.MiteInSubject(bus.DefaultCluster, ns, name)
	d := startDeniedInboundServer(t, subject)
	op := d.connect("operator", "op")
	gw := d.connect("gateway", "gw")
	tr := NewBusTransport(bus.DefaultCluster, op, nil, nil)

	watch, release := op.WatchSubscribeDenied(subject)
	defer release()
	tr.DrainResults(ns, name)
	waitDenied(t, watch)
	d.allow()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.DrainResults(ns, name)
		}()
	}
	wg.Wait()
	settle(t, op)

	if n := op.NATSConn().NumSubscriptions(); n != 1 {
		t.Fatalf("NATS subscriptions = %d, want 1", n)
	}
	tr.mu.Lock()
	entries := len(tr.subs)
	tr.mu.Unlock()
	if entries != 1 {
		t.Fatalf("inbound entries = %d, want 1", entries)
	}

	if err := gw.Publish(subject, commandResult(t, "once")); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	var got []*mitev1.CommandResult
	waitUntil(t, func() bool {
		got = append(got, tr.DrainResults(ns, name)...)
		return len(got) > 0
	}, "the rebuilt subscription delivered no command result")
	settle(t, gw)
	got = append(got, tr.DrainResults(ns, name)...)
	if len(got) != 1 {
		t.Fatalf("results = %d, want exactly 1", len(got))
	}
}

// TestBusTransport_ForgetReleasesTheDenialWatcher pins that Forget releases
// the denial watcher of the entry: a later denial of the same subject does
// not reach it.
func TestBusTransport_ForgetReleasesTheDenialWatcher(t *testing.T) {
	ns, name := "varroa-system", "cell-gone"
	subject := bus.MiteInSubject(bus.DefaultCluster, ns, name)
	d := startDeniedInboundServer(t, subject)
	op := d.connect("operator", "op")
	tr := NewBusTransport(bus.DefaultCluster, op, nil, nil)

	tr.DrainResults(ns, name)
	tr.mu.Lock()
	entry := tr.subs[ns+"/"+name]
	tr.mu.Unlock()
	waitDenied(t, entry.denied)

	tr.Forget(ns, name)

	watch, release := op.WatchSubscribeDenied(subject)
	defer release()
	sub, err := op.SubscribeData(subject, func([]byte) {})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	waitDenied(t, watch)
	select {
	case <-entry.denied:
		t.Fatal("the watcher of the forgotten entry still got a denial")
	default:
	}
}
