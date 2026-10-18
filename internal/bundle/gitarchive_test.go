package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTar writes the given headers and bodies into an in-memory tar stream.
func buildTar(t *testing.T, entries ...tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.hdr.Typeflag == tar.TypeReg {
			e.hdr.Size = int64(len(e.body))
		}
		must(t, tw.WriteHeader(&e.hdr))
		if e.hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			must(t, err)
		}
	}
	must(t, tw.Close())
	return &buf
}

type tarEntry struct {
	hdr  tar.Header
	body string
}

func TestExtractTar_WritesTreeModesAndLinks(t *testing.T) {
	modTime := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	buf := buildTar(t,
		tarEntry{hdr: tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": "abc"}}},
		tarEntry{hdr: tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o775}},
		tarEntry{hdr: tar.Header{Name: "d/e/", Typeflag: tar.TypeDir, Mode: 0o775}},
		tarEntry{hdr: tar.Header{Name: "d/e/f.yaml", Typeflag: tar.TypeReg, Mode: 0o664, ModTime: modTime}, body: "key: value\n"},
		tarEntry{hdr: tar.Header{Name: "d/e/run.sh", Typeflag: tar.TypeReg, Mode: 0o775, ModTime: modTime}, body: "#!/bin/sh\n"},
		tarEntry{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "d/e/f.yaml"}},
		tarEntry{hdr: tar.Header{Name: "chain", Typeflag: tar.TypeSymlink, Linkname: "link"}},
		tarEntry{hdr: tar.Header{Name: "dangling", Typeflag: tar.TypeSymlink, Linkname: "d/e/missing.yaml"}},
		tarEntry{hdr: tar.Header{Name: "loop_a", Typeflag: tar.TypeSymlink, Linkname: "loop_b"}},
		tarEntry{hdr: tar.Header{Name: "loop_b", Typeflag: tar.TypeSymlink, Linkname: "loop_a"}},
		tarEntry{hdr: tar.Header{Name: "empty/", Typeflag: tar.TypeDir, Mode: 0o775}},
	)

	target := t.TempDir()
	if err := extractTar(buf, target); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(target, "d", "e", "f.yaml"))
	must(t, err)
	if string(data) != "key: value\n" {
		t.Errorf("f.yaml content = %q", data)
	}

	info, err := os.Stat(filepath.Join(target, "d", "e", "run.sh"))
	must(t, err)
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("run.sh mode = %o, want 755", got)
	}
	if !info.ModTime().Equal(modTime) {
		t.Errorf("run.sh mtime = %v, want %v", info.ModTime(), modTime)
	}

	link, err := os.ReadFile(filepath.Join(target, "link"))
	must(t, err)
	if string(link) != "key: value\n" {
		t.Errorf("read through link = %q", link)
	}

	if st, err := os.Stat(filepath.Join(target, "empty")); err != nil || !st.IsDir() {
		t.Errorf("empty dir missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "pax_global_header")); !os.IsNotExist(err) {
		t.Errorf("global header written as a file: %v", err)
	}
}

func TestExtractTar_RejectsUnsafeEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"absolute path", []tarEntry{{hdr: tar.Header{Name: "/abs/file", Typeflag: tar.TypeReg}, body: "x"}}},
		{"parent escape", []tarEntry{{hdr: tar.Header{Name: "../escape", Typeflag: tar.TypeReg}, body: "x"}}},
		{"nested parent escape", []tarEntry{{hdr: tar.Header{Name: "a/../../escape", Typeflag: tar.TypeReg}, body: "x"}}},
		{"relative symlink escape", []tarEntry{{hdr: tar.Header{Name: "bad", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"}}}},
		{"absolute symlink", []tarEntry{{hdr: tar.Header{Name: "abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}}},
		{"symlink chain escape", []tarEntry{
			{hdr: tar.Header{Name: "deep/", Typeflag: tar.TypeDir}},
			{hdr: tar.Header{Name: "deep/link", Typeflag: tar.TypeSymlink, Linkname: ".."}},
			{hdr: tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: "deep/link/../etc"}},
		}},
		{"write below dir symlink", []tarEntry{
			{hdr: tar.Header{Name: "d", Typeflag: tar.TypeSymlink, Linkname: "."}},
			{hdr: tar.Header{Name: "d/x", Typeflag: tar.TypeReg}, body: "x"},
		}},
		{"duplicate file", []tarEntry{
			{hdr: tar.Header{Name: "dup.txt", Typeflag: tar.TypeReg}, body: "one"},
			{hdr: tar.Header{Name: "dup.txt", Typeflag: tar.TypeReg}, body: "two"},
		}},
		{"hard link", []tarEntry{
			{hdr: tar.Header{Name: "f", Typeflag: tar.TypeReg}, body: "x"},
			{hdr: tar.Header{Name: "hl", Typeflag: tar.TypeLink, Linkname: "f"}},
		}},
		{"device", []tarEntry{{hdr: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "target")
			must(t, os.MkdirAll(target, 0o755))

			if err := extractTar(buildTar(t, tc.entries...), target); err == nil {
				t.Fatal("extractTar succeeded, want error")
			}

			// The only entry in the parent must be the target itself.
			entries, err := os.ReadDir(parent)
			must(t, err)
			if len(entries) != 1 || entries[0].Name() != "target" {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("files outside target: %v", names)
			}
		})
	}
}

func TestArchiveCommit_CleansTargetOnFailure(t *testing.T) {
	bareURL, _ := newBareFixture(t)
	bare := strings.TrimPrefix(bareURL, "file://")
	missing := strings.Repeat("d", 40)

	t.Run("existing target stays empty", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "out")
		must(t, os.MkdirAll(target, 0o755))

		if err := archiveCommit(context.Background(), bare, missing, target); err == nil {
			t.Fatal("archiveCommit with unknown sha succeeded, want error")
		}
		entries, err := os.ReadDir(target)
		must(t, err)
		if len(entries) != 0 {
			t.Errorf("target has %d entries after failure, want 0", len(entries))
		}
	})

	t.Run("created target is removed", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "out")

		if err := archiveCommit(context.Background(), bare, missing, target); err == nil {
			t.Fatal("archiveCommit with unknown sha succeeded, want error")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("target still exists after failure: %v", err)
		}
	})
}

func TestArchiveCommit_RejectsNonEmptyTarget(t *testing.T) {
	bareURL, commit := newBareFixture(t)
	bare := strings.TrimPrefix(bareURL, "file://")
	sha := commit("a.txt", "a")

	target := t.TempDir()
	keep := filepath.Join(target, "keep.txt")
	must(t, os.WriteFile(keep, []byte("keep"), 0o644))

	if err := archiveCommit(context.Background(), bare, sha, target); err == nil {
		t.Fatal("archiveCommit into non-empty target succeeded, want error")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("existing file removed: %v", err)
	}
}
