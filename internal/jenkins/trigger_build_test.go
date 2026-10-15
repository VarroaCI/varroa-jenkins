package jenkins

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTriggerBuildReportsNotFoundStatus asserts that a refused trigger carries
// the Jenkins HTTP status beside the error. The gateway maps that status onto a
// reason code.
func TestTriggerBuildReportsNotFoundStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such job"))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	status, err := c.TriggerBuild(context.Background(), TriggerBuildRequest{
		RunID:     "0199d0b7-0000-7000-8000-000000000001",
		IntentKey: "manual:abc",
		JobPath:   "team/job",
	})
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestTriggerBuildReportsNoStatusWhenUnreachable asserts that a call that
// reaches no Jenkins reports the status 0.
func TestTriggerBuildReportsNoStatusWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(url, "user", "token")
	c.retryBaseWait = time.Millisecond
	c.retryMaxWait = 2 * time.Millisecond
	status, err := c.TriggerBuild(context.Background(), TriggerBuildRequest{
		RunID:     "0199d0b7-0000-7000-8000-000000000002",
		IntentKey: "manual:def",
		JobPath:   "team/job",
	})
	if err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
}
