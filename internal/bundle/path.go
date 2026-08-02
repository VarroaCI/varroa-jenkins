package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateRelPath rejects a manifest/catalog-declared path that is not
// lexically local to its base directory (filepath.IsLocal: absolute paths,
// "..", empty string, embedded NUL, Windows-reserved forms). This is a pure
// string check — it cannot see through a symlink, which is why every read
// site also calls ResolveContainedPath below.
func validateRelPath(rel string) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("path %q must be local to its base directory", rel)
	}
	return nil
}

// ResolveContainedPath validates rel (via validateRelPath), joins it onto
// root, resolves symlinks in the result, and verifies the resolved path
// still falls under root's own resolved location. root is resolved too
// (not just the joined child) because root itself may be reached through a
// symlink (e.g. macOS /tmp -> /private/tmp); comparing an unresolved root
// against a resolved child would false-positive-reject legitimate files.
// Returns the resolved, safe-to-read path.
func ResolveContainedPath(root, rel string) (string, error) {
	if err := validateRelPath(rel); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", rel, err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve base directory: %w", err)
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the base directory (symlink)", rel)
	}
	return resolved, nil
}
