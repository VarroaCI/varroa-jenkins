package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestCutNoteBody keeps a short body whole and cuts a long one at a rune
// boundary, inside the note limit with the marker at the end.
func TestCutNoteBody(t *testing.T) {
	t.Parallel()
	short := "a short answer"
	if got := cutNoteBody(short); got != short {
		t.Fatalf("short body changed: %q", got)
	}
	long := strings.Repeat("é", recordstore.RunNoteMaxBytes)
	got := cutNoteBody(long)
	if len(got) > recordstore.RunNoteMaxBytes {
		t.Fatalf("cut body is %d bytes, want at most %d", len(got), recordstore.RunNoteMaxBytes)
	}
	if !strings.HasSuffix(got, noteTruncMarker) {
		t.Fatalf("cut body lacks the marker: %q", got[len(got)-40:])
	}
	if !utf8.ValidString(got) {
		t.Fatal("cut body splits a rune")
	}
}

// TestNewestFinalTurnPagesEvents seeds more events than one page holds and
// asserts the answer is the highest final turn, with every chunk joined.
func TestNewestFinalTurnPagesEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := store.NewMemory(store.Options{Now: time.Now})
	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: "u", Cluster: "core",
		Trigger: agent.TriggerManual, Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateCompleted,
	}
	var events []agent.EventInput
	for i := 0; i < 1200; i++ {
		ev, err := agent.NewEvent(agent.EventAssistant, agent.AssistantPayload{Turn: 1, Text: "old"})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	for _, p := range []agent.AssistantPayload{{Turn: 2, Text: "new "}, {Turn: 2, Text: "answer", Final: true}} {
		ev, err := agent.NewEvent(agent.EventAssistant, p)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	if _, _, err := mem.CreateSession(ctx, sess, events); err != nil {
		t.Fatal(err)
	}
	got, err := newestFinalTurn(ctx, mem, id)
	if err != nil {
		t.Fatalf("newestFinalTurn: %v", err)
	}
	if got != "new answer" {
		t.Fatalf("answer = %q, want %q", got, "new answer")
	}
}

// createNoteRequest builds a POST /runs/{run}/notes request for claims.
func createNoteRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/r1/notes", strings.NewReader(string(data)))
	req.SetPathValue("run", "r1")
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "alice", IssuedAt: time.Now()}
	return req.WithContext(auth.ContextWithClaims(context.Background(), claims))
}

// TestCreateRunNoteRefusesMalformedBodies asserts a body that sets both or
// neither of body and sessionId is a plain 400, before any lookup.
func TestCreateRunNoteRefusesMalformedBodies(t *testing.T) {
	t.Parallel()
	srv := &Server{deps: &Dependencies{RecordStore: newFakeRecordReader(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	body := "x"
	session := "0190a1f0-0000-7000-8000-000000000001"
	cases := map[string]any{
		"neither": map[string]any{},
		"both":    map[string]any{"body": body, "sessionId": session},
	}
	for name, payload := range cases {
		rec := httptest.NewRecorder()
		srv.handleCreateRunNote(rec, createNoteRequest(t, payload))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}
