package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// assistantKeepalive is how often an idle stream sends a comment line and
// rereads its session.
const assistantKeepalive = 15 * time.Second

// handleStreamAssistantEvents serves GET /assistant/sessions/{id}/events as
// server-sent events. Each event carries its sequence number as its id, so a
// reconnect with Last-Event-ID resumes after the last event the client read.
func (s *Server) handleStreamAssistantEvents(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	sess, ok := s.visibleSession(w, r, rt, claims)
	if !ok {
		return
	}
	after, err := streamCursor(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "after must be a whole number")
		return
	}
	if s.deps.Broadcaster == nil {
		writeAssistantError(s, w, http.StatusServiceUnavailable, assistantErrDisabled, "the event stream is not configured")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	key := sse.AgentSessionKeyPrefix + sess.ID.String()
	ch := s.deps.Broadcaster.Subscribe(key)
	defer s.deps.Broadcaster.Unsubscribe(key, ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	last, done, err := writeAssistantEvents(ctx, rt, w, sess.ID, after)
	if err != nil {
		return
	}
	flusher.Flush()
	ticker := time.NewTicker(assistantKeepalive)
	defer ticker.Stop()
	for !done {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
		}
		last, done, err = writeAssistantEvents(ctx, rt, w, sess.ID, last)
		if err != nil {
			return
		}
		flusher.Flush()
	}
}

// streamCursor reads the resume point from Last-Event-ID, or from the after
// query value. It answers zero when neither is set.
func streamCursor(r *http.Request) (int, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad cursor")
	}
	return n, nil
}

// writeAssistantEvents writes every event after the cursor and reports the
// new cursor, and whether a done event was written.
func writeAssistantEvents(ctx context.Context, rt *AssistantRuntime, w io.Writer, id uuid.UUID, after int) (int, bool, error) {
	last := after
	for {
		page, err := rt.Store.Events(ctx, id, last, 500)
		if err != nil {
			return last, false, err
		}
		if len(page) == 0 {
			return last, false, nil
		}
		for _, e := range page {
			frame, err := assistantEventFrame(e)
			if err != nil {
				return last, false, err
			}
			if _, err := w.Write(frame); err != nil {
				return last, false, err
			}
			last = e.Seq
			if e.Type == agent.EventDone {
				return last, true, nil
			}
		}
	}
}

// assistantEventFrame encodes one event as an SSE frame. The created_at
// time joins the payload, and the sequence number is the frame id.
func assistantEventFrame(e agent.Event) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		payload = map[string]any{}
	}
	payload["created_at"] = e.CreatedAt.UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, body)), nil
}
