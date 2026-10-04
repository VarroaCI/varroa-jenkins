package recordstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// seedArtifactRow inserts one varroa.run_artifacts row at path.
func seedArtifactRow(t *testing.T, db *sql.DB, runID, path, blobKey string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_artifacts (run_id, path, created_at, size_bytes, digest, content_type, blob_key)
		VALUES ($1, $2, now(), 10, 'sha256:abc', 'application/json', $3)
	`, uuid.MustParse(runID), path, blobKey); err != nil {
		t.Fatalf("seed run_artifacts row %q: %v", path, err)
	}
}

func TestIsInternalArtifactPath(t *testing.T) {
	cases := map[string]bool{
		TestReportPath:        true,
		".varroa/other.json":  true,
		"dist/app.jar":        false,
		".varroaish/file.txt": false,
		"sub/.varroa/x.json":  false,
		"":                    false,
	}
	for path, want := range cases {
		if got := IsInternalArtifactPath(path); got != want {
			t.Errorf("IsInternalArtifactPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestListArtifactsOmitsInternalKeys asserts the artifact list holds the
// user's files alone, while GetArtifact still reads the internal report.
func TestListArtifactsOmitsInternalKeys(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "alpha", "alpha/release")
	seedArtifactRow(t, db, runID, "dist/app.jar", "artifacts/alpha/dist/app.jar")
	seedArtifactRow(t, db, runID, TestReportPath, "artifacts/alpha/.varroa/tests.json")

	ctx := context.Background()
	artifacts, err := deps.Reader.ListArtifacts(ctx, runID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != "dist/app.jar" {
		t.Fatalf("ListArtifacts = %+v, want dist/app.jar alone", artifacts)
	}

	report, found, err := deps.Reader.GetArtifact(ctx, runID, TestReportPath)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if !found || report.BlobKey != "artifacts/alpha/.varroa/tests.json" {
		t.Fatalf("GetArtifact = %+v found=%v, want the report row", report, found)
	}

	if _, found, err := deps.Reader.GetArtifact(ctx, runID, "missing.txt"); err != nil || found {
		t.Fatalf("GetArtifact(missing) found=%v err=%v, want not found", found, err)
	}
}

// TestCrossTenantArtifactsOmitInternalKeys asserts a cross-tenant caller
// can neither list nor sign an internal key.
func TestCrossTenantArtifactsOmitInternalKeys(t *testing.T) {
	t.Parallel()

	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "alpha", "alpha/release")
	seedArtifactRow(t, db, runID, TestReportPath, "artifacts/alpha/.varroa/tests.json")
	signer := &fakeCrossTenantBlobSigner{url: "https://example.invalid/signed"}
	deps.Blob = signer

	req := CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
	}
	list, err := handleCrossTenantArtifactList(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantArtifactList: %v", err)
	}
	if len(list.Artifacts) != 0 {
		t.Fatalf("artifacts = %+v, want none", list.Artifacts)
	}

	req.Path = TestReportPath
	url, err := handleCrossTenantArtifactURL(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantArtifactURL: %v", err)
	}
	if url.Code != CodeNotFound || signer.gotKey != "" {
		t.Fatalf("url response = %+v, signed %q; want NOT_FOUND and no signature", url, signer.gotKey)
	}
}
