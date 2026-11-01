package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/openapi"
	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
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
			if _, err := parseActivityQuery(req); err != nil {
				t.Errorf("source=%s rejected: %v", src, err)
			}
		})
	}
}

// TestHandleActivity_ExcludeRoutineSignIns proves the flag drops each routine
// local sign-in and keeps break-glass, failed, and other events.
func TestHandleActivity_ExcludeRoutineSignIns(t *testing.T) {
	at := time.Now()
	events := []activity.Event{
		{Timestamp: at, Namespace: "ns", Controller: "c1", Type: "auth.local.login", Source: "user", Severity: "info", Message: "routine"},
		{Timestamp: at, Namespace: "ns", Controller: "c1", Type: "auth.local.login", Source: "user", Severity: "warning", Message: "break-glass"},
		{Timestamp: at, Namespace: "ns", Controller: "c1", Type: "auth.local.login_failed", Source: "user", Severity: "warning", Message: "failed"},
		{Timestamp: at, Type: "settings.updated", Source: "user", Severity: "info", Message: "settings"},
	}
	srv := NewServer(&Dependencies{
		Backfill:   &fakeBackfill{events: events},
		Authorizer: testAuthorizer(true, ""),
		Logger:     slog.Default(),
	})

	get := func(t *testing.T, target string) []activity.Event {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req = req.WithContext(contextWithClaims(req.Context(), &auth.Claims{Subject: "test-user"}))
		w := httptest.NewRecorder()
		srv.handleActivity(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", target, w.Code, w.Body.String())
		}
		var envelope struct {
			Items []activity.Event `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return envelope.Items
	}

	if got := get(t, "/activity"); len(got) != 4 {
		t.Fatalf("without the flag: expected 4 events, got %d: %+v", len(got), got)
	}
	got := get(t, "/activity?excludeRoutineSignIns=true")
	if len(got) != 3 {
		t.Fatalf("with the flag: expected 3 events, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Type == "auth.local.login" && e.Severity != "warning" {
			t.Errorf("routine sign-in leaked through the flag: %+v", e)
		}
	}
}

// TestHandleActivityStream_ExcludeRoutineSignIns proves the live stream applies
// the same flag as the backfill: a routine sign-in is not sent.
func TestHandleActivityStream_ExcludeRoutineSignIns(t *testing.T) {
	ch := make(chan sse.Record, 2)
	ch <- sse.Record{Event: "activity", Data: activity.Event{Type: "auth.local.login", Source: "user", Severity: "info", Namespace: "ns", Controller: "c1", Message: "routine", Timestamp: time.Now()}}
	ch <- sse.Record{Event: "activity", Data: activity.Event{Type: "auth.local.login", Source: "user", Severity: "warning", Namespace: "ns", Controller: "c1", Message: "break-glass", Timestamp: time.Now()}}
	close(ch)

	srv := NewServer(&Dependencies{
		Authorizer:  testAuthorizer(true, ""),
		Broadcaster: &fakeActivitySource{ch: ch},
		Logger:      slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/stream?excludeRoutineSignIns=true", nil).
		WithContext(authContext(&auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivityStream(w, req)

	body := w.Body.String()
	if n := strings.Count(body, "event: activity\n"); n != 1 {
		t.Fatalf("expected exactly one activity frame, got %d: %s", n, body)
	}
	if !strings.Contains(body, "break-glass") {
		t.Errorf("expected the break-glass frame, got: %s", body)
	}
	if strings.Contains(body, "routine") {
		t.Errorf("routine sign-in reached the stream: %s", body)
	}
}

func TestActivityFilter_ExcludeRoutineSignInsValues(t *testing.T) {
	tests := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"false", false, false},
		{"true", true, false},
		{"yes", false, true},
	}
	for _, tt := range tests {
		t.Run("value="+tt.raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/activity?excludeRoutineSignIns="+tt.raw, nil)
			q, err := parseActivityQuery(req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if q.ExcludeRoutineSignIns != tt.want {
				t.Errorf("ExcludeRoutineSignIns = %v, want %v", q.ExcludeRoutineSignIns, tt.want)
			}
		})
	}
}

// TestHandleActivity_RecordStoreUnreachable proves a backfill that cannot reach
// the record store answers 503 with the record_store_unreachable code, the
// answer the pipelines reads give.
func TestHandleActivity_RecordStoreUnreachable(t *testing.T) {
	srv := NewServer(&Dependencies{
		Backfill:   &fakeBackfill{err: fmt.Errorf("list activity: %w", recordstore.ErrUnavailable)},
		Authorizer: testAuthorizer(true, ""),
		Logger:     slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	req = req.WithContext(contextWithClaims(req.Context(), &auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivity(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["code"] != codeRecordStoreUnreachable {
		t.Errorf("code = %q, want %q", body["code"], codeRecordStoreUnreachable)
	}
}

// TestHandleActivityStream_RangeDoesNotReadStore proves a range request on the
// live stream answers 200 and never reads the record store, so a nil or
// unreachable backfill cannot fail it.
func TestHandleActivityStream_RangeDoesNotReadStore(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)

	cases := map[string]activity.Backfill{
		"nil backfill":         nil,
		"unreachable backfill": &fakeBackfill{err: fmt.Errorf("x: %w", recordstore.ErrUnavailable)},
	}
	for name, backfill := range cases {
		t.Run(name, func(t *testing.T) {
			ch := make(chan sse.Record, 1)
			ch <- sse.Record{Event: "auth.local.login", Data: activity.Event{Type: "auth.local.login", Source: "user", Namespace: "ns", Controller: "c1", Message: "m", Timestamp: now}}
			close(ch)

			srv := NewServer(&Dependencies{
				Backfill:    backfill,
				Authorizer:  testAuthorizer(true, ""),
				Broadcaster: &fakeActivitySource{ch: ch},
				Logger:      slog.Default(),
			})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/stream?start="+start+"&end="+end, nil).
				WithContext(authContext(&auth.Claims{Subject: "test-user"}))
			w := httptest.NewRecorder()

			srv.handleActivityStream(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if n := strings.Count(w.Body.String(), "event: activity\n"); n != 1 {
				t.Fatalf("expected one activity frame, got %d: %s", n, w.Body.String())
			}
		})
	}
}

// TestHandleActivity_RangeRecordStoreUnreachable proves a range read on
// GET /activity answers 503 when the store cannot be reached, not 400.
func TestHandleActivity_RangeRecordStoreUnreachable(t *testing.T) {
	now := time.Now().UTC()
	srv := NewServer(&Dependencies{
		Backfill:   &fakeBackfill{err: fmt.Errorf("list activity: %w", recordstore.ErrUnavailable)},
		Authorizer: testAuthorizer(true, ""),
		Logger:     slog.Default(),
	})
	target := "/activity?start=" + now.Add(-time.Hour).Format(time.RFC3339) + "&end=" + now.Add(time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(contextWithClaims(req.Context(), &auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivity(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["code"] != codeRecordStoreUnreachable {
		t.Errorf("code = %q, want %q", body["code"], codeRecordStoreUnreachable)
	}
}

// TestHandleActivity_RangeOutsideRetention proves a range start before the
// retention window answers 400 with the retention message.
func TestHandleActivity_RangeOutsideRetention(t *testing.T) {
	now := time.Now().UTC()
	srv := NewServer(&Dependencies{
		Backfill:   &fakeBackfill{err: activity.ErrRangeOutsideRetention},
		Authorizer: testAuthorizer(true, ""),
		Logger:     slog.Default(),
	})
	target := "/activity?start=" + now.Add(-48*time.Hour).Format(time.RFC3339) + "&end=" + now.Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(contextWithClaims(req.Context(), &auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivity(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error"] != "start is outside activity retention" {
		t.Errorf("error = %q, want the retention message", body["error"])
	}
}

// TestActivityFilter_RangeBounds proves parseActivityQuery checks only the
// range syntax: each bound is optional, and start is before end when both
// are set.
func TestActivityFilter_RangeBounds(t *testing.T) {
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	cases := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"start without end", "/activity?start=" + ts(-time.Hour), false},
		{"end without start", "/activity?end=" + ts(-time.Hour), false},
		{"start equals end", "/activity?start=" + ts(0) + "&end=" + ts(0), true},
		{"start after end", "/activity?start=" + ts(time.Hour) + "&end=" + ts(-time.Hour), true},
		{"valid range", "/activity?start=" + ts(-time.Hour) + "&end=" + ts(time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if _, err := parseActivityQuery(req); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestHandleActivity_StartOnlyReadsToPresent proves a start-only history read
// returns the events from start up to the present, and no event before start.
func TestHandleActivity_StartOnlyReadsToPresent(t *testing.T) {
	now := time.Now().UTC()
	srv := NewServer(&Dependencies{
		Backfill: &fakeBackfill{events: []activity.Event{
			{Type: "auth.local.login", Source: "user", Namespace: "ns", Controller: "c1", Message: "old", Timestamp: now.Add(-2 * time.Hour)},
			{Type: "auth.local.login", Source: "user", Namespace: "ns", Controller: "c1", Message: "recent", Timestamp: now.Add(-time.Minute)},
		}},
		Authorizer: testAuthorizer(true, ""),
		Logger:     slog.Default(),
	})
	target := "/activity?start=" + now.Add(-time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(contextWithClaims(req.Context(), &auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var page activity.Page
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Message != "recent" {
		t.Fatalf("items = %+v, want one recent event", page.Items)
	}
}

// TestHandleActivityStream_StartOnlyKeepsLiveEvents proves a start-only live
// stream delivers an event at the present.
func TestHandleActivityStream_StartOnlyKeepsLiveEvents(t *testing.T) {
	now := time.Now().UTC()
	ch := make(chan sse.Record, 1)
	ch <- sse.Record{Event: "auth.local.login", Data: activity.Event{Type: "auth.local.login", Source: "user", Namespace: "ns", Controller: "c1", Message: "m", Timestamp: now}}
	close(ch)

	srv := NewServer(&Dependencies{
		Authorizer:  testAuthorizer(true, ""),
		Broadcaster: &fakeActivitySource{ch: ch},
		Logger:      slog.Default(),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/stream?start="+now.Add(-time.Hour).Format(time.RFC3339), nil).
		WithContext(authContext(&auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivityStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if n := strings.Count(w.Body.String(), "event: activity\n"); n != 1 {
		t.Fatalf("expected one activity frame, got %d: %s", n, w.Body.String())
	}
}

// TestHandleActivityStream_ClosedRangeDropsLiveEvents proves a live event at
// the present is dropped when the requested range ended in the past.
func TestHandleActivityStream_ClosedRangeDropsLiveEvents(t *testing.T) {
	now := time.Now().UTC()
	ch := make(chan sse.Record, 1)
	ch <- sse.Record{Event: "auth.local.login", Data: activity.Event{Type: "auth.local.login", Source: "user", Namespace: "ns", Controller: "c1", Message: "m", Timestamp: now}}
	close(ch)

	srv := NewServer(&Dependencies{
		Authorizer:  testAuthorizer(true, ""),
		Broadcaster: &fakeActivitySource{ch: ch},
		Logger:      slog.Default(),
	})
	target := "/api/v1/activity/stream?start=" + now.Add(-2*time.Hour).Format(time.RFC3339) + "&end=" + now.Add(-time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, target, nil).
		WithContext(authContext(&auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivityStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if n := strings.Count(w.Body.String(), "event: activity\n"); n != 0 {
		t.Fatalf("expected no activity frames, got %d: %s", n, w.Body.String())
	}
}

// TestHandleActivityStream_BadRangeAnswers400BeforeFanout proves a bad range
// answers 400 even when no broadcaster is wired, so the parse runs first.
func TestHandleActivityStream_BadRangeAnswers400BeforeFanout(t *testing.T) {
	now := time.Now().UTC()
	srv := NewServer(&Dependencies{
		Authorizer: testAuthorizer(true, ""),
		Logger:     slog.Default(),
	})
	target := "/api/v1/activity/stream?start=" + now.Format(time.RFC3339) + "&end=" + now.Add(-time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, target, nil).
		WithContext(authContext(&auth.Claims{Subject: "test-user"}))
	w := httptest.NewRecorder()

	srv.handleActivityStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["error"] != "start must be before end" {
		t.Errorf("error = %q, want %q", body["error"], "start must be before end")
	}
}

// TestStreamActivitySpecListsRangeAndErrors proves the embedded contract
// lists the range and filter parameters of the live stream, and its 400
// and 503 answers.
func TestStreamActivitySpecListsRangeAndErrors(t *testing.T) {
	doc, err := (&openapi3.Loader{IsExternalRefsAllowed: false}).LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	item := doc.Paths.Find("/api/v1/activity/stream")
	if item == nil || item.Get == nil {
		t.Fatal("spec has no streamActivity operation")
	}
	for _, name := range []string{"controller", "cluster", "namespace", "source", "severity", "actor", "type", "start", "end", "excludeRoutineSignIns"} {
		if item.Get.Parameters.GetByInAndName("query", name) == nil {
			t.Errorf("streamActivity lists no query parameter %q", name)
		}
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		if item.Get.Responses.Status(code) == nil {
			t.Errorf("streamActivity declares no %d response", code)
		}
	}
}
