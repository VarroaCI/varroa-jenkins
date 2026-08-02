package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// errUCNotSupported is returned when the caller requests a uc:// scheme.
var errUCNotSupported = fmt.Errorf("uc:// requires the update-center service (not available in this build)")

// openOCIDest opens a BlobStore for writing (destination).
// The returned finalize function MUST be called after the store operation completes.
// For tar://, finalize archives the temp layout into the target .tar.gz and cleans up.
func openOCIDest(scheme, target, registryConfig string, insecure bool) (oci.BlobStore, func() error, error) {
	switch scheme {
	case "dir":
		store, err := oci.NewLayoutStore(target)
		if err != nil {
			return nil, nil, fmt.Errorf("open layout store %q: %w", target, err)
		}
		return store, func() error { return nil }, nil

	case "oci":
		store, err := oci.NewRegistryStore(target, oci.RegistryOptions{
			CredentialConfigPath: registryConfig,
			Insecure:             insecure,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("open registry store %q: %w", target, err)
		}
		return store, func() error { return nil }, nil

	case "tar":
		tmpDir, err := os.MkdirTemp("", "varroactl-tar-*")
		if err != nil {
			return nil, nil, fmt.Errorf("create temp dir for tar: %w", err)
		}
		store, err := oci.NewLayoutStore(tmpDir)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, nil, fmt.Errorf("open layout store in temp dir: %w", err)
		}
		finalize := func() error {
			defer func() { _ = os.RemoveAll(tmpDir) }()
			return createTarGz(target, tmpDir)
		}
		return store, finalize, nil

	case "uc":
		return nil, nil, errUCNotSupported

	default:
		return nil, nil, &ErrUnrecognizedScheme{Scheme: scheme}
	}
}

// openOCISrc opens a BlobStore for reading (source).
// The returned finalize function MUST be called after the store operation completes.
// For tar://, finalize cleans up the temp dir that was extracted from the archive.
func openOCISrc(scheme, target, registryConfig string, insecure bool) (oci.BlobStore, func() error, error) {
	switch scheme {
	case "dir":
		store, err := oci.NewLayoutStore(target)
		if err != nil {
			return nil, nil, fmt.Errorf("open layout store %q: %w", target, err)
		}
		return store, func() error { return nil }, nil

	case "oci":
		store, err := oci.NewRegistryStore(target, oci.RegistryOptions{
			CredentialConfigPath: registryConfig,
			Insecure:             insecure,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("open registry store %q: %w", target, err)
		}
		return store, func() error { return nil }, nil

	case "tar":
		tmpDir, err := os.MkdirTemp("", "varroactl-tar-*")
		if err != nil {
			return nil, nil, fmt.Errorf("create temp dir for tar: %w", err)
		}
		if err := extractTarGz(target, tmpDir); err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, nil, fmt.Errorf("extract tar.gz %q: %w", target, err)
		}
		store, err := oci.NewLayoutStore(tmpDir)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, nil, fmt.Errorf("open layout store from tar: %w", err)
		}
		finalize := func() error {
			return os.RemoveAll(tmpDir)
		}
		return store, finalize, nil

	case "uc":
		return nil, nil, errUCNotSupported

	default:
		return nil, nil, &ErrUnrecognizedScheme{Scheme: scheme}
	}
}

// createTarGz creates a .tar.gz archive from the contents of srcDir at dstPath.
func createTarGz(dstPath, srcDir string) error {
	f, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create tar.gz: %w", err)
	}
	defer func() { _ = f.Close() }()

	gw := gzip.NewWriter(f)
	defer func() { _ = gw.Close() }()

	tw := tar.NewWriter(gw)
	defer func() { _ = tw.Close() }()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Build the header name relative to srcDir.
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("rel path: %w", err)
		}
		if rel == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("tar header for %q: %w", path, err)
		}
		header.Name = rel

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write tar header %q: %w", rel, err)
		}

		if info.IsDir() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %q: %w", path, err)
		}
		defer func() { _ = src.Close() }()

		if _, err := io.Copy(tw, src); err != nil {
			return fmt.Errorf("copy %q: %w", path, err)
		}
		return nil
	})
}

// extractTarGz extracts a .tar.gz archive from srcPath into dstDir.
func extractTarGz(srcPath, dstDir string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open tar.gz: %w", err)
	}
	defer func() { _ = f.Close() }()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		// Clean the name to prevent path traversal.
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, ".."+string(os.PathSeparator)) || cleanName == ".." {
			return fmt.Errorf("tar entry escapes destination: %q", header.Name)
		}

		target := filepath.Join(dstDir, cleanName)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", target, err)
			}
			out, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create %q: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return fmt.Errorf("write %q: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %q: %w", target, err)
			}
		default:
			// Skip symlinks, devices, etc.
		}
	}
	return nil
}
