package loop

import (
	"context"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// Sweep expires the approvals that are due, and it ends the sessions whose
// lease has lapsed. A lapsed run is recorded as lost.
func Sweep(ctx context.Context, st store.Store) error {
	_, _, err := st.Sweep(ctx)
	return err
}

// StartSweeper runs Sweep every interval until ctx is done.
func StartSweeper(ctx context.Context, st store.Store, every time.Duration, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := Sweep(ctx, st); err != nil && ctx.Err() == nil {
				logger.Warn("assistant sweep failed", "error", err.Error())
			}
		}
	}
}
