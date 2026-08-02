package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// stubBroodSchedules implements BroodSchedules for tests.
type stubBroodSchedules struct {
	createResp *bus.BroodScheduleResponse
	createErr  error
	getResp    *bus.BroodScheduleResponse
	getErr     error
	listResp   []bus.BroodScheduleResponse
	listErr    error
	deleteErr  error
	suspendErr error
}

func (s *stubBroodSchedules) Create(ctx context.Context, cluster, ns, name string, spec v1alpha1.BroodScheduleSpec) (*bus.BroodScheduleResponse, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.createResp != nil {
		return s.createResp, nil
	}
	return &bus.BroodScheduleResponse{Namespace: ns, Name: name}, nil
}

func (s *stubBroodSchedules) Get(ctx context.Context, cluster, ns, name string) (*bus.BroodScheduleResponse, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getResp != nil {
		return s.getResp, nil
	}
	return &bus.BroodScheduleResponse{Namespace: ns, Name: name}, nil
}

func (s *stubBroodSchedules) List(ctx context.Context, ns string) ([]bus.BroodScheduleResponse, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listResp != nil {
		return s.listResp, nil
	}
	return []bus.BroodScheduleResponse{}, nil
}

func (s *stubBroodSchedules) Delete(ctx context.Context, cluster, ns, name string) error {
	return s.deleteErr
}

func (s *stubBroodSchedules) Suspend(ctx context.Context, cluster, ns, name string, suspend bool) error {
	return s.suspendErr
}

// newBroodScheduleTestServer creates a Server with minimal deps for brood schedule tests.
func newBroodScheduleTestServer(ss BroodSchedules) *Server {
	return NewServer(&Dependencies{
		Authorizer:        newBroodTestAuthorizer(),
		Logger:            slog.Default(),
		OperatorNamespace: "operator-ns",
		BroodSchedules:    ss,
	})
}

func TestBroodScheduleHandlers_MethodNotAllowed(t *testing.T) {
	srv := newBroodScheduleTestServer(&stubBroodSchedules{})
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/brood-schedules"},
		{http.MethodPatch, "/brood-schedules/ns/name"},
		{http.MethodPut, "/brood-schedules/ns/name/suspend"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{}))
			w := httptest.NewRecorder()
			srv.handleBroodSchedules(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405, got %d", w.Code)
			}
		})
	}
}

func TestBroodScheduleHandlers_Create(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	body := `{"namespace":"team-ns","name":"test-sched","spec":{"schedule":"*/5 * * * *","waitForCompletion":true,"template":{"targets":{"names":["ctrl-1"]},"action":{"verb":"reconcile"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_CreateUnauthenticated(t *testing.T) {
	srv := newBroodScheduleTestServer(&stubBroodSchedules{})
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestBroodScheduleHandlers_CreateForbidden(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	body := `{"namespace":"operator-ns","name":"test-sched","spec":{"schedule":"*/5 * * * *","waitForCompletion":true,"template":{"targets":{"names":["ctrl-1"]},"action":{"verb":"reconcile"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_CreateNameRequired(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	body := `{"spec":{"schedule":"*/5 * * * *","waitForCompletion":true,"template":{"targets":{"names":["ctrl-1"]},"action":{"verb":"reconcile"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_CreateTenancyViolationNamespaces(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	// Team namespace with namespaces set → tenancy violation.
	body := `{"namespace":"team-ns","name":"test-sched","spec":{"schedule":"*/5 * * * *","waitForCompletion":true,"template":{"targets":{"names":["ctrl-1"],"namespaces":["other-ns"]},"action":{"verb":"reconcile"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for tenancy violation, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_CreateTenancyViolationMultiCluster(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	// Team namespace with 2+ clusters → tenancy violation.
	body := `{"namespace":"team-ns","name":"test-sched","spec":{"schedule":"*/5 * * * *","waitForCompletion":true,"template":{"targets":{"names":["ctrl-1"]},"action":{"verb":"reconcile"},"clusters":["c1","c2"]}}}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for multi-cluster in team ns, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_List(t *testing.T) {
	ss := &stubBroodSchedules{
		listResp: []bus.BroodScheduleResponse{
			{Namespace: "ns1", Name: "sched-a"},
		},
	}
	srv := newBroodScheduleTestServer(ss)

	req := httptest.NewRequest(http.MethodGet, "/brood-schedules", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items, ok := resp["items"].([]interface{})
	if !ok || len(items) != 1 {
		t.Errorf("expected 1 item, got %+v", resp)
	}
}

func TestBroodScheduleHandlers_Get(t *testing.T) {
	ss := &stubBroodSchedules{
		getResp: &bus.BroodScheduleResponse{Namespace: "team-ns", Name: "my-sched"},
	}
	srv := newBroodScheduleTestServer(ss)

	req := httptest.NewRequest(http.MethodGet, "/brood-schedules/team-ns/my-sched", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_GetNotFound(t *testing.T) {
	ss := &stubBroodSchedules{
		getErr: &stubError{"not found"},
	}
	srv := newBroodScheduleTestServer(ss)

	req := httptest.NewRequest(http.MethodGet, "/brood-schedules/team-ns/missing", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_Delete(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	req := httptest.NewRequest(http.MethodDelete, "/brood-schedules/team-ns/my-sched", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_DeleteForbidden(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	req := httptest.NewRequest(http.MethodDelete, "/brood-schedules/operator-ns/my-sched", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "viewer", Groups: []string{"viewers"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_Suspend(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	body := `{"suspend":true}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules/team-ns/my-sched/suspend", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBroodScheduleHandlers_SuspendResume(t *testing.T) {
	ss := &stubBroodSchedules{}
	srv := newBroodScheduleTestServer(ss)

	body := `{"suspend":false}`
	req := httptest.NewRequest(http.MethodPost, "/brood-schedules/team-ns/my-sched/suspend", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "admin", Groups: []string{"admins"}}))
	w := httptest.NewRecorder()
	srv.handleBroodSchedules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// stubError is a simple error type for tests.
type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }
