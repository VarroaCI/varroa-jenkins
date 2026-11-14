package bus

import (
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

// startProbeServer starts an embedded server with one user. The user may
// publish only the JetStream account status request and subscribe only to
// the test inbox, so a probe that drops the credentials or the inbox prefix
// fails to connect or to get its answer.
func startProbeServer(t *testing.T, jetStream bool) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host: "127.0.0.1",
		Port: -1,
		Users: []*server.User{{
			Username: "svc",
			Password: "pw",
			Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: []string{"$JS.API.INFO"}},
				Subscribe: &server.SubjectPermission{Allow: []string{"_INBOX_test.>"}},
			},
		}},
	}
	if jetStream {
		opts.JetStream = true
		opts.StoreDir = t.TempDir()
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// connectProbeTest connects with the probe test credentials and a fast probe
// interval. The list order is fixed, so the first URL is the first choice.
func connectProbeTest(t *testing.T, urls string) *Conn {
	t.Helper()
	c, err := Connect(urls, Config{
		Username:        "svc",
		Password:        "pw",
		InboxPrefix:     "_INBOX_test",
		jsProbeInterval: 50 * time.Millisecond,
		extraOptions:    []nats.Option{nats.DontRandomize()},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// A server without JetStream answers the account status request with no
// responders. That stands in for a server cut off from the meta leader.
func TestJSHealthMovesToServerThatAnswersJetStream(t *testing.T) {
	cut := startProbeServer(t, false)
	healthy := startProbeServer(t, true)
	c := connectProbeTest(t, cut.ClientURL()+","+healthy.ClientURL())
	if got := c.nc.ConnectedServerId(); got != cut.ID() {
		t.Fatalf("started on %s, want the first server %s", got, cut.ID())
	}
	bustest.Eventually(t, "the connection moves to the JetStream server", func() bool {
		return c.nc.ConnectedServerId() == healthy.ID()
	})
	time.Sleep(500 * time.Millisecond)
	if got := c.nc.ConnectedServerId(); got != healthy.ID() {
		t.Fatalf("moved back to %s, want %s", got, healthy.ID())
	}
}

// When no server answers, the connection keeps its server and never reconnects.
// The probe dials the other entry only, never the connected server.
func TestJSHealthKeepsConnectionWhenNoServerAnswers(t *testing.T) {
	a := startProbeServer(t, false)
	b := startProbeServer(t, false)
	c := connectProbeTest(t, a.ClientURL()+","+b.ClientURL())
	bustest.Eventually(t, "the health loop probes the other server", func() bool {
		v, err := b.Varz(nil)
		return err == nil && v.TotalConnections >= 3
	})
	v, err := a.Varz(nil)
	if err != nil {
		t.Fatalf("varz: %v", err)
	}
	if v.TotalConnections != 1 {
		t.Fatalf("connections to the connected server = %d, want 1 (the probe must not dial it)", v.TotalConnections)
	}
	if got := c.nc.ConnectedServerId(); got != a.ID() {
		t.Fatalf("connection on %s, want %s", got, a.ID())
	}
	if n := c.nc.Stats().Reconnects; n != 0 {
		t.Fatalf("reconnects = %d, want 0", n)
	}
}

// deadURL gives an address that refuses connections: a listener that is closed.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "nats://" + addr
}

// An unreachable entry in the list does not stop the probe. It dials the
// entries in order, skips the connected server, and reaches the healthy one.
func TestJSHealthProbeSkipsUnreachableServer(t *testing.T) {
	cut := startProbeServer(t, false)
	healthy := startProbeServer(t, true)
	c := connectProbeTest(t, cut.ClientURL()+","+deadURL(t)+","+healthy.ClientURL())
	bustest.Eventually(t, "the connection moves to the JetStream server", func() bool {
		return c.nc.ConnectedServerId() == healthy.ID()
	})
	v, err := cut.Varz(nil)
	if err != nil {
		t.Fatalf("varz: %v", err)
	}
	if v.TotalConnections != 1 {
		t.Fatalf("connections to the cut-off server = %d, want 1 (the probe must not dial it)", v.TotalConnections)
	}
}

// A reachable entry that does not answer JetStream does not end the probe.
// The probe moves on to the next entry, which answers.
func TestJSHealthProbeSkipsServerThatDoesNotAnswerJetStream(t *testing.T) {
	cut := startProbeServer(t, false)
	noJS := startProbeServer(t, false)
	healthy := startProbeServer(t, true)
	c := connectProbeTest(t, cut.ClientURL()+","+noJS.ClientURL()+","+healthy.ClientURL())
	bustest.Eventually(t, "the connection moves past the server without JetStream", func() bool {
		return c.nc.ConnectedServerId() == healthy.ID()
	})
}

// A healthy server gets no probe connection: only the main connection exists.
func TestJSHealthHealthyServerOpensNoProbeConnection(t *testing.T) {
	s := startProbeServer(t, true)
	c := connectProbeTest(t, s.ClientURL())
	bustest.Eventually(t, "the health loop sends probes", func() bool {
		v, err := s.Varz(nil)
		return err == nil && v.InMsgs >= 10
	})
	v, err := s.Varz(nil)
	if err != nil {
		t.Fatalf("varz: %v", err)
	}
	if v.TotalConnections != 1 {
		t.Fatalf("total connections = %d, want 1", v.TotalConnections)
	}
	if n := c.nc.Stats().Reconnects; n != 0 {
		t.Fatalf("reconnects = %d, want 0", n)
	}
}

func TestCloseStopsJetStreamHealth(t *testing.T) {
	s := startProbeServer(t, true)
	c := connectProbeTest(t, s.ClientURL())
	c.Close()
	select {
	case <-c.jsDone:
	case <-time.After(time.Second):
		t.Fatal("JetStream health loop still running after Close")
	}
}
