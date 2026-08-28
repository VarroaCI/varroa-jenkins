package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// tickerRunnable runs a named reconcile function on a fixed interval, by
// default only on the elected leader. It implements manager.Runnable and
// manager.LeaderElectionRunnable: controller-runtime starts it after this
// replica wins the varroa-operator.varroa.dev lease and cancels ctx when the
// manager stops. Set everyReplica for read-only loops that must feed the
// sharded Controller reconciler, which runs on all replicas.
type tickerRunnable struct {
	name         string
	interval     time.Duration
	jitter       time.Duration                   // optional ±jitter applied per wait, de-phasing replicas
	immediate    bool                            // run one tick right after start (before the first interval)
	everyReplica bool                            // run on every replica, not just the leader
	setup        func(ctx context.Context) error // optional one-shot; error fails manager start
	tick         func(ctx context.Context)       // the loop body; must honor ctx
	logger       *slog.Logger
}

func (t *tickerRunnable) NeedLeaderElection() bool { return !t.everyReplica }

// nextWait returns the interval with ±jitter applied.
func (t *tickerRunnable) nextWait() time.Duration {
	if t.jitter <= 0 {
		return t.interval
	}
	return t.interval - t.jitter + time.Duration(rand.Int64N(int64(2*t.jitter)))
}

func (t *tickerRunnable) Start(ctx context.Context) error {
	if t.setup != nil {
		if err := t.setup(ctx); err != nil {
			return fmt.Errorf("%s setup: %w", t.name, err)
		}
	}
	t.logger.Info("reconciler runnable started", "runnable", t.name, "interval", t.interval, "everyReplica", t.everyReplica)
	if t.immediate {
		t.tick(ctx)
	}
	timer := time.NewTimer(t.nextWait())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			t.logger.Info("reconciler runnable stopped", "runnable", t.name)
			return nil
		case <-timer.C:
			t.tick(ctx)
			timer.Reset(t.nextWait())
		}
	}
}
