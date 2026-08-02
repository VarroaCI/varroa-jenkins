package updatecenter

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// Import resource limits (hardening against decompression-bomb DoS).
const (
	maxImportEntries          = 100000
	maxImportEntryBytes int64 = 512 << 20 // 512 MiB per entry
	maxImportTotalBytes int64 = 4 << 30   // 4 GiB total
)

// Sentinel errors returned by extractTarGz for recognisable HTTP mapping.
var (
	errImportTooLarge = errors.New("import tarball exceeds resource limits")
	errImportBadEntry = errors.New("import tarball contains disallowed entry type")
)

// handleImport handles POST /api/v1/import.
// Requires Authorization: Bearer <token>. Body is an OCI-layout .tar.gz.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Auth: require Bearer token.
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		s.logger.Warn("import request missing Bearer token")
		writeError(w, http.StatusUnauthorized, "unauthorized: missing Bearer token")
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")

	if !s.verifyToken(token) {
		s.logger.Warn("import request with invalid token")
		writeError(w, http.StatusUnauthorized, "unauthorized: invalid token")
		return
	}

	// Extract the OCI-layout tarball to a temp directory.
	tmpDir, err := os.MkdirTemp("", "varroa-uc-import-")
	if err != nil {
		s.logger.Error("failed to create temp dir for import", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create temp directory")
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := s.extractTarGz(r.Body, tmpDir); err != nil {
		s.logger.Warn("failed to extract import tarball", "error", err)
		if errors.Is(err, errImportTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("import too large: %v", err))
		} else if errors.Is(err, errImportBadEntry) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid tarball entry: %v", err))
		} else {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid tarball: %v", err))
		}
		return
	}

	// Open the extracted directory as a LayoutStore.
	srcStore, err := oci.NewLayoutStore(tmpDir)
	if err != nil {
		s.logger.Error("failed to open layout store from import", "error", err)
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid OCI layout: %v", err))
		return
	}

	// List all manifests in the source and copy each to the service's store.
	srcDescs, err := srcStore.ListManifests(r.Context())
	if err != nil {
		s.logger.Error("failed to list manifests in import", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list manifests")
		return
	}

	imported := 0
	for _, d := range srcDescs {
		ref := d.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			ref = d.Digest
		}

		if err := oci.Copy(r.Context(), srcStore, ref, s.store, ref); err != nil {
			s.logger.Warn("failed to copy manifest during import", "ref", ref, "error", err)
			continue
		}
		imported++
	}

	// Count plugins in the imported manifests.
	pluginCount := s.countPluginsInLayout(r.Context(), srcStore, srcDescs)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":       "accepted",
		"manifests":    imported,
		"plugin_count": pluginCount,
	})
}

// extractTarGz extracts a .tar.gz archive from r into destDir.
// Rejects entries that would escape destDir (zip-slip guard), enforces per-entry
// and total byte caps (with remaining-budget-aware per-entry limiting), caps
// entry count, and rejects non-regular/non-directory typeflags.
func (s *Server) extractTarGz(r io.Reader, destDir string) error {
	gzReader, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	tr := tar.NewReader(gzReader)
	cleanDest := filepath.Clean(destDir)
	var entryCount int
	var totalBytes int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		entryCount++
		if entryCount > s.maxImportEntries {
			return errImportTooLarge
		}

		// Reject typeflags that are not regular files or directories.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("%w: entry %q has disallowed type %d", errImportBadEntry, hdr.Name, hdr.Typeflag)
		}

		targetPath := filepath.Join(cleanDest, filepath.Clean(hdr.Name))

		// Zip-slip guard: reject entries that escape the destination.
		if !strings.HasPrefix(targetPath, cleanDest+string(os.PathSeparator)) && targetPath != cleanDest {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", hdr.Name, err)
			}

			// Bound the per-entry read by the smaller of the per-entry cap
			// and the remaining total budget, so the copy stops at the exact
			// limit — no over-shoot that wastes disk IO.
			remaining := s.maxImportTotalBytes - totalBytes
			perEntryCap := s.maxImportEntryBytes
			if remaining < perEntryCap {
				perEntryCap = remaining
			}
			limited := io.LimitReader(tr, perEntryCap+1)
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("create %s: %w", hdr.Name, err)
			}
			written, err := io.Copy(f, limited)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			if written > perEntryCap {
				return fmt.Errorf("%w: entry %q exceeds import byte limit", errImportTooLarge, hdr.Name)
			}
			totalBytes += written
		}
	}
	return nil
}

// countPluginsInLayout counts plugins across all manifest refs in the layout.
func (s *Server) countPluginsInLayout(ctx context.Context, store oci.BlobStore, descs []oci.Descriptor) int {
	count := 0
	for _, d := range descs {
		ref := d.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			ref = d.Digest
		}
		manifest, err := store.Pull(ctx, ref)
		if err != nil {
			continue
		}
		if manifest.ArtifactType != oci.ArtifactTypePluginPack {
			continue
		}
		plugins := pluginLayersFromManifest(manifest, s.logger)
		count += len(plugins)
	}
	return count
}
