package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifestValid(t *testing.T) {
	dir := t.TempDir()

	manifest := `id: "test-bundle"
version: "1"
apiVersion: "2"
description: "Test bundle"
jcasc:
  - "jenkins.yaml"
plugins:
  - "plugins.yaml"
items:
  - "items.yaml"
variables:
  - "vars.yaml"
`
	writeFile(t, dir, "bundle.yaml", manifest)
	writeFile(t, dir, "jenkins.yaml", "jenkins:\n  systemMessage: hello")
	writeFile(t, dir, "plugins.yaml", "plugins: []")
	writeFile(t, dir, "items.yaml", "items: []")
	writeFile(t, dir, "vars.yaml", "namespace: default")

	m, err := ParseManifest(dir)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.ID != "test-bundle" {
		t.Errorf("expected id test-bundle, got %q", m.ID)
	}
	if m.APIVersion != "2" {
		t.Errorf("expected apiVersion 2, got %q", m.APIVersion)
	}
	if len(m.Jcasc) != 1 || m.Jcasc[0] != "jenkins.yaml" {
		t.Errorf("expected jcasc to reference jenkins.yaml, got %v", m.Jcasc)
	}
	if len(m.Items) != 1 || m.Items[0] != "items.yaml" {
		t.Errorf("expected items to reference items.yaml, got %v", m.Items)
	}
}

func TestParseManifestMissingBundleYAML(t *testing.T) {
	dir := t.TempDir()

	_, err := ParseManifest(dir)
	if err == nil {
		t.Fatal("expected error for missing bundle.yaml")
	}
}

func TestParseManifestEmptyBundleYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bundle.yaml", "")

	_, err := ParseManifest(dir)
	if err == nil {
		t.Fatal("expected error for empty bundle.yaml")
	}
}

func TestParseManifestMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{"missing id", `version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
`},
		{"missing version", `id: "test"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
`},
		{"missing apiVersion", `id: "test"
version: "1"
jcasc:
  - "jenkins.yaml"
`},
		{"empty jcasc", `id: "test"
version: "1"
apiVersion: "1"
jcasc: []
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "bundle.yaml", tt.manifest)

			_, err := ParseManifest(dir)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestParseManifestInvalidFieldValues(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{"bad apiVersion", `id: "test"
version: "1"
apiVersion: "3"
jcasc:
  - "jenkins.yaml"
`},
		{"bad jcascMergeStrategy", `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
jcascMergeStrategy: "merge"
`},
		{"bad itemRemoveStrategy.items", `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
itemRemoveStrategy:
  items: "delete-all"
`},
		{"bad itemRemoveStrategy.rbac", `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
itemRemoveStrategy:
  rbac: "delete"
`},
		{"bad rbacRemoveStrategy", `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
rbacRemoveStrategy: "delete"
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "bundle.yaml", tt.manifest)

			_, err := ParseManifest(dir)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestParseManifestMissingReferencedFile(t *testing.T) {
	dir := t.TempDir()

	manifest := `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
plugins:
  - "plugins.yaml"
`
	writeFile(t, dir, "bundle.yaml", manifest)
	writeFile(t, dir, "jenkins.yaml", "jenkins: {}")
	// plugins.yaml is intentionally missing

	_, err := ParseManifest(dir)
	if err == nil {
		t.Fatal("expected error for missing referenced file plugins.yaml")
	}
}

func TestMergeJcascSingleFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "jenkins.yaml", "jenkins:\n  systemMessage: hello")

	result, err := mergeJcasc(dir, []string{"jenkins.yaml"}, "errorOnConflict")
	if err != nil {
		t.Fatalf("mergeJcasc: %v", err)
	}
	if result != "jenkins:\n  systemMessage: hello" {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestMergeJcascOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base.yaml", "jenkins:\n  systemMessage: hello\n  numExecutors: 2")
	writeFile(t, dir, "override.yaml", "jenkins:\n  systemMessage: overridden")

	result, err := mergeJcasc(dir, []string{"base.yaml", "override.yaml"}, "override")
	if err != nil {
		t.Fatalf("mergeJcasc: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestMergeJcascErrorOnConflict(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base.yaml", "jenkins:\n  systemMessage: hello")
	writeFile(t, dir, "override.yaml", "jenkins:\n  systemMessage: conflict")

	_, err := mergeJcasc(dir, []string{"base.yaml", "override.yaml"}, "errorOnConflict")
	if err == nil {
		t.Fatal("expected error for duplicate key")
	}
}

func TestMergeJenkinsYAML_OverlayOverridesKey(t *testing.T) {
	base := "jenkins:\n  systemMessage: hello\n  numExecutors: 2"
	overlay := "jenkins:\n  systemMessage: overridden"

	result, err := MergeJenkinsYAML(base, overlay)
	if err != nil {
		t.Fatalf("MergeJenkinsYAML: %v", err)
	}

	// systemMessage should be overridden, numExecutors should remain.
	if !strings.Contains(result, "overridden") {
		t.Error("expected overlay value 'overridden' in result")
	}
	if !strings.Contains(result, "numExecutors: 2") {
		t.Error("expected base-only key 'numExecutors: 2' to survive deep-merge")
	}
}

func TestMergeJenkinsYAML_DeepMergeMaps(t *testing.T) {
	base := "jenkins:\n  systemMessage: hello\n  security:\n    realm: ldap"
	overlay := "jenkins:\n  security:\n    authorization: loggedInUsersCanDoAnything"

	result, err := MergeJenkinsYAML(base, overlay)
	if err != nil {
		t.Fatalf("MergeJenkinsYAML: %v", err)
	}
	if !strings.Contains(result, "realm: ldap") {
		t.Error("expected base sub-key 'realm' to survive deep-merge")
	}
	if !strings.Contains(result, "loggedInUsersCanDoAnything") {
		t.Error("expected overlay sub-key 'authorization' in result")
	}
}

func TestMergeJenkinsYAML_EmptyOverlayReturnsBase(t *testing.T) {
	base := "jenkins:\n  systemMessage: hello"
	result, err := MergeJenkinsYAML(base, "")
	if err != nil {
		t.Fatalf("MergeJenkinsYAML: %v", err)
	}
	if result != base {
		t.Errorf("expected base unchanged, got %q", result)
	}
}

func TestMergeJenkinsYAML_EmptyBaseReturnsOverlay(t *testing.T) {
	overlay := "jenkins:\n  systemMessage: from-overlay"
	result, err := MergeJenkinsYAML("", overlay)
	if err != nil {
		t.Fatalf("MergeJenkinsYAML: %v", err)
	}
	if result != overlay {
		t.Errorf("expected overlay unchanged, got %q", result)
	}
}

func TestMergeJenkinsYAML_VariableResolvesAfterMerge(t *testing.T) {
	base := "jenkins:\n  systemMessage: hello"
	overlay := "jenkins:\n  description: ${varroa_controller_name}"

	merged, err := MergeJenkinsYAML(base, overlay)
	if err != nil {
		t.Fatalf("MergeJenkinsYAML: %v", err)
	}

	vars := Variables{"varroa_controller_name": "test-controller"}
	resolved := ResolveVars(merged, vars)
	if !strings.Contains(resolved, "test-controller") {
		t.Errorf("expected variable substitution in overlay-contributed key, got %q", resolved)
	}
	if !strings.Contains(resolved, "hello") {
		t.Error("expected base key 'hello' to survive merge and resolve")
	}
}

func TestValidatorRequiresBundleYAML(t *testing.T) {
	dir := t.TempDir()
	v := NewValidator()

	result := v.Validate(dir)
	if result.Valid {
		t.Error("expected invalid result for missing bundle.yaml")
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors")
	}
}

func TestValidatorWithBundleYAML(t *testing.T) {
	dir := t.TempDir()

	manifest := `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
plugins:
  - "plugins.yaml"
variables:
  - "vars.yaml"
`
	writeFile(t, dir, "bundle.yaml", manifest)
	writeFile(t, dir, "jenkins.yaml", "jenkins: {}")
	writeFile(t, dir, "plugins.yaml", "plugins: []")
	writeFile(t, dir, "vars.yaml", "namespace: default")

	v := NewValidator()
	result := v.Validate(dir)
	if !result.Valid {
		t.Errorf("expected valid, got errors: %v", result.Errors)
	}
}

func TestValidatorWarnsNoVariables(t *testing.T) {
	dir := t.TempDir()

	manifest := `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
`
	writeFile(t, dir, "bundle.yaml", manifest)
	writeFile(t, dir, "jenkins.yaml", "jenkins: {}")

	v := NewValidator()
	result := v.Validate(dir)
	if !result.Valid {
		t.Errorf("expected valid, got errors: %v", result.Errors)
	}
	if len(result.Warnings) == 0 {
		t.Error("expected warning about missing variables.yaml")
	}
}

func TestResolverValidation(t *testing.T) {
	dir := t.TempDir()
	r := NewResolver(dir)

	// Empty repoURL
	_, err := r.Resolve("", "path", "main", "ctrl", "ns")
	if err == nil {
		t.Error("expected error for empty repoURL")
	}

	// Empty path
	_, err = r.Resolve("https://example.com/repo", "", "main", "ctrl", "ns")
	if err == nil {
		t.Error("expected error for empty path")
	}
}

func TestGitCloner(t *testing.T) {
	g := NewGitCloner()
	if g == nil {
		t.Error("expected non-nil GitCloner")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestParseManifestRejectsPathTraversal(t *testing.T) {
	fields := []string{"jcasc", "plugins", "items", "rbac", "variables"}
	for _, f := range fields {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()

			manifest := `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
` + f + `:
  - "../secret.yaml"
`
			writeFile(t, dir, "bundle.yaml", manifest)
			if f == "jcasc" {
				writeFile(t, dir, "../secret.yaml", "dummy")
			} else {
				writeFile(t, dir, "jenkins.yaml", "jenkins: {}")
			}

			_, err := ParseManifest(dir)
			if err == nil {
				t.Error("expected error for path traversal, got nil")
			}
		})
	}
}

func TestParseManifestRejectsSymlinkEscape(t *testing.T) {
	fields := []string{"jcasc", "plugins", "items", "rbac", "variables"}
	for _, f := range fields {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()

			// Create a file outside the bundle dir.
			outsideDir := t.TempDir()
			outsideFile := filepath.Join(outsideDir, "secret.yaml")
			if err := os.WriteFile(outsideFile, []byte("sensitive"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Create a symlink inside bundleDir pointing outside.
			linkPath := filepath.Join(dir, "link")
			if err := os.Symlink(outsideFile, linkPath); err != nil {
				t.Fatal(err)
			}

			manifest := `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
` + f + `:
  - "link"
`
			writeFile(t, dir, "bundle.yaml", manifest)
			writeFile(t, dir, "jenkins.yaml", "jenkins: {}")

			_, err := ParseManifest(dir)
			if err == nil {
				t.Error("expected error for symlink escape, got nil")
			}
		})
	}
}

func TestReadFilesRejectsSymlinkedDirectoryEntry(t *testing.T) {
	dir := t.TempDir()

	// Create a file outside the bundle dir.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "evil-content.yaml")
	if err := os.WriteFile(outsideFile, []byte("outside: data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create items/ directory with a real file and a symlinked file.
	itemsDir := filepath.Join(dir, "items")
	if err := os.MkdirAll(itemsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, itemsDir, "a.yaml", "real: content")
	// Symlink a file inside items/ that points outside.
	evilLink := filepath.Join(itemsDir, "evil.yaml")
	if err := os.Symlink(outsideFile, evilLink); err != nil {
		t.Fatal(err)
	}

	manifest := `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
items:
  - "items"
`
	writeFile(t, dir, "bundle.yaml", manifest)
	writeFile(t, dir, "jenkins.yaml", "jenkins: {}")

	// ParseManifest validates declared paths (the directory itself passes).
	// The symlink is caught by materializeDir when readFiles scans directory
	// contents and hits the evil symlink.
	r := NewResolver(t.TempDir())
	_, err := r.materializeDir(dir)
	if err == nil {
		t.Fatal("expected error for symlinked directory entry, got nil")
	}
}
