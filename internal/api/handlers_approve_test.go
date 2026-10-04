package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/varroaci/varroa-jenkins/api/openapi"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// approveTestServer builds a Server whose admin caller may approve, over a
// fake reconciler that answers err.
func approveTestServer(err error) (*Server, *fakeActionReconciler) {
	client := newFakeResourceClient()
	rec := &fakeActionReconciler{approveErr: err}
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		Authorizer:        adminAuthorizer(),
		Reconciler:        rec,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})
	return srv, rec
}

func postApprove(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/controllers/ns/ctrl/approve", strings.NewReader(body))
	req = req.WithContext(contextWithClaims(req.Context(), publishClaims))
	w := httptest.NewRecorder()
	srv.handleApproveRestart(w, req, "core", "ns", "ctrl")
	return w
}

// approve409Schema answers the 409 body schema of approveController from
// the embedded spec.
func approve409Schema(t *testing.T) *openapi3.Schema {
	t.Helper()
	doc, err := (&openapi3.Loader{IsExternalRefsAllowed: false}).LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	item := doc.Paths.Find("/api/v1/clusters/{cluster}/controllers/{ns}/{name}/approve")
	if item == nil || item.Post == nil {
		t.Fatal("spec has no approveController operation")
	}
	resp := item.Post.Responses.Status(http.StatusConflict)
	if resp == nil || resp.Value == nil {
		t.Fatal("approveController declares no 409 response")
	}
	return resp.Value.Content.Get("application/json").Schema.Value
}

// TestApproveRestartContentChangedReturns409 pins the refusal contract: a
// changed pending change answers 409 with the new sections, in the shape
// the spec declares, and the handler passes the reviewed hash on.
func TestApproveRestartContentChangedReturns409(t *testing.T) {
	srv, rec := approveTestServer(&controller.PendingRestartChangedError{Changes: []string{"config", "items"}})
	w := postApprove(t, srv, `{"action":"approve","expectedContentHash":"seen"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if rec.expected != "seen" {
		t.Fatalf("expected content hash = %q, want seen", rec.expected)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	changes, _ := body["changes"].([]any)
	if len(changes) != 2 || changes[0] != "config" || changes[1] != "items" {
		t.Fatalf("changes = %v, want [config items]", body["changes"])
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatal("409 body carries no error text")
	}
	if err := approve409Schema(t).VisitJSON(body); err != nil {
		t.Fatalf("409 body does not match the spec: %v", err)
	}
}

// TestApproveRestartNoPendingSentinelReturns409 pins that the refusal keeps
// its 409 when it reaches the BFF as the wrapped sentinel.
func TestApproveRestartNoPendingSentinelReturns409(t *testing.T) {
	srv, _ := approveTestServer(fmt.Errorf("%w for ns/ctrl", controller.ErrNoPendingRestart))
	w := postApprove(t, srv, `{"action":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

// TestApproveRestartNoPendingReturns409 pins that the coded refusal for a
// controller with no pending restart keeps its 409 status and message.
func TestApproveRestartNoPendingReturns409(t *testing.T) {
	msg := fmt.Sprintf("%s for ns/ctrl", controller.ErrNoPendingRestart)
	srv, _ := approveTestServer(&controller.ActionError{Code: bus.CodeConflict, Msg: msg})
	w := postApprove(t, srv, `{"action":"restart"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != msg {
		t.Fatalf("error = %v, want %q", body["error"], msg)
	}
	if err := approve409Schema(t).VisitJSON(body); err != nil {
		t.Fatalf("409 body does not match the spec: %v", err)
	}
}
