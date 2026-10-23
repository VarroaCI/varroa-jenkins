package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedLocalUser stores a User alice in the operator namespace at the given
// resourceVersion and returns the store it wrote to.
func seedLocalUser(t *testing.T, srv *Server, managedBy string, rv string) *crdstore.Fake {
	t.Helper()
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "alice",
			Namespace:       "varroa-system",
			ResourceVersion: rv,
			Labels:          map[string]string{v1alpha1.LabelManagedBy: managedBy},
		},
		Spec: v1alpha1.UserSpec{Email: "alice@example.com"},
	})
	return st
}

// TestHandleUpdateUser_ConcurrentWriteKeepsFormerIdentifiers pins that a
// write between the read and the update of PUT /users/{name} survives it.
func TestHandleUpdateUser_ConcurrentWriteKeepsFormerIdentifiers(t *testing.T) {
	srv, _ := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "local"}
	ctx := context.Background()
	st := seedLocalUser(t, srv, v1alpha1.ManagedByLocal, "1")
	gvr, _ := crdstore.GVRFor[v1alpha1.User]()
	fired := false
	st.SetAfterGet(gvr, func(ns, name string) {
		if fired {
			return
		}
		fired = true
		if _, err := st.PatchObjectMetaAt(ctx, gvr, ns, name, "1", map[string]any{
			"annotations": map[string]any{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
		}); err != nil {
			t.Errorf("concurrent patch: %v", err)
		}
	})

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/users/alice", strings.NewReader(`{"email":"new@example.com"}`)))
	srv.HandleUpdateUser(w, r, "alice")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	got, err := crdstore.Get[v1alpha1.User](ctx, st, "alice", "varroa-system")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] != `["bob"]` {
		t.Errorf("former identifiers annotation = %q, want the concurrent value", got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers])
	}
	if got.Spec.Email != "new@example.com" {
		t.Errorf("email = %q, want new@example.com", got.Spec.Email)
	}
}

// TestHandleUpdateUser_RefusesIdPUserFromFreshRead pins that the managed-by
// check runs on the fresh read and refuses an IdP-managed User with 400.
func TestHandleUpdateUser_RefusesIdPUserFromFreshRead(t *testing.T) {
	srv, _ := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "local"}
	seedLocalUser(t, srv, v1alpha1.ManagedByIDP, "1")

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/users/alice", strings.NewReader(`{"email":"new@example.com"}`)))
	srv.HandleUpdateUser(w, r, "alice")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "only a local account can be edited") {
		t.Errorf("body = %s, want the local-account message", w.Body.String())
	}
}

// TestHandleCreateUser_ExistingNameAnswers409 pins that POST /users refuses a
// name that already exists. The stored User keeps its email, labels,
// annotations, and empty password, and no activity event is published.
func TestHandleCreateUser_ExistingNameAnswers409(t *testing.T) {
	srv, sink := publishTestServer(t)
	srv.deps.IdentityConfig = IdentityConfig{Mode: "local"}
	st := srv.deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(st, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice",
			Namespace: "varroa-system",
			Labels: map[string]string{
				v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal,
				"tenant":                "ops",
			},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
		},
		Spec: v1alpha1.UserSpec{Email: "alice@example.com"},
	})

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`{"username":"alice","password":"x"}`)))
	srv.handleCreateUser(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}

	got, err := crdstore.Get[v1alpha1.User](context.Background(), st, "alice", "varroa-system")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", got.Spec.Email)
	}
	if got.Labels["tenant"] != "ops" || got.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByLocal {
		t.Errorf("labels = %v, want the existing labels", got.Labels)
	}
	if got.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] != `["bob"]` {
		t.Errorf("annotations = %v, want the former identifiers kept", got.Annotations)
	}
	if got.Spec.Password != "" {
		t.Errorf("spec.password was set on the existing user; want it untouched")
	}
	if len(sink.events) != 0 {
		t.Errorf("published %d activity events on 409, want 0", len(sink.events))
	}
}
