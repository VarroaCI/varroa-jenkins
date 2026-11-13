package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// activityExecer runs one statement that returns a row. A pgx transaction and
// a pgx pool both satisfy it.
type activityExecer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// activityOutbox holds the activity events that one transaction wrote. The
// events reach the bus only after the commit, so a rolled-back write publishes
// nothing. The zero value is an empty outbox.
type activityOutbox struct {
	events []activity.Event
}

// insertActivityForPublish writes one activity row through db and queues the
// event for publish when the row is new. Every recorder path that writes its
// own activity row uses this helper. The timestamp is truncated to the
// microsecond that the table stores, so the event key of a re-ingested copy
// matches the key of the row. A duplicate writes nothing and queues nothing.
func insertActivityForPublish(ctx context.Context, db activityExecer, e activity.Event, out *activityOutbox) error {
	e.Timestamp = e.Timestamp.UTC().Truncate(time.Microsecond)
	e.Normalize()
	payloadJSON, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal activity payload: %w", err)
	}
	var key string
	err = db.QueryRow(ctx, `
		INSERT INTO varroa.activity
			(event_key, ts, type, source, severity, cluster, namespace, controller,
			 actor, message, phase, reason, item_path, build_number, result, url, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (ts, event_key) DO NOTHING
		RETURNING event_key
	`,
		activity.EventKey(e), e.Timestamp, e.Type, e.Source, e.Severity, e.Cluster, e.Namespace,
		e.Controller, e.Actor, e.Message, e.Phase, e.Reason, e.ItemPath,
		nullableBuildNumber(e.BuildNumber), e.Result, e.URL, string(payloadJSON),
	).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert activity row: %w", err)
	}
	if out != nil {
		out.events = append(out.events, e)
	}
	return nil
}

// publishActivityAfterCommit gives each queued event to activityPub, then
// empties the outbox. A nil activityPub publishes nothing and still empties
// the outbox. The data must be the bytes that the row was written from. The
// activity-recorder consumer then finds that row and writes no second row.
// Publish can set the cluster and the timestamp, so this path uses PublishData.
func publishActivityAfterCommit(activityPub *activity.Publisher, out *activityOutbox) {
	if out == nil {
		return
	}
	events := out.events
	out.events = nil
	if activityPub == nil {
		return
	}
	for _, e := range events {
		subject := bus.ActivityGlobal(e.Cluster)
		// A controller without a namespace has no valid subject. It routes to
		// _global, as Publisher.routeSubject does, so ack mode does not retry it.
		if e.Controller != "" && e.Namespace != "" {
			subject = bus.ActivitySubject(e.Cluster, e.Namespace, e.Controller)
		}
		data, err := json.Marshal(e)
		if err != nil {
			slog.Default().Warn("marshal activity publish", "type", e.Type, "error", err)
			continue
		}
		activityPub.PublishData(subject, e.Type, data)
	}
}
