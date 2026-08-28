package ucmeta

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPlugin    = "workflow-cps"
	testDigestHex = "4ac9d55737cfe5a1583826a5eea8f6b2046cb95ab5c828e0d48e7ea4ce2a0b57"
	testGAV       = "org.jenkins-ci.plugins.workflow:workflow-cps:4362.vfake"
)

func testDigestB64(t *testing.T) string {
	t.Helper()
	raw, err := hex.DecodeString(testDigestHex)
	if err != nil {
		t.Fatalf("decode fixture digest: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// gavMetaServer serves metadata listing testPlugin at listedVersion, including a gav so
// the archive fallback has a groupId to work with.
func gavMetaServer(t *testing.T, listedVersion, gav string) *httptest.Server {
	t.Helper()
	const name = testPlugin
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"plugins":{%q:{"version":%q,"sha256":%q,"gav":%q}}}`,
			name, listedVersion, "unused-base64", gav)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// archiveServer serves .sha256 sidecars from a path->body map and counts requests.
func archiveServer(t *testing.T, bodies map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const wantSidecarPath = "/org/jenkins-ci/plugins/workflow/workflow-cps/4360.vpinned/workflow-cps-4360.vpinned.hpi.sha256"

func newArchiveTestResolver(t *testing.T, metaURL, archiveURL string) *Resolver {
	t.Helper()
	r := NewResolver(staticSources(metaURL), time.Minute, http.DefaultClient, nil)
	r.SetArchiveBaseURL(archiveURL)
	return r
}

// An aged pin is the whole reason the fallback exists: metadata has moved on to a newer
// version, so only the archive can supply the checksum.
func TestResolveSHA256_AgedPinResolvesFromArchive(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	got, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if err != nil {
		t.Fatalf("ResolveSHA256: %v", err)
	}
	if want := testDigestB64(t); got != want {
		t.Errorf("digest = %q, want %q (base64 of the hex sidecar)", got, want)
	}
	if *hits != 1 {
		t.Errorf("archive hits = %d, want 1", *hits)
	}
}

// The sidecar is hex but metadata is base64; callers verify against one encoding, so the
// fallback must not leak hex through.
func TestResolveSHA256_ArchiveDigestIsBase64NotHex(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, _ := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	got, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if err != nil {
		t.Fatalf("ResolveSHA256: %v", err)
	}
	if got == testDigestHex {
		t.Fatal("digest returned as hex; callers decode base64 and would reject every download")
	}
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("digest is not valid base64: %v", err)
	}
	if hex.EncodeToString(raw) != testDigestHex {
		t.Errorf("decoded digest = %s, want %s", hex.EncodeToString(raw), testDigestHex)
	}
}

// A metadata hit is authoritative and must short-circuit the network call.
func TestResolveSHA256_MetadataHitSkipsArchive(t *testing.T) {
	meta := gavMetaServer(t, "4360.vpinned", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	got, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if err != nil {
		t.Fatalf("ResolveSHA256: %v", err)
	}
	if got != "unused-base64" {
		t.Errorf("digest = %q, want the metadata value", got)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0 when metadata already answered", *hits)
	}
}

func TestResolveSHA256_ArchiveMissStillUnavailable(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, _ := archiveServer(t, map[string]string{}) // everything 404s
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	_, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if !errors.Is(err, ErrVersionUnavailable) {
		t.Fatalf("err = %v, want ErrVersionUnavailable", err)
	}
}

// A corrupt sidecar must fail closed. Returning a malformed digest would make the
// verifier reject good downloads; returning something truncated would be worse.
func TestResolveSHA256_MalformedSidecarFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not hex", "zzzz-not-a-digest"},
		{"too short", "4ac9d557"},
		{"html error page", "<!DOCTYPE html><html><body>404</body></html>"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := gavMetaServer(t, "4362.vnewer", testGAV)
			arch, _ := archiveServer(t, map[string]string{wantSidecarPath: tc.body})
			r := newArchiveTestResolver(t, meta.URL, arch.URL)

			got, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
			if !errors.Is(err, ErrVersionUnavailable) {
				t.Fatalf("err = %v, want ErrVersionUnavailable (got digest %q)", err, got)
			}
		})
	}
}

// "<digest>  <filename>" is a common sidecar shape; accept it rather than failing a
// download we could verify.
func TestResolveSHA256_SidecarWithFilenameField(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, _ := archiveServer(t, map[string]string{
		wantSidecarPath: testDigestHex + "  workflow-cps-4360.vpinned.hpi\n",
	})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	got, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if err != nil {
		t.Fatalf("ResolveSHA256: %v", err)
	}
	if want := testDigestB64(t); got != want {
		t.Errorf("digest = %q, want %q", got, want)
	}
}

// Sidecars for a released version never change, so a repeat lookup must not re-fetch.
func TestResolveSHA256_ArchiveResultIsCached(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	for i := 0; i < 3; i++ {
		if _, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if *hits != 1 {
		t.Errorf("archive hits = %d, want 1 (result should be cached)", *hits)
	}
}

// A negative is cached too, so a genuinely absent version does not hammer the archive
// once per plugin per provision.
func TestResolveSHA256_ArchiveMissIsCached(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, hits := archiveServer(t, map[string]string{})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	for i := 0; i < 3; i++ {
		_, _ = r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	}
	if *hits != 1 {
		t.Errorf("archive hits = %d, want 1 (miss should be cached)", *hits)
	}
}

// Without a gav there is no group path, so the fallback must not guess a URL.
func TestResolveSHA256_NoGAVSkipsArchive(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", "")
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)

	_, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if !errors.Is(err, ErrVersionUnavailable) {
		t.Fatalf("err = %v, want ErrVersionUnavailable", err)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0 without a gav", *hits)
	}
}

func TestResolveSHA256_EmptyArchiveBaseURLDisablesFallback(t *testing.T) {
	meta := gavMetaServer(t, "4362.vnewer", testGAV)
	arch, hits := archiveServer(t, map[string]string{wantSidecarPath: testDigestHex})
	r := newArchiveTestResolver(t, meta.URL, arch.URL)
	r.SetArchiveBaseURL("")

	_, err := r.ResolveSHA256(context.Background(), "workflow-cps", "4360.vpinned")
	if !errors.Is(err, ErrVersionUnavailable) {
		t.Fatalf("err = %v, want ErrVersionUnavailable", err)
	}
	if *hits != 0 {
		t.Errorf("archive hits = %d, want 0 when the fallback is disabled", *hits)
	}
}

func TestGroupPath(t *testing.T) {
	for _, tc := range []struct {
		gav  string
		want string
	}{
		{"org.jenkins-ci.plugins.workflow:workflow-cps:4362.v1", "org/jenkins-ci/plugins/workflow"},
		{"org.jenkins-ci.plugins:cloudbees-folder:6.1", "org/jenkins-ci/plugins"},
		{"io.jenkins.plugins:mcp-server:0.188", "io/jenkins/plugins"},
		{"", ""},
		{"no-colon-here", ""},
		{":artifact:1.0", ""},
		// Path traversal must not reach the URL builder.
		{"../../etc:artifact:1.0", ""},
		{"org/jenkins:artifact:1.0", ""},
	} {
		if got := groupPath(tc.gav); got != tc.want {
			t.Errorf("groupPath(%q) = %q, want %q", tc.gav, got, tc.want)
		}
	}
}

// The URL must be the Maven layout the repository actually serves; a wrong shape is
// exactly the class of bug that produced the 404 storm this fallback addresses.
func TestArchiveResolver_RequestPathMatchesMavenLayout(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(testDigestHex))
	}))
	t.Cleanup(srv.Close)

	a := newArchiveResolver(srv.URL, http.DefaultClient)
	if _, err := a.resolve(context.Background(), testGAV, "workflow-cps", "4360.vpinned"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotPath != wantSidecarPath {
		t.Errorf("request path = %q, want %q", gotPath, wantSidecarPath)
	}
	if !strings.HasSuffix(gotPath, ".hpi.sha256") {
		t.Errorf("request path %q does not address a .hpi.sha256 sidecar", gotPath)
	}
}
