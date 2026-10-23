package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestWakeURL_TenantsRouteIsServed asserts the wake URL under /tenants/
// reaches the queue handler. The request names a known Tenant and its own
// token, so the route must not answer 404.
func TestWakeURL_TenantsRouteIsServed(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/github-webhook/", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("status = 404 for /tenants/ wake URL, want the queue handler to answer")
	}
}

// TestWakeURL_TeamsRouteAnswers404 asserts the retired /teams/ wake URL no
// longer routes. Every request on it answers 404.
func TestWakeURL_TeamsRouteAnswers404(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/teams/payments/queue/github-webhook/", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for the retired /teams/ wake URL", rec.Code)
	}
}
