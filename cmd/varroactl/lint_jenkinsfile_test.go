package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestJenkinsfile writes a Jenkinsfile to a fresh file under t.TempDir.
func writeTestJenkinsfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Jenkinsfile")
	if err := os.WriteFile(path, []byte("pipeline {\n  agent any\n}\n"), 0o644); err != nil {
		t.Fatalf("write Jenkinsfile: %v", err)
	}
	return path
}

// TestLintJenkinsfile_ZeroExitOnValid asserts a valid file exits zero and
// posts to the tenant's lint path with the file text.
func TestLintJenkinsfile_ZeroExitOnValid(t *testing.T) {
	testSetup(t)
	path := writeTestJenkinsfile(t)

	var gotPath, gotText string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText = body["jenkinsfile"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid": true, "cell": "cell-a", "namespace": "cells", "errors": []any{},
		})
	})
	defer srv.Close()

	code := run([]string{"lint", "jenkinsfile", "--tenant", "payments", "--file", path})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.HasSuffix(gotPath, "/api/v1/pipelines/payments/lint") {
		t.Errorf("expected the lint path, got %s", gotPath)
	}
	if !strings.Contains(gotText, "agent any") {
		t.Errorf("posted text = %q, want the file text", gotText)
	}
}

// TestLintJenkinsfile_NonZeroExitOnErrors asserts a file with lint errors
// exits non-zero after the findings print.
func TestLintJenkinsfile_NonZeroExitOnErrors(t *testing.T) {
	testSetup(t)
	path := writeTestJenkinsfile(t)

	srv := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid": false, "cell": "cell-a", "namespace": "cells",
			"errors": []any{map[string]any{"source": "jenkins", "line": 3, "column": 5, "message": "Unknown stage"}},
		})
	})
	defer srv.Close()

	if code := run([]string{"lint", "jenkinsfile", "--tenant", "payments", "--file", path}); code == 0 {
		t.Fatal("expected a non-zero exit for a file with errors")
	}
}

// TestLintJenkinsfile_ConflictIsError asserts a refusal, such as a
// hibernated cell, exits non-zero.
func TestLintJenkinsfile_ConflictIsError(t *testing.T) {
	testSetup(t)
	path := writeTestJenkinsfile(t)

	srv := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"cell cell-a is hibernated"}`))
	})
	defer srv.Close()

	if code := run([]string{"lint", "jenkinsfile", "--tenant", "payments", "--file", path}); code == 0 {
		t.Fatal("expected a non-zero exit for a 409 refusal")
	}
}

// TestLintJenkinsfile_RequiresTenantFlag asserts --tenant is required.
func TestLintJenkinsfile_RequiresTenantFlag(t *testing.T) {
	testSetup(t)
	path := writeTestJenkinsfile(t)
	if code := run([]string{"lint", "jenkinsfile", "--file", path}); code == 0 {
		t.Fatal("expected a non-zero exit without --tenant")
	}
}

// TestLintJenkinsfile_MissingFileIsError asserts an unreadable file exits
// non-zero before any request is sent.
func TestLintJenkinsfile_MissingFileIsError(t *testing.T) {
	testSetup(t)
	missing := filepath.Join(t.TempDir(), "absent")
	if code := run([]string{"lint", "jenkinsfile", "--tenant", "payments", "--file", missing}); code == 0 {
		t.Fatal("expected a non-zero exit for a missing file")
	}
}
