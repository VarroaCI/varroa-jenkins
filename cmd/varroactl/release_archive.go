package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// maxReleaseManifestBytes bounds the manifest read from an archive. The manifest
// is read into memory before its digest is checked.
const maxReleaseManifestBytes = 16 << 20

// maxLayoutFileBytes bounds the two layout files, index.json and oci-layout.
const maxLayoutFileBytes = 16 << 20

// createReleaseTarGz writes a release bundle as a gzipped tar. The manifest is
// the first entry, so an import checks it before it reads any other entry.
func createReleaseTarGz(dstPath, srcDir string) (err error) {
	f, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := addReleaseEntry(tw, srcDir, releaseManifestFile); err != nil {
		return err
	}
	walkErr := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == releaseManifestFile {
			return nil
		}
		return addReleaseEntry(tw, srcDir, rel)
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// addReleaseEntry writes one file or directory of the bundle to the archive.
func addReleaseEntry(tw *tar.Writer, srcDir, rel string) error {
	path := filepath.Join(srcDir, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeDir, Mode: 0755})
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0644, Size: info.Size()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, in)
	return err
}

// extractReleaseTar unpacks a release transfer archive into dst. The archive is
// untrusted, so the extraction checks each entry as it reads it:
//   - The first entry must be the manifest. Its digest must equal manifestDigest.
//   - Every later entry must be a layout file or a blob that the manifest lists.
//     A blob entry must have the size that the manifest records, and its bytes
//     must hash to its name. A mismatch stops the extraction at that entry.
//   - Each name appears once, and every blob the manifest lists must appear.
func extractReleaseTar(archivePath, dst, manifestDigest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	first, err := tr.Next()
	if err == io.EOF {
		return fmt.Errorf("archive is empty")
	}
	if err != nil {
		return fmt.Errorf("read archive entry: %w", err)
	}
	if first.Name != releaseManifestFile || first.Typeflag != tar.TypeReg {
		return fmt.Errorf("the first archive entry must be %s, got %q", releaseManifestFile, first.Name)
	}
	if first.Size > maxReleaseManifestBytes {
		return fmt.Errorf("archive manifest is %d bytes, more than the %d-byte limit", first.Size, maxReleaseManifestBytes)
	}
	raw := make([]byte, 0, first.Size)
	buf := bytes.NewBuffer(raw)
	if n, err := io.Copy(buf, io.LimitReader(tr, first.Size)); err != nil || n != first.Size {
		return fmt.Errorf("archive manifest is truncated")
	}
	raw = buf.Bytes()
	got, _, err := oci.Sha256Digest(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if got != manifestDigest {
		return fmt.Errorf("archive manifest digest is %s, want %s: the archive is not the bundle the release published", got, manifestDigest)
	}
	if err := os.WriteFile(filepath.Join(dst, releaseManifestFile), raw, 0644); err != nil {
		return err
	}
	_, m, err := readReleaseManifest(dst)
	if err != nil {
		return err
	}
	listed := map[string]int64{}
	for _, b := range m.Blobs {
		if err := oci.ParseDigest(b.Digest); err != nil {
			return fmt.Errorf("manifest blob: %w", err)
		}
		listed[strings.TrimPrefix(b.Digest, "sha256:")] = b.Size
	}

	seen := map[string]bool{releaseManifestFile: true}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}
		name := hdr.Name
		if seen[name] {
			return fmt.Errorf("archive repeats entry %q", name)
		}
		seen[name] = true
		target := filepath.Join(dst, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if !releaseDirAllowed(name) {
				return fmt.Errorf("archive entry %q is not part of a release bundle", name)
			}
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("create %q: %w", name, err)
			}
		case tar.TypeReg:
			if err := extractReleaseFile(tr, hdr, target, listed); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has a type that a release bundle never holds", name)
		}
	}

	for _, need := range []string{"oci-layout", "index.json"} {
		if !seen[need] {
			return fmt.Errorf("archive is missing %s", need)
		}
	}
	missing := make([]string, 0)
	for hex := range listed {
		if !seen[blobPrefix+hex] {
			missing = append(missing, hex)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("archive is missing blob sha256:%s", missing[0])
	}
	return nil
}

const blobPrefix = "blobs/sha256/"

// releaseDirAllowed reports whether a directory entry belongs to the layout.
func releaseDirAllowed(name string) bool {
	return name == "blobs" || name == "blobs/sha256"
}

// extractReleaseFile writes one regular file entry, after the entry passes the
// layout rules.
func extractReleaseFile(tr *tar.Reader, hdr *tar.Header, target string, listed map[string]int64) error {
	name := hdr.Name
	switch name {
	case "oci-layout", "index.json":
		if hdr.Size > maxLayoutFileBytes {
			return fmt.Errorf("archive entry %q is %d bytes, more than the %d-byte limit", name, hdr.Size, maxLayoutFileBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return writeArchiveEntry(target, tr, hdr.Size)
	}
	if !strings.HasPrefix(name, blobPrefix) {
		return fmt.Errorf("archive entry %q is not part of a release bundle", name)
	}
	hex := strings.TrimPrefix(name, blobPrefix)
	if err := oci.ParseDigest("sha256:" + hex); err != nil {
		return fmt.Errorf("archive entry %q is not part of a release bundle", name)
	}
	size, ok := listed[hex]
	if !ok {
		return fmt.Errorf("archive blob sha256:%s is not listed by the manifest", hex)
	}
	if hdr.Size != size {
		return fmt.Errorf("archive blob sha256:%s is %d bytes, the manifest records %d", hex, hdr.Size, size)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return writeHashedBlob(target, tr, size, "sha256:"+hex)
}

// writeHashedBlob writes exactly size bytes to a new file at target, hashing
// them as they are written. A digest other than want is an error.
func writeHashedBlob(target string, r io.Reader, size int64, want string) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("blob %s: wrote %d of %d bytes", want, n, size)
	}
	if got := fmt.Sprintf("sha256:%x", h.Sum(nil)); got != want {
		return fmt.Errorf("blob bytes hash to %s, not %s", got, want)
	}
	return nil
}

// writeArchiveEntry writes exactly size bytes of r to a new file at target.
func writeArchiveEntry(target string, r io.Reader, size int64) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("wrote %d of %d bytes", n, size)
	}
	return nil
}

// checkProfileNames refuses a profile name that cannot name a pack. It refuses a
// name that repeats, and a name that the release layout reserves: the manifest
// tag, and the prefixes of the chart and image tags.
func checkProfileNames(profiles []string) error {
	seen := map[string]bool{}
	for _, p := range profiles {
		if !oci.IsLayoutTagSafe(p) {
			return fmt.Errorf("profile name %q cannot name a pack: use letters, digits, '.', '_', and '-', with no leading '-' or '.'", p)
		}
		if p == releaseManifestTag || strings.HasPrefix(p, "chart-") || strings.HasPrefix(p, "img-") {
			return fmt.Errorf("profile name %q is reserved for a release artifact", p)
		}
		if seen[p] {
			return fmt.Errorf("profile %q is given more than once", p)
		}
		seen[p] = true
	}
	return nil
}

// checkUniqueLayoutTags refuses a bundle in which two artifacts share a layout
// tag. A shared tag would drop one artifact silently.
func checkUniqueLayoutTags(m releaseManifest) error {
	tags := make([]string, 0, 1+len(m.Images)+len(m.PluginPacks))
	tags = append(tags, m.Chart.LayoutTag)
	for _, img := range m.Images {
		tags = append(tags, img.LayoutTag)
	}
	for _, p := range m.PluginPacks {
		tags = append(tags, p.LayoutTag)
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if seen[t] {
			return fmt.Errorf("two artifacts share layout tag %q", t)
		}
		seen[t] = true
	}
	return nil
}

// checkDestinationNames checks every name that the import push computes, and
// every recorded digest, before anything is pushed.
func checkDestinationNames(m releaseManifest) error {
	if !oci.IsValidRepoPath(m.Chart.Name) {
		return fmt.Errorf("chart name %q is not a valid OCI repository name", m.Chart.Name)
	}
	if !oci.IsLayoutTagSafe(sanitizeTag(m.Chart.Version)) {
		return fmt.Errorf("chart version %q is not a valid OCI tag", m.Chart.Version)
	}
	if err := oci.ParseDigest(m.Chart.Digest); err != nil {
		return fmt.Errorf("chart digest: %w", err)
	}
	for _, img := range m.Images {
		ref, err := parseImageRef(img.Source)
		if err != nil {
			return err
		}
		if !oci.IsValidRepoPath(ref.Path) {
			return fmt.Errorf("image %s: repository path %q is not a valid OCI name", img.Source, ref.Path)
		}
		if !oci.IsLayoutTagSafe(ref.destTag()) {
			return fmt.Errorf("image %s: destination tag %q is not a valid OCI tag", img.Source, ref.destTag())
		}
		if err := oci.ParseDigest(img.Digest); err != nil {
			return fmt.Errorf("image %s digest: %w", img.Source, err)
		}
	}
	for _, p := range m.PluginPacks {
		if !oci.IsLayoutTagSafe(p.Profile) {
			return fmt.Errorf("plugin pack profile %q is not a valid OCI tag", p.Profile)
		}
		if p.LayoutTag != p.Profile {
			return fmt.Errorf("plugin pack %q names layout tag %q: a pack's layout tag is its profile", p.Profile, p.LayoutTag)
		}
		if err := oci.ParseDigest(p.Digest); err != nil {
			return fmt.Errorf("plugin pack %q digest: %w", p.Profile, err)
		}
	}
	return nil
}

// copyInto copies the file at src into out and syncs out before it returns.
func copyInto(src string, out *os.File) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// copyFile copies src to a new file at dst. It fails when dst exists.
func copyFile(src, dst string) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if err := copyInto(src, out); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies a directory of regular files into dst, which must exist.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return copyFile(path, target)
	})
}

// writeArchiveAtomic places a finished archive at target. The copy goes to
// target.partial, which is renamed to target only after its data is synced. An
// existing partial file is refused, and this call removes a partial file only
// when it created the file itself.
func writeArchiveAtomic(src, target string) error {
	partial := target + ".partial"
	if _, err := os.Lstat(partial); err == nil {
		return fmt.Errorf("%s exists: remove it and retry", partial)
	}
	out, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create %s: %w", partial, err)
	}
	if err := copyInto(src, out); err != nil {
		_ = out.Close()
		_ = os.Remove(partial)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(partial)
		return err
	}
	if err := os.Rename(partial, target); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}

// writeDirAtomic places a finished layout directory at target. The tree is
// copied into target.partial, which is renamed to target only when the copy has
// succeeded. Missing parent directories are created. A failure removes the
// partial tree.
func writeDirAtomic(srcDir, target string) error {
	if err := checkEmptyDirTarget(target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("create parent of %s: %w", target, err)
	}
	partial := target + ".partial"
	if err := os.Mkdir(partial, 0755); err != nil {
		return fmt.Errorf("create %s: %w", partial, err)
	}
	if err := copyTree(srcDir, partial); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		// An empty directory stands in the way: remove it, then rename into place.
		if err := os.Remove(target); err != nil {
			_ = os.RemoveAll(partial)
			return fmt.Errorf("replace empty destination %s: %w", target, err)
		}
	}
	if err := os.Rename(partial, target); err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	return nil
}

// checkEmptyDirTarget accepts a missing directory or an empty one. It refuses a
// stale partial sibling, so an earlier failed write is never mixed in.
func checkEmptyDirTarget(target string) error {
	if _, err := os.Lstat(target + ".partial"); err == nil {
		return fmt.Errorf("%s.partial exists: remove it and retry", target)
	}
	info, err := os.Stat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("destination %s exists and is not a directory", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("destination %s is not empty: choose an empty directory", target)
	}
	return nil
}
