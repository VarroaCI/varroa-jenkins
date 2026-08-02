package oci

import (
	"strings"
	"testing"
)

func TestSha256Digest(t *testing.T) {
	input := "hello world"
	expectedDigest := "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	expectedSize := int64(len(input))

	r := strings.NewReader(input)
	digest, size, err := Sha256Digest(r)
	if err != nil {
		t.Fatalf("Sha256Digest failed: %v", err)
	}
	if digest != expectedDigest {
		t.Errorf("digest = %q, want %q", digest, expectedDigest)
	}
	if size != expectedSize {
		t.Errorf("size = %d, want %d", size, expectedSize)
	}
}

func TestParseDigest_Valid(t *testing.T) {
	valid := []string{
		"sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	for _, s := range valid {
		if err := ParseDigest(s); err != nil {
			t.Errorf("ParseDigest(%q) = %v, want nil", s, err)
		}
	}
}

func TestParseDigest_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"sha256:",
		"sha256:abc", // too short
		"sha256:xyz", // not hex and too short
		"SHA256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",  // wrong prefix case
		"sha256:gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg",  // non-hex chars
		"sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde",   // 63 hex chars
		"sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde90", // 65 hex chars (extra)
		"sha-256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", // wrong separator
		"sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9 ", // trailing space
		" sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", // leading space
	}
	for _, s := range invalid {
		if err := ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) = nil, want error", s)
		}
	}
}
