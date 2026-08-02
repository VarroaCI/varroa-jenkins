package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func writeHPIFixture(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// TestExportAddon_IdentityComesFromManifest is the load-bearing property: name
// and version are read out of META-INF/MANIFEST.MF and no flag can override
// them, so a pack cannot be mislabeled relative to the bytes it holds.
func TestExportAddon_IdentityComesFromManifest(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()
	hpiBytes := testHPIBytes(t, "varroa-mcp-tools", "1.0.0", "Varroa MCP Tools", "2.533",
		"mcp-server:1.0,workflow-api:2.0;resolution:=optional")
	hpiPath := writeHPIFixture(t, tmp, "plugin.hpi", hpiBytes)
	outDir := filepath.Join(tmp, "out")

	root := newRootCmd()
	root.SetArgs([]string{
		"export", "plugin-addon",
		"--hpi", hpiPath,
		"--to", "dir://" + outDir,
		"--tag", "varroa", "--tag", "mcp",
		"--description", "First-party MCP tooling",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("export plugin-addon: %v", err)
	}

	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}
	cfg, plugins, err := oci.ReadPluginPack(context.Background(), store, outDir)
	if err != nil {
		t.Fatalf("ReadPluginPack: %v", err)
	}
	if cfg.Kind != oci.PackKindAddon {
		t.Errorf("cfg.Kind = %q, want %q", cfg.Kind, oci.PackKindAddon)
	}
	if cfg.Profile != "" {
		t.Errorf("addon pack must carry an empty profile, got %q", cfg.Profile)
	}
	if cfg.JenkinsVersion != "2.533" {
		t.Errorf("cfg.JenkinsVersion = %q, want the manifest's Jenkins-Version", cfg.JenkinsVersion)
	}
	if cfg.PluginCount != 1 {
		t.Errorf("cfg.PluginCount = %d", cfg.PluginCount)
	}
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins", len(plugins))
	}
	p := plugins[0]
	if p.Name != "varroa-mcp-tools" || p.Version != "1.0.0" {
		t.Errorf("identity = %s@%s, want varroa-mcp-tools@1.0.0", p.Name, p.Version)
	}
	if p.DisplayName != "Varroa MCP Tools" || p.RequiredCore != "2.533" {
		t.Errorf("derived metadata = %+v", p)
	}
	if p.Description != "First-party MCP tooling" {
		t.Errorf("Description = %q", p.Description)
	}
	if len(p.Tags) != 2 || p.Tags[0] != "varroa" || p.Tags[1] != "mcp" {
		t.Errorf("Tags = %v", p.Tags)
	}
	if len(p.Dependencies) != 2 || p.Dependencies[0].Name != "mcp-server" || !p.Dependencies[1].Optional {
		t.Errorf("Dependencies = %+v", p.Dependencies)
	}
	if p.UpstreamURL != "" {
		t.Errorf("a local artifact has no upstream, got %q", p.UpstreamURL)
	}
}

func TestExportAddon_UnparseableHPIPushesNothing(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()
	outDir := filepath.Join(tmp, "out")

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"not a zip", []byte("this is not an hpi")},
		{"zip without plugin identity", testHPIBytesNoIdentity(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hpiPath := writeHPIFixture(t, tmp, "bad.hpi", tc.body)
			root := newRootCmd()
			root.SetArgs([]string{"export", "plugin-addon", "--hpi", hpiPath, "--to", "dir://" + outDir})
			if err := root.Execute(); err == nil {
				t.Fatal("expected an error for an unparseable HPI")
			}
			if _, err := os.Stat(filepath.Join(outDir, "index.json")); err == nil {
				t.Error("nothing may be written to the destination")
			}
		})
	}
}

func TestExportAddon_DryRunPushesNothing(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()
	hpiPath := writeHPIFixture(t, tmp, "plugin.hpi",
		testHPIBytes(t, "demo", "2.0.0", "Demo", "2.500", ""))
	outDir := filepath.Join(tmp, "out")

	var runErr error
	stdout := captureStdout(t, func() {
		root := newRootCmd()
		root.SetArgs([]string{"export", "plugin-addon", "--hpi", hpiPath, "--to", "dir://" + outDir, "--dry-run"})
		runErr = root.Execute()
	})
	if runErr != nil {
		t.Fatalf("dry run failed: %v", runErr)
	}
	if _, err := os.Stat(outDir); err == nil {
		t.Error("--dry-run must not create the destination")
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("dry-run output is not JSON: %v\n%s", err, stdout)
	}
	if out["dryRun"] != true || out["name"] != "demo" || out["version"] != "2.0.0" {
		t.Errorf("dry-run output = %v", out)
	}
	if _, ok := out["config"]; !ok {
		t.Error("dry run must print the resolved pack config")
	}
	if _, ok := out["annotations"]; !ok {
		t.Error("dry run must print the resolved annotations")
	}
}

func TestExportAddon_RejectsUCDestination(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()
	hpiPath := writeHPIFixture(t, tmp, "plugin.hpi",
		testHPIBytes(t, "demo", "1.0.0", "Demo", "", ""))

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugin-addon", "--hpi", hpiPath, "--to", "uc://uc.example.com"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected uc:// to be rejected")
	}
	if !strings.Contains(err.Error(), "uc://") {
		t.Errorf("error should name the scheme: %v", err)
	}
	var ue usageError
	if !errorsAs(err, &ue) {
		t.Errorf("expected a usage error, got %T: %v", err, err)
	}
}

func TestExportAddon_RequiresFlags(t *testing.T) {
	testSetup(t)
	for _, args := range [][]string{
		{"export", "plugin-addon", "--to", "dir:///tmp/x"},
		{"export", "plugin-addon", "--hpi", "/tmp/x.hpi"},
	} {
		root := newRootCmd()
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Errorf("expected a required-flag error for %v", args)
		}
	}
}

func TestExportAddon_RejectsNonJSONOutput(t *testing.T) {
	testSetup(t)
	tmp := t.TempDir()
	hpiPath := writeHPIFixture(t, tmp, "plugin.hpi",
		testHPIBytes(t, "demo", "1.0.0", "Demo", "", ""))

	root := newRootCmd()
	root.SetArgs([]string{"export", "plugin-addon", "--hpi", hpiPath, "--to", "dir://" + tmp + "/out", "-o", "yaml"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "only supports -o json") {
		t.Fatalf("expected an -o json restriction, got %v", err)
	}
}

// testHPIBytesNoIdentity builds a valid zip whose manifest has no Short-Name.
func testHPIBytesNoIdentity(t *testing.T) []byte {
	t.Helper()
	return testHPIBytesRaw(t, "Manifest-Version: 1.0\r\nPlugin-Version: 1.0\r\n\r\n")
}
