package oci

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBlob(t *testing.T, dir string, data []byte) string {
	t.Helper()
	digest, _, err := Sha256Digest(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(digest, "sha256:")
	if err := os.WriteFile(filepath.Join(blobDir, name), data, 0644); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestVerifyLayoutBlobs(t *testing.T) {
	dir := t.TempDir()
	d1 := writeBlob(t, dir, []byte("alpha"))
	d2 := writeBlob(t, dir, []byte("beta"))

	blobs, err := VerifyLayoutBlobs(dir)
	if err != nil {
		t.Fatalf("VerifyLayoutBlobs: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("got %d blobs, want 2", len(blobs))
	}
	got := map[string]int64{}
	for _, b := range blobs {
		got[b.Digest] = b.Size
	}
	if got[d1] != int64(len("alpha")) || got[d2] != int64(len("beta")) {
		t.Errorf("sizes wrong: %v", got)
	}
}

func TestVerifyLayoutBlobs_TamperedContentFails(t *testing.T) {
	dir := t.TempDir()
	d := writeBlob(t, dir, []byte("original"))
	name := strings.TrimPrefix(d, "sha256:")
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", name), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLayoutBlobs(dir); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Fatalf("expected altered-blob error, got %v", err)
	}
}

func TestVerifyLayoutBlobs_BadNameFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", "notes.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLayoutBlobs(dir); err == nil {
		t.Fatal("expected error for a non-digest blob name")
	}
}

func TestIsLayoutTagSafe(t *testing.T) {
	cases := map[string]bool{
		"plugin-pack":  true,
		"lts-2.479":    true,
		"":             false,
		"a/b":          false,
		"host:5000":    false,
		"x@sha256:abc": false,
		"with space":   false,
	}
	for tag, want := range cases {
		if got := IsLayoutTagSafe(tag); got != want {
			t.Errorf("IsLayoutTagSafe(%q) = %v, want %v", tag, got, want)
		}
	}
}
