package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
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

// newCatalogFixtureRepo creates a bare git repository whose main branch holds
// files, and returns its file:// URL.
func newCatalogFixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "bare.git")
	work := filepath.Join(root, "work")
	gitRun(t, "", "init", "--bare", "-b", "main", bare)
	gitRun(t, "", "clone", bare, work)
	for name, content := range files {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "catalog")
	gitRun(t, work, "push", "origin", "main")
	return "file://" + bare
}

// gitRun runs git in dir (or the current directory when dir is empty) with a
// fixed author identity.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// useLocalCatalogCloner lets export catalog clone file:// fixtures for the
// duration of the test.
func useLocalCatalogCloner(t *testing.T) {
	t.Helper()
	old := newCatalogCloner
	newCatalogCloner = func() *bundle.GitCloner {
		c := bundle.NewGitCloner()
		c.AllowLocalTransportForTest()
		return c
	}
	t.Cleanup(func() { newCatalogCloner = old })
}

// catalogSourceServer serves one CatalogSource from the BFF path.
func catalogSourceServer(t *testing.T, repoURL, path string) *httptest.Server {
	t.Helper()
	srcJSON, _ := json.Marshal(v1alpha1.CatalogSource{
		Spec: v1alpha1.CatalogSourceSpec{RepoURL: repoURL, Path: path},
	})
	return testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/catalogsources/ns1/src1") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(srcJSON)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

// runExportCatalogTo runs export catalog from the BFF stub to dest.
func runExportCatalogTo(t *testing.T, srv *httptest.Server, dest string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs([]string{"export", "catalog",
		"--namespace", "ns1", "--name", "src1",
		"--to", dest,
		"--server", srv.URL,
		"--context", "test",
	})
	return root.Execute()
}

// catalogIndexKeys returns the sorted type and path of each item in root.
func catalogIndexKeys(t *testing.T, root string) []string {
	t.Helper()
	idx, err := bundle.ParseCatalogIndex(root)
	if err != nil {
		t.Fatalf("ParseCatalogIndex(%s): %v", root, err)
	}
	keys := make([]string, 0, len(idx.Items))
	for _, item := range idx.Items {
		keys = append(keys, item.Type+" "+item.Path)
	}
	sort.Strings(keys)
	return keys
}

var catalogConventionFiles = map[string]string{
	"items/a.yaml":         "a",
	"items/b.yaml":         "b",
	"pod-templates/p.yaml": "p",
}

func TestExportCatalog_DirectoryConventionRoundTrip(t *testing.T) {
	testSetup(t)
	useLocalCatalogCloner(t)

	repo := newCatalogFixtureRepo(t, catalogConventionFiles)
	srv := catalogSourceServer(t, repo, "")
	defer srv.Close()

	outDir := filepath.Join(t.TempDir(), "out")
	if err := runExportCatalogTo(t, srv, "dir://"+outDir); err != nil {
		t.Fatalf("export catalog: %v", err)
	}

	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatal(err)
	}
	ext := t.TempDir()
	if _, err := bundle.FetchCatalogLayer(context.Background(), store, outDir, ext); err != nil {
		t.Fatalf("FetchCatalogLayer: %v", err)
	}

	if _, err := os.Stat(filepath.Join(ext, "bundle.yaml")); !os.IsNotExist(err) {
		t.Errorf("exported catalog must not need bundle.yaml, stat err = %v", err)
	}

	want := t.TempDir()
	for name, content := range catalogConventionFiles {
		p := filepath.Join(want, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := catalogIndexKeys(t, ext)
	if strings.Join(got, "|") != strings.Join(catalogIndexKeys(t, want), "|") {
		t.Errorf("index keys = %v, want %v", got, catalogIndexKeys(t, want))
	}
}

func TestExportCatalog_SubPathRoundTrip(t *testing.T) {
	testSetup(t)
	useLocalCatalogCloner(t)

	files := map[string]string{}
	for name, content := range catalogConventionFiles {
		files["catalog/"+name] = content
	}
	repo := newCatalogFixtureRepo(t, files)
	srv := catalogSourceServer(t, repo, "catalog")
	defer srv.Close()

	outDir := filepath.Join(t.TempDir(), "out")
	if err := runExportCatalogTo(t, srv, "dir://"+outDir); err != nil {
		t.Fatalf("export catalog: %v", err)
	}

	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatal(err)
	}
	ext := t.TempDir()
	if _, err := bundle.FetchCatalogLayer(context.Background(), store, outDir, ext); err != nil {
		t.Fatalf("FetchCatalogLayer: %v", err)
	}

	// The layer root is the repository root, so spec.path still applies.
	if _, err := os.Stat(filepath.Join(ext, "catalog", "items", "a.yaml")); err != nil {
		t.Fatalf("layer must hold the repository root: %v", err)
	}
	root, err := bundle.CatalogRoot(ext, "catalog")
	if err != nil {
		t.Fatal(err)
	}
	if got := catalogIndexKeys(t, root); len(got) != 3 {
		t.Errorf("index items = %v, want 3", got)
	}
}

func TestExportCatalog_TarDestImportable(t *testing.T) {
	testSetup(t)
	useLocalCatalogCloner(t)

	repo := newCatalogFixtureRepo(t, catalogConventionFiles)
	srv := catalogSourceServer(t, repo, "")
	defer srv.Close()

	tarPath := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := runExportCatalogTo(t, srv, "tar://"+tarPath); err != nil {
		t.Fatalf("export catalog: %v", err)
	}

	// varroactl import opens the tar source with the path as the ref.
	store, finalize, err := openOCISrc("tar", tarPath, "", false)
	if err != nil {
		t.Fatalf("open tar source: %v", err)
	}
	defer func() { _ = finalize() }()

	ext := t.TempDir()
	if _, err := bundle.FetchCatalogLayer(context.Background(), store, tarPath, ext); err != nil {
		t.Fatalf("FetchCatalogLayer from tar: %v", err)
	}
	if got := catalogIndexKeys(t, ext); len(got) != 3 {
		t.Errorf("index items = %v, want 3", got)
	}
}

func TestExportCatalog_SymlinksSkipped(t *testing.T) {
	testSetup(t)
	useLocalCatalogCloner(t)

	// Build a repository that commits a dangling link and a directory link.
	root := t.TempDir()
	bare := filepath.Join(root, "bare.git")
	work := filepath.Join(root, "work")
	gitRun(t, "", "init", "--bare", "-b", "main", bare)
	gitRun(t, "", "clone", bare, work)
	for name, content := range catalogConventionFiles {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("does-not-exist", filepath.Join(work, "items", "dangling.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("items", filepath.Join(work, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "catalog with links")
	gitRun(t, work, "push", "origin", "main")

	srv := catalogSourceServer(t, "file://"+bare, "")
	defer srv.Close()

	outDir := filepath.Join(t.TempDir(), "out")
	if err := runExportCatalogTo(t, srv, "dir://"+outDir); err != nil {
		t.Fatalf("export catalog with symlinks: %v", err)
	}

	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatal(err)
	}
	ext := t.TempDir()
	if _, err := bundle.FetchCatalogLayer(context.Background(), store, outDir, ext); err != nil {
		t.Fatalf("FetchCatalogLayer: %v", err)
	}
	if got := catalogIndexKeys(t, ext); len(got) != 3 {
		t.Errorf("index items = %v, want 3", got)
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
	if err := pushBundleAsOCIArtifact(context.TODO(), store, ref, bundleDir, "application/vnd.varroa.bundle.v1.tar+gzip", "application/vnd.varroa.bundle.v1", nil); err != nil {
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
