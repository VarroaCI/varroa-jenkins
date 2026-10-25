package jenkins

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTriggerBuildBodyCarriesCause asserts that the trigger body names the run
// cause, so the plugin can set the description of the build cause.
func TestTriggerBuildBodyCarriesCause(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	if _, err := c.TriggerBuild(context.Background(), TriggerBuildRequest{
		RunID:     "0199d0b7-0000-7000-8000-000000000002",
		IntentKey: "manual:abc",
		JobPath:   "team/job",
		Actor:     "github:alice",
		Cause:     "Started by a pull request comment of alice",
	}); err != nil {
		t.Logf("trigger returned %v; the body is checked below", err)
	}
	if got := body["cause"]; got != "Started by a pull request comment of alice" {
		t.Fatalf("cause in body = %v", got)
	}
}
