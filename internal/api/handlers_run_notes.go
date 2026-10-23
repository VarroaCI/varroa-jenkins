package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// The note error codes a refusal carries in its code field.
const (
	noteCodeTooLong        = "note_too_long"
	noteCodeSessionInvalid = "note_session_invalid"
	noteTruncMarker        = "\n\n[truncated at 16384 bytes]"
	noteEventPageSize      = 1000
	noteJobListLimit       = 10
)

// runNoteJSON is one note in the API shape. A job note carries a null
// runId, and a human note carries a null sessionId.
type runNoteJSON struct {
	ID          string    `json:"id"`
	RunID       *string   `json:"runId"`
	Cluster     string    `json:"cluster"`
	Tenant      string    `json:"tenant"`
	JobPath     string    `json:"jobPath"`
	Principal   string    `json:"principal"`
	SessionID   *string   `json:"sessionId"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"createdAt"`
	BuildNumber *int64    `json:"buildNumber,omitempty"`
}

// toRunNoteJSON converts a record-store note into the API shape.
func toRunNoteJSON(n recordstore.RunNote) runNoteJSON {
	out := runNoteJSON{
		ID: n.ID.String(), Cluster: n.Cluster, Tenant: n.Tenant, JobPath: n.JobPath,
		Principal: n.Principal, Body: n.Body, CreatedAt: n.CreatedAt, BuildNumber: n.BuildNumber,
	}
	if n.RunID != nil {
		s := n.RunID.String()
		out.RunID = &s
	}
	if n.SessionID != nil {
		s := n.SessionID.String()
		out.SessionID = &s
	}
	return out
}

// cutNoteBody keeps a body of at most the note limit. A longer body is
// cut at a rune boundary, and the truncation marker is appended inside the
// limit.
func cutNoteBody(s string) string {
	if len(s) <= recordstore.RunNoteMaxBytes {
		return s
	}
	n := recordstore.RunNoteMaxBytes - len(noteTruncMarker)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + noteTruncMarker
}

// newestFinalTurn answers the text of the highest turn of session id that
// reached its final chunk. It pages the event log, so a long session reads
// in full. It answers the empty string when no turn is final.
func newestFinalTurn(ctx context.Context, st store.Store, id uuid.UUID) (string, error) {
	chunks := map[int]string{}
	final := map[int]bool{}
	after := 0
	for {
		events, err := st.Events(ctx, id, after, noteEventPageSize)
		if err != nil {
			return "", err
		}
		for _, ev := range events {
			after = ev.Seq
			if ev.Type != agent.EventAssistant {
				continue
			}
			var p agent.AssistantPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				continue
			}
			chunks[p.Turn] += p.Text
			if p.Final {
				final[p.Turn] = true
			}
		}
		if len(events) < noteEventPageSize {
			break
		}
	}
	best := -1
	for turn := range final {
		if turn > best {
			best = turn
		}
	}
	if best < 0 {
		return "", nil
	}
	return chunks[best], nil
}

// handleListRunNotes serves GET /runs/{run}/notes. The run read check
// decides whether the caller may see the build at all.
func (s *Server) handleListRunNotes(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}
	id, err := uuid.Parse(run.RunID)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "run id is not a uuid")
		return
	}
	notes, err := s.deps.RecordStore.ListRunNotes(r.Context(), id, run.CreatedAt)
	if s.handleReaderError(w, err, "list run notes") {
		return
	}
	s.writeJSON(w, http.StatusOK, runNoteListJSON(notes))
}

// runNoteListJSON wraps notes in the list body. The items array is never
// null.
func runNoteListJSON(notes []recordstore.RunNote) map[string]any {
	items := make([]runNoteJSON, 0, len(notes))
	for _, n := range notes {
		items = append(items, toRunNoteJSON(n))
	}
	return map[string]any{"items": items}
}

// handleListJobNotes serves GET /pipelines/{tenant}/jobs/{job}/notes. The
// caller must read the job, and the list holds the newest ten notes.
func (s *Server) handleListJobNotes(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	jobPath := r.PathValue("job")
	refs, serr := s.resolveTenantTenants(ctx, claims, r.PathValue("tenant"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	ref, ok := s.tenantForJob(claims, refs, jobPath)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}
	notes, err := s.deps.RecordStore.ListJobNotes(ctx, ref.Cluster, ref.Tenant, jobPath, noteJobListLimit)
	if s.handleReaderError(w, err, "list job notes") {
		return
	}
	s.writeJSON(w, http.StatusOK, runNoteListJSON(notes))
}

// createRunNoteRequest is the POST /runs/{run}/notes body. Exactly one
// field is set.
type createRunNoteRequest struct {
	Body      *string `json:"body"`
	SessionID *string `json:"sessionId"`
}

// handleCreateRunNote serves POST /runs/{run}/notes. A body writes a human
// note under the tenant write check. A session id writes the newest final
// answer of an assistant session the caller owns.
func (s *Server) handleCreateRunNote(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	var req createRunNoteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if (req.Body == nil) == (req.SessionID == nil) {
		s.writeJSONError(w, http.StatusBadRequest, "send exactly one of body and sessionId")
		return
	}
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}

	post := recordstore.RunNotePostRequest{RunID: run.RunID, Principal: claims.Subject}
	if req.Body != nil {
		if !s.writeHumanNote(w, r, claims, run, *req.Body, &post) {
			return
		}
	} else if !s.writeAssistantNote(w, r, claims, run, *req.SessionID, &post) {
		return
	}

	if s.deps.RunNotePoster == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "the record store is not reachable")
		return
	}
	postCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := s.deps.RunNotePoster.PostRunNote(postCtx, post)
	switch {
	case err != nil:
		s.deps.Logger.Error("post run note", "run", run.RunID, "error", err)
		s.writeJSONError(w, http.StatusServiceUnavailable, "the record store is not reachable")
	case resp.Error == recordstore.RunNoteErrNotFound:
		s.writeJSONError(w, http.StatusNotFound, "run not found")
	case resp.Error != "":
		s.deps.Logger.Error("run note refused", "run", run.RunID, "code", resp.Error)
		s.writeJSONError(w, http.StatusServiceUnavailable, "the record store refused the note")
	case resp.Note == nil:
		s.writeJSONError(w, http.StatusServiceUnavailable, "the record store refused the note")
	case resp.Created:
		s.writeJSON(w, http.StatusCreated, toRunNoteJSON(*resp.Note))
	default:
		s.writeJSON(w, http.StatusOK, toRunNoteJSON(*resp.Note))
	}
}

// writeHumanNote checks the body of a human note and fills post with it.
// It writes the refusal and reports false when the note may not be posted.
func (s *Server) writeHumanNote(w http.ResponseWriter, r *http.Request, claims *auth.Claims,
	run recordstore.Run, body string, post *recordstore.RunNotePostRequest) bool {
	if s.deps.TenantResolver == nil {
		s.pipelinesUnavailable(w)
		return false
	}
	ref, err := s.deps.TenantResolver.ResolveRun(r.Context(), claims, run)
	if err != nil {
		s.deps.Logger.Error("resolve run tenant", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "resolve run tenant failed")
		return false
	}
	if !s.canWritePipeline(claims, ref, run.JobPath) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return false
	}
	if len(body) == 0 {
		s.writeJSONError(w, http.StatusBadRequest, "the note is empty")
		return false
	}
	if len(body) > recordstore.RunNoteMaxBytes {
		s.writeJSONErrorStatus(w, http.StatusBadRequest, "note is longer than 16384 bytes",
			map[string]any{"code": noteCodeTooLong})
		return false
	}
	post.Body = body
	return true
}

// writeAssistantNote checks an assistant session and fills post with its
// newest final answer. It writes the refusal and reports false when the
// note may not be posted.
func (s *Server) writeAssistantNote(w http.ResponseWriter, r *http.Request, claims *auth.Claims,
	run recordstore.Run, rawSession string, post *recordstore.RunNotePostRequest) bool {
	rt := s.deps.Assistant
	if rt == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "the assistant is not enabled")
		return false
	}
	id, ok := sessionUUID(rawSession)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return false
	}
	sess, err := rt.Store.GetSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeJSONError(w, http.StatusNotFound, "not found")
			return false
		}
		s.assistantStoreError(w, err)
		return false
	}
	// An assistant answer is a note on the build, so the writer needs the same
	// tenant write check as a human note.
	if s.deps.TenantResolver == nil {
		s.pipelinesUnavailable(w)
		return false
	}
	ref, err := s.deps.TenantResolver.ResolveRun(r.Context(), claims, run)
	if err != nil {
		s.deps.Logger.Error("resolve run tenant", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "resolve run tenant failed")
		return false
	}
	if !s.canWritePipeline(claims, ref, run.JobPath) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return false
	}
	if sess.Principal != claims.Subject || sess.Kind != agent.KindAssistant {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return false
	}
	ctxRun, _ := sess.Context[agent.CtxRun].(string)
	if ctxRun != run.RunID || sess.State != agent.StateCompleted {
		s.writeJSONErrorStatus(w, http.StatusConflict, "the session cannot write a note on this build",
			map[string]any{"code": noteCodeSessionInvalid})
		return false
	}
	text, err := newestFinalTurn(r.Context(), rt.Store, id)
	if err != nil {
		s.assistantStoreError(w, err)
		return false
	}
	if text == "" {
		s.writeJSONErrorStatus(w, http.StatusConflict, "the session has no answer to write",
			map[string]any{"code": noteCodeSessionInvalid})
		return false
	}
	post.Body = cutNoteBody(text)
	post.SessionID = id.String()
	return true
}
