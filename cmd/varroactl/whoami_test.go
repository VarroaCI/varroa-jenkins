package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWhoami tests the whoami command
func TestWhoami(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", tmpDir+"/config.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/me") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"email":    "user@example.com",
				"subject":  "user-1",
				"authMode": "local",
				"name":     "user",
				"groups":   []string{"team-a", "team-b"},
			})
			return
		}
		if strings.HasSuffix(path, "/me/permissions") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"manage": true,
				"view":   true,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VARROACTL_SERVER", srv.URL)
	t.Setenv("VARROACTL_API_KEY", "vk_test_key")

	root := newRootCmd()
	root.SetArgs([]string{"whoami"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
