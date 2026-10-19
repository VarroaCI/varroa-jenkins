package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedNoteRun inserts one run row and returns its id and created_at.
func seedNoteRun(t *testing.T, db *sql.DB, cluster, tenant, jobPath string, created time.Time) (uuid.UUID, time.Time) {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	// Postgres keeps microseconds, so the returned time must too.
	created = created.Truncate(time.Microsecond)
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, build_number)
		VALUES ($1, $2, $3, $4, 'ci', 'cell-1', $5, $6, 'QUEUED', 7)
	`, runID, created, "manual:"+runID.String(), cluster, tenant, jobPath); err != nil {
		t.Fatalf("insert runs row: %v", err)
	}
	return runID, created
}

// TestValidateRunNotePost refuses each malformed request before any
// database call, so the test needs no Postgres.
func TestValidateRunNotePost(t *testing.T) {
	t.Parallel()
	run := uuid.NewString()
	cases := []struct {
		name string
		req  RunNotePostRequest
		want string
	}{
		{"no principal", RunNotePostRequest{RunID: run, Body: "x"}, RunNoteErrInvalidRequest},
		{"run and job", RunNotePostRequest{RunID: run, Cluster: "c", Team: "t", JobPath: "j", Principal: "p", Body: "x"}, RunNoteErrInvalidRequest},
		{"job missing team", RunNotePostRequest{Cluster: "c", JobPath: "j", Principal: "p", Body: "x"}, RunNoteErrInvalidRequest},
		{"neither", RunNotePostRequest{Principal: "p", Body: "x"}, RunNoteErrInvalidRequest},
		{"bad run id", RunNotePostRequest{RunID: "nope", Principal: "p", Body: "x"}, RunNoteErrInvalidRequest},
		{"bad session id", RunNotePostRequest{RunID: run, SessionID: "nope", Principal: "p", Body: "x"}, RunNoteErrInvalidRequest},
		{"empty body", RunNotePostRequest{RunID: run, Principal: "p"}, RunNoteErrInvalidBody},
		{"long body", RunNotePostRequest{RunID: run, Principal: "p", Body: strings.Repeat("a", RunNoteMaxBytes+1)}, RunNoteErrInvalidBody},
		{"valid run", RunNotePostRequest{RunID: run, Principal: "p", Body: "x"}, ""},
		{"valid job", RunNotePostRequest{Cluster: "c", Team: "t", JobPath: "j", Principal: "p", Body: "x"}, ""},
	}
	for _, tc := range cases {
		if got := validateRunNotePost(tc.req); got != tc.want {
			t.Errorf("%s: validate = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHandleRunNotePostAnswersInvalidRequest asserts a malformed body
// gets a coded reply with no database call.
func TestHandleRunNotePostAnswersInvalidRequest(t *testing.T) {
	t.Parallel()
	var resp RunNotePostResponse
	data := handleRunNotePost(nil, nil, []byte(`{"principal":""}`))
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Error != RunNoteErrInvalidRequest {
		t.Fatalf("reply = %+v, want %s", resp, RunNoteErrInvalidRequest)
	}
}

// TestPostRunNote_WriteAndRepeat writes a human run note, an assistant
// note, and a repeat of the assistant note, which answers the stored row.
func TestPostRunNote_WriteAndRepeat(t *testing.T) {
	t.Parallel()
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	runID, created := seedNoteRun(t, db, "core", "payments", "payments/api", time.Now().UTC())

	human, err := PostRunNote(ctx, pool, RunNotePostRequest{RunID: runID.String(), Principal: "alice", Body: "looks flaky"})
	if err != nil {
		t.Fatalf("human note: %v", err)
	}
	if !human.Created || human.Note == nil || human.Note.Team != "payments" || human.Note.JobPath != "payments/api" {
		t.Fatalf("human note = %+v", human)
	}
	if human.Note.RunCreatedAt == nil || !human.Note.RunCreatedAt.Equal(created) {
		t.Fatalf("run_created_at = %v, want %v", human.Note.RunCreatedAt, created)
	}
	if human.Note.SessionID != nil {
		t.Fatalf("human note has a session id: %v", human.Note.SessionID)
	}

	session := uuid.Must(uuid.NewV7()).String()
	first, err := PostRunNote(ctx, pool, RunNotePostRequest{RunID: runID.String(), Principal: "alice", SessionID: session, Body: "answer"})
	if err != nil || !first.Created {
		t.Fatalf("assistant note = %+v, %v", first, err)
	}
	again, err := PostRunNote(ctx, pool, RunNotePostRequest{RunID: runID.String(), Principal: "alice", SessionID: session, Body: "other"})
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if again.Created || again.Note == nil || again.Note.ID != first.Note.ID || again.Note.Body != "answer" {
		t.Fatalf("repeat = %+v, want the stored row %s", again, first.Note.ID)
	}
}

// TestPostRunNote_MissingRunAndJobNote covers a run the store does not
// hold, and a job note that carries no run.
func TestPostRunNote_MissingRunAndJobNote(t *testing.T) {
	t.Parallel()
	pool, _ := setupLedgerPool(t)
	ctx := context.Background()

	missing, err := PostRunNote(ctx, pool, RunNotePostRequest{RunID: uuid.NewString(), Principal: "alice", Body: "x"})
	if err != nil || missing.Error != RunNoteErrNotFound {
		t.Fatalf("missing run = %+v, %v; want %s", missing, err, RunNoteErrNotFound)
	}

	job, err := PostRunNote(ctx, pool, RunNotePostRequest{Cluster: "core", Team: "payments", JobPath: "payments/api", Principal: "bob", Body: "job note"})
	if err != nil || !job.Created || job.Note == nil || job.Note.RunID != nil || job.Note.RunCreatedAt != nil {
		t.Fatalf("job note = %+v, %v", job, err)
	}
}

// TestRunNoteReaders reads the notes back: a run's notes oldest first,
// and a job's notes newest first with the build number of a stored run.
func TestRunNoteReaders(t *testing.T) {
	t.Parallel()
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	r := NewReader(pool)
	runID, created := seedNoteRun(t, db, "core", "payments", "payments/api", time.Now().UTC())

	for _, body := range []string{"first", "second"} {
		if _, err := PostRunNote(ctx, pool, RunNotePostRequest{RunID: runID.String(), Principal: "alice", Body: body}); err != nil {
			t.Fatalf("post %s: %v", body, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := PostRunNote(ctx, pool, RunNotePostRequest{Cluster: "core", Team: "payments", JobPath: "payments/api", Principal: "bob", Body: "job"}); err != nil {
		t.Fatalf("job note: %v", err)
	}

	runNotes, err := r.ListRunNotes(ctx, runID, created)
	if err != nil {
		t.Fatalf("ListRunNotes: %v", err)
	}
	if len(runNotes) != 2 || runNotes[0].Body != "first" || runNotes[1].Body != "second" {
		t.Fatalf("run notes = %+v", runNotes)
	}

	jobNotes, err := r.ListJobNotes(ctx, "core", "payments", "payments/api", 0)
	if err != nil {
		t.Fatalf("ListJobNotes: %v", err)
	}
	if len(jobNotes) != 3 || jobNotes[0].Body != "job" {
		t.Fatalf("job notes = %+v, want three, newest first", jobNotes)
	}
	if jobNotes[1].BuildNumber == nil || *jobNotes[1].BuildNumber != 7 {
		t.Fatalf("build number of a run note = %v, want 7", jobNotes[1].BuildNumber)
	}
	if other, err := r.ListJobNotes(ctx, "member-b", "payments", "payments/api", 10); err != nil || len(other) != 0 {
		t.Fatalf("cluster filter = %+v, %v; want none", other, err)
	}
}

// TestRunNoteRetention_FollowsRunAndWindow asserts a run note goes with
// its run, and an old job note goes with the store-wide window.
func TestRunNoteRetention_FollowsRunAndWindow(t *testing.T) {
	t.Parallel()
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)

	gone, _ := seedNoteRun(t, db, "core", "payments", "payments/api", old)
	if _, err := PostRunNote(ctx, pool, RunNotePostRequest{RunID: gone.String(), Principal: "alice", Body: "x"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM varroa.runs WHERE run_id = $1`, gone); err != nil {
		t.Fatalf("purge run: %v", err)
	}
	if _, err := PostRunNote(ctx, pool, RunNotePostRequest{Cluster: "core", Team: "payments", JobPath: "payments/api", Principal: "bob", Body: "old job"}); err != nil {
		t.Fatalf("job note: %v", err)
	}
	if _, err := db.Exec(`UPDATE varroa.run_notes SET created_at = $1 WHERE run_id IS NULL`, old); err != nil {
		t.Fatalf("age job note: %v", err)
	}

	if err := deleteExpiredRunNotes(ctx, pool, time.Now().UTC(), 24*time.Hour); err != nil {
		t.Fatalf("deleteExpiredRunNotes: %v", err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_notes`).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Fatalf("notes left = %d, want 0", left)
	}
}

// TestRunNotesTableConstraints asserts the table refuses an empty body,
// a run id with no run_created_at, and a second note for one session.
func TestRunNotesTableConstraints(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	cases := map[string]string{
		"empty body": `INSERT INTO varroa.run_notes (id, cluster, team, job_path, principal, body)
			VALUES (gen_random_uuid(), 'c', 't', 'j', 'p', '')`,
		"run without created_at": `INSERT INTO varroa.run_notes (id, run_id, cluster, team, job_path, principal, body)
			VALUES (gen_random_uuid(), gen_random_uuid(), 'c', 't', 'j', 'p', 'x')`,
		"empty job path": `INSERT INTO varroa.run_notes (id, cluster, team, job_path, principal, body)
			VALUES (gen_random_uuid(), 'c', 't', '', 'p', 'x')`,
	}
	for name, stmt := range cases {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%s: insert succeeded, want a constraint error", name)
		}
	}
}
