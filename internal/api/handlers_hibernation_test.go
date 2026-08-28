package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestHibernationRedirectUsesSharedInterstitial(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": {
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins", Namespace: "team-a"},
			Status: v1alpha1.ControllerStatus{
				WakeToken: "wake-token",
				Endpoint:  "https://jenkins.example.com/",
				Phase:     v1alpha1.ControllerPhaseHibernated,
			},
		},
	}
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/redirect/jenkins/job/example/", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`var statusPath = "/hibernation/wake-token/clusters/core/ns/team-a/status/jenkins";`,
		`var targetURL = "https://jenkins.example.com/job/example/";`,
		"var redirectOnNonWake = false;",
		"d.varroaWake === true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q", want)
		}
	}
}

func TestHibernationStatusIncludesWakeMarker(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": {
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins", Namespace: "team-a"},
			Status: v1alpha1.ControllerStatus{
				WakeToken: "wake-token",
				Phase:     v1alpha1.ControllerPhaseConnected,
			},
		},
	}
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/status/jenkins", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"varroaWake":true,"phase":"Connected"}` {
		t.Fatalf("status response = %d %q", rec.Code, rec.Body.String())
	}
}
