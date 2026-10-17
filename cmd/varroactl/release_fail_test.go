package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// rewriteManifest applies change to the manifest of a fixture bundle and writes
// it back, so that the manifest digest matches the new content.
func rewriteManifest(t *testing.T, dir string, change func(*releaseManifest)) string {
	t.Helper()
	_, m, err := readReleaseManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	change(&m)
	digest, err := writeReleaseManifest(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

const otherDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

func TestImportRelease_ArtifactTagResolvesToOtherDigestFails(t *testing.T) {
	dir, _ := buildReleaseFixture(t)
	digest := rewriteManifest(t, dir, func(m *releaseManifest) {
		m.Images[0].Digest = otherDigest
	})
	_, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
	if err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("a tag that resolves to another digest must fail, got %v", err)
	}
}

func TestImportRelease_UnsafeLayoutTagOrProfileFails(t *testing.T) {
	cases := []struct {
		name   string
		change func(*releaseManifest)
		want   string
	}{
		{
			name: "profile with plus sign",
			change: func(m *releaseManifest) {
				m.PluginPacks[0].Profile = "lts+2.479"
				m.PluginPacks[0].LayoutTag = "lts+2.479"
			},
			want: "is not a valid OCI tag",
		},
		{
			name: "image layout tag with traversal",
			change: func(m *releaseManifest) {
				m.Images[0].LayoutTag = "../escape"
			},
			want: "unsafe layout tag",
		},
		{
			name: "pack layout tag names another profile",
			change: func(m *releaseManifest) {
				m.PluginPacks[0].LayoutTag = "other"
			},
			want: "a pack's layout tag is its profile",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := buildReleaseFixture(t)
			digest := rewriteManifest(t, dir, c.change)
			_, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestImportRelease_ReleaseLevelSymlinkOrSubdirectoryFails(t *testing.T) {
	name := strings.Repeat("a", 64)
	t.Run("symlink", func(t *testing.T) {
		dir, digest := buildReleaseFixture(t)
		blobs, err := oci.VerifyLayoutBlobs(dir)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(blobs[0].Digest, "sha256:"))
		if err := os.Symlink(target, filepath.Join(dir, "blobs", "sha256", name)); err != nil {
			t.Fatal(err)
		}
		_, err = verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a symlink under blobs/sha256 must fail, got %v", err)
		}
	})
	t.Run("subdirectory", func(t *testing.T) {
		dir, digest := buildReleaseFixture(t)
		if err := os.Mkdir(filepath.Join(dir, "blobs", "sha256", name), 0755); err != nil {
			t.Fatal(err)
		}
		_, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a sub-directory under blobs/sha256 must fail, got %v", err)
		}
	})
}

// tarEntry describes one entry of a hand-built test archive.
type tarEntry struct {
	name string
	typ  byte
	body string
	link string
}

func writeTestArchive(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0644, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// fixtureManifestEntry returns the manifest of a fixture bundle as the first
// archive entry, with the parsed manifest.
func fixtureManifestEntry(t *testing.T, dir string) (tarEntry, releaseManifest) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, releaseManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	_, m, err := readReleaseManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tarEntry{name: releaseManifestFile, typ: tar.TypeReg, body: string(raw)}, m
}

func TestExtractReleaseTar_AcceptsReleaseLayoutWithManifestFirst(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := createReleaseTarGz(archive, dir); err != nil {
		t.Fatal(err)
	}

	// The round trip: the first entry is the manifest.
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != releaseManifestFile {
		t.Errorf("first archive entry = %q, want %s", hdr.Name, releaseManifestFile)
	}
	_ = f.Close()

	dst := t.TempDir()
	if err := extractReleaseTar(archive, dst, digest); err != nil {
		t.Fatalf("a release bundle must extract: %v", err)
	}
	if _, err := verifyReleaseBundle(dst, digest, "registry.example.com/varroa"); err != nil {
		t.Errorf("extracted bundle does not verify: %v", err)
	}
}

func TestExtractReleaseTar_RejectsUnsafeEntries(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	manifest, m := fixtureManifestEntry(t, dir)
	blobHex := strings.TrimPrefix(m.Blobs[0].Digest, "sha256:")
	blobSize := m.Blobs[0].Size
	unlisted := "blobs/sha256/" + strings.Repeat("c", 64)

	cases := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{"path traversal", []tarEntry{{name: "../escape.txt", typ: tar.TypeReg, body: "x"}}, "not part of a release bundle"},
		{"absolute path", []tarEntry{{name: "/etc/escape.txt", typ: tar.TypeReg, body: "x"}}, "not part of a release bundle"},
		{"symlink", []tarEntry{{name: "index.json", typ: tar.TypeSymlink, link: "/etc/passwd"}}, "has a type that a release bundle never holds"},
		{"duplicate entry", []tarEntry{{name: "oci-layout", typ: tar.TypeReg, body: "{}"}, {name: "oci-layout", typ: tar.TypeReg, body: "{}"}}, "repeats entry"},
		{"unexpected entry", []tarEntry{{name: "extra.txt", typ: tar.TypeReg, body: "x"}}, "not part of a release bundle"},
		{"blob name that is not a digest", []tarEntry{{name: "blobs/sha256/notes.txt", typ: tar.TypeReg, body: "x"}}, "not part of a release bundle"},
		{"unexpected directory", []tarEntry{{name: "other", typ: tar.TypeDir}}, "not part of a release bundle"},
		{"blob name with traversal", []tarEntry{{name: blobPrefix + blobHex + "/../../escape", typ: tar.TypeReg, body: "x"}}, "not part of a release bundle"},
		{"blob the manifest does not list", []tarEntry{{name: unlisted, typ: tar.TypeReg, body: "x"}}, "is not listed by the manifest"},
		{"blob larger than recorded", []tarEntry{{name: blobPrefix + blobHex, typ: tar.TypeReg, body: strings.Repeat("x", int(blobSize)+1)}}, "the manifest records"},
		{"blob bytes do not hash to the name", []tarEntry{{name: blobPrefix + blobHex, typ: tar.TypeReg, body: strings.Repeat("x", int(blobSize))}}, "hash to"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "bad.tar.gz")
			writeTestArchive(t, archive, append([]tarEntry{manifest}, c.entries...))
			err := extractReleaseTar(archive, t.TempDir(), digest)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestExtractReleaseTar_ManifestMustBeFirstAndMatch(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	manifest, _ := fixtureManifestEntry(t, dir)

	t.Run("first entry is not the manifest", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "bad.tar.gz")
		writeTestArchive(t, archive, []tarEntry{{name: "index.json", typ: tar.TypeReg, body: "{}"}, manifest})
		err := extractReleaseTar(archive, t.TempDir(), digest)
		if err == nil || !strings.Contains(err.Error(), "the first archive entry must be") {
			t.Fatalf("want a first-entry refusal, got %v", err)
		}
	})
	t.Run("manifest digest mismatch", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "bad.tar.gz")
		writeTestArchive(t, archive, []tarEntry{manifest})
		err := extractReleaseTar(archive, t.TempDir(), otherDigest)
		if err == nil || !strings.Contains(err.Error(), "not the bundle the release published") {
			t.Fatalf("want a digest refusal, got %v", err)
		}
	})
	t.Run("missing blob", func(t *testing.T) {
		archive := filepath.Join(t.TempDir(), "short.tar.gz")
		writeTestArchive(t, archive, []tarEntry{manifest})
		err := extractReleaseTar(archive, t.TempDir(), digest)
		if err == nil || !strings.Contains(err.Error(), "archive is missing") {
			t.Fatalf("an archive without its layout must fail, got %v", err)
		}
	})
}

func TestImportRelease_TarWithUnexpectedFirstEntryStopsBeforePush(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bad.tar.gz")
	writeTestArchive(t, archive, []tarEntry{{name: "extra.txt", typ: tar.TypeReg, body: "x"}})
	cmd := newRootCmd()
	cmd.SetArgs([]string{"import", "release", "--from", "tar://" + archive, "--to", "oci://registry.example.com/varroa",
		"--manifest-digest", "sha256:" + strings.Repeat("1", 64), "--work-dir", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "the first archive entry must be") {
		t.Fatalf("an archive that does not start with the manifest must stop the import, got %v", err)
	}
}

// TestImportRelease_MidPushFailureReportsProgress runs a push against a registry
// that refuses every request. The error must say how many artifacts completed.
func TestImportRelease_MidPushFailureReportsProgress(t *testing.T) {
	dir, digest := buildReleaseFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	plan, err := verifyReleaseBundle(dir, digest, strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	pushed, err := pushReleaseBundle(context.Background(), plan, oci.RegistryOptions{Insecure: true})
	if err == nil {
		t.Fatal("a refusing registry must fail the push")
	}
	if len(pushed) != 0 {
		t.Errorf("no artifact completed, but %d are reported as pushed", len(pushed))
	}
	if !strings.Contains(err.Error(), "push stopped after 0 of 3 artifacts") || !strings.Contains(err.Error(), "remain in the registry") {
		t.Errorf("the failure does not report progress honestly: %v", err)
	}
}

func TestExport_DuplicateAndReservedProfileNames(t *testing.T) {
	cases := map[string][]string{
		"duplicate":         {"lts-2.479", "lts-2.479"},
		"manifest tag":      {"release-manifest"},
		"chart prefix":      {"chart-varroa"},
		"image prefix":      {"img-000000000001"},
		"plus sign":         {"lts+2.479"},
		"leading dot":       {".hidden"},
		"slash in the name": {"a/b"},
	}
	for name, profiles := range cases {
		if err := checkProfileNames(profiles); err == nil {
			t.Errorf("%s: checkProfileNames accepted %v", name, profiles)
		}
	}
	if err := checkProfileNames([]string{"lts-2.479", "jenkins-version-2-570"}); err != nil {
		t.Errorf("valid profiles refused: %v", err)
	}
}

func TestExport_DuplicateProfileFailsBeforeNetwork(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"export", "release", "--version", "3.0.0", "--profile", "lts-2.479", "--profile", "lts-2.479",
		"--to", "dir://" + filepath.Join(t.TempDir(), "out")})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate profile must fail at export, got %v", err)
	}
}

func TestExport_ExistingTarDestinationRefused(t *testing.T) {
	testSetup(t)
	existing := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(existing, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	root.SetArgs([]string{"export", "release", "--version", "3.0.0", "--profile", "lts-2.479", "--to", "tar://" + existing})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("an existing tar destination must be refused, got %v", err)
	}
	got, _ := os.ReadFile(existing)
	if string(got) != "keep me" {
		t.Errorf("existing archive was modified")
	}
}

// TestWriteArchiveAtomic_RefusesExistingPartial checks that a partial file this
// call did not create is refused, and that it is left in place.
func TestWriteArchiveAtomic_RefusesExistingPartial(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.tar.gz")
	if err := os.WriteFile(src, []byte("archive"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "out.tar.gz")
	partial := target + ".partial"
	if err := os.WriteFile(partial, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	err := writeArchiveAtomic(src, target)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("an existing partial file must be refused, got %v", err)
	}
	got, err := os.ReadFile(partial)
	if err != nil || string(got) != "stale" {
		t.Errorf("a partial file this call did not create was removed or changed: %q %v", got, err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("target exists after a refused write")
	}
}

// TestWriteDirAtomic_CreatesMissingParent covers a dir:// target under parents that
// do not exist yet, such as a path on removable media.
func TestWriteDirAtomic_CreatesMissingParent(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "usb", "new", "bundle")
	if err := writeDirAtomic(src, target); err != nil {
		t.Fatalf("a missing parent must be created: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "file.txt"))
	if err != nil || string(got) != "x" {
		t.Errorf("bundle not written under the new parent: %q %v", got, err)
	}
	if _, err := os.Stat(target + ".partial"); err == nil {
		t.Errorf("partial directory left behind")
	}
}

func TestParseImageRef_ShortOrUppercaseDigestFails(t *testing.T) {
	for _, raw := range []string{
		"ghcr.io/example/app@sha256:abc",
		"ghcr.io/example/app@sha256:" + strings.Repeat("A", 64),
		"ghcr.io/example/app@sha256:" + strings.Repeat("a", 63),
	} {
		if _, err := parseImageRef(raw); err == nil {
			t.Errorf("parseImageRef(%q) accepted a malformed digest", raw)
		}
	}
	if _, err := parseImageRef("ghcr.io/example/app@sha256:" + strings.Repeat("a", 64)); err != nil {
		t.Errorf("valid digest refused: %v", err)
	}
}

func TestImportRelease_DigestOnlyImageThroughVerifyAndOverrides(t *testing.T) {
	dir, _ := buildReleaseFixture(t)
	digestHex := strings.Repeat("ab", 32)
	digest := rewriteManifest(t, dir, func(m *releaseManifest) {
		m.Images[0].Source = "ghcr.io/example/varroa-jenkins@sha256:" + digestHex
	})
	plan, err := verifyReleaseBundle(dir, digest, "registry.example.com/varroa")
	if err != nil {
		t.Fatalf("digest-only image must verify: %v", err)
	}
	overrides, unattributed := releaseOverrides(plan)
	if len(unattributed) != 0 {
		t.Fatalf("unexpected unattributed images: %v", unattributed)
	}
	op := overrides["operator"].(map[string]any)["image"].(map[string]any)
	wantTag := "digest-" + digestHex[:12]
	if op["tag"] != wantTag {
		t.Errorf("operator.image.tag = %v, want %s", op["tag"], wantTag)
	}
	if op["repository"] != "registry.example.com/varroa/example/varroa-jenkins" {
		t.Errorf("operator.image.repository = %v", op["repository"])
	}
}

// writeFakeHelmStderr writes a helm stand-in that prints body to standard error
// and exits 1.
func writeFakeHelmStderr(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "helm")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestHelmStderr_SecretsAreRedactedAndTruncated(t *testing.T) {
	script := writeFakeHelmStderr(t, "echo 'Error: execution error: clientSecret is sekrit-value-9911 invalid' >&2\necho 'second line with more detail' >&2")
	h := helmRunner{bin: script, secrets: []string{"sekrit-value-9911"}}
	_, err := h.template(context.Background(), "chart.tgz", "varroa-system", nil)
	if err == nil {
		t.Fatal("a failing helm must fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "sekrit-value-9911") {
		t.Errorf("error carries a values secret: %s", msg)
	}
	if !strings.Contains(msg, "<redacted>") {
		t.Errorf("the redaction marker is missing: %s", msg)
	}
	if strings.Contains(msg, "second line") {
		t.Errorf("error carries more than the first stderr line: %s", msg)
	}
}

func TestHelmStderr_MultiLineSecretIsRedactedLineByLine(t *testing.T) {
	pem := "-----BEGIN KEY-----\nMIIBsecretline0001\n-----END KEY-----"
	script := writeFakeHelmStderr(t, "printf 'Error: key is %s\\n' '-----BEGIN KEY-----' >&2\nprintf '%s\\n' 'MIIBsecretline0001' >&2\nprintf '%s\\n' '-----END KEY-----' >&2")
	h := helmRunner{bin: script, secrets: []string{pem}}
	_, err := h.template(context.Background(), "chart.tgz", "varroa-system", nil)
	if err == nil {
		t.Fatal("a failing helm must fail")
	}
	msg := err.Error()
	for _, leak := range []string{"MIIBsecretline0001", "BEGIN KEY", "END KEY"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error carries part of a multi-line secret (%q): %s", leak, msg)
		}
	}
}

func TestHelmStderr_OverlappingSecretsAreRedactedLongestFirst(t *testing.T) {
	script := writeFakeHelmStderr(t, "echo 'Error: login with passw0rd-prod failed' >&2")
	h := helmRunner{bin: script, secrets: []string{"passw0rd", "passw0rd-prod"}}
	_, err := h.template(context.Background(), "chart.tgz", "varroa-system", nil)
	if err == nil {
		t.Fatal("a failing helm must fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "passw0rd") || strings.Contains(msg, "-prod") {
		t.Errorf("a prefix secret left part of the longer one: %s", msg)
	}
}
