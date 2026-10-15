package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

// fakeActivitySink records every published event, standing in for the bus.
type fakeActivitySink struct {
	events []activity.Event
}

func (f *fakeActivitySink) Publish(e activity.Event) {
	f.events = append(f.events, e)
}

// newBreakGlassTestServer builds a server with every dependency
// handleBreakGlass needs on the happy path: an admin-capable Authorizer, a
// real MiteTokenSigner, a recording ActivityPublisher, and a single-cluster
// Brood whose local cluster is "core". Individual tests unset one field to
// exercise its refusal step.
func newBreakGlassTestServer(t *testing.T) (*Server, *fakeResourceClient, *fakeActivitySink) {
	t.Helper()
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{}
	client.namespaces["team-a"] = true

	signer, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	sink := &fakeActivitySink{}

	srv := NewServer(&Dependencies{
		Client:             client,
		Store:              storeFromFake(client),
		Authorizer:         adminTestAuthorizer(),
		OperatorNamespace:  "test-ns",
		Logger:             slog.Default(),
		Brood:              newFakeBrood(client),
		JenkinsTokenSigner: signer,
		ActivityPublisher:  sink,
	})
	return srv, client, sink
}

// seedHeadlessController seeds a headless controller named "ci" in team-a
// with a fixed UID, so the derived Service name is deterministic across
// tests (mirrors the UID fixture in TestFetchPodLogsOneShot_UsesUIDPodName).
func seedHeadlessController(t *testing.T, client *fakeResourceClient) *v1alpha1.Controller {
	t.Helper()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a", UID: "54c62aff-0000-0000-0000-000000000000"},
		Spec:       v1alpha1.ControllerSpec{Headless: true},
	}
	crdstore.MustSeed(client.crdStore, cr)
	return cr
}

func postBreakGlass(t *testing.T, srv *Server, claims *auth.Claims, cluster, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/"+cluster+"/controllers/team-a/"+name+"/break-glass", strings.NewReader(body))
	ctx := context.Background()
	if claims != nil {
		ctx = contextWithClaims(ctx, claims)
	}
	srv.handleBreakGlass(w, req.WithContext(ctx), cluster, "team-a", name)
	return w
}

// TestHandleBreakGlass_ProcessingOrder pins one case per row of the
// break-glass-access delta's ten-step processing-order table. Every case
// before the row under test is set up to pass, so the row under test is the
// first (and only) failure.
func TestHandleBreakGlass_ProcessingOrder(t *testing.T) {
	t.Run("1 wrong method is rejected", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		srv.handleBreakGlass(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", "team-a", "ci")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
		}
	})

	t.Run("2 caller without the verb is forbidden", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, operatorClaims, "core", "ci", `{"reason":"incident"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("3 no signer configured", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		srv.deps.JenkinsTokenSigner = nil
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident"}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
		}
	})

	t.Run("4 no activity publisher configured", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		srv.deps.ActivityPublisher = nil
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident"}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
		}
	})

	t.Run("5 non-core cluster is rejected", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "hive", "ci", `{"reason":"incident"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "core cluster") {
			t.Errorf("error body should name the limitation, got %s", w.Body.String())
		}
	})

	t.Run("6 missing controller is not found", func(t *testing.T) {
		srv, _, _ := newBreakGlassTestServer(t)
		w := postBreakGlass(t, srv, adminClaims, "core", "missing", `{"reason":"incident"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
		}
	})

	t.Run("7 a reachable controller needs no grant", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		crdstore.MustSeed(client.crdStore, &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "reachable", Namespace: "team-a"},
			Status:     v1alpha1.ControllerStatus{Endpoint: "https://reachable.example.com"},
		})
		w := postBreakGlass(t, srv, adminClaims, "core", "reachable", `{"reason":"incident"}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "reachable.example.com") {
			t.Errorf("error body should name the controller endpoint, got %s", w.Body.String())
		}
	})

	t.Run("8 empty reason is rejected", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":""}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("9 out-of-bounds ttlSeconds is rejected", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident","ttlSeconds":299}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("10 every check passes", func(t *testing.T) {
		srv, client, sink := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if len(sink.events) != 1 {
			t.Fatalf("events = %d, want 1", len(sink.events))
		}
	})
}

// TestHandleBreakGlass_TTL pins task 8.3: the default, both bounds, and the
// reason-length rejection.
func TestHandleBreakGlass_TTL(t *testing.T) {
	t.Run("default is 900 seconds", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		before := time.Now()
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp breakGlassResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
		if err != nil {
			t.Fatalf("expiresAt %q: %v", resp.ExpiresAt, err)
		}
		got := expiresAt.Sub(before)
		if got < 895*time.Second || got > 905*time.Second {
			t.Errorf("expiresAt - now = %v, want ~900s", got)
		}
	})

	t.Run("lower bound accepted", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident","ttlSeconds":300}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("upper bound accepted", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident","ttlSeconds":3600}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("above the upper bound is rejected", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident","ttlSeconds":3601}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a 256-character reason is accepted", func(t *testing.T) {
		srv, client, _ := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		reason := strings.Repeat("a", 256)
		body, _ := json.Marshal(breakGlassRequest{Reason: reason})
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", string(body))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a 257-character reason is rejected, not truncated", func(t *testing.T) {
		srv, client, sink := newBreakGlassTestServer(t)
		seedHeadlessController(t, client)
		reason := strings.Repeat("a", 257)
		body, _ := json.Marshal(breakGlassRequest{Reason: reason})
		w := postBreakGlass(t, srv, adminClaims, "core", "ci", string(body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if len(sink.events) != 0 {
			t.Errorf("expected no audit event for a rejected reason, got %d", len(sink.events))
		}
	})
}

// TestHandleBreakGlass_AuditBeforeMint pins task 8.4: the audit event is
// published before the mint is attempted. A claims value with an empty
// Subject passes CanBreakGlass (group-based) but fails
// GenerateUserJenkinsToken, so a successful publish alongside a failed mint
// proves the publish is not contingent on — and therefore precedes — the
// mint, and is never skipped when the mint later fails.
func TestHandleBreakGlass_AuditBeforeMint(t *testing.T) {
	srv, client, sink := newBreakGlassTestServer(t)
	seedHeadlessController(t, client)
	subjectlessClaims := &auth.Claims{Groups: []string{"admins"}}

	w := postBreakGlass(t, srv, subjectlessClaims, "core", "ci", `{"reason":"incident 4711"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the mint fails on an empty subject): %s", w.Code, w.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1 (published even though the mint failed)", len(sink.events))
	}
	ev := sink.events[0]
	if ev.Type != "controller.breakglass.granted" {
		t.Errorf("event type = %q, want controller.breakglass.granted", ev.Type)
	}
	if ev.Reason != "incident 4711" {
		t.Errorf("event reason = %q, want %q", ev.Reason, "incident 4711")
	}
	if ev.Severity != "warning" {
		t.Errorf("event severity = %q, want warning", ev.Severity)
	}
}

// TestHandleBreakGlass_ResponseShape pins task 8.5's response body.
func TestHandleBreakGlass_ResponseShape(t *testing.T) {
	srv, client, sink := newBreakGlassTestServer(t)
	cr := seedHeadlessController(t, client)

	w := postBreakGlass(t, srv, adminClaims, "core", "ci", `{"reason":"incident 4711","ttlSeconds":600}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp breakGlassResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Error("token is empty")
	}
	if _, err := time.Parse(time.RFC3339, resp.ExpiresAt); err != nil {
		t.Errorf("expiresAt %q is not RFC3339: %v", resp.ExpiresAt, err)
	}

	wantService := strings.TrimSuffix(controller.PodName(cr, 0), "-0") + "-svc"
	if resp.Target.Service != wantService {
		t.Errorf("target.service = %q, want %q", resp.Target.Service, wantService)
	}
	if resp.Target.Namespace != "team-a" {
		t.Errorf("target.namespace = %q, want team-a", resp.Target.Namespace)
	}
	if resp.Target.Port != 8080 {
		t.Errorf("target.port = %d, want 8080", resp.Target.Port)
	}
	wantURL := "http://" + wantService + ".team-a.svc.cluster.local:8080/"
	if resp.LocationURL != wantURL {
		t.Errorf("locationUrl = %q, want %q", resp.LocationURL, wantURL)
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	if sink.events[0].Reason != "incident 4711" {
		t.Errorf("event reason = %q, want %q", sink.events[0].Reason, "incident 4711")
	}
}
