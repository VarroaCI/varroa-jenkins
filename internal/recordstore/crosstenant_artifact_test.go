package recordstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// fakeCrossTenantBlobSigner captures the key and ttl its one call
// receives, and answers a fixed URL.
type fakeCrossTenantBlobSigner struct {
	gotKey string
	gotTTL time.Duration
	url    string
	err    error
}

func (f *fakeCrossTenantBlobSigner) SignedURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	f.gotKey = key
	f.gotTTL = ttl
	return f.url, f.err
}

// crossTenantTestArtifactPath is the one path every artifact test seeds
// under.
const crossTenantTestArtifactPath = "build.log"

// seedCrossTenantArtifact inserts one varroa.run_artifacts row directly,
// at crossTenantTestArtifactPath.
func seedCrossTenantArtifact(t *testing.T, db *sql.DB, runID, blobKey string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.run_artifacts (run_id, path, created_at, size_bytes, digest, content_type, blob_key)
		VALUES ($1, $2, now(), 100, 'sha256:abc', 'text/plain', $3)
	`, uuid.MustParse(runID), crossTenantTestArtifactPath, blobKey); err != nil {
		t.Fatalf("seed run_artifacts row: %v", err)
	}
}

// TestCrossTenantArtifactURL_SignsStoredKeyForFixedLifetime covers task
// 3.7's first two acceptances: the signed key equals the stored key, and
// the lifetime is 300 seconds.
func TestCrossTenantArtifactURL_SignsStoredKeyForFixedLifetime(t *testing.T) {
	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "alpha", "alpha/release")
	seedCrossTenantArtifact(t, db, runID, "artifacts/alpha/release/build.log")

	signer := &fakeCrossTenantBlobSigner{url: "https://signed.example/build.log"}
	deps.Blob = signer

	resp, err := handleCrossTenantArtifactURL(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
		Path:         crossTenantTestArtifactPath,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantArtifactURL: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if signer.gotKey != "artifacts/alpha/release/build.log" {
		t.Fatalf("signed key: got %q, want the stored blob_key verbatim", signer.gotKey)
	}
	if signer.gotTTL != crossTenantArtifactURLTTL {
		t.Fatalf("ttl: got %s, want %s", signer.gotTTL, crossTenantArtifactURLTTL)
	}
	if signer.gotTTL != 300*time.Second {
		t.Fatalf("ttl: got %s, want 300s", signer.gotTTL)
	}
	if resp.URL != signer.url {
		t.Fatalf("url: got %q, want %q", resp.URL, signer.url)
	}
}

// TestCrossTenantArtifactURL_PgblobAnswersBlobsModeError covers task
// 3.7's third acceptance: pgblob answers an error naming
// recordStore.blobs.mode.
func TestCrossTenantArtifactURL_PgblobAnswersBlobsModeError(t *testing.T) {
	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "alpha", "alpha/release")
	seedCrossTenantArtifact(t, db, runID, "artifacts/alpha/release/build.log")

	deps.Blob = blob.NewPGBlobStore(deps.Pool)

	resp, err := handleCrossTenantArtifactURL(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
		Path:         crossTenantTestArtifactPath,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantArtifactURL: %v", err)
	}
	if resp.Code != CodeInternal {
		t.Fatalf("code: got %q, want %q", resp.Code, CodeInternal)
	}
	if !strings.Contains(resp.Error, "recordStore.blobs.mode") {
		t.Fatalf("error: got %q, want it to name recordStore.blobs.mode", resp.Error)
	}
}

// TestCrossTenantArtifactURL_UngrantedCallerAnswersCrossTenant covers
// task 3.7's fourth acceptance: an ungranted caller gets CROSS_TENANT.
func TestCrossTenantArtifactURL_UngrantedCallerAnswersCrossTenant(t *testing.T) {
	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "beta", "beta/release")
	seedCrossTenantArtifact(t, db, runID, "artifacts/beta/release/build.log")

	deps.Blob = &fakeCrossTenantBlobSigner{url: "https://signed.example/build.log"}

	resp, err := handleCrossTenantArtifactURL(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
		Path:         crossTenantTestArtifactPath,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantArtifactURL: %v", err)
	}
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonCrossTenant, resp.Message)
	}
}

// TestCrossTenantArtifactList_ExcludesBlobKey asserts artifact_list
// answers path/size/digest/content_type but never the stored blob_key.
func TestCrossTenantArtifactList_ExcludesBlobKey(t *testing.T) {
	deps, db := statusTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	runID := seedCrossTenantRun(t, deps.Pool, "alpha", "alpha/release")
	seedCrossTenantArtifact(t, db, runID, "artifacts/alpha/release/build.log")

	resp, err := handleCrossTenantArtifactList(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantArtifactList: %v", err)
	}
	if len(resp.Artifacts) != 1 {
		t.Fatalf("artifacts: got %d, want 1", len(resp.Artifacts))
	}
	if resp.Artifacts[0].Path != crossTenantTestArtifactPath {
		t.Fatalf("path: got %q, want %q", resp.Artifacts[0].Path, crossTenantTestArtifactPath)
	}
	if resp.Artifacts[0].SizeBytes != 100 {
		t.Fatalf("size_bytes: got %d, want 100", resp.Artifacts[0].SizeBytes)
	}
}
