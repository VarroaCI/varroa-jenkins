package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// importPackCmd runs the import command with the given args and returns any error.
func importPackCmd(t *testing.T, args ...string) error {
	t.Helper()
	testSetup(t)
	root := newRootCmd()
	root.SetArgs(append([]string{"import"}, args...))
	return root.Execute()
}

// importPackCapture runs the import command and captures stdout.
func importPackCapture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	testSetup(t)

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root := newRootCmd()
	root.SetArgs(append([]string{"import"}, args...))
	err := root.Execute()

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout

	return buf.String(), err
}

// ---------------------------------------------------------------------------
// 2.5 — import tests
// ---------------------------------------------------------------------------

// (a) dir:// → dir:// round trip
func TestImport_DirToDirRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Build a plugin pack in the source layout
	srcStore, err := oci.NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore src: %v", err)
	}

	ctx := t.Context()
	content := []byte("fake-hpi-content")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Sha256Digest: %v", err)
	}

	plugins := []oci.ResolvedPlugin{{
		Name:    "test-plugin",
		Version: "1.0.0",
		SHA256:  digest,
		Content: bytes.NewReader(content),
	}}
	lockHash := oci.LockHash(plugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "2.479.3",
		Profile:        "test",
		LockHash:       lockHash,
		PluginCount:    1,
		CreatedAt:      "2025-07-18T12:00:00Z",
	}
	if err := oci.BuildPluginPack(ctx, srcStore, srcDir, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	// Verify the source manifest exists
	srcDesc, err := srcStore.Resolve(ctx, srcDir)
	if err != nil {
		t.Fatalf("Resolve src: %v", err)
	}

	// Run import from dir://src to dir://dst
	err = importPackCmd(t,
		"--from", "dir://"+srcDir,
		"--to", "dir://"+dstDir,
	)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	// Verify the destination has the same manifest digest
	dstStore, err := oci.NewLayoutStore(dstDir)
	if err != nil {
		t.Fatalf("NewLayoutStore dst: %v", err)
	}
	dstDesc, err := dstStore.Resolve(ctx, dstDir)
	if err != nil {
		t.Fatalf("Resolve dst: %v", err)
	}
	if dstDesc.Digest != srcDesc.Digest {
		t.Errorf("manifest digest mismatch: src=%q, dst=%q", srcDesc.Digest, dstDesc.Digest)
	}

	// Verify the pack can be read from the destination
	dstCfg, dstPlugins, err := oci.ReadPluginPack(ctx, dstStore, dstDir)
	if err != nil {
		t.Fatalf("ReadPluginPack dst: %v", err)
	}
	if dstCfg.PluginCount != 1 {
		t.Errorf("pluginCount = %d, want 1", dstCfg.PluginCount)
	}
	if len(dstPlugins) != 1 || dstPlugins[0].Name != "test-plugin" {
		t.Errorf("plugins mismatch: %+v", dstPlugins)
	}
}

// (b) tampered source blob causes import to exit 1
func TestImport_TamperedBlob(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcStore, err := oci.NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore src: %v", err)
	}

	ctx := t.Context()
	content := []byte("original-content")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Sha256Digest: %v", err)
	}

	plugins := []oci.ResolvedPlugin{{
		Name: "p", Version: "1.0",
		SHA256:  digest,
		Content: bytes.NewReader(content),
	}}
	lockHash := oci.LockHash(plugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "1.0",
		Profile:        "test",
		LockHash:       lockHash,
		PluginCount:    1,
		CreatedAt:      "2025-07-18T12:00:00Z",
	}
	if err := oci.BuildPluginPack(ctx, srcStore, srcDir, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	// Corrupt a blob on disk — files are read-only, so chmod first
	found := false
	blobsDir := filepath.Join(srcDir, "blobs", "sha256")
	if entries, err := os.ReadDir(blobsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				blobPath := filepath.Join(blobsDir, e.Name())
				_ = os.Chmod(blobPath, 0644)
				if err := os.WriteFile(blobPath, []byte("corrupted!"), 0644); err != nil {
					t.Fatalf("write corrupted blob: %v", err)
				}
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("no blob files found to corrupt")
	}

	// Import should fail (exit 1, not usage error)
	err = importPackCmd(t,
		"--from", "dir://"+srcDir,
		"--to", "dir://"+dstDir,
	)
	if err == nil {
		t.Fatal("expected error for tampered blob, got nil")
	}
	if isUsageError(err) {
		t.Errorf("expected non-usage error (exit 1), got usage error: %v", err)
	}
}

// (c) uc:// on --from exits 1 with the exact error message; uc:// on --to
// requires VARROACTL_UC_TOKEN and fails with a different error when unset.
func TestImport_UCNotSupported(t *testing.T) {
	t.Run("uc-from", func(t *testing.T) {
		_ = t.TempDir()
		err := importPackCmd(t,
			"--from", "uc://example.com/repo:v1",
			"--to", "dir://"+t.TempDir(),
		)
		if err == nil {
			t.Fatal("expected errUCNotSupported, got nil")
		}
		if isUsageError(err) {
			t.Errorf("expected exit 1 error (not usage), got usage error: %v", err)
		}
		if err.Error() != errUCNotSupported.Error() {
			t.Errorf("error = %q, want %q", err.Error(), errUCNotSupported.Error())
		}
	})

	t.Run("uc-to-missing-token", func(t *testing.T) {
		_ = t.TempDir()
		err := importPackCmd(t,
			"--from", "dir://"+t.TempDir(),
			"--to", "uc://example.com:8080",
		)
		if err == nil {
			t.Fatal("expected error for missing token, got nil")
		}
		if isUsageError(err) {
			t.Errorf("expected exit 1 error (not usage), got usage error: %v", err)
		}
		// Must NOT be the old errUCNotSupported error.
		if err.Error() == errUCNotSupported.Error() {
			t.Errorf("expected token-related error, got errUCNotSupported")
		}
		// Must contain the env var name.
		if !strings.Contains(err.Error(), "VARROACTL_UC_TOKEN") {
			t.Errorf("expected error mentioning VARROACTL_UC_TOKEN, got: %v", err)
		}
	})
}

// (d) an unrecognized scheme (e.g. ftp://) exits 2 (usageError)
func TestImport_UnrecognizedScheme(t *testing.T) {
	tests := []struct {
		name     string
		side     string // "from" or "to"
		from, to string
	}{
		{
			name: "ftp-from",
			from: "ftp://example.com/repo:v1",
			to:   "dir://" + t.TempDir(),
		},
		{
			name: "ftp-to",
			from: "dir://" + t.TempDir(),
			to:   "ftp://example.com/repo:v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = t.TempDir() // ensure unique temp dirs
			err := importPackCmd(t,
				"--from", tt.from,
				"--to", tt.to,
			)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ue usageError
			if !errorsAs(err, &ue) {
				t.Errorf("expected usageError (exit 2), got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), "unrecognized scheme") {
				t.Errorf("expected 'unrecognized scheme' in error, got: %v", err)
			}
		})
	}
}

// (e) default output shape and -o json output shape
func TestImport_DefaultOutput(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Build a source pack
	srcStore, err := oci.NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	ctx := t.Context()
	content := []byte("hpi-content-for-output-test")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Sha256Digest: %v", err)
	}

	plugins := []oci.ResolvedPlugin{{
		Name: "p", Version: "1.0",
		SHA256:  digest,
		Content: bytes.NewReader(content),
	}}
	lockHash := oci.LockHash(plugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "1.0", Profile: "test",
		LockHash: lockHash, PluginCount: 1,
		CreatedAt: "2025-07-18T12:00:00Z",
	}
	if err := oci.BuildPluginPack(ctx, srcStore, srcDir, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	// Capture default output
	output, err := importPackCapture(t,
		"--from", "dir://"+srcDir,
		"--to", "dir://"+dstDir,
	)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	// Default output should be one line like "Imported dir://<src> → dir://<dst> with digest sha256:..."
	if !strings.Contains(output, "Imported") {
		t.Errorf("default output should contain 'Imported', got: %s", output)
	}
	if !strings.Contains(output, "with digest") {
		t.Errorf("default output should contain 'with digest', got: %s", output)
	}
	if !strings.Contains(output, "sha256:") {
		t.Errorf("default output should contain digest, got: %s", output)
	}
}

func TestImport_JSONOutput(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcStore, err := oci.NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	ctx := t.Context()
	content := []byte("hpi-json-output-test")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Sha256Digest: %v", err)
	}

	plugins := []oci.ResolvedPlugin{{
		Name: "p", Version: "1.0",
		SHA256:  digest,
		Content: bytes.NewReader(content),
	}}
	lockHash := oci.LockHash(plugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "1.0", Profile: "test",
		LockHash: lockHash, PluginCount: 1,
		CreatedAt: "2025-07-18T12:00:00Z",
	}
	if err := oci.BuildPluginPack(ctx, srcStore, srcDir, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	output, err := importPackCapture(t,
		"--from", "dir://"+srcDir,
		"--to", "dir://"+dstDir,
		"-o", "json",
	)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(output), &out); err != nil {
		t.Fatalf("invalid JSON output: %v\nOutput: %s", err, output)
	}

	// Must have exactly 'digest' and 'ref' keys
	if _, ok := out["digest"]; !ok {
		t.Error("missing 'digest' key")
	}
	if _, ok := out["ref"]; !ok {
		t.Error("missing 'ref' key")
	}
	// Must NOT have 'pluginCount' key
	if _, ok := out["pluginCount"]; ok {
		t.Error("import JSON output should NOT contain 'pluginCount'")
	}
	// Must NOT have any other unexpected keys beyond digest, ref
	for k := range out {
		if k != "digest" && k != "ref" {
			t.Errorf("unexpected key in JSON output: %q", k)
		}
	}
}

// (f) uc:// import: missing token fails fast without network
func TestImport_UC_MissingToken(t *testing.T) {
	// Point --to at a uc:// host with no listener; the env check should
	// precede any dial attempt.
	err := importPackCmd(t,
		"--from", "dir://"+t.TempDir(),
		"--to", "uc://127.0.0.1:1",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if isUsageError(err) {
		t.Errorf("expected exit 1 (not usage), got usage error: %v", err)
	}
	if !strings.Contains(err.Error(), "VARROACTL_UC_TOKEN") {
		t.Errorf("expected error mentioning VARROACTL_UC_TOKEN, got: %v", err)
	}
	// Must NOT contain "connection refused" or similar network error.
	if strings.Contains(err.Error(), "connect") || strings.Contains(err.Error(), "refused") {
		t.Errorf("expected fail-fast before network, but got network error: %v", err)
	}
}

// (g) uc:// import: success against an httptest server
func TestImport_UC_Success(t *testing.T) {
	srcDir := t.TempDir()

	// Build a plugin pack in the source layout.
	srcStore, err := oci.NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	ctx := t.Context()
	content := []byte("fake-hpi-for-uc-import")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Sha256Digest: %v", err)
	}
	plugins := []oci.ResolvedPlugin{{
		Name: "uc-plugin", Version: "1.0.0",
		SHA256:  digest,
		Content: bytes.NewReader(content),
	}}
	lockHash := oci.LockHash(plugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "2.479.3", Profile: "test",
		LockHash: lockHash, PluginCount: 1,
		CreatedAt: "2025-07-18T12:00:00Z",
	}
	if err := oci.BuildPluginPack(ctx, srcStore, srcDir, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	// Start an httptest server that accepts the import.
	var gotToken bool
	importSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer valid-token" {
			gotToken = true
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "accepted"})
	}))
	defer importSrv.Close()

	t.Setenv("VARROACTL_UC_TOKEN", "valid-token")

	err = importPackCmd(t,
		"--from", "dir://"+srcDir,
		"--to", "uc://"+importSrv.Listener.Addr().String(),
	)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if !gotToken {
		t.Error("server did not receive the expected token")
	}
}

// (h) uc:// import: 401 returns distinct error
func TestImport_UC_401(t *testing.T) {
	srcDir := t.TempDir()
	srcStore, err := oci.NewLayoutStore(srcDir)
	if err != nil {
		t.Fatalf("NewLayoutStore: %v", err)
	}
	ctx := t.Context()
	content := []byte("hpi-for-401-test")
	digest, _, err := oci.Sha256Digest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Sha256Digest: %v", err)
	}
	plugins := []oci.ResolvedPlugin{{
		Name: "plugin-401", Version: "1.0.0",
		SHA256:  digest,
		Content: bytes.NewReader(content),
	}}
	lockHash := oci.LockHash(plugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: "1.0", Profile: "test",
		LockHash: lockHash, PluginCount: 1,
		CreatedAt: "2025-07-18T12:00:00Z",
	}
	if err := oci.BuildPluginPack(ctx, srcStore, srcDir, cfg, plugins); err != nil {
		t.Fatalf("BuildPluginPack: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("VARROACTL_UC_TOKEN", "bad-token")

	err = importPackCmd(t,
		"--from", "dir://"+srcDir,
		"--to", "uc://"+srv.Listener.Addr().String(),
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if isUsageError(err) {
		t.Errorf("expected exit 1 (not usage), got usage error: %v", err)
	}
	// Must be the distinct "invalid or expired" error, NOT the missing-token error.
	if strings.Contains(err.Error(), "VARROACTL_UC_TOKEN") {
		t.Errorf("expected distinct 401 error, got missing-token error: %v", err)
	}
}
