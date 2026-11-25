package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/connectivity"

	"github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// windowGateway is a command stream with a fake gateway behind it. Each
// direction holds at most window messages, as a flow-control window of a
// gRPC stream does. The gateway reads each mite message and answers each
// log chunk and each build event with a STORED ack. While the mite reads no
// ack, the gateway blocks on its full window and stops reading, and the
// next send of the mite then blocks too.
type windowGateway struct {
	toGateway chan *mitev1.MiteMessage
	toMite    chan *mitev1.OperatorMessage
	closed    chan struct{}
	closeOnce sync.Once
	snapshots atomic.Int32
}

func newWindowGateway(window int) *windowGateway {
	g := &windowGateway{
		toGateway: make(chan *mitev1.MiteMessage, window),
		toMite:    make(chan *mitev1.OperatorMessage, window),
		closed:    make(chan struct{}),
	}
	go g.serve()
	return g
}

// stream returns the mite end of the stream.
func (g *windowGateway) stream() *concurrentSendStream {
	return &concurrentSendStream{
		sendFn: func(m *mitev1.MiteMessage) error {
			select {
			case g.toGateway <- m:
				return nil
			case <-g.closed:
				return io.EOF
			}
		},
		recvFn: func() (*mitev1.OperatorMessage, error) {
			select {
			case m := <-g.toMite:
				return m, nil
			case <-g.closed:
				return nil, io.EOF
			}
		},
	}
}

func (g *windowGateway) close() { g.closeOnce.Do(func() { close(g.closed) }) }

func (g *windowGateway) serve() {
	for {
		var m *mitev1.MiteMessage
		select {
		case m = <-g.toGateway:
		case <-g.closed:
			return
		}
		var ack *mitev1.OperatorMessage
		switch {
		case m.GetLogChunk() != nil:
			c := m.GetLogChunk()
			ack = &mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_LogAck{LogAck: &mitev1.LogAck{
				RunId: c.RunId, AckSeq: c.Seq, Status: ackStatusStored,
			}}}
		case m.GetBuildEvent() != nil:
			e := m.GetBuildEvent()
			ack = &mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_EventAck{EventAck: &mitev1.EventAck{
				RunId: e.RunId, EventSeq: e.Seq, Status: ackStatusStored,
			}}}
		case m.GetStateSnapshot() != nil:
			g.snapshots.Add(1)
		}
		if ack == nil {
			continue
		}
		select {
		case g.toMite <- ack:
		case <-g.closed:
			return
		}
	}
}

// TestSessionReceivesBeforeTheReplay reconnects with a retained backlog
// eight times the window, in both directions of the stream. The receive
// loop must run during the replay: every ack then releases its chunk or
// event, and the startup snapshot follows the replay. A replay that runs
// before the receive loop stops when the gateway's ack window fills.
func TestSessionReceivesBeforeTheReplay(t *testing.T) {
	const window = 4
	const backlog = 8 * window

	agent := newSessionTestAgent(t)
	agent.miteDir = t.TempDir()
	// A stub Jenkins answers each probe at once, so the startup snapshot
	// does not wait for the retries of an unreachable Jenkins.
	agent.cfg.JenkinsURL = startLocalJenkins(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	for seq := int64(1); seq <= backlog; seq++ {
		chunk := &mitev1.LogChunk{RunId: "run-1", Seq: seq, Data: []byte("line\n")}
		size := int64(len(chunk.Data))
		if !agent.logRetention.reserve(size) {
			t.Fatalf("reserve log chunk %d", seq)
		}
		agent.logRetention.commit(chunk, size)

		evt := &mitev1.BuildEvent{RunId: "run-1", Seq: seq, Phase: "STARTED", JobPath: "j", BuildNumber: 1}
		evtSize := eventSize(evt)
		id, _, ok := agent.eventRetention.reserve(retentionKey{runID: evt.RunId, seq: evt.Seq}, evtSize)
		if !ok {
			t.Fatalf("reserve event %d", seq)
		}
		agent.eventRetention.commit(id, evt, evtSize)
	}

	gw := newWindowGateway(window)
	defer gw.close()
	agent.installSession(&miteSession{stream: gw.stream()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runSession(ctx)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		logs := len(agent.logRetention.snapshotOrdered())
		events := len(agent.eventRetention.snapshotOrdered())
		if logs == 0 && events == 0 && gw.snapshots.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session stalled: %d log chunks and %d events still retained, %d snapshots sent",
				logs, events, gw.snapshots.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	gw.close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runSession did not return after the stream ended")
	}
}

// TestSessionEndReleasesAStalledReplay holds the replay send, as a full
// flow-control window does, and then ends the receive loop. The end of the
// receive loop must close the session's connection, so the replay send
// returns and runSession returns.
func TestSessionEndReleasesAStalledReplay(t *testing.T) {
	agent := newSessionTestAgent(t)
	agent.miteDir = t.TempDir()
	agent.cfg.JenkinsURL = startLocalJenkins(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	chunk := &mitev1.LogChunk{RunId: "run-1", Seq: 1, Data: []byte("line\n")}
	if !agent.logRetention.reserve(5) {
		t.Fatal("reserve log chunk")
	}
	agent.logRetention.commit(chunk, 5)

	if err := agent.connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn := agent.conn
	sendEntered := make(chan struct{})
	var enterOnce sync.Once
	stream := &concurrentSendStream{
		sendFn: func(*mitev1.MiteMessage) error {
			enterOnce.Do(func() { close(sendEntered) })
			for conn.GetState() != connectivity.Shutdown {
				conn.WaitForStateChange(context.Background(), conn.GetState())
			}
			return errors.New("connection closed")
		},
		recvFn: func() (*mitev1.OperatorMessage, error) {
			<-sendEntered
			return nil, io.EOF
		},
	}
	agent.installSession(&miteSession{stream: stream, conn: conn, client: agent.client})

	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runSession(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runSession did not return while the replay send was stalled")
	}
	if got := conn.GetState(); got != connectivity.Shutdown {
		t.Errorf("connection state = %v, want Shutdown", got)
	}
}
