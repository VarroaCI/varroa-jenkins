// Package spend keeps the team_agent_spend rollup current. The status ticker
// calls Rollover on each pass. The rollup is written per Team and UTC day, so
// the rows of a day stay correct after its last session ends.
package spend

import (
	"context"
	"time"
)

// Upserter writes the rollup row of one Team and UTC day.
type Upserter interface {
	UpsertSpend(ctx context.Context, team string, day time.Time) error
}

// Rollover refreshes the rollup of the accepted Teams. On the first pass of a
// UTC day it also writes the previous day, so the sessions that ended after
// midnight count against the day they started on.
type Rollover struct {
	Store Upserter
	day   string
}

// Tick refreshes today's rows for teams. On a new UTC day it refreshes the
// previous day first. A failed write stops the pass, so the next pass retries.
func (r *Rollover) Tick(ctx context.Context, teams []string, now time.Time) error {
	today := now.UTC().Truncate(24 * time.Hour)
	if key := today.Format("2006-01-02"); key != r.day {
		for _, team := range teams {
			if err := r.Store.UpsertSpend(ctx, team, today.AddDate(0, 0, -1)); err != nil {
				return err
			}
		}
		r.day = key
	}
	for _, team := range teams {
		if err := r.Store.UpsertSpend(ctx, team, today); err != nil {
			return err
		}
	}
	return nil
}
