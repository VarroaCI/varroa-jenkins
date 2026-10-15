package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/plugininv"
)

// setTestStream installs a new session that carries s and keeps the
// connection of the current session. A nil s clears the session.
func setTestStream(a *Agent, s mitev1.Mite_CommandStreamClient) {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if s == nil {
		a.sess = nil
		return
	}
	next := &miteSession{stream: s}
	if a.sess != nil {
		next.conn = a.sess.conn
		next.client = a.sess.client
	}
	a.sess = next
}

// setTestConn installs a new session that carries c and keeps the stream
// of the current session.
func setTestConn(a *Agent, c *grpc.ClientConn) {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	next := &miteSession{conn: c}
	if a.sess != nil {
		next.stream = a.sess.stream
		next.client = a.sess.client
	}
	a.sess = next
}

// newLazyConn returns a never-dialed client connection. Close on it is safe.
func newLazyConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func failingStream() *concurrentSendStream {
	return &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return errors.New("stream is dead") }}
}

// newSessionTestAgent returns an agent whose connect builds a lazy
// connection to a closed port.
func newSessionTestAgent(t *testing.T) *Agent {
	t.Helper()
	agent := mustNewAgent(t, Config{VarroaEndpoint: "127.0.0.1:1", CAPEM: string(generateTestCAPEM(t))})
	agent.Logger = slog.Default()
	agent.loopbackToken = runStateTestToken
	agent.crossTenantTimeout = time.Second
	agent.claimTimeout = time.Second
	return agent
}

func authed(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+runStateTestToken)
	return req
}

// loopbackPosts runs one post on every loopback route that sends on the
// command stream, and returns each route's status code by name.
func loopbackPosts(agent *Agent, seq int64) map[string]int {
	codes := map[string]int{}

	rec := httptest.NewRecorder()
	agent.handleLog(rec, newLogRequest(runStateTestToken, "folder/job", 1, seq, "1", "1", 1, 0, "line"))
	codes["log"] = rec.Code

	rec = httptest.NewRecorder()
	body := "[" + newBuildEventJSON(seq, "STARTED") + "]"
	agent.handleEvents(rec, authed(httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))))
	codes["events"] = rec.Code

	rec = httptest.NewRecorder()
	agent.handleClaim(rec, authed(httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))))
	codes["claim"] = rec.Code

	rec = httptest.NewRecorder()
	agent.crossTenantHandler("build")(rec, authed(httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/build", strings.NewReader(crossTenantBody("build")))))
	codes["cross-tenant"] = rec.Code

	rec = httptest.NewRecorder()
	agent.handleRunState(rec, newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, seq, seq+1, 5, 5, "hello"))
	codes["run-state"] = rec.Code
	return codes
}

// TestLoopbackSendAfterSessionEndKeepsNewConnOpen ends a session whose
// stream is dead, lets connect install a new connection, and then posts on
// every loopback route. Each route answers 503, and no connection closes.
func TestLoopbackSendAfterSessionEndKeepsNewConnOpen(t *testing.T) {
	agent := newSessionTestAgent(t)
	oldConn := newLazyConn(t)
	agent.installSession(&miteSession{stream: failingStream(), conn: oldConn})

	agent.endSession()
	if err := agent.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	newConn := agent.conn
	t.Cleanup(func() { _ = newConn.Close() })

	for route, code := range loopbackPosts(agent, 0) {
		if code != http.StatusServiceUnavailable {
			t.Errorf("route %s: expected 503 with no session, got %d", route, code)
		}
	}
	if _, err := agent.fetchRunStateFile(context.Background(), "run-1", "jobs/team/builds/1/program.dat", 1, 1); !errors.Is(err, errStreamNotConnected) {
		t.Errorf("run-state fetch: expected errStreamNotConnected, got %v", err)
	}
	if got := newConn.GetState(); got == connectivity.Shutdown {
		t.Error("a loopback send with no session closed the new connection")
	}
	if got := oldConn.GetState(); got == connectivity.Shutdown {
		t.Error("a loopback send with no session closed the old connection")
	}
}

// TestSendErrorClosesOwningConnOnly fails a send on a session while
// a.conn already names a newer connection. Only the session's own
// connection closes.
func TestSendErrorClosesOwningConnOnly(t *testing.T) {
	agent := newSessionTestAgent(t)
	ownConn := newLazyConn(t)
	agent.installSession(&miteSession{stream: failingStream(), conn: ownConn})
	if err := agent.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	newConn := agent.conn
	t.Cleanup(func() { _ = newConn.Close() })

	if err := agent.send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_Heartbeat{Heartbeat: &mitev1.Heartbeat{}}}); err == nil {
		t.Fatal("expected the send on a dead stream to fail")
	}
	if got := ownConn.GetState(); got != connectivity.Shutdown {
		t.Errorf("expected the session's own connection closed, state %v", got)
	}
	if got := newConn.GetState(); got == connectivity.Shutdown {
		t.Error("a send error closed a connection that the session does not own")
	}
}

// TestLoopbackSendRacesReconnect posts on the loopback routes while a
// reconnect loop ends sessions, connects, and installs new sessions. A
// session's connection may close only after its stream is dead.
func TestLoopbackSendRacesReconnect(t *testing.T) {
	agent := newSessionTestAgent(t)
	agent.claimTimeout = time.Millisecond
	agent.crossTenantTimeout = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var seq atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				loopbackPosts(agent, seq.Add(1))
			}
		}()
	}

	for i := 0; i < 20; i++ {
		var dead atomic.Bool
		stream := &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
			if dead.Load() {
				return errors.New("stream is dead")
			}
			return nil
		}}
		if err := agent.connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		conn := agent.conn
		agent.installSession(&miteSession{stream: stream, conn: conn, client: agent.client})

		time.Sleep(2 * time.Millisecond)
		if got := conn.GetState(); got == connectivity.Shutdown {
			t.Fatalf("session %d: its connection closed while its stream was live", i)
		}

		dead.Store(true)
		time.Sleep(time.Millisecond)
		agent.teardownSession()
	}
	cancel()
	wg.Wait()
}

// TestNoStreamSendersDoNotPanic runs every non-loopback sender with no
// session. Each one returns, and none panics on a nil stream.
func TestNoStreamSendersDoNotPanic(t *testing.T) {
	agent := newSessionTestAgent(t)
	agent.cfg.JenkinsURL = "http://127.0.0.1:1"

	if err := agent.send(&mitev1.MiteMessage{}); !errors.Is(err, errStreamNotConnected) {
		t.Errorf("send: expected errStreamNotConnected, got %v", err)
	}
	setTestConn(agent, newLazyConn(t))
	if err := agent.send(&mitev1.MiteMessage{}); !errors.Is(err, errStreamNotConnected) {
		t.Errorf("send on a session with no stream: expected errStreamNotConnected, got %v", err)
	}
	agent.endSession()

	done := make(chan struct{})
	go func() {
		defer close(done)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		agent.requestTokenRefresh(context.Background())
		agent.sendStateSnapshot(cancelled)
		agent.handleTailControl(cancelled, &mitev1.TailControl{RunId: "run-1", ControlId: "c1", Action: tailControlActionStart})
		agent.pushInventoryUnlocked(context.Background(), &plugininv.Inventory{CollectedAt: time.Now()})
		agent.replayRetainedEvents()
		agent.replayRetainedLogChunks()
		agent.processCommands(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the senders with no session did not return")
	}
}

// TestTeardownUnblocksStalledSend stalls a send, as gRPC flow control
// does, while the sender holds sendMu. The run-loop teardown must still
// return: its connection close unblocks the stalled send.
func TestTeardownUnblocksStalledSend(t *testing.T) {
	agent := newSessionTestAgent(t)
	if err := agent.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn := agent.conn
	stalled := make(chan struct{})
	stream := &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		close(stalled)
		for conn.GetState() != connectivity.Shutdown {
			conn.WaitForStateChange(context.Background(), conn.GetState())
		}
		return errors.New("connection closed")
	}}
	agent.installSession(&miteSession{stream: stream, conn: conn, client: agent.client})

	go func() { _ = agent.send(&mitev1.MiteMessage{}) }()
	<-stalled

	done := make(chan struct{})
	go func() {
		agent.teardownSession()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("teardown did not return while a send was stalled")
	}
	if err := agent.send(&mitev1.MiteMessage{}); !errors.Is(err, errStreamNotConnected) {
		t.Errorf("send after teardown: expected errStreamNotConnected, got %v", err)
	}
}
