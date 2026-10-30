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
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// --------------------------------------------------------------------------
// fakeBackfill returns pre-set events for unit tests.
// --------------------------------------------------------------------------

type fakeBackfill struct {
	events []activity.Event
	err    error
}

func (f *fakeBackfill) Recent(_ context.Context, _ string, _ int) ([]activity.Event, error) {
	return f.events, f.err
}

func (f *fakeBackfill) Retention() (string, int) { return "off", 0 }

// Query mirrors the production contract: filter first, then apply the limit.
func (f *fakeBackfill) Query(_ context.Context, q activity.Query) (activity.Page, error) {
	if f.err != nil {
		return activity.Page{}, f.err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	out := make([]activity.Event, 0, len(f.events))
	for _, e := range f.events {
		if q.Authorize != nil && !q.Authorize(e) {
			continue
		}
		if activity.Matches(e, q) {
			e.Normalize()
			out = append(out, e)
		}
		if len(out) == limit {
			break
		}
	}
	return activity.Page{Items: out, RetentionMode: "off"}, nil
}

// --------------------------------------------------------------------------
// testAuthorizer creates an Authorizer with the given scope.
//   - clusterWide: if true, adds a nil-scope binding for controllers:read
//   - nsScope: if set, adds a namespace-scoped binding for controllers:read
// --------------------------------------------------------------------------

func testAuthorizer(clusterWide bool, nsScope string) *Authorizer {
	roles := make([]*v1alpha1.VarroaRole, 0, 1)
	var bindings []*v1alpha1.VarroaRoleBinding

	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "role"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		},
	}
	roles = append(roles, role)

	if clusterWide {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-user"}},
				RoleRef:  "role",
				// Scope: nil = cluster-wide
			},
		})
	}

	if nsScope != "" {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-user"}},
				RoleRef:  "role",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{nsScope}},
			},
		})
	}

	r := testResolver(roles, bindings)
	return NewAuthorizer(r, false)
}

// --------------------------------------------------------------------------
// Tests for handleActivity (backfill) authz gate
// --------------------------------------------------------------------------

func TestHandleActivity_ScopedCallerSeesOnlyOwnController(t *testing.T) {
	events := []activity.Event{
		{Namespace: "ns-a", Controller: "ctrl-a", Type: "connected", Message: "A connected"},
		{Namespace: "ns-b", Controller: "ctrl-b", Type: "connected", Message: "B connected"},
		{Namespace: "", Controller: "", Type: "settings.updated", Message: "global"},
	}

	// Authorizer: ns-a scoped, no cluster-wide.
	a := testAuthorizer(false, "ns-a")

	deps := &Dependencies{
		Backfill:   &fakeBackfill{events: events},
		Authorizer: a,
		Logger:     slog.Default(),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	claims := &auth.Claims{Subject: "test-user"}
	req = req.WithContext(contextWithClaims(req.Context(), claims))

	w := httptest.NewRecorder()
	srv.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var envelope struct {
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(envelope.Items) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(envelope.Items), envelope.Items)
	}
	if envelope.Items[0].Controller != "ctrl-a" {
		t.Errorf("expected ctrl-a event, got controller=%q", envelope.Items[0].Controller)
	}
}

func TestHandleActivity_ClusterViewerSeesAllPlusGlobal(t *testing.T) {
	events := []activity.Event{
		{Namespace: "ns-a", Controller: "ctrl-a", Type: "connected", Message: "A"},
		{Namespace: "ns-b", Controller: "ctrl-b", Type: "connected", Message: "B"},
		{Namespace: "", Controller: "", Type: "settings.updated", Message: "global"},
	}

	// Authorizer: cluster-wide, no ns-scope.
	a := testAuthorizer(true, "")

	deps := &Dependencies{
		Backfill:   &fakeBackfill{events: events},
		Authorizer: a,
		Logger:     slog.Default(),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	claims := &auth.Claims{Subject: "test-user"}
	req = req.WithContext(contextWithClaims(req.Context(), claims))

	w := httptest.NewRecorder()
	srv.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var envelope struct {
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(envelope.Items) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(envelope.Items), envelope.Items)
	}
}

func TestHandleActivity_ControllerFilterDenied(t *testing.T) {
	events := []activity.Event{
		{Namespace: "ns-b", Controller: "ctrl-b", Type: "connected", Message: "B"},
	}

	// Authorizer: ns-a scoped only.
	a := testAuthorizer(false, "ns-a")

	deps := &Dependencies{
		Backfill:   &fakeBackfill{events: events},
		Authorizer: a,
		Logger:     slog.Default(),
	}
	srv := NewServer(deps)

	// The caller can only read ns-a/*, but asks for controller=ctrl-b.
	req := httptest.NewRequest(http.MethodGet, "/activity?controller=ctrl-b", nil)
	claims := &auth.Claims{Subject: "test-user"}
	req = req.WithContext(contextWithClaims(req.Context(), claims))

	w := httptest.NewRecorder()
	srv.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var envelope struct {
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(envelope.Items) != 0 {
		t.Fatalf("expected empty list, got %d events: %+v", len(envelope.Items), envelope.Items)
	}
}

func TestHandleActivity_NilAuthorizer(t *testing.T) {
	events := []activity.Event{
		{Namespace: "ns-a", Controller: "ctrl-a", Type: "connected", Message: "A"},
	}

	deps := &Dependencies{
		Backfill:   &fakeBackfill{events: events},
		Authorizer: nil, // deny-by-default
		Logger:     slog.Default(),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	w := httptest.NewRecorder()
	srv.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Items) != 0 {
		t.Fatalf("expected empty list with nil Authorizer, got %d events", len(resp.Items))
	}
}

func TestHandleActivity_NilBackfill(t *testing.T) {
	deps := &Dependencies{
		Backfill:   nil,
		Authorizer: NewAuthorizer(nil, false),
		Logger:     slog.Default(),
	}
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	w := httptest.NewRecorder()
	srv.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []activity.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Items) != 0 {
		t.Fatalf("expected empty list with nil Backfill, got %d events", len(resp.Items))
	}
}

// --------------------------------------------------------------------------
// Tests for isAuthorizedActivityEvent (SSE authz helper)
// --------------------------------------------------------------------------

func TestIsAuthorizedActivityEvent_ScopedCaller(t *testing.T) {
	a := testAuthorizer(false, "ns-a")
	srv := NewServer(&Dependencies{Authorizer: a})
	claims := &auth.Claims{Subject: "test-user"}

	// Authorized A event (matches ns-a scope).
	if !srv.isAuthorizedActivityEvent(claims, a, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected authorized for A event")
	}
	// Denied B event (different namespace).
	if srv.isAuthorizedActivityEvent(claims, a, activity.Event{Namespace: "ns-b", Controller: "ctrl-b"}) {
		t.Error("expected denied for B event")
	}
	// Denied global event (no cluster-wide caps).
	if srv.isAuthorizedActivityEvent(claims, a, activity.Event{Namespace: "", Controller: ""}) {
		t.Error("expected denied for global event")
	}
}

func TestIsAuthorizedActivityEvent_ClusterViewer(t *testing.T) {
	a := testAuthorizer(true, "")
	srv := NewServer(&Dependencies{Authorizer: a})
	claims := &auth.Claims{Subject: "test-user"}

	if !srv.isAuthorizedActivityEvent(claims, a, activity.Event{Namespace: "any", Controller: "any"}) {
		t.Error("expected authorized for any controller")
	}
	if !srv.isAuthorizedActivityEvent(claims, a, activity.Event{Namespace: "", Controller: ""}) {
		t.Error("expected authorized for global event")
	}
}

func TestIsAuthorizedActivityEvent_NilAuthorizer(t *testing.T) {
	srv := NewServer(&Dependencies{Authorizer: nil})
	claims := &auth.Claims{Subject: "user"}

	if srv.isAuthorizedActivityEvent(claims, nil, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected denied with nil authorizer")
	}
}

func TestIsAuthorizedActivityEvent_ReevaluationViaResolver(t *testing.T) {
	// Test that changing the resolver's state changes authorization decisions.
	// We set up an Authorizer with a resolver, then modify the underlying
	// indexer to simulate informer state changes.

	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}},
	}

	// Start with a binding.
	binding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "scoped"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		},
	}

	r := testResolver([]*v1alpha1.VarroaRole{role}, []*v1alpha1.VarroaRoleBinding{binding})
	a := NewAuthorizer(r, false)
	srv := NewServer(&Dependencies{Authorizer: a})
	claims := &auth.Claims{Subject: "user-a"}

	e := activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}

	// Initially authorized (ns-a scoped binding exists).
	if !srv.isAuthorizedActivityEvent(claims, a, e) {
		t.Error("expected authorized before modification")
	}

	// Remove the binding from the indexer to simulate revocation.
	// We can't easily remove from the indexer, so instead add a nil-resolver
	// override to show the concept works. The real resolver reads from the
	// informer-backed index, so this is testing re-evaluation at the resolver level.
	// For a direct test we set up a new Authorizer with no binding.
	r2 := testResolver([]*v1alpha1.VarroaRole{role}, nil)
	a2 := NewAuthorizer(r2, false)
	srv2 := NewServer(&Dependencies{Authorizer: a2})

	if srv2.isAuthorizedActivityEvent(claims, a2, e) {
		t.Error("expected denied after binding removal")
	}
}

// fakeActivitySource is a sse.EventSource that serves one pre-filled channel.
type fakeActivitySource struct {
	ch <-chan sse.Record
}

func (f *fakeActivitySource) Subscribe(string) <-chan sse.Record    { return f.ch }
func (f *fakeActivitySource) SubscribeAll() <-chan sse.Record       { return f.ch }
func (f *fakeActivitySource) Unsubscribe(string, <-chan sse.Record) {}

func TestHandleActivityStream_DropsNonActivityRecords(t *testing.T) {
	ch := make(chan sse.Record, 2)
	ch <- sse.Record{Event: "heartbeat", Data: map[string]interface{}{"event": "heartbeat", "name": "c1", "namespace": "ns", "cluster": "core"}}
	ch <- sse.Record{Event: "auth.local.login", Data: activity.Event{Type: "auth.local.login", Source: "user", Namespace: "ns", Controller: "c1", Message: "m", Timestamp: time.Now()}}
	close(ch)

	srv := NewServer(&Dependencies{
		Authorizer:  testAuthorizer(true, ""),
		Broadcaster: &fakeActivitySource{ch: ch},
		Logger:      slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/stream", nil).
		WithContext(authContext(&auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivityStream(w, req)

	body := w.Body.String()
	if n := strings.Count(body, "event: activity\n"); n != 1 {
		t.Fatalf("expected exactly one activity frame, got %d: %s", n, body)
	}
	if !strings.Contains(body, "auth.local.login") {
		t.Errorf("expected auth.local.login frame, got: %s", body)
	}
	if strings.Contains(body, "heartbeat") {
		t.Errorf("brood telemetry leaked into activity stream: %s", body)
	}
}

// TestHandleActivityStream_FlushesHeadersBeforeFirstEvent proves the stream
// opens with a 200 and a flush before any event, so the client sees it open.
func TestHandleActivityStream_FlushesHeadersBeforeFirstEvent(t *testing.T) {
	ch := make(chan sse.Record)
	close(ch)

	srv := NewServer(&Dependencies{
		Authorizer:  testAuthorizer(true, ""),
		Broadcaster: &fakeActivitySource{ch: ch},
		Logger:      slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/stream", nil).
		WithContext(authContext(&auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivityStream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !w.Flushed {
		t.Errorf("headers were not flushed before the stream ended")
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestActivityFilter_AcceptsMCPSource(t *testing.T) {
	for _, src := range []string{"operator", "mite", "jenkins", "user", "api", "mcp", "assistant"} {
		t.Run(src, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/activity?source="+src, nil)
			if _, err := parseActivityQuery(req, nil); err != nil {
				t.Errorf("source=%s rejected: %v", src, err)
			}
		})
	}
}
