package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRelPathRejectsNonLocal(t *testing.T) {
	cases := []string{
		"../etc/passwd",
		"..",
		"a/../../b",
		"/etc/passwd",
		"sub/../../escape",
		"",
	}
	for _, c := range cases {
		err := validateRelPath(c)
		if err == nil {
			t.Errorf("validateRelPath(%q): expected error, got nil", c)
		}
	}
}

func TestValidateRelPathAllowsLegitimate(t *testing.T) {
	cases := []string{
		"jenkins.yaml",
		"jcasc/base.yaml",
		"sub/dir/file.yml",
		"./jenkins.yaml",
		".",
	}
	for _, c := range cases {
		err := validateRelPath(c)
		if err != nil {
			t.Errorf("validateRelPath(%q): unexpected error: %v", c, err)
		}
	}
}

func TestResolveContainedPathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	secret := t.TempDir()
	secretFile := filepath.Join(secret, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("sensitive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveContainedPath(root, "link/secret.txt")
	if err == nil {
		t.Fatal("expected error for symlink escape, got nil")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected escape-related error, got: %v", err)
	}
}

func TestResolveContainedPathAllowsRealNestedFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file.yaml"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ResolveContainedPath(root, "sub/file.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("read resolved path: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("expected %q, got %q", "content", string(data))
	}
}

func TestResolveContainedPathRejectsMissingFile(t *testing.T) {
	root := t.TempDir()
	_, err := ResolveContainedPath(root, "nonexistent.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
