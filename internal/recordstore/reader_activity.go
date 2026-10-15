package recordstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// defaultActivityPageLimit is ListActivity's page size when the caller
// gives no limit, or a limit of zero or less. maxActivityPageLimit bounds
// an explicit limit, matching the BFF's existing activity page-size cap.
const (
	defaultActivityPageLimit = 100
	maxActivityPageLimit     = 250
)

// ActivityQuery is ListActivity's filter, scope, and paging input. Every
// filter but Start and End is an exact match; Start and End bound the
// timestamp range. Scope is D5's SQL-pushed-down read scope — a sound
// superset of what the caller may read. The caller applies the
// authoritative per-event predicate afterward; ListActivity does not.
type ActivityQuery struct {
	Cluster, Controller, Namespace, Source, Severity, Actor, Type string
	Start, End                                                    *time.Time
	Limit                                                         int
	Cursor                                                        string
	Scope                                                         activity.Scope
}

// ActivityPage is one page of ListActivity. NextCursor is empty on the
// last page.
type ActivityPage struct {
	Items      []activity.Event
	NextCursor string
	HasMore    bool
}

// activityCursor is ListActivity's decoded keyset position: the primary
// key of the last row a page kept, the direction it was read in, and a
// fingerprint of the filters and scope that produced it. A cursor from a
// different query returns activity.ErrInvalidCursor rather than silently
// resuming at the wrong position.
type activityCursor struct {
	Direction string    `json:"direction"`
	Filters   string    `json:"filters"`
	TS        time.Time `json:"ts"`
	EventKey  string    `json:"eventKey"`
}

// activityFilterKey fingerprints the nine filters ListActivity accepts
// plus the scope, so a cursor minted for one query cannot silently resume
// a differently filtered or differently scoped one.
func activityFilterKey(q ActivityQuery) string {
	start, end := "", ""
	if q.Start != nil {
		start = q.Start.UTC().Format(time.RFC3339Nano)
	}
	if q.End != nil {
		end = q.End.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{
		start, end, q.Cluster, q.Controller, q.Namespace, q.Source, q.Severity, q.Actor, q.Type,
		fmt.Sprintf("%v|%v|%v", q.Scope.All, q.Scope.Global, q.Scope.Namespaces),
	}, "\x1f")
}

func encodeActivityCursor(c activityCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeActivityCursor(raw, filters, direction string) (activityCursor, error) {
	var c activityCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Filters != filters || c.Direction != direction {
		return c, activity.ErrInvalidCursor
	}
	return c, nil
}

// ListActivity pages varroa.activity by keyset on (ts, event_key). A
// default page (Start nil) reads backward, newest first, with
// (ts, event_key) < cursor; an explicit Start/End range reads forward,
// oldest first, with (ts, event_key) > cursor. It reads limit+1 rows and
// keeps the first limit: HasMore is true, and NextCursor anchors on the
// limit-th kept row, exactly when the (limit+1)-th row existed — the
// probe row itself is never returned and is re-scanned by the next page.
func (r *pgReader) ListActivity(ctx context.Context, q ActivityQuery) (ActivityPage, error) {
	if err := r.checkSchema(ctx); err != nil {
		return ActivityPage{}, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultActivityPageLimit
	}
	if limit > maxActivityPageLimit {
		limit = maxActivityPageLimit
	}

	direction := "backward"
	if q.Start != nil {
		direction = "forward"
	}
	filters := activityFilterKey(q)

	var args []any
	var clauses []string
	addEq := func(col, val string) {
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if q.Cluster != "" {
		addEq("cluster", q.Cluster)
	}
	if q.Controller != "" {
		args = append(args, q.Controller, q.Controller)
		clauses = append(clauses, fmt.Sprintf("(controller = $%d OR namespace || '/' || controller = $%d)", len(args)-1, len(args)))
	}
	if q.Namespace != "" {
		addEq("namespace", q.Namespace)
	}
	if q.Source != "" {
		addEq("source", q.Source)
	}
	if q.Severity != "" {
		addEq("severity", q.Severity)
	}
	if q.Actor != "" {
		addEq("actor", q.Actor)
	}
	if q.Type != "" {
		addEq("type", q.Type)
	}
	if q.Start != nil {
		args = append(args, *q.Start)
		clauses = append(clauses, fmt.Sprintf("ts >= $%d", len(args)))
	}
	if q.End != nil {
		args = append(args, *q.End)
		clauses = append(clauses, fmt.Sprintf("ts < $%d", len(args)))
	}
	if !q.Scope.All {
		args = append(args, q.Scope.Namespaces, q.Scope.Global)
		nsIdx, globalIdx := len(args)-1, len(args)
		clauses = append(clauses, fmt.Sprintf("(namespace = ANY($%d) OR ($%d AND controller = ''))", nsIdx, globalIdx))
	}

	if q.Cursor != "" {
		cur, err := decodeActivityCursor(q.Cursor, filters, direction)
		if err != nil {
			return ActivityPage{}, err
		}
		if r.activityRetentionDays > 0 && cur.TS.Before(time.Now().Add(-time.Duration(r.activityRetentionDays)*24*time.Hour)) {
			return ActivityPage{}, activity.ErrCursorExpired
		}
		args = append(args, cur.TS, cur.EventKey)
		op := "<"
		if direction == "forward" {
			op = ">"
		}
		clauses = append(clauses, fmt.Sprintf("(ts, event_key) %s ($%d, $%d)", op, len(args)-1, len(args)))
	}

	where := "TRUE"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}
	orderDir := "DESC"
	if direction == "forward" {
		orderDir = "ASC"
	}
	args = append(args, limit+1)
	sql := fmt.Sprintf(`
		SELECT event_key, ts, payload
		FROM varroa.activity
		WHERE %s
		ORDER BY ts %s, event_key %s
		LIMIT $%d
	`, where, orderDir, orderDir, len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return ActivityPage{}, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()

	type scannedRow struct {
		key string
		ts  time.Time
		e   activity.Event
	}
	var all []scannedRow
	for rows.Next() {
		var key string
		var ts time.Time
		var payload []byte
		if err := rows.Scan(&key, &ts, &payload); err != nil {
			return ActivityPage{}, fmt.Errorf("scan activity: %w", err)
		}
		var e activity.Event
		if err := json.Unmarshal(payload, &e); err != nil {
			return ActivityPage{}, fmt.Errorf("unmarshal activity payload: %w", err)
		}
		all = append(all, scannedRow{key: key, ts: ts, e: e})
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, fmt.Errorf("list activity: %w", err)
	}

	var page ActivityPage
	for _, s := range all {
		page.Items = append(page.Items, s.e)
	}
	if len(page.Items) > limit {
		last := all[limit-1]
		page.NextCursor = encodeActivityCursor(activityCursor{Direction: direction, Filters: filters, TS: last.ts, EventKey: last.key})
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	return page, nil
}

// ActivityRetentionDays reports the configured activity retention window
// in days, from VARROA_ACTIVITY_RETENTION_DAYS. It is the day count
// Backfill.Retention reports and the bound ListActivity checks an
// incoming cursor's timestamp against.
func (r *pgReader) ActivityRetentionDays(ctx context.Context) (int, error) {
	if err := r.checkSchema(ctx); err != nil {
		return 0, err
	}
	return r.activityRetentionDays, nil
}
