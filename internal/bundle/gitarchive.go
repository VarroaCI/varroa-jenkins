package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// archiveCommit writes the tree at sha from the bare store into targetDir.
// It streams `git archive --format=tar` and extracts the stream with
// extractTar. The target receives the files of the tree and no .git
// directory. targetDir must not exist or must be empty. On failure the
// target is left as it was before the call.
func archiveCommit(ctx context.Context, bare, sha, targetDir string) error {
	created, err := prepareEmptyTarget(targetDir)
	if err != nil {
		return err
	}

	archCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var stderr bytes.Buffer
	cmd := gitCommand(archCtx, "-C", bare, "archive", "--format=tar", sha)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanupTarget(targetDir, created)
		return fmt.Errorf("git archive %s: %w", sha, err)
	}
	if err := cmd.Start(); err != nil {
		cleanupTarget(targetDir, created)
		return fmt.Errorf("git archive %s: %w", sha, err)
	}

	if extractErr := extractTar(stdout, targetDir); extractErr != nil {
		// Cancel kills git's process group. That also unblocks a git that
		// is still writing to the pipe, so Wait does not hang.
		cancel()
		_ = cmd.Wait()
		cleanupTarget(targetDir, created)
		if ctx.Err() != nil {
			return fmt.Errorf("extract git archive %s: %w", sha, ctx.Err())
		}
		return fmt.Errorf("extract git archive %s: %w", sha, extractErr)
	}

	// Drain git's trailing padding before Wait closes the pipe.
	_, _ = io.Copy(io.Discard, stdout)
	if waitErr := cmd.Wait(); waitErr != nil {
		cleanupTarget(targetDir, created)
		if ctx.Err() != nil {
			return fmt.Errorf("git archive %s: %w", sha, ctx.Err())
		}
		return fmt.Errorf("git archive %s: %s", sha, stderr.String())
	}
	return nil
}

// prepareEmptyTarget creates targetDir when it does not exist. It returns
// created=true in that case, so cleanup can remove the whole directory. An
// existing targetDir must be empty; a non-empty one is an error.
func prepareEmptyTarget(targetDir string) (created bool, err error) {
	info, statErr := os.Stat(targetDir)
	switch {
	case os.IsNotExist(statErr):
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return false, fmt.Errorf("create target %s: %w", targetDir, err)
		}
		return true, nil
	case statErr != nil:
		return false, fmt.Errorf("stat target %s: %w", targetDir, statErr)
	case !info.IsDir():
		return false, fmt.Errorf("target %s is not a directory", targetDir)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return false, fmt.Errorf("read target %s: %w", targetDir, err)
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("target %s is not empty", targetDir)
	}
	return false, nil
}

// cleanupTarget removes what a failed archiveCommit wrote. When the
// directory was created by the call, it removes the directory. Otherwise it
// removes each child and keeps the directory.
func cleanupTarget(dir string, created bool) {
	if created {
		_ = os.RemoveAll(dir)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// extractTar reads a tar stream and writes its entries below targetDir. All
// writes go through an os.Root, so no entry can land outside targetDir. It
// accepts directories, regular files, and symlinks that stay inside
// targetDir. It rejects any other entry type, a non-local name, a duplicate
// file, and an entry below a symlink.
func extractTar(r io.Reader, targetDir string) error {
	root, err := os.OpenRoot(targetDir)
	if err != nil {
		return fmt.Errorf("open target %s: %w", targetDir, err)
	}
	defer func() { _ = root.Close() }()

	symlinks := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if err := validateRelPath(name); err != nil {
			return fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}
		if ancestorIsSymlink(name, symlinks) {
			return fmt.Errorf("tar entry %q is below a symlink", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := writeRegular(root, name, hdr, tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !symlinkStaysInside(name, hdr.Linkname) {
				return fmt.Errorf("tar entry %q: symlink target %q leaves the bundle directory", hdr.Name, hdr.Linkname)
			}
			if err := mkdirParent(root, name); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", hdr.Name, err)
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("symlink %q: %w", hdr.Name, err)
			}
			symlinks[name] = true
		default:
			return fmt.Errorf("unsupported tar entry %q type %q", hdr.Name, hdr.Typeflag)
		}
	}

	// A lexical check cannot see a chain of links: each link may stay inside
	// on its own while a link on its path leads out. Resolve every symlink
	// through the root, which refuses to follow a link that leaves it.
	// A dangling link inside the root is not an escape.
	for name := range symlinks {
		_, err := root.Stat(name)
		switch {
		case err == nil, os.IsNotExist(err):
			// Inside the root, or dangling inside it.
		case errors.Is(err, syscall.ELOOP):
			// A cycle stays inside the root. git checkout writes it too.
		default:
			return fmt.Errorf("symlink %q resolves outside the bundle directory: %w", name, err)
		}
	}
	return nil
}

// writeRegular writes one regular file. O_EXCL refuses a duplicate entry
// and never follows a symlink that already exists at the name.
func writeRegular(root *os.Root, name string, hdr *tar.Header, body io.Reader) error {
	if err := mkdirParent(root, name); err != nil {
		return fmt.Errorf("mkdir parent of %q: %w", hdr.Name, err)
	}
	perm := os.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", hdr.Name, err)
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %q: %w", hdr.Name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", hdr.Name, err)
	}
	if err := root.Chtimes(name, hdr.ModTime, hdr.ModTime); err != nil {
		return fmt.Errorf("set mtime %q: %w", hdr.Name, err)
	}
	return nil
}

// mkdirParent creates the parent directory of name inside root, if any.
func mkdirParent(root *os.Root, name string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	return root.MkdirAll(parent, 0o755)
}

// ancestorIsSymlink reports whether any parent of name is a symlink entry.
func ancestorIsSymlink(name string, symlinks map[string]bool) bool {
	for dir := filepath.Dir(name); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if symlinks[dir] {
			return true
		}
	}
	return false
}

// symlinkStaysInside reports whether a symlink at name with the given target
// resolves lexically to a path inside the target directory. An absolute
// target never stays inside.
func symlinkStaysInside(name, linkname string) bool {
	if linkname == "" || filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") {
		return false
	}
	resolved := filepath.Join(filepath.Dir(name), filepath.FromSlash(linkname))
	return filepath.IsLocal(resolved)
}
