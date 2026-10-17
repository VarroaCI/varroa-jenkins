package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// BlobInfo describes one blob file under an OCI layout's blobs/sha256 directory.
type BlobInfo struct {
	Digest string // "sha256:<hex>"
	Size   int64
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// tagRe is the OCI tag grammar.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// repoPathRe is the OCI repository-path grammar: lowercase components joined by
// separators. It has no host, no tag, and no digest.
var repoPathRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// VerifyLayoutBlobs hashes every file under <dir>/blobs/sha256. Each file name
// must equal the SHA-256 of its content, and each entry must be a regular file.
// It makes no network call. It returns the verified blobs sorted by digest.
// Any mismatch, symbolic link, or sub-directory is an error.
func VerifyLayoutBlobs(dir string) ([]BlobInfo, error) {
	blobDir := filepath.Join(dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		return nil, fmt.Errorf("read blob directory: %w", err)
	}

	blobs := make([]BlobInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !sha256HexRe.MatchString(name) {
			return nil, fmt.Errorf("blob %q is not named by a sha256 digest", name)
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("blob %q is not a regular file", name)
		}
		f, err := os.Open(filepath.Join(blobDir, name))
		if err != nil {
			return nil, fmt.Errorf("open blob %q: %w", name, err)
		}
		digest, size, err := Sha256Digest(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("hash blob %q: %w", name, err)
		}
		if digest != "sha256:"+name {
			return nil, fmt.Errorf("blob %q has content digest %s: the file is altered", name, digest)
		}
		blobs = append(blobs, BlobInfo{Digest: digest, Size: size})
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Digest < blobs[j].Digest })
	return blobs, nil
}

// IsLayoutTagSafe reports whether a name is a valid OCI tag. A layout tag, a
// profile name, a chart version, and an image tag all pass through it. The
// grammar is the OCI tag grammar, so a name pushes unchanged to a registry.
func IsLayoutTagSafe(tag string) bool {
	return tagRe.MatchString(tag)
}

// IsValidRepoPath reports whether p is a valid OCI repository path, with no
// host, tag, or digest.
func IsValidRepoPath(p string) bool {
	return len(p) <= 255 && repoPathRe.MatchString(p)
}
