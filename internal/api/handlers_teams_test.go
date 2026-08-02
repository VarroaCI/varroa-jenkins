package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func TestTeamHandlers_NonAdmin(t *testing.T) {
	// nil Authorizer → 403 on all endpoints.
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client: client,
		Store:  storeFromFake(client),
		Logger: slog.Default(),
	})

	tests := []struct {
		name    string
		method  string
		path    string
		body    io.Reader
		handler http.HandlerFunc
	}{
		{"list teams", http.MethodGet, "/teams", nil, srv.HandleTeams},
		{"create team", http.MethodPost, "/teams", bytes.NewReader([]byte(`{"name":"t","members":["u"],"namespaces":["ns"]}`)), srv.HandleTeams},
		{"get team", http.MethodGet, "/teams/my-team", nil, srv.HandleTeamDispatch},
		{"delete team", http.MethodDelete, "/teams/my-team", nil, srv.HandleTeamDispatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, tt.body)
			req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{}))
			w := httptest.NewRecorder()
			tt.handler(w, req)
			resp := w.Result()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("expected 403, got %d", resp.StatusCode)
			}
		})
	}
}

func TestTeamHandlers_AdminCreate(t *testing.T) {
	// With an Authorizer that uses an admin-capable resolver, admin can create.
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		},
	}
	adminBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-admin"}},
			RoleRef:  "admin",
		},
	}

	resolver := testResolver([]*v1alpha1.VarroaRole{adminRole}, []*v1alpha1.VarroaRoleBinding{adminBinding})
	authz := NewAuthorizer(resolver, false)

	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      storeFromFake(client),
		Logger:     slog.Default(),
		Authorizer: authz,
	})

	// Test create with valid request
	body := `{"name":"new-team","members":["alice"],"namespaces":["ns1"]}`
	req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(body)))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{Subject: "test-admin"}))
	w := httptest.NewRecorder()
	srv.HandleTeams(w, req)
	resp := w.Result()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

func TestTeamHandlers_AdminCreate_Validation(t *testing.T) {
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		},
	}
	adminBinding := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-admin"}},
			RoleRef:  "admin",
		},
	}

	resolver := testResolver([]*v1alpha1.VarroaRole{adminRole}, []*v1alpha1.VarroaRoleBinding{adminBinding})
	authz := NewAuthorizer(resolver, false)

	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Client:     client,
		Store:      storeFromFake(client),
		Logger:     slog.Default(),
		Authorizer: authz,
	})

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "empty members and subjects",
			body:       `{"name":"t","namespaces":["ns"]}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "admin roleRef",
			body:       `{"name":"t","members":["u"],"namespaces":["ns"],"roleRef":"admin"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty namespaces",
			body:       `{"name":"t","members":["u"],"namespaces":[]}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "test-admin"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/teams", bytes.NewReader([]byte(tt.body)))
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			srv.HandleTeams(w, req)
			resp := w.Result()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}
