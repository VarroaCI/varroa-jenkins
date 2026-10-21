package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPollJobBranchesMapsSourceURL asserts that the plugin's sourceUrl of each
// project reaches the proto field SourceUrl, and that a project without one keeps it empty.
func TestPollJobBranchesMapsSourceURL(t *testing.T) {
	const body = `{"projects":[` +
		`{"jobPath":"payments/mb","indexedAt":"","indexResult":"FAILURE","heads":[],"headsTruncated":false,` +
		`"lastScan":null,"sourceUrl":"https://git.example.org/o/r.git"},` +
		`{"jobPath":"payments/org","indexedAt":"","indexResult":"","heads":[],"headsTruncated":false,` +
		`"lastScan":null,"sourceUrl":""}],"total":2,"truncated":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	a := newJobScheduleTestAgent(t, srv.URL)
	set, err := a.pollJobBranches(context.Background())
	if err != nil {
		t.Fatalf("poll job branches: %v", err)
	}
	if len(set.Items) != 2 {
		t.Fatalf("projects = %d, want 2", len(set.Items))
	}
	if got := set.Items[0].SourceUrl; got != "https://git.example.org/o/r.git" {
		t.Fatalf("SourceUrl = %q, want the plugin source URL", got)
	}
	if got := set.Items[1].SourceUrl; got != "" {
		t.Fatalf("SourceUrl = %q, want empty for a project without a source", got)
	}
}
