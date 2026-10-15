package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// maxDownstreamRunPageLimit caps one page of ListDownstreamRuns.
const maxDownstreamRunPageLimit = 200

// ErrInvalidRunCursor means a keyset cursor does not decode. The caller
// maps it to a client error, not a store failure.
var ErrInvalidRunCursor = errors.New("invalid run cursor")

// ListDownstreamRuns pages the runs that one upstream run started, by
// keyset on (created_at DESC, run_id DESC). The partial index
// runs_upstream_run_id_idx serves the filter. An invalid upstream run id
// is an error, and a run with no downstream runs answers an empty page.
func (r *pgReader) ListDownstreamRuns(ctx context.Context, q DownstreamRunQuery) (RunPage, error) {
	if err := r.checkSchema(ctx); err != nil {
		return RunPage{}, err
	}
	upstreamID, err := uuid.Parse(q.UpstreamRunID)
	if err != nil {
		return RunPage{}, fmt.Errorf("upstream run id is not a uuid: %w", err)
	}

	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultRunPageLimit
	case limit > maxDownstreamRunPageLimit:
		limit = maxDownstreamRunPageLimit
	}

	args := []any{upstreamID}
	clause := "upstream_run_id = $1"
	if q.Cursor != "" {
		cur, err := decodeRunCursor(q.Cursor)
		if err != nil {
			return RunPage{}, fmt.Errorf("%w: %v", ErrInvalidRunCursor, err)
		}
		args = append(args, cur.createdAt, cur.runID)
		clause += " AND (created_at, run_id) < ($2, $3)"
	}
	args = append(args, limit+1)

	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT %s
		FROM varroa.runs
		WHERE %s
		ORDER BY created_at DESC, run_id DESC
		LIMIT $%d
	`, runColumns, clause, len(args)), args...)
	if err != nil {
		return RunPage{}, fmt.Errorf("list downstream runs: %w", err)
	}
	defer rows.Close()

	var page RunPage
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return RunPage{}, fmt.Errorf("scan downstream run: %w", err)
		}
		page.Runs = append(page.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return RunPage{}, fmt.Errorf("list downstream runs: %w", err)
	}

	if len(page.Runs) > limit {
		last := page.Runs[limit-1]
		page.NextCursor = encodeRunCursor(last.CreatedAt, last.RunID)
		page.Runs = page.Runs[:limit]
	}
	return page, nil
}
