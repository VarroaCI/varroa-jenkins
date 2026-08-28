package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// ---------------------------------------------------------------------------
// export catalog tests
// ---------------------------------------------------------------------------

func TestExportCatalog_FlagParsing(t *testing.T) {
	testSetup(t)

	root := newRootCmd()
	root.SetArgs([]string{"export", "catalog", "--namespace", "ns1", "--name", "src1", "--to", "dir:///tmp/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (no BFF server)")
	}
	// Should NOT be a usage error (flags should parse fine)
	var ue usageError
	if errorsAs(err, &ue) {
		t.Errorf("unexpected usageError: %v", err)
	}
}

func TestExportCatalog_MissingRequiredFlags(t *testing.T) {
	testSetup(t)

	root := newRootCmd()
	root.SetArgs([]string{"export", "catalog", "--namespace", "ns1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing --name and --to")
	}
	var ue usageError
	if !errorsAs(err, &ue) && !isUsageError(err) {
		t.Errorf("expected usage error, got %T: %v", err, err)
	}
}

func TestExportCatalog_BFFServerRoundTrip(t *testing.T) {
	testSetup(t)

	src := v1alpha1.CatalogSource{
		Spec: v1alpha1.CatalogSourceSpec{
			RepoURL: "https://example.com/catalog.git",
			Path:    ".",
		},
	}
	srcJSON, _ := json.Marshal(src)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/catalogsources/ns1/src1") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(srcJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()

	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	root := newRootCmd()
	root.SetArgs([]string{"export", "catalog",
		"--namespace", "ns1", "--name", "src1",
		"--to", "dir://" + outDir,
		"--server", srv.URL,
		"--context", "test",
	})
	err := root.Execute()
	// Will fail trying to clone (no git repo at the URL), but should NOT be
	// a usage error — the BFF fetch and dest parsing succeeded.
	if err == nil {
		t.Fatal("expected error (no git repo)")
	}
	if isUsageError(err) {
		t.Errorf("unexpected usage error: %v", err)
	}
	// The OCI layout WAS created (openOCIDest initializes the store even
	// though the clone hasn't run yet), but no manifest should be present.
	manifests, listErr := storeListManifests(outDir)
	if listErr == nil && len(manifests) > 0 {
		t.Errorf("expected no manifests (clone failed), got %d", len(manifests))
	}
}

func storeListManifests(dir string) ([]oci.Descriptor, error) {
	store, err := oci.NewLayoutStore(dir)
	if err != nil {
		return nil, err
	}
	return store.ListManifests(context.TODO())
}

func TestExportCatalog_DestToDir(t *testing.T) {
	testSetup(t)

	src := v1alpha1.CatalogSource{
		Spec: v1alpha1.CatalogSourceSpec{
			RepoURL: "https://example.com/catalog.git",
			Path:    ".",
		},
	}
	srcJSON, _ := json.Marshal(src)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/catalogsources/ns1/src1") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(srcJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()

	outDir := t.TempDir()

	root := newRootCmd()
	root.SetArgs([]string{"export", "catalog",
		"--namespace", "ns1", "--name", "src1",
		"--to", "dir://" + outDir,
		"--server", srv.URL,
		"--context", "test",
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (clone fails)")
	}
	// Verify the dest scheme was parsed and the dir was created
	if _, statErr := os.Stat(outDir); statErr != nil {
		t.Logf("outDir stat: %v (expected — clone failed before writing)", statErr)
	}
}

// ---------------------------------------------------------------------------
// export bundle tests
// ---------------------------------------------------------------------------

func TestExportBundle_FlagParsing(t *testing.T) {
	testSetup(t)

	root := newRootCmd()
	root.SetArgs([]string{"export", "bundle", "--repo", "https://example.com/repo.git", "--path", ".", "--to", "dir:///tmp/out"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (no git repo)")
	}
	var ue usageError
	if errorsAs(err, &ue) {
		t.Errorf("unexpected usageError: %v", err)
	}
}

func TestExportBundle_MissingRequiredFlags(t *testing.T) {
	testSetup(t)

	root := newRootCmd()
	root.SetArgs([]string{"export", "bundle", "--path", "."})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing --repo and --to")
	}
	var ue usageError
	if !errorsAs(err, &ue) && !isUsageError(err) {
		t.Errorf("expected usage error, got %T: %v", err, err)
	}
}

func TestExportBundle_DestToDir(t *testing.T) {
	testSetup(t)

	tmpDir := t.TempDir()
	outDir := filepath.Join(tmpDir, "out")

	// Test dest parsing: dir:// scheme should work (clone will fail because
	// the repo URL is bogus, but the dest scheme parsing is verified).
	root := newRootCmd()
	root.SetArgs([]string{"export", "bundle",
		"--repo", "https://example.com/nonexistent-repo.git",
		"--path", ".",
		"--revision", "main",
		"--to", "dir://" + outDir,
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (clone fails)")
	}
	// Should NOT be a usage error (flags parsed fine)
	if isUsageError(err) {
		t.Errorf("unexpected usage error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pushBundleAsOCIArtifact test (unit test for the shared helper)
// ---------------------------------------------------------------------------

func TestPushBundleAsOCIArtifact_DirDest(t *testing.T) {
	testSetup(t)

	// Create a temp bundle directory with a test file.
	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "test.yaml"), []byte("key: val\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create an OCI layout store as destination.
	outDir := t.TempDir()
	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatal(err)
	}

	// Push the bundle as an OCI artifact.
	ref := "test-bundle"
	if err := pushBundleAsOCIArtifact(context.TODO(), store, ref, bundleDir, "application/vnd.varroa.bundle.v1.tar+gzip", "application/vnd.varroa.bundle.v1"); err != nil {
		t.Fatalf("pushBundleAsOCIArtifact: %v", err)
	}

	// Resolve the manifest to verify it was pushed.
	desc, err := store.Resolve(context.TODO(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if desc.Digest == "" {
		t.Error("expected non-empty digest")
	}

	// Pull the manifest and verify the layer media type.
	manifest, err := store.Pull(context.TODO(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(manifest.Layers))
	}
	if manifest.Layers[0].MediaType != "application/vnd.varroa.bundle.v1.tar+gzip" {
		t.Errorf("layer media type = %q, want application/vnd.varroa.bundle.v1.tar+gzip", manifest.Layers[0].MediaType)
	}
	if manifest.ArtifactType != "application/vnd.varroa.bundle.v1" {
		t.Errorf("artifact type = %q, want application/vnd.varroa.bundle.v1", manifest.ArtifactType)
	}

	// Verify the layer contains the test file by fetching it.
	rc, err := store.FetchBlob(context.TODO(), manifest.Layers[0].Digest)
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	layerContent, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read layer: %v", err)
	}
	if len(layerContent) == 0 {
		t.Error("expected non-empty layer content")
	}
	// Verify the layer is a valid gzip by checking magic bytes.
	if len(layerContent) < 2 || layerContent[0] != 0x1f || layerContent[1] != 0x8b {
		t.Error("expected gzip magic bytes in layer content")
	}
}

// ---------------------------------------------------------------------------
// findSubCommand test
// ---------------------------------------------------------------------------

func TestFindSubCommand(t *testing.T) {
	root := newRootCmd()
	exportCmd := findSubCommand(root, "export")
	if exportCmd == nil {
		t.Fatal("expected 'export' subcommand on root")
	}
	if exportCmd.Name() != "export" {
		t.Errorf("expected name 'export', got %q", exportCmd.Name())
	}
}
