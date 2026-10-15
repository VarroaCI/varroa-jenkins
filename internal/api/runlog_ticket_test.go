package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/signing"
)

// --- 5.8: the run: ticket scope ---

// runTicketTestServer wires a Server with the given authorizer, record
// store, and a real TicketIssuer.
func runTicketTestServer(t *testing.T, authorizer *Authorizer, reader recordstore.Reader) *Server {
	t.Helper()
	issuer := newTicketIssuerForTest(t)
	deps := &Dependencies{
		Authorizer:   authorizer,
		RecordStore:  reader,
		TicketIssuer: issuer,
	}
	return NewServer(deps)
}

func mintTicketRequest(runID string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/stream/ticket?scope=run:"+runID, nil)
}

// TestMintRunTicket_ForbiddenOutsideTenant proves a 403 for a run whose
// tenant the caller may not read.
func TestMintRunTicket_ForbiddenOutsideTenant(t *testing.T) {
	reader := newFakeRecordReader()
	reader.addRun(recordstore.Run{RunID: "run-t1", Tenant: "team-b", JobPath: "team-b/ci", Cluster: "core"})

	authorizer := pipelineGrantAuthorizer([]string{"read"}, "team-a") // grants team-a, not team-b
	srv := runTicketTestServer(t, authorizer, reader)

	req := mintTicketRequest("run-t1").WithContext(authContext(testClaims()))
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestMintRunTicket_NotFoundForUnknownRun proves a 404 for an unknown
// run.
func TestMintRunTicket_NotFoundForUnknownRun(t *testing.T) {
	reader := newFakeRecordReader()
	authorizer := pipelineGrantAuthorizer([]string{"read"}, "team-a")
	srv := runTicketTestServer(t, authorizer, reader)

	req := mintTicketRequest("does-not-exist").WithContext(authContext(testClaims()))
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestMintRunTicket_SurvivesDeletedCell proves a minted ticket for a
// permitted caller even when the run's cell no longer exists: the check
// reads only the row's own tenant and job path.
func TestMintRunTicket_SurvivesDeletedCell(t *testing.T) {
	reader := newFakeRecordReader()
	// Cluster/Namespace/Cell left empty: the row names a cell that no
	// longer exists anywhere.
	reader.addRun(recordstore.Run{RunID: "run-t2", Tenant: "team-a", JobPath: "team-a/ci"})

	authorizer := pipelineGrantAuthorizer([]string{"read"}, "team-a")
	srv := runTicketTestServer(t, authorizer, reader)

	req := mintTicketRequest("run-t2").WithContext(authContext(testClaims()))
	w := httptest.NewRecorder()
	srv.handleStreamTicket(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRunTicket_ScopeMismatchRejectedOnConnect proves a ticket minted
// for one run cannot open another run's log stream (auth.TicketVerifier
// + sseScope, the connect-time half of this seam).
func TestRunTicket_ScopeMismatchRejectedOnConnect(t *testing.T) {
	signer, err := signing.New()
	if err != nil {
		t.Fatalf("signing.New: %v", err)
	}
	issuer := auth.NewTicketIssuer(signer, "https://bff.test", 30*time.Second)
	verifier := auth.NewTicketVerifier(signer.PublicKey(), "https://bff.test")

	ticket, _, err := issuer.Mint(testClaims(), "run:run-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The requested stream's scope, exactly as sseScope derives it for
	// GET /runs/{run}/log.
	if _, err := verifier.Verify(context.Background(), ticket, "run:run-b"); err == nil {
		t.Fatal("expected a scope mismatch to be rejected")
	}
	if _, err := verifier.Verify(context.Background(), ticket, "run:run-a"); err != nil {
		t.Fatalf("expected the matching scope to verify, got %v", err)
	}
}
