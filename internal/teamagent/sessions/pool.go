package sessions

import (
	"context"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
)

// DefaultPoolSize is the number of sessions one replica runs at once.
const DefaultPoolSize = 8

// Pool bounds the sessions that one replica runs. A slot is reserved before a
// row is created, so a full pool naks the message instead of starting work.
type Pool struct {
	slots chan struct{}
	// Run executes one session to its end. The pool calls it on its own
	// goroutine and releases the slot when it returns.
	Run func(ctx context.Context, sess agent.Session, trigger loop.Trigger)
	ctx context.Context
}

// NewPool returns a pool of size slots. ctx bounds every session the pool runs.
func NewPool(ctx context.Context, size int, run func(ctx context.Context, sess agent.Session, trigger loop.Trigger)) *Pool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	return &Pool{slots: make(chan struct{}, size), Run: run, ctx: ctx}
}

// TryReserve takes a slot without waiting. It reports false when none is free.
func (p *Pool) TryReserve() bool {
	select {
	case p.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot taken by TryReserve. A reservation that does not start a
// session must release it.
func (p *Pool) Release() {
	<-p.slots
}

// Start runs sess with trigger on its own goroutine and frees the slot when
// the run ends. A resumed session passes loop.TriggerResume. The caller holds a
// reservation for it.
func (p *Pool) Start(sess agent.Session, trigger loop.Trigger) {
	go func() {
		defer p.Release()
		p.Run(p.ctx, sess, trigger)
	}()
}
