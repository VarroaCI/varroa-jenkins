package mite

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// logAckMessage returns one log ack for seq.
func logAckMessage(seq int64) *mitev1.OperatorMessage {
	return &mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_LogAck{LogAck: &mitev1.LogAck{
		RunId: "run-1", AckSeq: seq, Status: logAckStatusStored,
	}}}
}

// TestStreamSenderBoundsAStalledSend holds the Send of the stream, as a
// full flow-control window does. The caller gets errStreamStalled after
// the bound, every later send gets it at once, and the stall is reported
// once.
func TestStreamSenderBoundsAStalledSend(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	stalls := make(chan string, 4)
	s := newStreamSender(func(*mitev1.OperatorMessage) error {
		<-done
		return errors.New("stream ended")
	}, done, 50*time.Millisecond, func(kind string) { stalls <- kind })

	errc := make(chan error, 1)
	go func() { errc <- s.Send(logAckMessage(1)) }()
	select {
	case err := <-errc:
		if !errors.Is(err, errStreamStalled) {
			t.Fatalf("stalled send = %v, want errStreamStalled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a send that flow control holds did not return")
	}
	select {
	case kind := <-stalls:
		if kind != "*mitev1.OperatorMessage_LogAck" {
			t.Errorf("stall kind = %q, want the log ack type", kind)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stall was not reported")
	}
	if !s.isStalled() {
		t.Error("the sender does not report the stall")
	}
	if err := s.Send(logAckMessage(2)); !errors.Is(err, errStreamStalled) {
		t.Errorf("send after the stall = %v, want errStreamStalled", err)
	}
	select {
	case kind := <-stalls:
		t.Errorf("the stall was reported again with %q", kind)
	default:
	}
}

// TestStreamSenderPassesResultsInOrder sends three messages through a
// writer whose Send fails the third. Each caller gets the result of its
// own Send, and the writer sends in the order of the calls.
func TestStreamSenderPassesResultsInOrder(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	fail := errors.New("send failed")
	var mu sync.Mutex
	var sent []int64
	s := newStreamSender(func(msg *mitev1.OperatorMessage) error {
		seq := msg.GetLogAck().AckSeq
		mu.Lock()
		sent = append(sent, seq)
		mu.Unlock()
		if seq == 3 {
			return fail
		}
		return nil
	}, done, time.Minute, nil)

	for seq := int64(1); seq <= 3; seq++ {
		err := s.Send(logAckMessage(seq))
		if seq < 3 && err != nil {
			t.Fatalf("send %d = %v, want nil", seq, err)
		}
		if seq == 3 && !errors.Is(err, fail) {
			t.Fatalf("send 3 = %v, want the Send error", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(sent) != "[1 2 3]" {
		t.Errorf("sent = %v, want [1 2 3]", sent)
	}
	if s.isStalled() {
		t.Error("a writer whose sends return reports a stall")
	}
}

// TestStreamSenderSendsNothingAfterTheStreamEnds ends the sender, or the
// stream context. A later send answers errStreamEnded, and the Send of the
// stream never runs.
func TestStreamSenderSendsNothingAfterTheStreamEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(s *streamSender, done chan struct{})
	}{
		{"end", func(s *streamSender, _ chan struct{}) { s.end() }},
		{"stream context", func(_ *streamSender, done chan struct{}) { close(done) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var calls atomic.Int32
			s := newStreamSender(func(*mitev1.OperatorMessage) error {
				calls.Add(1)
				return nil
			}, done, time.Minute, nil)
			tc.stop(s, done)
			for seq := int64(1); seq <= 20; seq++ {
				if err := s.Send(logAckMessage(seq)); !errors.Is(err, errStreamEnded) {
					t.Fatalf("send %d after the end = %v, want errStreamEnded", seq, err)
				}
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("Send of the stream ran %d times after the end", n)
			}
			s.end()
			select {
			case <-done:
			default:
				close(done)
			}
		})
	}
}

// TestStreamSendStallBelowPresenceStale pins the order of the two bounds:
// a stalled stream ends before the operator reads its heartbeat as stale.
func TestStreamSendStallBelowPresenceStale(t *testing.T) {
	if streamSendStallAfter >= bus.PresenceStaleAfter {
		t.Errorf("streamSendStallAfter %s must stay below PresenceStaleAfter %s",
			streamSendStallAfter, bus.PresenceStaleAfter)
	}
}

// ackingHandler answers each log chunk on the read loop with an ack of
// about 1 KiB, as sendLogAck answers a refused chunk inline.
type ackingHandler struct {
	recordingHandler
	mu           sync.Mutex
	send         Sender
	heartbeats   chan struct{}
	disconnected chan struct{}
}

func (h *ackingHandler) OnConnect(_, _, _ string, _ interface{}, send Sender) int64 {
	h.mu.Lock()
	h.send = send
	h.mu.Unlock()
	return 1
}

func (h *ackingHandler) OnLogChunk(_, _ string, chunk *mitev1.LogChunk) {
	h.mu.Lock()
	send := h.send
	h.mu.Unlock()
	_ = send(&mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_LogAck{LogAck: &mitev1.LogAck{
		RunId: chunk.RunId, AckSeq: chunk.Seq, Status: logAckStatusBusy, Reason: strings.Repeat("x", 1024),
	}}})
}

func (h *ackingHandler) OnHeartbeat(_, _ string, _ *mitev1.Heartbeat) {
	select {
	case h.heartbeats <- struct{}{}:
	default:
	}
}

func (h *ackingHandler) OnDisconnect(_, _ string, _ int64) { close(h.disconnected) }

// startAckingGateway serves one ackingHandler with the given send bound,
// and writes the server log to logs.
func startAckingGateway(t *testing.T, stallAfter time.Duration, logs io.Writer) (*ackingHandler, string, *ca.CA) {
	t.Helper()
	certAuth, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}
	h := &ackingHandler{heartbeats: make(chan struct{}, 1), disconnected: make(chan struct{})}
	srv := NewServer(certAuth, NewTokenSigner([]byte(certAuth.PrivateKey())), h, "localhost")
	srv.sendStallAfter = stallAfter
	srv.Logger = slog.New(slog.NewTextHandler(logs, nil))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.GRPCServer().Serve(lis) }()
	t.Cleanup(srv.GRPCServer().Stop)
	return h, lis.Addr().String(), certAuth
}

// pinnedWindow keeps the flow-control windows of a test client at the
// HTTP/2 default of 64 KiB. A fixed window turns off the window growth
// of gRPC, so a backlog fills the window at a known size.
func pinnedWindow() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithInitialWindowSize(64 << 10), grpc.WithInitialConnWindowSize(64 << 10)}
}

// logChunkMessage returns one log chunk of run-1 for seq.
func logChunkMessage(seq int64) *mitev1.MiteMessage {
	return &mitev1.MiteMessage{Message: &mitev1.MiteMessage_LogChunk{LogChunk: &mitev1.LogChunk{
		RunId: "run-1", Seq: seq, Data: []byte("line\n"),
	}}}
}

// backlogChunks is the size of the test backlog. Its acks hold about
// 512 KiB, eight times the 64 KiB window.
const backlogChunks = 512

// TestGatewayIntegration_UnreadAcksEndTheStream replays a backlog the way
// the mite did before its receive loop ran first: every chunk, and no
// read. The acks outgrow the flow-control window, and the Send of the next
// ack waits with no deadline of its own. The bound must end the stream with
// Unavailable, and the gateway must log the stall with the message type.
func TestGatewayIntegration_UnreadAcksEndTheStream(t *testing.T) {
	var out lockedBuffer
	h, addr, certAuth := startAckingGateway(t, 200*time.Millisecond, &out)
	const ns, name = "stall-ns", "ctl"
	gconn := dialWithMiteCert(t, addr, certAuth, name, ns, pinnedWindow()...)
	stream, end := openSilentStream(t, addr, func() mitev1.MiteClient { return mitev1.NewMiteClient(gconn) }, name, ns)
	defer end()

	go func() {
		for seq := int64(1); seq <= backlogChunks; seq++ {
			if err := stream.Send(logChunkMessage(seq)); err != nil {
				return
			}
		}
	}()

	select {
	case <-h.disconnected:
	case <-time.After(20 * time.Second):
		t.Fatal("the gateway kept the stream while the mite read no ack")
	}
	var err error
	for err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("the stream ended with %v, want Unavailable with the stall message", err)
	}
	if logs := out.String(); !strings.Contains(logs, "mite stream stalled") || !strings.Contains(logs, "OperatorMessage_LogAck") {
		t.Errorf("gateway log = %q, want the stall WARN line that names the log ack", logs)
	}
}

// pushingHandler sends messages of about 1 KiB from its own goroutine
// after the connect, as the desired-state bridge does, until a send fails.
type pushingHandler struct {
	recordingHandler
	disconnected chan struct{}
	pushErr      chan error
}

func (h *pushingHandler) OnConnect(_, _ string, _ string, _ interface{}, send Sender) int64 {
	go func() {
		for seq := int64(1); ; seq++ {
			if err := send(&mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_LogAck{LogAck: &mitev1.LogAck{
				RunId: "run-1", AckSeq: seq, Status: logAckStatusBusy, Reason: strings.Repeat("x", 1024),
			}}}); err != nil {
				h.pushErr <- err
				return
			}
		}
	}()
	return 1
}

func (h *pushingHandler) OnDisconnect(_, _ string, _ int64) { close(h.disconnected) }

// TestGatewayIntegration_MiteThatNeitherReadsNorSendsEndsTheStream covers
// a mite that sends its hello and then neither reads nor sends. The
// gateway pushes from another goroutine until the window is full. The read
// loop waits for a message that never comes, so only the stall can end the
// stream. The stream must end with Unavailable at the send bound, and the
// blocked send must return.
func TestGatewayIntegration_MiteThatNeitherReadsNorSendsEndsTheStream(t *testing.T) {
	certAuth, err := ca.NewCA()
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}
	h := &pushingHandler{disconnected: make(chan struct{}), pushErr: make(chan error, 1)}
	var out lockedBuffer
	srv := NewServer(certAuth, NewTokenSigner([]byte(certAuth.PrivateKey())), h, "localhost")
	srv.sendStallAfter = 200 * time.Millisecond
	srv.Logger = slog.New(slog.NewTextHandler(&out, nil))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.GRPCServer().Serve(lis) }()
	t.Cleanup(srv.GRPCServer().Stop)

	const ns, name = "quiet-ns", "ctl"
	addr := lis.Addr().String()
	gconn := dialWithMiteCert(t, addr, certAuth, name, ns, pinnedWindow()...)
	stream, end := openSilentStream(t, addr, func() mitev1.MiteClient { return mitev1.NewMiteClient(gconn) }, name, ns)
	defer end()

	select {
	case <-h.disconnected:
	case <-time.After(20 * time.Second):
		t.Fatal("the gateway kept the stream of a mite that neither reads nor sends")
	}
	select {
	case err := <-h.pushErr:
		if !errors.Is(err, errStreamStalled) && !errors.Is(err, errStreamEnded) {
			t.Errorf("blocked send = %v, want errStreamStalled or errStreamEnded", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the blocked send did not return after the stream ended")
	}
	if logs := out.String(); !strings.Contains(logs, "mite stream stalled") {
		t.Errorf("gateway log = %q, want the stall WARN line", logs)
	}
	for err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("the stream ended with %v, want Unavailable with the stall message", err)
	}
}

// TestGatewayIntegration_ReadingMiteTakesABacklog sends the same backlog
// while the receive loop already runs, as the mite does now. Every ack
// arrives, the heartbeat after the backlog reaches the gateway, and the
// stream stays open.
func TestGatewayIntegration_ReadingMiteTakesABacklog(t *testing.T) {
	h, addr, certAuth := startAckingGateway(t, 5*time.Second, io.Discard)
	const ns, name = "backlog-ns", "ctl"
	gconn := dialWithMiteCert(t, addr, certAuth, name, ns, pinnedWindow()...)
	stream, end := openSilentStream(t, addr, func() mitev1.MiteClient { return mitev1.NewMiteClient(gconn) }, name, ns)
	defer end()

	acks := make(chan error, 1)
	go func() {
		for n := 0; n < backlogChunks; n++ {
			msg, err := stream.Recv()
			if err != nil {
				acks <- err
				return
			}
			if msg.GetLogAck() == nil {
				acks <- fmt.Errorf("unexpected message %T", msg.Message)
				return
			}
		}
		acks <- nil
	}()

	for seq := int64(1); seq <= backlogChunks; seq++ {
		if err := stream.Send(logChunkMessage(seq)); err != nil {
			t.Fatalf("send chunk %d: %v", seq, err)
		}
	}
	if err := stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_Heartbeat{
		Heartbeat: &mitev1.Heartbeat{Version: "v1"},
	}}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}

	select {
	case <-h.heartbeats:
	case <-time.After(20 * time.Second):
		t.Fatal("the heartbeat after the backlog did not reach the gateway")
	}
	select {
	case err := <-acks:
		if err != nil {
			t.Fatalf("read acks: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the mite did not get every ack")
	}
	select {
	case <-h.disconnected:
		t.Fatal("the gateway ended a stream whose mite reads")
	default:
	}
}
