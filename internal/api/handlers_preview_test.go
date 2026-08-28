package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// previewResponse mirrors the JSON the preview handler writes.
type previewResponse struct {
	Merged       map[string]string `json:"merged"`
	Diff         map[string]string `json:"diff"`
	Warnings     []warningItem     `json:"warnings"`
	BaselineUsed string            `json:"baselineUsed"`
}

func newPreviewServer() *Server {
	fc := newFakeResourceClient()
	return NewServer(&Dependencies{
		Client:            fc,
		Store:             storeFromFake(fc),
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})
}

func TestPreviewController_AdminGate(t *testing.T) {
	srv := newPreviewServer()

	// Operator (controllers:create, no controllers:update) previewing a
	// controller that doesn't exist yet (as when previewing from the create
	// wizard) is allowed — there's nothing live to leak or mutate, and they
	// have the create right that will actually bring it into existence.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/controllers/team-a/ci/preview",
		strings.NewReader(`{"podOverrides":{"env":[{"name":"FOO","value":"bar"}]}}`))
	srv.handlePreviewController(w, req.WithContext(contextWithClaims(req.Context(), operatorClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusOK {
		t.Fatalf("operator preview of nonexistent controller: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// A caller with neither controllers:create nor controllers:update is still
	// forbidden, even when the controller doesn't exist.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/controllers/team-a/ci/preview",
		strings.NewReader(`{"podOverrides":{"env":[{"name":"FOO","value":"bar"}]}}`))
	noPermsClaims := &auth.Claims{Subject: "no-perms-user"}
	srv.handlePreviewController(w, req.WithContext(contextWithClaims(req.Context(), noPermsClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized preview: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	// Admin gets a merged preview.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/controllers/team-a/ci/preview",
		strings.NewReader(`{"podOverrides":{"env":[{"name":"FOO","value":"bar"}]}}`))
	srv.handlePreviewController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusOK {
		t.Fatalf("admin preview: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp previewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if resp.Merged["statefulSet"] == "" {
		t.Errorf("expected a merged statefulSet in the preview, got: %s", w.Body.String())
	}
	if !strings.Contains(resp.Merged["statefulSet"], "FOO") {
		t.Errorf("merged statefulSet should contain the override env FOO, got: %s", resp.Merged["statefulSet"])
	}
	// No live object in the fake → base baseline.
	if resp.BaselineUsed != "base" {
		t.Errorf("expected baselineUsed=base (no live object), got %q", resp.BaselineUsed)
	}
}

func TestPreviewController_TransientLookupErrorFailsClosed(t *testing.T) {
	// A transient GetControllerCRD error (anything other than NotFound) must
	// not be treated as "controller doesn't exist" — that would let a
	// create-only caller preview a controller that may well already exist and
	// they aren't authorized to see/mutate. It should fail the request
	// outright instead of silently falling back to create-permission auth.
	client := newFakeResourceClient()
	client.controllerErr = errors.New("etcd unavailable")
	ctrlGVR, gvrErr := crdstore.GVRFor[v1alpha1.Controller]()
	if gvrErr != nil {
		t.Fatal(gvrErr)
	}
	srv := NewServer(&Dependencies{
		Client: client,
		Store: func() *crdstore.Fake {
			st := storeFromFake(client)
			st.FailAlways("get", ctrlGVR, errors.New("etcd unavailable"))
			return st
		}(),
		Authorizer:        adminTestAuthorizer(),
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/controllers/team-a/ci/preview",
		strings.NewReader(`{"podOverrides":{"env":[{"name":"FOO","value":"bar"}]}}`))
	srv.handlePreviewController(w, req.WithContext(contextWithClaims(req.Context(), operatorClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on transient lookup error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPreviewController_MalformedOverlay(t *testing.T) {
	srv := newPreviewServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/controllers/team-a/ci/preview",
		strings.NewReader(`{"resourceOverlay":{"service":"{[ not valid yaml"}}`))
	srv.handlePreviewController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed overlay: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPreviewController_ProtectedEditWarns(t *testing.T) {
	srv := newPreviewServer()
	// An overlay that edits the operator-managed mite sidecar must still preview
	// (warn-but-allow) and surface a warning naming the mite container.
	body := `{"resourceOverlay":{"statefulSet":"spec:\n  template:\n    spec:\n      containers:\n      - name: mite\n        image: evil:latest\n"}}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/controllers/team-a/ci/preview", strings.NewReader(body))
	srv.handlePreviewController(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
	if w.Code != http.StatusOK {
		t.Fatalf("protected-edit preview: expected 200 (warn-but-allow), got %d: %s", w.Code, w.Body.String())
	}
	var resp previewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, wn := range resp.Warnings {
		if strings.Contains(strings.ToLower(wn.Message), "mite") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning naming the mite sidecar, got %+v", resp.Warnings)
	}
}
