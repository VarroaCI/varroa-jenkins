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

// tenantSessionWithApproval stores a running tenant_agent session that waits on one
// pending approval, and returns the session and the approval id.
func (f *tenantAgentFixture) tenantSessionWithApproval(t *testing.T) (agent.Session, string) {
	t.Helper()
	sess := f.newTenantSession(t, agent.StateRunning)
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

func TestTenantAgentSession_GetTenantReader(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess := f.newTenantSession(t, agent.StateCompleted)
	rec := f.do(t, f.reader, http.MethodGet, "/assistant/sessions/"+sess.ID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
}

func TestTenantAgentSession_GetStranger404(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess := f.newTenantSession(t, agent.StateCompleted)
	// A stranger without assistant:use still gets 404, not 403, because a
	// tenant_agent session follows the tenant table.
	rec := f.do(t, f.stranger, http.MethodGet, "/assistant/sessions/"+sess.ID.String(), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTenantAgentSession_TicketTenantReader(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess := f.newTenantSession(t, agent.StateCompleted)
	srv := NewServer(f.deps)
	mint := func(claims *auth.Claims) (bool, int) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
		ok := srv.mintAssistantTicketAuthorized(rec, req, claims, sess.ID.String())
		return ok, rec.Code
	}
	if ok, _ := mint(f.reader); !ok {
		t.Errorf("tenant reader must mint a ticket for a tenant_agent session")
	}
	if ok, code := mint(f.stranger); ok || code != http.StatusNotFound {
		t.Errorf("stranger mint = %v/%d, want refused with 404", ok, code)
	}
}

func TestTenantAgentSession_ApprovalsTenantReader(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess, _ := f.tenantSessionWithApproval(t)
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

func TestTenantAgentSession_ApproveTenantWriter(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess, approvalID := f.tenantSessionWithApproval(t)
	rec := f.do(t, f.writer, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body = %s", rec.Code, rec.Body)
	}
	// The approved call runs under the tenant_agent origin, as the approver.
	if got := f.calls.all(); len(got) != 1 || got[0] != "tenant_agent:cancel_run" {
		t.Errorf("calls = %v, want [tenant_agent:cancel_run]", got)
	}
	// The BFF takes no lease and does not resume a tenant_agent session. The
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

// TestTenantAgentSession_ResultVisibleToReaderWithheldFromAdmin proves a tenant
// reader reads a Tenant agent tool result, and an admin who is not a tenant
// reader reads the approval with its result withheld.
func TestTenantAgentSession_ResultVisibleToReaderWithheldFromAdmin(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess, approvalID := f.tenantSessionWithApproval(t)
	if rec := f.do(t, f.writer, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil); rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body = %s", rec.Code, rec.Body)
	}
	f.rt.Wait()

	type detail struct {
		Approvals []assistantApprovalJSON `json:"approvals"`
	}
	rec := f.do(t, f.reader, http.MethodGet, "/assistant/sessions/"+sess.ID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reader get = %d body = %s", rec.Code, rec.Body)
	}
	var reader detail
	decodeBody(t, rec, &reader)
	if len(reader.Approvals) != 1 || reader.Approvals[0].Result == nil || reader.Approvals[0].ResultWithheld {
		t.Errorf("reader approvals = %+v, want a visible result", reader.Approvals)
	}

	rec = f.do(t, f.admin, http.MethodGet, "/assistant/sessions/"+sess.ID.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin get = %d body = %s", rec.Code, rec.Body)
	}
	var admin detail
	decodeBody(t, rec, &admin)
	if len(admin.Approvals) != 1 || admin.Approvals[0].Result != nil || !admin.Approvals[0].ResultWithheld {
		t.Errorf("admin approvals = %+v, want a withheld result", admin.Approvals)
	}
}

func TestTenantAgentSession_ApproveAgentSubjectRefused(t *testing.T) {
	f := newTenantAgentFixture(t)
	_, approvalID := f.tenantSessionWithApproval(t)
	rec := f.do(t, f.agent, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an agent subject", rec.Code)
	}
	if len(f.calls.all()) != 0 {
		t.Errorf("a refused approval ran a tool")
	}
}

func TestTenantAgentSession_ApproveReader404(t *testing.T) {
	f := newTenantAgentFixture(t)
	_, approvalID := f.tenantSessionWithApproval(t)
	rec := f.do(t, f.reader, http.MethodPost, "/assistant/approvals/"+approvalID+"/approve", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a reader", rec.Code)
	}
	if len(f.calls.all()) != 0 {
		t.Errorf("a refused approval ran a tool")
	}
}

func TestTenantAgentSession_Cancel(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess := f.newTenantSession(t, agent.StateRunning)
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
	sess2 := f.newTenantSession(t, agent.StateRunning)
	if rec := f.do(t, f.reader, http.MethodPost, "/assistant/sessions/"+sess2.ID.String()+"/cancel", nil); rec.Code != http.StatusNotFound {
		t.Errorf("reader cancel status = %d, want 404", rec.Code)
	}
}

func TestTenantAgentSession_MessageConflict(t *testing.T) {
	f := newTenantAgentFixture(t)
	sess := f.newTenantSession(t, agent.StateCompleted)
	rec := f.do(t, f.writer, http.MethodPost, "/assistant/sessions/"+sess.ID.String()+"/messages", map[string]any{"message": "again"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"tenant_agent_session"`) {
		t.Errorf("status = %d body = %s, want 409 tenant_agent_session", rec.Code, rec.Body)
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
