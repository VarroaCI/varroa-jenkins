package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCatalogIndexRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()

	// Write a catalog.yaml with a path-traversal item.
	catalogYAML := `apiVersion: "1"
name: test-catalog
items:
  - type: jcasc
    name: bad
    path: "../../../../etc/passwd"
`
	if err := os.WriteFile(filepath.Join(root, "catalog.yaml"), []byte(catalogYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseCatalogIndex(root)
	if err == nil {
		t.Fatal("expected error for path-traversal item, got nil")
	}
}

// TestParseCatalogIndexRejectsSymlinkedCatalogYaml pins that a committed
// symlink at the fixed catalog.yaml name cannot point the operator at files
// outside the checkout (the read must go through ResolveContainedPath).
func TestParseCatalogIndexRejectsSymlinkedCatalogYaml(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(outside, []byte("apiVersion: \"1\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "catalog.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCatalogIndex(root); err == nil {
		t.Fatal("expected symlinked catalog.yaml escaping root to be rejected")
	} else if !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected a containment error, got: %v", err)
	}
}
