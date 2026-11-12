package controller

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// replayLanes runs queued webhook replays. A lane runs its messages one at a
// time, in the order that add received them. Lanes run at the same time, so a
// replay that waits for its result holds only its own lane.
//
// A message is held from add until its handler returns. A held message must
// get a progress ack more often than the consumer AckWait. Otherwise the server
// delivers it again while a lane still holds it.
type replayLanes struct {
	ctx    context.Context
	handle func(context.Context, *nats.Msg)
	// progress sends a progress ack. release returns a message that no handler
	// ran. Tests replace both before the first add.
	progress func(*nats.Msg)
	release  func(*nats.Msg)
	// pastBound reports a message past the replay bound. refresh hands each
	// waiting message that pastBound reports to expire, which terms it and
	// records the drop. Tests replace both before the first add.
	pastBound func(*nats.Msg) bool
	expire    func(*nats.Msg)

	mu    sync.Mutex
	lanes map[string]*replayLane
	seqs  map[uint64]bool // stream sequences of held messages
	wg    sync.WaitGroup
}

// replayLane is one lane. current is the message that the lane handles now.
// queue holds the messages that wait, oldest first.
type replayLane struct {
	current *nats.Msg
	queue   []heldReplay
}

type heldReplay struct {
	msg *nats.Msg
	seq uint64
}

func newReplayLanes(ctx context.Context, handle func(context.Context, *nats.Msg)) *replayLanes {
	return &replayLanes{
		ctx:      ctx,
		handle:   handle,
		progress: func(m *nats.Msg) { _ = m.InProgress() },
		release:  func(m *nats.Msg) { _ = m.NakWithDelay(replayResultTimeout) },
		pastBound: func(m *nats.Msg) bool {
			return pastReplayBound(m, time.Now())
		},
		expire: func(m *nats.Msg) { _ = m.Term() },
		lanes:  make(map[string]*replayLane),
		seqs:   make(map[uint64]bool),
	}
}

// add queues msg on the lane that key names, and starts that lane when it is
// idle. seq is the stream sequence of msg, or zero when it is not known. add
// reports false, and queues nothing, when a held message has the same
// sequence. The held copy acks or naks that sequence for both.
func (l *replayLanes) add(key string, seq uint64, msg *nats.Msg) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq != 0 {
		if l.seqs[seq] {
			return false
		}
		l.seqs[seq] = true
	}
	lane, ok := l.lanes[key]
	if !ok {
		lane = &replayLane{}
		l.lanes[key] = lane
		l.wg.Add(1)
		go l.run(key, lane)
	}
	lane.queue = append(lane.queue, heldReplay{msg: msg, seq: seq})
	return true
}

// run handles the messages of one lane until its queue is empty. After the
// context ends, it releases each message and runs no handler.
func (l *replayLanes) run(key string, lane *replayLane) {
	defer l.wg.Done()
	for {
		l.mu.Lock()
		if len(lane.queue) == 0 {
			delete(l.lanes, key)
			l.mu.Unlock()
			return
		}
		next := lane.queue[0]
		lane.queue = lane.queue[1:]
		lane.current = next.msg
		l.mu.Unlock()

		if l.ctx.Err() != nil {
			l.release(next.msg)
		} else {
			l.handle(l.ctx, next.msg)
		}

		l.mu.Lock()
		lane.current = nil
		if next.seq != 0 {
			delete(l.seqs, next.seq)
		}
		l.mu.Unlock()
	}
}

// refresh sends a progress ack for every held message. It also expires each
// waiting message that is past the replay bound. A handler in progress has its
// own give-up timer, so the sweep never touches the current message.
func (l *replayLanes) refresh() {
	// Collect the messages under the lock, and send the acks after it. A slow
	// publish must not block add or the lane handoffs.
	l.mu.Lock()
	var held, expired []*nats.Msg
	for _, lane := range l.lanes {
		if lane.current != nil {
			held = append(held, lane.current)
		}
		lane.queue = slices.DeleteFunc(lane.queue, func(h heldReplay) bool {
			if !l.pastBound(h.msg) {
				return false
			}
			expired = append(expired, h.msg)
			if h.seq != 0 {
				delete(l.seqs, h.seq)
			}
			return true
		})
		for _, h := range lane.queue {
			held = append(held, h.msg)
		}
	}
	l.mu.Unlock()
	for _, msg := range expired {
		l.expire(msg)
	}
	for _, msg := range held {
		l.progress(msg)
	}
}

// keepAlive calls refresh every replayHoldRefresh until the context ends.
func (l *replayLanes) keepAlive() {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(replayHoldRefresh)
		defer t.Stop()
		for {
			select {
			case <-l.ctx.Done():
				return
			case <-t.C:
				l.refresh()
			}
		}
	}()
}

// wait blocks until every lane and the keep-alive loop return. Call it only
// after the last add.
func (l *replayLanes) wait() { l.wg.Wait() }
