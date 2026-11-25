package mite

import (
	"errors"
	"fmt"
	"sync"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// streamSendStallAfter bounds one Send on a mite stream. gRPC Send has no
// deadline. It waits while the flow-control window of the stream is full,
// and the window stays full while the mite reads nothing. The bound must
// stay below bus.PresenceStaleAfter, so the gateway ends a stalled stream
// before the operator reads its heartbeat as stale. One message is at most
// 4 MiB, the gRPC receive limit of the mite, so a mite that reads gets each
// message well inside the bound.
const streamSendStallAfter = 60 * time.Second

var (
	// errStreamStalled answers a send on a stream after one send passed
	// the stall bound.
	errStreamStalled = errors.New("mite stream stalled: the mite reads no message")
	// errStreamEnded answers a send after the stream ended.
	errStreamEnded = errors.New("mite stream ended")
)

// streamSender owns every Send of one CommandStream. Only its writer
// goroutine calls Send, so two sends never overlap. A caller waits for the
// result of its own send. The wait ends when the Send returns, when one
// Send passes the stall bound, or when the stream ends.
//
// No caller calls Send itself. Only the end of the stream releases a Send
// that flow control holds, and gRPC ends a server stream only when the
// CommandStream handler returns. The handler runs the read loop, so a read
// loop that waits on Send can never end its own stream.
type streamSender struct {
	send       func(*mitev1.OperatorMessage) error
	stallAfter time.Duration
	onStall    func(kind string)
	done       <-chan struct{}

	reqs      chan streamSendRequest
	stalled   chan struct{}
	stallOnce sync.Once
	ended     chan struct{}
	endOnce   sync.Once
}

// streamSendRequest is one message for the writer, with the channel that
// carries the result of its Send.
type streamSendRequest struct {
	msg    *mitev1.OperatorMessage
	result chan error
}

// newStreamSender starts the writer of one stream. send is the Send of the
// stream, and done is the done channel of the stream context. onStall runs
// once, with the Go type of the message whose Send passed stallAfter.
func newStreamSender(send func(*mitev1.OperatorMessage) error, done <-chan struct{}, stallAfter time.Duration, onStall func(kind string)) *streamSender {
	s := &streamSender{
		send:       send,
		stallAfter: stallAfter,
		onStall:    onStall,
		done:       done,
		reqs:       make(chan streamSendRequest),
		stalled:    make(chan struct{}),
		ended:      make(chan struct{}),
	}
	go s.run()
	return s
}

// run is the writer. It sends one message at a time. A watchdog marks the
// stream stalled when one Send passes the stall bound. That Send returns
// only when the stream ends.
func (s *streamSender) run() {
	for {
		select {
		case <-s.done:
			return
		case <-s.ended:
			return
		case req := <-s.reqs:
			if s.isEnded() {
				req.result <- errStreamEnded
				continue
			}
			msg := req.msg
			watchdog := time.AfterFunc(s.stallAfter, func() {
				s.markStalled(fmt.Sprintf("%T", msg.Message))
			})
			err := s.send(msg)
			watchdog.Stop()
			req.result <- err
		}
	}
}

// markStalled marks the stream stalled. Only the first call acts.
func (s *streamSender) markStalled(kind string) {
	s.stallOnce.Do(func() {
		close(s.stalled)
		if s.onStall != nil {
			s.onStall(kind)
		}
	})
}

// isStalled reports whether one Send passed the stall bound.
func (s *streamSender) isStalled() bool {
	select {
	case <-s.stalled:
		return true
	default:
		return false
	}
}

// isEnded reports whether the stream context ended or end ran. The writer
// sends nothing after either.
func (s *streamSender) isEnded() bool {
	select {
	case <-s.ended:
		return true
	case <-s.done:
		return true
	default:
		return false
	}
}

// end stops the writer. A send after end answers errStreamEnded. The
// CommandStream handler calls it when it returns.
func (s *streamSender) end() {
	s.endOnce.Do(func() { close(s.ended) })
}

// Send gives msg to the writer and waits for the result of its Send. A
// Send that passes the stall bound answers errStreamStalled, and every
// later send answers the same at once. The message of that Send can still
// reach the mite, when the mite reads before the stream ends.
func (s *streamSender) Send(msg *mitev1.OperatorMessage) error {
	if s.isStalled() {
		return errStreamStalled
	}
	req := streamSendRequest{msg: msg, result: make(chan error, 1)}
	select {
	case s.reqs <- req:
	case <-s.stalled:
		return errStreamStalled
	case <-s.ended:
		return errStreamEnded
	case <-s.done:
		return errStreamEnded
	}
	select {
	case err := <-req.result:
		return err
	case <-s.stalled:
		return errStreamStalled
	}
}
