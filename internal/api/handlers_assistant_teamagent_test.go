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
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// teamSessionWithApproval stores a running team_agent session that waits on one
// pending approval, and returns the session and the approval id.
func (f *teamAgentFixture) teamSessionWithApproval(t *testing.T) (agent.Session, string) {
	t.Helper()
	sess := f.newTeamSession(t, agent.StateRunning)
	approvalID, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.mem.CreateApprovals(context.Background(), sess.ID, "", []agent.Approval{{
		ID: approvalID, CallID: "call-1", Tool: "cancel_run", Arguments: json.RawMessage(`{"run":"r1"}`),
		TargetKind: agent.TargetRun, TargetRef: "r1", State: agent.ApprovalPending,
		ExpiresAt: time.Now().Add(time.Hour),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// ClearLease moves the running session to waiting_approval, as the loop does.
	if _, err := f.mem.ClearLease(context.Background(), sess.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	return sess, approvalID.String()
}

func TestTeamAgentSession_GetTenantReader(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess := f.newTeamSession(t, agent.StateCompleted)
	rec := f.do(t, f.reader, http.MethodGet, "/assistant/sessions/"+sess.ID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
}

func TestTeamAgentSession_GetStranger404(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess := f.newTeamSession(t, agent.StateCompleted)
	// A stranger without assistant:use still gets 404, not 403, because a
	// team_agent session follows the tenant table.
	rec := f.do(t, f.stranger, http.MethodGet, "/assistant/sessions/"+sess.ID.String(), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTeamAgentSession_TicketTenantReader(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess := f.newTeamSession(t, agent.StateCompleted)
	srv := NewServer(f.deps)
	mint := func(claims *auth.Claims) (bool, int) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
		ok := srv.mintAssistantTicketAuthorized(rec, req, claims, sess.ID.String())
		return ok, rec.Code
	}
	if ok, _ := mint(f.reader); !ok {
		t.Errorf("tenant reader must mint a ticket for a team_agent session")
	}
	if ok, code := mint(f.stranger); ok || code != http.StatusNotFound {
		t.Errorf("stranger mint = %v/%d, want refused with 404", ok, code)
	}
}

func TestTeamAgentSession_ApprovalsTenantReader(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess, _ := f.teamSessionWithApproval(t)
	rec := f.do(t, f.reader, http.MethodGet, "/assistant/approvals?session="+sess.ID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var page struct {
		Items []assistantApprovalJSON `json:"items"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].Tool != "cancel_run" {
		t.Errorf("items = %+v", page.Items)
	}
}

func TestTeamAgentSession_ApproveTenantWriter(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess, approvalID := f.teamSessionWithApproval(t)
	rec := f.do(t, f.writer, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body = %s", rec.Code, rec.Body)
	}
	// The approved call runs under the team_agent origin, as the approver.
	if got := f.calls.all(); len(got) != 1 || got[0] != "team_agent:cancel_run" {
		t.Errorf("calls = %v, want [team_agent:cancel_run]", got)
	}
	// The BFF takes no lease and does not resume a team_agent session. The
	// runner's resume scan does that.
	got, err := f.mem.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != agent.StateWaitingApproval || got.LeaseOwner != "" {
		t.Errorf("after approve: state = %s lease = %q, want waiting_approval with no lease", got.State, got.LeaseOwner)
	}
	f.rt.Wait()
}

func TestTeamAgentSession_ApproveAgentSubjectRefused(t *testing.T) {
	f := newTeamAgentFixture(t)
	_, approvalID := f.teamSessionWithApproval(t)
	rec := f.do(t, f.agent, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an agent subject", rec.Code)
	}
	if len(f.calls.all()) != 0 {
		t.Errorf("a refused approval ran a tool")
	}
}

func TestTeamAgentSession_ApproveReader404(t *testing.T) {
	f := newTeamAgentFixture(t)
	_, approvalID := f.teamSessionWithApproval(t)
	rec := f.do(t, f.reader, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a reader", rec.Code)
	}
	if len(f.calls.all()) != 0 {
		t.Errorf("a refused approval ran a tool")
	}
}

func TestTeamAgentSession_Cancel(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess := f.newTeamSession(t, agent.StateRunning)
	rec := f.do(t, f.writer, http.MethodPost, "/assistant/sessions/"+sess.ID.String()+"/cancel", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	got, err := f.mem.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != agent.StateCancelled {
		t.Errorf("state = %s, want cancelled", got.State)
	}
	// A reader may not cancel.
	sess2 := f.newTeamSession(t, agent.StateRunning)
	if rec := f.do(t, f.reader, http.MethodPost, "/assistant/sessions/"+sess2.ID.String()+"/cancel", nil); rec.Code != http.StatusNotFound {
		t.Errorf("reader cancel status = %d, want 404", rec.Code)
	}
}

func TestTeamAgentSession_MessageConflict(t *testing.T) {
	f := newTeamAgentFixture(t)
	sess := f.newTeamSession(t, agent.StateCompleted)
	rec := f.do(t, f.writer, http.MethodPost, "/assistant/sessions/"+sess.ID.String()+"/messages", map[string]any{"message": "again"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"team_agent_session"`) {
		t.Errorf("status = %d body = %s, want 409 team_agent_session", rec.Code, rec.Body)
	}
}

func TestAssistantSession_Unchanged(t *testing.T) {
	f := newAssistantFixture(t)
	ctx := context.Background()
	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.rt.Store.CreateSession(ctx, agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: "admin-user", Cluster: "core",
		Trigger: agent.TriggerManual, Provider: "smoke", Model: "m1",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs, State: agent.StateCompleted,
	}, nil); err != nil {
		t.Fatal(err)
	}
	// The owner reads the session.
	if rec := f.do(t, f.admin, http.MethodGet, "/assistant/sessions/"+id.String(), nil); rec.Code != http.StatusOK {
		t.Errorf("owner status = %d, want 200", rec.Code)
	}
	// A caller with assistant:use who is not the owner gets 404.
	if rec := f.do(t, f.viewer, http.MethodGet, "/assistant/sessions/"+id.String(), nil); rec.Code != http.StatusNotFound {
		t.Errorf("other user status = %d, want 404", rec.Code)
	}
	// A caller without assistant:use gets 403 on an assistant session.
	noUse := &auth.Claims{Subject: "nobody", PreferredUsername: "nobody", IssuedAt: time.Now()}
	if rec := f.do(t, noUse, http.MethodGet, "/assistant/sessions/"+id.String(), nil); rec.Code != http.StatusForbidden {
		t.Errorf("no-use status = %d, want 403", rec.Code)
	}
}
