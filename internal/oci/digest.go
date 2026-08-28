package oci

import (
	"crypto/sha256" // register sha256 for go-digest
	"fmt"
	"io"
	"regexp"
)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Sha256Digest streams r through SHA-256 and returns the digest string ("sha256:<hex>")
// and the total number of bytes read.
func Sha256Digest(r io.Reader) (digest string, size int64, err error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), n, nil
}

// ParseDigest validates that s is a valid digest string of the form "sha256:<64 hex chars>".
func ParseDigest(s string) error {
	if !digestRe.MatchString(s) {
		return fmt.Errorf("invalid digest format: %q", s)
	}
	return nil
}
