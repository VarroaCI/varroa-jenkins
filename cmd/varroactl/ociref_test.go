package main

import (
	"errors"
	"testing"
)

func TestParseOCIDest_ValidSchemes(t *testing.T) {
	tests := []struct {
		input      string
		wantScheme string
		wantTarget string
	}{
		{"oci://example.com/repo:tag", "oci", "example.com/repo:tag"},
		{"oci://localhost:5000/ns/repo", "oci", "localhost:5000/ns/repo"},
		{"dir:///tmp/my-layout", "dir", "/tmp/my-layout"},
		{"dir://./relative/path", "dir", "./relative/path"},
		{"tar:///tmp/archive.tar.gz", "tar", "/tmp/archive.tar.gz"},
		{"tar://out.tar.gz", "tar", "out.tar.gz"},
		{"uc://update-center.example.com", "uc", "update-center.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			scheme, target, err := ParseOCIDest(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if scheme != tt.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tt.wantScheme)
			}
			if target != tt.wantTarget {
				t.Errorf("target = %q, want %q", target, tt.wantTarget)
			}
		})
	}
}

func TestParseOCIDest_InvalidScheme(t *testing.T) {
	_, _, err := ParseOCIDest("ftp://example.com")
	if err == nil {
		t.Fatal("expected error for ftp://, got nil")
	}

	var ue *ErrUnrecognizedScheme
	if !errors.As(err, &ue) {
		t.Fatalf("expected *ErrUnrecognizedScheme, got %T: %v", err, err)
	}
	if ue.Scheme != "ftp" {
		t.Errorf("Scheme = %q, want %q", ue.Scheme, "ftp")
	}
}

func TestParseOCIDest_NoScheme(t *testing.T) {
	_, _, err := ParseOCIDest("just/a/path")
	if err == nil {
		t.Fatal("expected error for no-scheme input, got nil")
	}

	var ue *ErrUnrecognizedScheme
	if !errors.As(err, &ue) {
		t.Fatalf("expected *ErrUnrecognizedScheme, got %T: %v", err, err)
	}
}

func TestParseOCIDest_UCDistinguishableFromUnrecognized(t *testing.T) {
	// uc:// is recognized syntactically; the "unsupported" error comes from
	// openOCIDest, not from ParseOCIDest.
	scheme, target, err := ParseOCIDest("uc://some-target")
	if err != nil {
		t.Fatalf("uc:// should parse successfully, got: %v", err)
	}
	if scheme != "uc" {
		t.Errorf("scheme = %q, want %q", scheme, "uc")
	}
	if target != "some-target" {
		t.Errorf("target = %q, want %q", target, "some-target")
	}

	// ftp:// returns *ErrUnrecognizedScheme, not something else.
	_, _, err = ParseOCIDest("ftp://x")
	var ue *ErrUnrecognizedScheme
	if !errors.As(err, &ue) {
		t.Fatalf("ftp:// should return *ErrUnrecognizedScheme, got %T: %v", err, err)
	}
}
