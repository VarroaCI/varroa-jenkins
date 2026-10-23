// Package recordstore: the run note writer. The BFF asks the recorder to
// write a note on a run or on a job. The recorder is the only writer of
// varroa.run_notes, so every note passes the same validation and the
// same session uniqueness rule.
package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// runNoteQueueGroup is the queue group the note responder subscribes
// with, so one recorder replica answers each request.
const runNoteQueueGroup = "recorder"

// runNotePostTimeout bounds one note write's database work.
const runNotePostTimeout = 4 * time.Second

// The error codes a note post answers. Each one is a fixed string, so
// the BFF maps it to an HTTP status without reading the message.
const (
	RunNoteErrInvalidRequest = "invalid_request"
	RunNoteErrNotFound       = "not_found"
	RunNoteErrInvalidBody    = "invalid_body"
	RunNoteErrInternal       = "internal"
)

// RunNotePostRequest is the request body on bus.RunNotePostSubject. A
// run note sets RunID and leaves the job fields empty. A job note sets
// Cluster, Tenant, and JobPath and leaves RunID empty. SessionID is set
// only for an assistant note.
type RunNotePostRequest struct {
	RunID     string `json:"run_id,omitempty"`
	Cluster   string `json:"cluster,omitempty"`
	Tenant    string `json:"tenant,omitempty"`
	JobPath   string `json:"job_path,omitempty"`
	Principal string `json:"principal"`
	SessionID string `json:"session_id,omitempty"`
	Body      string `json:"body"`
}

// RunNotePostResponse is the reply body. Created is false when the
// session already has its note, and Note then holds that stored row.
type RunNotePostResponse struct {
	Note    *RunNote `json:"note,omitempty"`
	Created bool     `json:"created"`
	Error   string   `json:"error,omitempty"`
}

// RegisterRunNotePostResponder answers note posts on the note subject.
func RegisterRunNotePostResponder(conn *bus.Conn, pool *pgxpool.Pool, logger *slog.Logger) (*bus.Responder, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		return handleRunNotePost(pool, logger, data)
	}
	return conn.ServeRequests(bus.RunNotePostSubject, runNoteQueueGroup, handler)
}

// handleRunNotePost decodes one request, writes the note, and answers
// the reply body. A request that fails validation writes nothing.
func handleRunNotePost(pool *pgxpool.Pool, logger *slog.Logger, data []byte) []byte {
	var req RunNotePostRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("run note request decode failed", "error", err)
		return marshalRunNote(logger, RunNotePostResponse{Error: RunNoteErrInvalidRequest})
	}
	if code := validateRunNotePost(req); code != "" {
		return marshalRunNote(logger, RunNotePostResponse{Error: code})
	}

	ctx, cancel := context.WithTimeout(context.Background(), runNotePostTimeout)
	defer cancel()
	resp, err := PostRunNote(ctx, pool, req)
	if err != nil {
		logger.Error("run note write failed", "run_id", req.RunID, "session_id", req.SessionID, "error", err)
		return marshalRunNote(logger, RunNotePostResponse{Error: RunNoteErrInternal})
	}
	logger.Info("run note written", "run_id", req.RunID, "cluster", req.Cluster, "tenant", req.Tenant,
		"session_id", req.SessionID, "principal", req.Principal, "created", resp.Created, "error", resp.Error)
	return marshalRunNote(logger, resp)
}

// validateRunNotePost returns the error code for a malformed request, or
// the empty string when the request may reach the database.
func validateRunNotePost(req RunNotePostRequest) string {
	if req.Principal == "" {
		return RunNoteErrInvalidRequest
	}
	hasRun := req.RunID != ""
	hasJob := req.Cluster != "" || req.Tenant != "" || req.JobPath != ""
	if hasRun && hasJob {
		return RunNoteErrInvalidRequest
	}
	if !hasRun && (req.Cluster == "" || req.Tenant == "" || req.JobPath == "") {
		return RunNoteErrInvalidRequest
	}
	if hasRun {
		if _, err := uuid.Parse(req.RunID); err != nil {
			return RunNoteErrInvalidRequest
		}
	}
	if req.SessionID != "" {
		if _, err := uuid.Parse(req.SessionID); err != nil {
			return RunNoteErrInvalidRequest
		}
	}
	if n := len(req.Body); n == 0 || n > RunNoteMaxBytes {
		return RunNoteErrInvalidBody
	}
	return ""
}

// PostRunNote writes one note in one transaction. A run note reads the
// run row first, so a run the store does not hold answers not_found. A
// note whose session already has a row answers that stored row with
// Created false, so a repeated post never writes a second note.
func PostRunNote(ctx context.Context, pool *pgxpool.Pool, req RunNotePostRequest) (RunNotePostResponse, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return RunNotePostResponse{}, fmt.Errorf("begin run note: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runID *uuid.UUID
	var runCreatedAt *time.Time
	var cluster, tenant, jobPath string
	if req.RunID != "" {
		id := uuid.MustParse(req.RunID)
		var createdAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT created_at, cluster, tenant, job_path FROM varroa.runs
			WHERE run_id = $1 ORDER BY created_at DESC LIMIT 1
		`, id).Scan(&createdAt, &cluster, &tenant, &jobPath)
		if errors.Is(err, pgx.ErrNoRows) {
			return RunNotePostResponse{Error: RunNoteErrNotFound}, nil
		}
		if err != nil {
			return RunNotePostResponse{}, fmt.Errorf("read run for note: %w", err)
		}
		runID = &id
		runCreatedAt = &createdAt
	} else {
		cluster, tenant, jobPath = req.Cluster, req.Tenant, req.JobPath
	}

	var sessionID *uuid.UUID
	if req.SessionID != "" {
		id := uuid.MustParse(req.SessionID)
		sessionID = &id
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO varroa.run_notes
			(id, run_id, run_created_at, cluster, tenant, job_path, principal, session_id, body)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (session_id) WHERE session_id IS NOT NULL DO NOTHING
		RETURNING `+runNoteColumns,
		uuid.Must(uuid.NewV7()), runID, runCreatedAt, cluster, tenant, jobPath,
		req.Principal, sessionID, req.Body)
	note, err := scanOneRunNote(row)
	created := true
	if errors.Is(err, pgx.ErrNoRows) {
		// The session already holds its note. Answer that row.
		created = false
		note, err = scanOneRunNote(tx.QueryRow(ctx, `
			SELECT `+runNoteColumns+` FROM varroa.run_notes WHERE session_id = $1
		`, *sessionID))
		// A session that another principal or run owns answers nothing.
		if err == nil && (note.Principal != req.Principal || !sameRun(note.RunID, runID)) {
			return RunNotePostResponse{Error: RunNoteErrInvalidRequest}, nil
		}
	}
	if err != nil {
		return RunNotePostResponse{}, fmt.Errorf("write run note: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RunNotePostResponse{}, fmt.Errorf("commit run note: %w", err)
	}
	return RunNotePostResponse{Note: &note, Created: created}, nil
}

// sameRun reports whether two optional run ids name the same run.
func sameRun(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// scanOneRunNote reads one note row from a single-row query.
func scanOneRunNote(row pgx.Row) (RunNote, error) {
	var n RunNote
	err := row.Scan(&n.ID, &n.RunID, &n.RunCreatedAt, &n.Cluster, &n.Tenant, &n.JobPath,
		&n.Principal, &n.SessionID, &n.Body, &n.CreatedAt)
	return n, err
}

// marshalRunNote encodes a reply body. An encode failure answers a fixed
// error body, so the requester never waits for a timeout.
func marshalRunNote(logger *slog.Logger, resp RunNotePostResponse) []byte {
	data, err := json.Marshal(resp)
	if err != nil {
		logger.Error("run note reply encode failed", "error", err)
		return []byte(`{"error":"internal"}`)
	}
	return data
}
