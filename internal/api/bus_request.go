package api

import (
	"context"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// connRequestWithContext returns a request seam that ends the NATS request
// when ctx ends.
func connRequestWithContext(conn *bus.Conn) func(ctx context.Context, subject string, data []byte, timeout time.Duration) ([]byte, error) {
	return func(ctx context.Context, subject string, data []byte, timeout time.Duration) ([]byte, error) {
		resp, err := conn.RequestWithContext(ctx, subject, data, timeout)
		if err != nil {
			return nil, err
		}
		return resp.Data, nil
	}
}

// requestWithFallback sends through withCtx when it is set. Otherwise it runs
// plain on a goroutine and returns when ctx ends. plain then runs until its
// timeout.
func requestWithFallback(
	ctx context.Context,
	withCtx func(context.Context, string, []byte, time.Duration) ([]byte, error),
	plain func(string, []byte, time.Duration) ([]byte, error),
	subject string, data []byte, timeout time.Duration,
) ([]byte, error) {
	if withCtx != nil {
		return withCtx(ctx, subject, data, timeout)
	}
	type reply struct {
		data []byte
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		d, err := plain(subject, data, timeout)
		ch <- reply{d, err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
