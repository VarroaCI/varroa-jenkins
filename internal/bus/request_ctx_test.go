package bus

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// silentSubject subscribes to subj with a handler that never replies.
func silentSubject(t *testing.T, c *Conn, subj string) {
	t.Helper()
	if _, err := c.Subscribe(subj, func(*nats.Msg) {}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := c.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestRequestWithContext_Reply(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	if _, err := c.Subscribe("test.req.echo", func(m *nats.Msg) {
		_ = m.Respond(m.Data)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := c.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	msg, err := c.RequestWithContext(context.Background(), "test.req.echo", []byte("ping"), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if string(msg.Data) != "ping" {
		t.Fatalf("reply data = %q, want %q", msg.Data, "ping")
	}
}

func TestRequestWithContext_CancelReturnsPromptlyAndReleases(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)
	silentSubject(t, c, "test.req.silent.cancel")

	// A first request makes the embedded server start its lazy request
	// goroutines, so they do not count as growth caused by the cancel.
	_, _ = c.RequestWithContext(context.Background(), "test.req.silent.cancel", []byte("warm"), 50*time.Millisecond)

	before := runtime.NumGoroutine()
	subsBefore := c.NATSConn().NumSubscriptions()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := c.RequestWithContext(ctx, "test.req.silent.cancel", []byte("x"), 30*time.Second)
	elapsed := time.Since(start)

	if elapsed >= time.Second {
		t.Fatalf("return took %v, want under 1s", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines = %d after cancel, want <= %d", n, before)
	}
	if got := c.NATSConn().NumSubscriptions(); got != subsBefore {
		t.Fatalf("subscriptions = %d, want %d", got, subsBefore)
	}
}

func TestRequestWithContext_TimeoutReturnsErrTimeout(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)
	silentSubject(t, c, "test.req.silent.timeout")

	_, err := c.RequestWithContext(context.Background(), "test.req.silent.timeout", []byte("x"), 100*time.Millisecond)
	if !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("err = %v, want nats.ErrTimeout", err)
	}
	if !IsNoReply(err) {
		t.Fatalf("IsNoReply(%v) = false, want true", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, must not wrap context.DeadlineExceeded", err)
	}
}

func TestRequestWithContext_ParentDeadlineReturnsCtxErr(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)
	silentSubject(t, c, "test.req.silent.parent")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.RequestWithContext(ctx, "test.req.silent.parent", []byte("x"), 30*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("return took %v, want under 1s", elapsed)
	}
}

func TestRequestWithContext_NoResponders(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	_, err := c.RequestWithContext(context.Background(), "test.req.nobody", []byte("x"), 2*time.Second)
	if !IsNoResponder(err) {
		t.Fatalf("IsNoResponder(%v) = false, want true", err)
	}
}

func TestRequestWithContext_EndedContextPublishesNothing(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	var count atomic.Int32
	if _, err := c.Subscribe("test.req.ended", func(*nats.Msg) {
		count.Add(1)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := c.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.RequestWithContext(ctx, "test.req.ended", []byte("x"), 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if err := c.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("subscriber received %d requests, want 0", got)
	}
}
