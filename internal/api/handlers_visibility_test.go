package api

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

func ctrl(ns, name string) *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

// TestVisibleControllers covers the BOLA fix (#237): the controller collection endpoints
// must return only controllers the caller may read, must enforce ?namespace= against the
// caller's grants (narrow, never widen), and must fail closed when no authorizer is wired.
func TestVisibleControllers(t *testing.T) {
	client := &fakeResourceClient{controllers: map[string]*v1alpha1.Controller{
		"ns-a/ctrl-a": ctrl("ns-a", "ctrl-a"),
		"ns-b/ctrl-b": ctrl("ns-b", "ctrl-b"),
	}}
	scopedClaims := &auth.Claims{Subject: "test-user"}

	t.Run("ns-scoped caller sees only its own controller", func(t *testing.T) {
		srv := NewServer(&Dependencies{Client: client, Store: storeFromFake(client), Authorizer: testAuthorizer(false, "ns-a"), Logger: slog.Default()})
		got, err := srv.visibleControllers(context.Background(), scopedClaims, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Namespace != "ns-a" {
			t.Fatalf("expected only ns-a/ctrl-a, got %+v", got)
		}
	})

	t.Run("namespace filter cannot widen beyond grants", func(t *testing.T) {
		srv := NewServer(&Dependencies{Client: client, Store: storeFromFake(client), Authorizer: testAuthorizer(false, "ns-a"), Logger: slog.Default()})
		got, err := srv.visibleControllers(context.Background(), scopedClaims, "ns-b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty result for unauthorized namespace filter, got %+v", got)
		}
	})

	t.Run("cluster-wide caller sees all", func(t *testing.T) {
		srv := NewServer(&Dependencies{Client: client, Store: storeFromFake(client), Authorizer: testAuthorizer(true, ""), Logger: slog.Default()})
		got, err := srv.visibleControllers(context.Background(), scopedClaims, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 controllers, got %d", len(got))
		}
	})

	t.Run("nil authorizer denies by default", func(t *testing.T) {
		srv := NewServer(&Dependencies{Client: client, Store: storeFromFake(client), Authorizer: nil, Logger: slog.Default()})
		got, err := srv.visibleControllers(context.Background(), scopedClaims, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected deny-by-default (empty), got %+v", got)
		}
	})
}
