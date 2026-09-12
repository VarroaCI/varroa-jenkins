package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMarkerYAML = `
apiVersion: varroa.dev/v1
kind: Pipeline
template: go-service
`

// writeTestMarker writes testMarkerYAML to a fresh file under t.TempDir
// and returns its path.
func writeTestMarker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yaml")
	if err := os.WriteFile(path, []byte(testMarkerYAML), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

// TestLintMarker_ZeroExitOnCleanMarker asserts varroa-cli's "A valid
// marker exits zero" scenario.
func TestLintMarker_ZeroExitOnCleanMarker(t *testing.T) {
	testSetup(t)
	markerPath := writeTestMarker(t)

	var gotPath string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body["marker"], "go-service") {
			t.Errorf("expected posted marker text to include go-service, got %q", body["marker"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"findings":        []any{},
			"templateName":    "go-service",
			"templateVersion": "2.1.4",
		})
	})
	defer srv.Close()

	code := run([]string{"lint", "marker", "--team", "payments", "--marker", markerPath})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.HasSuffix(gotPath, "/api/v1/pipelines/payments/markers/validate") {
		t.Errorf("expected the markers/validate path, got %s", gotPath)
	}
}

// TestLintMarker_NonZeroExitOnFindings asserts varroa-cli's "An invalid
// value exits non-zero" scenario.
func TestLintMarker_NonZeroExitOnFindings(t *testing.T) {
	testSetup(t)
	markerPath := writeTestMarker(t)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"findings": []map[string]string{
				{"severity": "error", "path": "values.goVersion", "message": "goVersion is required"},
			},
		})
	})
	defer srv.Close()

	code := run([]string{"lint", "marker", "--team", "payments", "--marker", markerPath})
	if code == 0 {
		t.Fatal("expected non-zero exit for a finding")
	}
}

// TestLintMarker_MissingFileIsError asserts varroa-cli's "A missing
// marker is an error" scenario: the command fails naming the path, and
// never reaches the network (no server is started).
func TestLintMarker_MissingFileIsError(t *testing.T) {
	testSetup(t)
	missingPath := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	code := run([]string{"lint", "marker", "--team", "payments", "--marker", missingPath})
	if code == 0 {
		t.Fatal("expected non-zero exit for a missing marker file")
	}
}

// TestLintMarker_NoBundleReportsOneFinding asserts varroa-cli's "A team
// with no bundle produces a finding" scenario: the BFF's one finding
// reaches the command as a non-zero exit, the same as any other finding.
func TestLintMarker_NoBundleReportsOneFinding(t *testing.T) {
	testSetup(t)
	markerPath := writeTestMarker(t)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"findings": []map[string]string{
				{"severity": "error", "message": `team "payments" delivers no pipeline template`},
			},
		})
	})
	defer srv.Close()

	code := run([]string{"lint", "marker", "--team", "payments", "--marker", markerPath})
	if code == 0 {
		t.Fatal("expected non-zero exit when the team delivers no pipeline template")
	}
}

// TestLintMarker_RequiresTeamFlag asserts --team is required.
func TestLintMarker_RequiresTeamFlag(t *testing.T) {
	testSetup(t)
	markerPath := writeTestMarker(t)

	code := run([]string{"lint", "marker", "--marker", markerPath})
	if code == 0 {
		t.Fatal("expected non-zero exit with no --team")
	}
}

// TestLintMarker_DefaultsMarkerPath asserts --marker defaults to
// .varroa/pipeline.yaml, per the varroa-cli delta.
func TestLintMarker_DefaultsMarkerPath(t *testing.T) {
	testSetup(t)
	dir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	if err := os.MkdirAll(filepath.Join(dir, ".varroa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".varroa", "pipeline.yaml"), []byte(testMarkerYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"findings": []any{}})
	})
	defer srv.Close()

	code := run([]string{"lint", "marker", "--team", "payments"})
	if code != 0 {
		t.Fatalf("expected exit 0 reading the default marker path, got %d", code)
	}
}
