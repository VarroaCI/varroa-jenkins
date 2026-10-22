package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// ticketFor mints an assistant stream ticket through the stream handler.
func ticketFor(t *testing.T, srv *Server, claims *auth.Claims, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/stream/ticket?scope=assistant:"+sessionID, nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	srv.handleStreamTicket(rec, req)
	return rec
}

// ticketFixture creates one session owned by the viewer and returns the
// fixture, the session id, and a server with a real ticket issuer.
func ticketFixture(t *testing.T) (*assistantFixture, string, *Server, *auth.TicketVerifier) {
	t.Helper()
	f := newAssistantFixture(t, assistantTextTurn("hi"))
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hello"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	signer, err := signing.New()
	if err != nil {
		t.Fatal(err)
	}
	f.deps.TicketIssuer = auth.NewTicketIssuer(signer, "https://bff.test", 30*time.Second)
	ver := auth.NewTicketVerifier(signer.PublicKey(), "https://bff.test")
	return f, sess.ID, NewServer(f.deps), ver
}

// TestStreamTicket_AssistantOwner proves the owner of a session mints a ticket.
func TestStreamTicket_AssistantOwner(t *testing.T) {
	f, id, srv, _ := ticketFixture(t)
	rec := ticketFor(t, srv, f.viewer, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner ticket = %d body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ticket") {
		t.Errorf("no ticket in body: %s", rec.Body)
	}
}

// TestStreamTicket_AssistantOther404 proves another caller gets not found, so
// the answer does not reveal the session.
func TestStreamTicket_AssistantOther404(t *testing.T) {
	f, id, srv, _ := ticketFixture(t)
	other := &auth.Claims{Subject: "someone-else", IssuedAt: time.Now()}
	if rec := ticketFor(t, srv, other, id); rec.Code != http.StatusNotFound {
		t.Errorf("other caller = %d, want 404", rec.Code)
	}
	if rec := ticketFor(t, srv, f.admin, id); rec.Code != http.StatusOK {
		t.Errorf("admin caller = %d, want 200", rec.Code)
	}
}

// TestStreamTicket_AssistantMismatch401 proves a ticket for one session does
// not open another. The stream route verifies the ticket against its own scope.
func TestStreamTicket_AssistantMismatch401(t *testing.T) {
	f, id, srv, ver := ticketFixture(t)
	rec := ticketFor(t, srv, f.viewer, id)
	var body struct {
		Ticket string `json:"ticket"`
	}
	decodeBody(t, rec, &body)
	if _, err := ver.Verify(context.Background(), body.Ticket, "assistant:"+otherSessionID(id)); err == nil {
		t.Error("a ticket for one session verified for another")
	}
}

// otherSessionID returns an id that differs from id.
func otherSessionID(id string) string {
	if strings.HasSuffix(id, "0") {
		return id[:len(id)-1] + "1"
	}
	return id[:len(id)-1] + "0"
}

// TestStreamTicket_AssistantDisabled503 proves the ticket route answers 503
// when the assistant is off.
func TestStreamTicket_AssistantDisabled503(t *testing.T) {
	srv := NewServer(&Dependencies{TicketIssuer: newTicketIssuerForTest(t), Authorizer: NewAuthorizer(testResolver(nil, nil), false)})
	claims := &auth.Claims{Subject: "viewer-user", IssuedAt: time.Now()}
	if rec := ticketFor(t, srv, claims, "0190d1e2-0000-7000-8000-000000000001"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled = %d, want 503", rec.Code)
	}
}

// TestAssistantList_ViewerAndAdmin proves the list answers the caller's own
// sessions, and that scope=all needs the admin verb.
func TestAssistantList_ViewerAndAdmin(t *testing.T) {
	f, id, _, _ := ticketFixture(t)
	rec := f.do(t, f.viewer, http.MethodGet, "/assistant/sessions?limit=10", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("viewer list = %d body = %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, f.viewer, http.MethodGet, "/assistant/sessions?scope=all", nil); rec.Code != http.StatusForbidden {
		t.Errorf("viewer scope=all = %d, want 403", rec.Code)
	}
	if rec := f.do(t, f.admin, http.MethodGet, "/assistant/sessions?scope=all", nil); rec.Code != http.StatusOK {
		t.Errorf("admin scope=all = %d, want 200", rec.Code)
	}
	if rec := f.do(t, f.viewer, http.MethodGet, "/assistant/sessions?limit=0", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0 = %d, want 400", rec.Code)
	}
}

// TestAssistantGet_ReturnsApprovalsWithResult proves the detail carries the
// approvals, and each result of its tool call.
func TestAssistantGet_ReturnsApprovalsWithResult(t *testing.T) {
	f := newAssistantFixture(t, assistantRestartTurn(), assistantTextTurn("done"))
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	page, _ := f.mem.ListApprovals(context.Background(), store.ApprovalQuery{Principal: "viewer-user", SessionID: mustUUID(t, sess.ID)})
	f.do(t, f.viewer, http.MethodPost, "/assistant/approvals/"+page.Items[0].ID.String()+"/approve", nil)
	f.rt.Wait()

	rec = f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+sess.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	var detail struct {
		Approvals []assistantApprovalJSON `json:"approvals"`
	}
	decodeBody(t, rec, &detail)
	if len(detail.Approvals) != 1 || detail.Approvals[0].Result == nil || *detail.Approvals[0].Result != "ok:restart_run" {
		t.Errorf("approvals = %+v", detail.Approvals)
	}
	if rec := f.do(t, &auth.Claims{Subject: "viewer2-user", IssuedAt: time.Now()}, http.MethodGet, "/assistant/sessions/"+sess.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("other caller get = %d, want 404", rec.Code)
	}
}

// TestAssistantListApprovals_PendingFilter proves the approval list returns a
// pending approval to its owner.
func TestAssistantListApprovals_PendingFilter(t *testing.T) {
	f := newAssistantFixture(t, assistantRestartTurn())
	f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart"})
	f.rt.Wait()
	rec := f.do(t, f.viewer, http.MethodGet, "/assistant/approvals?state=pending", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"pending"`) {
		t.Errorf("pending list = %d body = %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, f.viewer, http.MethodGet, "/assistant/approvals?scope=all", nil); rec.Code != http.StatusForbidden {
		t.Errorf("viewer scope=all = %d, want 403", rec.Code)
	}
}

// TestAssistantMessage_CompletedSessionRuns proves a new message runs a
// completed session again, and a running session refuses it.
func TestAssistantMessage_CompletedSessionRuns(t *testing.T) {
	f := newAssistantFixture(t, assistantTextTurn("first"), assistantTextTurn("second"))
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()

	rec = f.do(t, f.viewer, http.MethodPost, "/assistant/sessions/"+sess.ID+"/messages", map[string]any{"message": "again"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message = %d body = %s", rec.Code, rec.Body)
	}
	f.rt.Wait()
	got, _ := f.mem.GetSession(context.Background(), mustUUID(t, sess.ID))
	if got.State != agent.StateCompleted {
		t.Errorf("state after second run = %s, want completed", got.State)
	}
	if rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions/"+sess.ID+"/messages", map[string]any{"message": ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty message = %d, want 400", rec.Code)
	}
}

// TestAssistantEvents_StreamsTranscript proves the stream writes each event
// with its sequence id, and closes after the done event.
func TestAssistantEvents_StreamsTranscript(t *testing.T) {
	f := newAssistantFixture(t, assistantTextTurn("streamed"))
	f.deps.Broadcaster = sse.NewBroadcaster()
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "hi"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/assistant/sessions/"+sess.ID+"/events", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), f.viewer))
	rec = httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewRouter(f.deps).ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not close after done")
	}
	body := rec.Body.String()
	for _, want := range []string{"id: 1\n", "event: user", "event: assistant", "event: done", "created_at"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %q: %s", want, body)
		}
	}
}

// TestAssistantGet_WithholdsResultsFromAdmin proves an admin who is not the
// owner reads each approval with its result withheld, and the owner reads it.
func TestAssistantGet_WithholdsResultsFromAdmin(t *testing.T) {
	f := newAssistantFixture(t, assistantRestartTurn(), assistantTextTurn("done"))
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	page, _ := f.mem.ListApprovals(context.Background(), store.ApprovalQuery{Principal: "viewer-user", SessionID: mustUUID(t, sess.ID)})
	f.do(t, f.viewer, http.MethodPost, "/assistant/approvals/"+page.Items[0].ID.String()+"/approve", nil)
	f.rt.Wait()

	rec = f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+sess.ID, nil)
	var owner struct {
		Approvals []assistantApprovalJSON `json:"approvals"`
	}
	decodeBody(t, rec, &owner)
	if len(owner.Approvals) != 1 || owner.Approvals[0].Result == nil || *owner.Approvals[0].Result != "ok:restart_run" || owner.Approvals[0].ResultWithheld {
		t.Errorf("owner approvals = %+v", owner.Approvals)
	}

	rec = f.do(t, f.admin, http.MethodGet, "/assistant/sessions/"+sess.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin get = %d", rec.Code)
	}
	var admin struct {
		Approvals []assistantApprovalJSON `json:"approvals"`
	}
	decodeBody(t, rec, &admin)
	if len(admin.Approvals) != 1 {
		t.Fatalf("admin approvals = %+v", admin.Approvals)
	}
	got := admin.Approvals[0]
	if got.Result != nil || !got.ResultWithheld || got.Tool != "restart_run" || len(got.Arguments) == 0 {
		t.Errorf("admin approval = %+v, want withheld result with tool and arguments", got)
	}
}

// TestAssistantEvents_WithholdsToolResultFromAdmin proves the stream writes a
// tool_result frame without content to an admin, and with content to the owner.
func TestAssistantEvents_WithholdsToolResultFromAdmin(t *testing.T) {
	f := newAssistantFixture(t, assistantRestartTurn(), assistantTextTurn("done"))
	f.deps.Broadcaster = sse.NewBroadcaster()
	rec := f.do(t, f.viewer, http.MethodPost, "/assistant/sessions", map[string]any{"message": "restart"})
	var sess assistantSessionJSON
	decodeBody(t, rec, &sess)
	f.rt.Wait()
	page, _ := f.mem.ListApprovals(context.Background(), store.ApprovalQuery{Principal: "viewer-user", SessionID: mustUUID(t, sess.ID)})
	f.do(t, f.viewer, http.MethodPost, "/assistant/approvals/"+page.Items[0].ID.String()+"/approve", nil)
	f.rt.Wait()

	// The stream closes at the first done event, which is seq 5 here. A cursor
	// of 5 reads the tool_result that the approval wrote after it.
	stream := func(claims *auth.Claims, query string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/assistant/sessions/"+sess.ID+"/events"+query, nil)
		req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			NewRouter(f.deps).ServeHTTP(rec, req)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not close after done")
		}
		return rec.Body.String()
	}

	body := stream(f.admin, "")
	for _, want := range []string{"event: tool_call", "restart_run"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin stream lacks %q: %s", want, body)
		}
	}

	body = stream(f.admin, "?after=5")
	for _, want := range []string{"event: tool_result", `"withheld":true`, `"content":""`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin stream lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "ok:restart_run") {
		t.Errorf("admin stream carries the tool result: %s", body)
	}

	body = stream(f.viewer, "?after=5")
	if !strings.Contains(body, "ok:restart_run") || strings.Contains(body, `"withheld"`) {
		t.Errorf("owner stream = %s, want the result and no withheld key", body)
	}
}

// TestAssistantEventFrame_WithheldOnlyToolResult proves only a tool_result
// frame changes when the caller may not read the transcript in full.
func TestAssistantEventFrame_WithheldOnlyToolResult(t *testing.T) {
	result := agent.Event{Seq: 3, Type: agent.EventToolResult, Payload: json.RawMessage(`{"call_id":"c1","name":"restart_run","content":"ok","is_error":false}`), CreatedAt: time.Unix(0, 0)}
	frame, err := assistantEventFrame(result, false)
	if err != nil {
		t.Fatal(err)
	}
	body := string(frame)
	for _, want := range []string{`"call_id":"c1"`, `"name":"restart_run"`, `"content":""`, `"withheld":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("withheld frame lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"content":"ok"`) {
		t.Errorf("withheld frame carries content: %s", body)
	}

	text := agent.Event{Seq: 4, Type: agent.EventAssistant, Payload: json.RawMessage(`{"text":"hello"}`), CreatedAt: time.Unix(0, 0)}
	frame, err = assistantEventFrame(text, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(frame), "withheld") || !strings.Contains(string(frame), `"text":"hello"`) {
		t.Errorf("assistant frame changed: %s", frame)
	}
}

// TestAssistantEvents_BadCursor400 proves a non-numeric cursor is refused.
func TestAssistantEvents_BadCursor400(t *testing.T) {
	f, id, _, _ := ticketFixture(t)
	f.deps.Broadcaster = sse.NewBroadcaster()
	rec := f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+id+"/events?after=abc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
}

// TestAssistantMe_PutAndSummary proves PUT /me/assistant stores the caller's
// choice and answers the view with it.
func TestAssistantMe_PutAndSummary(t *testing.T) {
	f := newAssistantFixture(t)
	rec := f.do(t, f.viewer, http.MethodPut, "/me/assistant", map[string]any{"provider": "smoke", "model": "m1", "dataSharing": "metadata"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d body = %s", rec.Code, rec.Body)
	}
	var view struct {
		Available   bool   `json:"available"`
		Provider    string `json:"provider"`
		DataSharing string `json:"dataSharing"`
		DayBudget   int64  `json:"dayBudget"`
	}
	decodeBody(t, rec, &view)
	if !view.Available || view.Provider != "smoke" || view.DataSharing != "metadata" || view.DayBudget != 1000000 {
		t.Errorf("view = %+v", view)
	}
	if rec := f.do(t, f.viewer, http.MethodPut, "/me/assistant", map[string]any{"dataSharing": "all"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad level = %d, want 400", rec.Code)
	}
	if rec := f.do(t, f.viewer, http.MethodGet, "/me/assistant", nil); rec.Code != http.StatusOK {
		t.Errorf("get = %d, want 200", rec.Code)
	}
}

// TestAssistantKey_PutThenDelete proves a stored key cookie is cleared by DELETE.
func TestAssistantKey_PutThenDelete(t *testing.T) {
	f := newAssistantFixture(t)
	f.rt.Sealer = mustSealer(t)
	rec := f.do(t, f.viewer, http.MethodPut, "/me/assistant/key", map[string]any{"provider": "smoke", "apiKey": "k"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put key = %d", rec.Code)
	}
	rec = f.do(t, f.viewer, http.MethodDelete, "/me/assistant/key", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete key = %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == AssistantKeyCookie && c.MaxAge != -1 {
			t.Errorf("delete did not clear the cookie: MaxAge = %d", c.MaxAge)
		}
	}
}

// TestAssistantSettings_GetAsViewer proves the kill-switch read answers the
// caller with the paused flag only.
func TestAssistantSettings_GetAsViewer(t *testing.T) {
	f := newAssistantFixture(t)
	rec := f.do(t, f.viewer, http.MethodGet, "/assistant/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	var out map[string]json.RawMessage
	decodeBody(t, rec, &out)
	if _, ok := out["paused"]; !ok {
		t.Errorf("no paused flag: %s", rec.Body)
	}
}
