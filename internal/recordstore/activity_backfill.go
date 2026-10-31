package recordstore

import (
	"context"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// maxActivityBatches bounds pgBackfill's predicate-driven batch loop (D4).
// The scope pushdown makes a rejected row rare, so the bound rarely
// fires; when it does, HasMore stays true and the caller learns the true
// state from the next page.
const maxActivityBatches = 10

// activityRecentLimit is Recent's default when the caller passes limit<=0,
// matching the retired ring/JetStream backfills' default.
const activityRecentLimit = 200

// pgBackfill is the activity.Backfill implementation over a Reader. It
// lives in this package, not internal/api/activity, because it calls
// Reader.ListActivity and internal/recordstore already imports
// internal/api/activity for the Event type — the reverse import would
// cycle.
type pgBackfill struct {
	reader Reader
}

// NewActivityBackfill returns an activity.Backfill backed by reader.
func NewActivityBackfill(reader Reader) activity.Backfill {
	return &pgBackfill{reader: reader}
}

// Retention reports ("on", days), where days comes from
// Reader.ActivityRetentionDays. A failed read reports ("off", 0). Only the
// deny-by-default page of GET /activity reads it. Query reads the days
// itself and returns the error.
func (b *pgBackfill) Retention() (string, int) {
	days, err := b.reader.ActivityRetentionDays(context.Background())
	if err != nil {
		return "off", 0
	}
	return "on", days
}

// Recent returns up to limit most-recent events, newest-first, with no
// per-event authorization — it carries forward the retired ring/JetStream
// backfills' contract, which never applied one either. scope parses the
// same "" / "ns/name" / "cluster/ns/name" forms those implementations
// accepted.
func (b *pgBackfill) Recent(ctx context.Context, scope string, limit int) ([]activity.Event, error) {
	if limit <= 0 {
		limit = activityRecentLimit
	}
	q := ActivityQuery{Limit: limit, Scope: activity.Scope{All: true}}
	if scope != "" {
		parts := strings.SplitN(scope, "/", 3)
		switch len(parts) {
		case 3: // "cluster/ns/name"
			q.Cluster, q.Namespace, q.Controller = parts[0], parts[1], parts[2]
		case 2: // "ns/name"
			q.Namespace, q.Controller = parts[0], parts[1]
		}
	}
	page, err := b.reader.ListActivity(ctx, q)
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// Query implements the D4 batch-and-filter loop: it pages ListActivity
// (already scope-filtered in SQL), applies q.Authorize to every row
// ListActivity returns, and fetches another batch when the predicate
// leaves fewer than limit matches and ListActivity reports more rows
// exist — up to maxActivityBatches.
//
// Every item ListActivity returns is a "scanned candidate" whether or not
// Authorize accepts it. When accepted reaches limit mid-batch, NextCursor
// anchors on that accepted candidate. The batch's own trailing cursor would
// skip every candidate after the stopping point. HasMore is then true only
// when a later row of the batch passes Authorize or ListActivity reports more
// rows. Otherwise the page is the last one and carries no cursor.
// When a whole batch is consumed without filling limit, NextCursor is
// that batch's own cursor (ListActivity already anchors it on the
// limit-th kept row, so continuing from it re-scans nothing this call
// already processed).
//
// Query reads the retention days first and returns a read error at once,
// so an unreachable store answers ErrUnavailable. A range Start before the
// retention window returns activity.ErrRangeOutsideRetention.
func (b *pgBackfill) Query(ctx context.Context, q activity.Query) (activity.Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 250 {
		limit = 250
	}

	days, err := b.reader.ActivityRetentionDays(ctx)
	if err != nil {
		return activity.Page{}, err
	}
	// Zero days set no bound, the same rule as the cursor expiry check.
	if q.Start != nil && days > 0 && q.Start.Before(time.Now().Add(-time.Duration(days)*24*time.Hour)) {
		return activity.Page{}, activity.ErrRangeOutsideRetention
	}

	rq := ActivityQuery{
		Cluster: q.Cluster, Controller: q.Controller, Namespace: q.Namespace,
		Source: q.Source, Severity: q.Severity, Actor: q.Actor, Type: q.Type,
		Start: q.Start, End: q.End, Limit: limit, Cursor: q.Cursor, Scope: q.Scope,
		ExcludeRoutineSignIns: q.ExcludeRoutineSignIns,
	}
	direction := "backward"
	if q.Start != nil {
		direction = "forward"
	}
	filters := activityFilterKey(rq)

	var accepted []activity.Event
	nextCursor := ""
	hasMore := false

	for batch := 0; batch < maxActivityBatches; batch++ {
		page, err := b.reader.ListActivity(ctx, rq)
		if err != nil {
			return activity.Page{}, err
		}

		filled := false
		for i, e := range page.Items {
			if q.Authorize != nil && !q.Authorize(e) {
				continue
			}
			accepted = append(accepted, e)
			if len(accepted) < limit {
				continue
			}
			filled = true
			hasMore = page.HasMore || anyAuthorized(page.Items[i+1:], q.Authorize)
			nextCursor = ""
			if hasMore {
				nextCursor = encodeActivityCursor(activityCursor{Direction: direction, Filters: filters, TS: e.Timestamp, EventKey: activity.EventKey(e)})
			}
			break
		}
		if filled {
			break
		}
		if !page.HasMore {
			nextCursor = ""
			hasMore = false
			break
		}
		nextCursor = page.NextCursor
		hasMore = true
		rq.Cursor = page.NextCursor
	}

	result := activity.Page{Items: accepted, RetentionMode: "on", RetentionDays: days, HasMore: hasMore}
	if hasMore {
		result.NextCursor = nextCursor
	}
	return result, nil
}

// anyAuthorized reports whether authorize accepts one or more of events.
// A nil authorize accepts every event.
func anyAuthorized(events []activity.Event, authorize func(activity.Event) bool) bool {
	for _, e := range events {
		if authorize == nil || authorize(e) {
			return true
		}
	}
	return false
}
