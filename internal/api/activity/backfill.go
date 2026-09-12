package activity

import (
	"context"
	"errors"
	"time"
)

// Backfill is the interface for reading activity history. The handlers use it
// to remain implementation-agnostic. pgBackfill (internal/recordstore) is the
// only production implementation; a nil Backfill means the record store is
// absent, and both readers (REST and MCP) already handle that case.
type Backfill interface {
	// Recent returns up to limit most-recent events, newest-first.
	// scope == "" means all controllers (global feed); "ns/name" scopes to a
	// local-cluster controller; "cluster/ns/name" scopes cross-cluster.
	Recent(ctx context.Context, scope string, limit int) ([]Event, error)
	// Query returns filtered, paginated activity history.
	Query(ctx context.Context, query Query) (Page, error)
	// Retention reports the retention mode ("on"/"off") and configured days.
	Retention() (string, int)
}

// Query describes filters, scope, and pagination for activity history.
type Query struct {
	Cursor                                                        string
	Limit                                                         int
	Start, End                                                    *time.Time
	Cluster, Controller, Namespace, Source, Severity, Actor, Type string
	// Scope is the SQL-pushed-down read scope (D5): a sound superset of the
	// rows the caller may read. Authorize stays the authoritative per-event
	// check — a Scope wider than the caller's real grant cannot disclose a
	// row, because Authorize still runs on every returned row.
	Scope     Scope
	Authorize func(Event) bool
}

// Page is one page of activity history results.
type Page struct {
	Items         []Event `json:"items"`
	NextCursor    string  `json:"nextCursor,omitempty"`
	HasMore       bool    `json:"hasMore"`
	RetentionMode string  `json:"retentionMode"`
	RetentionDays int     `json:"retentionDays,omitempty"`
}

var (
	// ErrInvalidCursor indicates a malformed cursor or one for different filters.
	ErrInvalidCursor = errors.New("invalid activity cursor")
	// ErrCursorExpired indicates a cursor outside the retained activity window.
	ErrCursorExpired = errors.New("activity cursor expired")
)

// Matches reports whether an event satisfies an activity query's filters.
// Used by the live SSE path (BusFanout), which has no SQL layer to push
// filters into.
func Matches(e Event, q Query) bool {
	e.Normalize()
	if q.Start != nil && e.Timestamp.Before(*q.Start) {
		return false
	}
	if q.End != nil && !e.Timestamp.Before(*q.End) {
		return false
	}
	return (q.Cluster == "" || e.Cluster == q.Cluster) && (q.Controller == "" || e.Controller == q.Controller || e.Namespace+"/"+e.Controller == q.Controller) &&
		(q.Namespace == "" || e.Namespace == q.Namespace) && (q.Source == "" || e.Source == q.Source) && (q.Severity == "" || e.Severity == q.Severity) &&
		(q.Actor == "" || e.Actor == q.Actor) && (q.Type == "" || e.Type == q.Type)
}
