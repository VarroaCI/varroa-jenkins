package main

import (
	"testing"
)

func TestOpenOCIDest_UCNotSupported(t *testing.T) {
	// Verify that uc:// returns errUCNotSupported with NO I/O.
	// We don't create any dirs or make any network calls — just check the error.
	_, _, err := openOCIDest("uc", "some-target", "", false)
	if err != errUCNotSupported {
		t.Fatalf("expected errUCNotSupported, got %v", err)
	}
}

func TestOpenOCISrc_UCNotSupported(t *testing.T) {
	_, _, err := openOCISrc("uc", "some-target", "", false)
	if err != errUCNotSupported {
		t.Fatalf("expected errUCNotSupported, got %v", err)
	}
}

func TestOpenOCIDest_DirScheme(t *testing.T) {
	tmpDir := t.TempDir()
	layoutPath := tmpDir + "/layout"

	store, finalize, err := openOCIDest("dir", layoutPath, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	if err := finalize(); err != nil {
		t.Fatalf("finalize error: %v", err)
	}
}

func TestOpenOCIDest_UnrecognizedScheme(t *testing.T) {
	_, _, err := openOCIDest("ftp", "target", "", false)
	if err == nil {
		t.Fatal("expected error")
	}
	ue, ok := err.(*ErrUnrecognizedScheme)
	if !ok {
		t.Fatalf("expected *ErrUnrecognizedScheme, got %T: %v", err, err)
	}
	if ue.Scheme != "ftp" {
		t.Errorf("scheme = %q, want %q", ue.Scheme, "ftp")
	}
}
