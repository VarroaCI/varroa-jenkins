// Package recordstore: the run note reads. A run note hangs on one run,
// and a job note hangs on one job path. Both reads answer the stored
// rows only. The BFF decides which notes a caller may see.
package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RunNoteMaxBytes is the longest body a note holds. The table's CHECK
// constraint enforces the same bound.
const RunNoteMaxBytes = 16384

// runNotesRunLimit caps the notes one run read answers.
const runNotesRunLimit = 200

// runNotesJobDefault and runNotesJobMax bound the notes one job read
// answers when the caller names no limit.
const (
	runNotesJobDefault = 10
	runNotesJobMax     = 10
)

// RunNote is one row of varroa.run_notes. RunID and RunCreatedAt are
// null for a job note. SessionID is null for a human note. BuildNumber
// is set on a job read only, when the note's run is still stored.
type RunNote struct {
	ID           uuid.UUID  `json:"id"`
	RunID        *uuid.UUID `json:"run_id"`
	RunCreatedAt *time.Time `json:"run_created_at"`
	Cluster      string     `json:"cluster"`
	Team         string     `json:"team"`
	JobPath      string     `json:"job_path"`
	Principal    string     `json:"principal"`
	SessionID    *uuid.UUID `json:"session_id"`
	Body         string     `json:"body"`
	CreatedAt    time.Time  `json:"created_at"`
	BuildNumber  *int64     `json:"build_number,omitempty"`
}

// runNoteColumns lists varroa.run_notes' columns in the order scanRunNotes
// expects.
const runNoteColumns = `id, run_id, run_created_at, cluster, team, job_path, principal,
	session_id, body, created_at`

// ErrRunNotesMissing reports that the run_notes table is absent. The
// schema is older than this reader requires.
var ErrRunNotesMissing = fmt.Errorf("%w: run_notes is missing", ErrSchemaTooOld)

// isUndefinedTable reports a missing relation, which an older schema
// answers for run_notes.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// ListRunNotes answers the notes of one run, oldest first, up to
// runNotesRunLimit rows.
func (r *pgReader) ListRunNotes(ctx context.Context, runID uuid.UUID, runCreatedAt time.Time) ([]RunNote, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	// The newest rows are kept when a run passes the cap, so a new note is
	// never the one that falls off. The window is reversed to oldest first.
	rows, err := r.pool.Query(ctx, `
		SELECT `+runNoteColumns+`
		FROM (
			SELECT `+runNoteColumns+`
			FROM varroa.run_notes
			WHERE run_id = $1 AND run_created_at = $2
			ORDER BY created_at DESC, id DESC
			LIMIT $3
		) newest
		ORDER BY created_at, id
	`, runID, runCreatedAt, runNotesRunLimit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, ErrRunNotesMissing
		}
		return nil, fmt.Errorf("list run notes: %w", err)
	}
	notes, err := scanRunNotes(rows, false)
	if err != nil {
		return nil, fmt.Errorf("list run notes: %w", err)
	}
	return notes, nil
}

// ListJobNotes answers the newest notes of one job, newest first. A limit
// outside 1 to runNotesJobMax falls back to runNotesJobDefault or the
// maximum. Each row carries the build number of its run when that run is
// still stored.
func (r *pgReader) ListJobNotes(ctx context.Context, cluster, team, jobPath string, limit int) ([]RunNote, error) {
	if err := r.checkSchema(ctx); err != nil {
		return nil, err
	}
	switch {
	case limit <= 0:
		limit = runNotesJobDefault
	case limit > runNotesJobMax:
		limit = runNotesJobMax
	}
	rows, err := r.pool.Query(ctx, `
		SELECT n.id, n.run_id, n.run_created_at, n.cluster, n.team, n.job_path, n.principal,
			n.session_id, n.body, n.created_at, x.build_number
		FROM varroa.run_notes n
		LEFT JOIN varroa.runs x ON x.run_id = n.run_id AND x.created_at = n.run_created_at
		WHERE n.cluster = $1 AND n.team = $2 AND n.job_path = $3
		ORDER BY n.created_at DESC, n.id DESC
		LIMIT $4
	`, cluster, team, jobPath, limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, ErrRunNotesMissing
		}
		return nil, fmt.Errorf("list job notes: %w", err)
	}
	notes, err := scanRunNotes(rows, true)
	if err != nil {
		return nil, fmt.Errorf("list job notes: %w", err)
	}
	return notes, nil
}

// scanRunNotes reads note rows. It closes rows. withBuild adds the
// build_number column that ListJobNotes selects last.
func scanRunNotes(rows pgx.Rows, withBuild bool) ([]RunNote, error) {
	defer rows.Close()
	notes := []RunNote{}
	for rows.Next() {
		var n RunNote
		dest := []any{&n.ID, &n.RunID, &n.RunCreatedAt, &n.Cluster, &n.Team, &n.JobPath,
			&n.Principal, &n.SessionID, &n.Body, &n.CreatedAt}
		if withBuild {
			dest = append(dest, &n.BuildNumber)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}
