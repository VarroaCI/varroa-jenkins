package updatecenter

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
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

// importServerFault is a fault of the update center's own disk while an import
// unpacks. handleImport answers 500 for it, so the sender retries the pack.
type importServerFault struct {
	op   string
	name string
	err  error
}

func (e *importServerFault) Error() string {
	cause := e.err
	var pathErr *fs.PathError
	if errors.As(cause, &pathErr) {
		cause = pathErr.Err
	}
	return fmt.Sprintf("%s %s: %v", e.op, e.name, cause)
}

func (e *importServerFault) Unwrap() error { return e.err }

// entryFault classifies a filesystem error for one tar entry. ENOTDIR, EISDIR,
// and ENAMETOOLONG come only from entry names in a fresh temporary directory,
// so they refuse the archive. Every other error is a fault of the update center disk.
func entryFault(op, name string, err error) error {
	if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENAMETOOLONG) {
		return fmt.Errorf("%s %s: %w", op, name, err)
	}
	return &importServerFault{op: op, name: name, err: err}
}

// entryWriter records the first write error, so a failed copy names the side
// that failed: the request body or the update center disk.
type entryWriter struct {
	w   io.Writer
	err error
}

func (e *entryWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

// openImportEntry opens one regular tar entry for writing.
func openImportEntry(path string) (io.WriteCloser, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// handleImport handles POST /api/v1/import.
// Requires Authorization: Bearer <token>. Body is an OCI-layout .tar.gz.
// The import runs in the request. It answers 200 when every manifest reads and copies.
// A fault of the update center disk answers 500, so the sender retries.
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
		var fault *importServerFault
		if errors.As(err, &fault) {
			s.logger.Error("failed to unpack import on the update center disk", "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("unpack the import on the update center: %v", err))
			return
		}
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

	pluginCount, err := s.checkImportLayout(r.Context(), srcStore, srcDescs)
	if err != nil {
		s.logger.Warn("import rejected", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	for _, d := range srcDescs {
		ref := d.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			ref = d.Digest
		}
		if err := oci.Copy(r.Context(), srcStore, ref, s.store, ref); err != nil {
			s.logger.Error("failed to copy manifest during import", "ref", ref, "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("copy pack %q into the store: %v", ref, err))
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "imported",
		"manifests":    len(srcDescs),
		"plugin_count": pluginCount,
	})
}

// extractTarGz extracts a .tar.gz archive from r into destDir.
// Rejects entries that would escape destDir (zip-slip guard), enforces per-entry
// and total byte caps (with remaining-budget-aware per-entry limiting), caps
// entry count, and rejects non-regular/non-directory typeflags.
// It returns *importServerFault for a fault of the destination disk.
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
				return entryFault("mkdir", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return entryFault("mkdir parent of", hdr.Name, err)
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
			f, err := s.openImportFile(targetPath)
			if err != nil {
				return entryFault("create", hdr.Name, err)
			}
			dst := &entryWriter{w: f}
			written, copyErr := io.Copy(dst, limited)
			closeErr := f.Close()
			// The size verdict comes first: an oversize entry is a content
			// refusal even when the disk also faults while it closes.
			if written > perEntryCap {
				return fmt.Errorf("%w: entry %q exceeds import byte limit", errImportTooLarge, hdr.Name)
			}
			if dst.err != nil {
				return entryFault("write", hdr.Name, dst.err)
			}
			if copyErr != nil {
				return fmt.Errorf("read %s: %w", hdr.Name, copyErr)
			}
			if closeErr != nil {
				return entryFault("close", hdr.Name, closeErr)
			}
			totalBytes += written
		}
	}
	return nil
}

// checkImportLayout reads every manifest of the import layout and returns the
// plugin count. The import calls it before any copy, so a refused layout leaves
// the store unchanged. A manifest passes only when the update center can serve it.
func (s *Server) checkImportLayout(ctx context.Context, store oci.BlobStore, descs []oci.Descriptor) (int, error) {
	if len(descs) == 0 {
		return 0, errors.New("import layout holds no manifest")
	}
	count := 0
	for _, d := range descs {
		ref := d.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			ref = d.Digest
		}
		manifest, err := store.Pull(ctx, ref)
		if err != nil {
			return 0, fmt.Errorf("read manifest %q: %w", ref, err)
		}
		if manifest.ArtifactType != oci.ArtifactTypePluginPack {
			return 0, fmt.Errorf("manifest %q is not a plugin pack: artifact type %q, want %q", ref, manifest.ArtifactType, oci.ArtifactTypePluginPack)
		}
		if _, _, err := oci.ReadPluginPack(ctx, store, ref); err != nil {
			return 0, fmt.Errorf("read pack %q: %w", ref, err)
		}
		for _, l := range manifest.Layers {
			if l.MediaType != oci.MediaTypePluginHPI {
				return 0, fmt.Errorf("pack %q has unsupported layer media type %q", ref, l.MediaType)
			}
			rc, err := store.FetchBlob(ctx, l.Digest)
			if err != nil {
				return 0, fmt.Errorf("pack %q is missing blob %s: %w", ref, l.Digest, err)
			}
			_ = rc.Close()
		}
		plugins := pluginLayersFromManifest(manifest, s.logger)
		for _, info := range plugins {
			if ucmeta.IsLatest(info.Version) {
				subject := info.Name
				if subject == "" {
					subject = "layer " + info.LayerDigest
				}
				return 0, fmt.Errorf("plugin %q in pack %q has no concrete version (%q): export the pack again with a concrete version", subject, ref, info.Version)
			}
		}
		count += len(plugins)
	}
	return count, nil
}
