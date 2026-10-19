package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// activityNoPartitionCode is the SQLSTATE Postgres raises when no RANGE
// partition of "activity" covers a row's "ts". D3's disposition table
// creates that month's partition and retries the insert once.
const activityNoPartitionCode = "23514"

// ActivityIngester writes one activity.Event to the Postgres "activity"
// table. The recorder is its only writer.
type ActivityIngester struct {
	pool *pgxpool.Pool
}

// NewActivityIngester returns an ActivityIngester over pool.
func NewActivityIngester(pool *pgxpool.Pool) *ActivityIngester {
	return &ActivityIngester{pool: pool}
}

// Insert writes e's typed columns and its complete JSON payload in one
// statement, with a conflict-ignoring clause on the primary key
// (ts, event_key). key is the caller's precomputed activity.EventKey(e). Insert
// reports whether the row was newly written; false means the conflict
// clause matched an existing row, which a redelivery relies on to avoid
// a duplicate.
func (a *ActivityIngester) Insert(ctx context.Context, e activity.Event, key string) (bool, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return false, fmt.Errorf("marshal activity payload: %w", err)
	}

	tag, err := a.pool.Exec(ctx, `
		INSERT INTO varroa.activity
			(event_key, ts, type, source, severity, cluster, namespace, controller,
			 actor, message, phase, reason, item_path, build_number, result, url, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (ts, event_key) DO NOTHING
	`,
		key, e.Timestamp, e.Type, e.Source, e.Severity, e.Cluster, e.Namespace, e.Controller,
		e.Actor, e.Message, e.Phase, e.Reason, e.ItemPath, nullableBuildNumber(e.BuildNumber),
		e.Result, e.URL, string(payload),
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// nullableBuildNumber returns nil for a zero build number, so the column
// stores SQL NULL rather than 0, matching D1's nullable build_number
// column.
func nullableBuildNumber(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// isNoActivityPartition reports whether err is the SQLSTATE Postgres
// raises when no RANGE partition of "activity" covers a row's "ts".
func isNoActivityPartition(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == activityNoPartitionCode
}
