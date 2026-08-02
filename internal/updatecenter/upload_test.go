package updatecenter

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

const testUploadToken = "upload-token"

// ---------------------------------------------------------------------------
// HPI fixtures
// ---------------------------------------------------------------------------

// hpiSpec describes a synthetic .hpi to build.
type hpiSpec struct {
	shortName    string
	version      string
	longName     string
	requiredCore string
	deps         string // raw Plugin-Dependencies value
	omitShort    bool
	filler       int // extra bytes, to exceed a byte cap
}

// buildHPI produces a real ZIP carrying a real META-INF/MANIFEST.MF.
func buildHPI(t *testing.T, spec hpiSpec) []byte {
	t.Helper()
	var mf bytes.Buffer
	mf.WriteString("Manifest-Version: 1.0\r\n")
	if !spec.omitShort {
		fmt.Fprintf(&mf, "Short-Name: %s\r\n", spec.shortName)
	}
	if spec.version != "" {
		fmt.Fprintf(&mf, "Plugin-Version: %s\r\n", spec.version)
	}
	if spec.longName != "" {
		fmt.Fprintf(&mf, "Long-Name: %s\r\n", spec.longName)
	}
	if spec.requiredCore != "" {
		fmt.Fprintf(&mf, "Jenkins-Version: %s\r\n", spec.requiredCore)
	}
	if spec.deps != "" {
		fmt.Fprintf(&mf, "Plugin-Dependencies: %s\r\n", spec.deps)
	}
	mf.WriteString("\r\n")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write(mf.Bytes()); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if spec.filler > 0 {
		fw, err := zw.Create("filler.bin")
		if err != nil {
			t.Fatalf("zip create filler: %v", err)
		}
		// Incompressible filler so the archive really grows past a byte cap.
		pad := make([]byte, spec.filler)
		rng := rand.New(rand.NewSource(1)) // #nosec G404 -- test fixture, not crypto
		_, _ = rng.Read(pad)
		if _, err := fw.Write(pad); err != nil {
			t.Fatalf("zip write filler: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Server fixtures
// ---------------------------------------------------------------------------

// declaredFile writes a declared-plugins file and returns its path.
func declaredFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "declared-plugins")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write declared file: %v", err)
	}
	return path
}

// uploadOpts is the baseline: single writer, an import token, and a readable
// (possibly empty) declared set.
func uploadOpts(t *testing.T, declaredLines ...string) []Option {
	return []Option{
		WithImportToken(testUploadToken),
		WithSingleWriter(true),
		WithDeclaredPluginsFile(declaredFile(t, declaredLines...)),
	}
}

// postUpload sends a multipart upload and returns the response.
func (ts *testServer) postUpload(t *testing.T, body []byte, query string, headers map[string]string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "plugin.hpi")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/plugins"+query, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testUploadToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload: %v", err)
	}
	return resp
}

func decodeRejection(t *testing.T, resp *http.Response) UploadRejection {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out UploadRejection
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	return out
}

func decodeResult(t *testing.T, resp *http.Response) UploadResult {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return out
}

// upstreamServer serves update-center metadata plus HPI downloads for the given
// plugins, so a planned-fetch can actually be committed.
func upstreamServer(t *testing.T, plugins map[string][]byte, versions map[string]string, deps map[string][]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/update-center.actual.json") {
			doc := map[string]any{"plugins": map[string]any{}}
			for name, body := range plugins {
				sum := sha256.Sum256(body)
				entry := map[string]any{
					"version": versions[name],
					"sha256":  base64.StdEncoding.EncodeToString(sum[:]),
				}
				if d, ok := deps[name]; ok {
					entry["dependencies"] = d
				}
				doc["plugins"].(map[string]any)[name] = entry
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		// /plugins/<name>/<version>/<name>.hpi
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 4 && parts[0] == "plugins" {
			if body, ok := plugins[parts[1]]; ok && versions[parts[1]] == parts[2] {
				_, _ = w.Write(body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedAddonPack stores a single plugin as a post-contract addon pack, with its
// dependency annotation.
func seedAddonPack(t *testing.T, store oci.BlobStore, ref string, name, version string, body []byte, deps []hpi.Dependency) {
	t.Helper()
	p := oci.ResolvedPlugin{
		Name:         name,
		Version:      version,
		SHA256:       digestOf(body),
		Dependencies: deps,
		Content:      bytes.NewReader(body),
	}
	cfg := oci.PackConfig{
		Kind:        oci.PackKindAddon,
		PluginCount: 1,
		LockHash:    oci.LockHash([]oci.ResolvedPlugin{p}),
		CreatedAt:   "2026-01-01T00:00:00Z",
	}
	if err := oci.BuildPluginPack(context.Background(), store, ref, cfg, []oci.ResolvedPlugin{p}); err != nil {
		t.Fatalf("seed addon pack %s: %v", ref, err)
	}
}

// ---------------------------------------------------------------------------
// 5.10 — response matrix
// ---------------------------------------------------------------------------

func TestUpload_HappyPathNoDependencies(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	body := buildHPI(t, hpiSpec{shortName: "varroa-mcp-tools", version: "1.0.0",
		longName: "Varroa MCP Tools", requiredCore: "2.492"})

	resp := ts.postUpload(t, body, "", map[string]string{uploadedByHeader: "alice@example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	res := decodeResult(t, resp)
	if res.Plugin.Name != "varroa-mcp-tools" || res.Plugin.Version != "1.0.0" {
		t.Fatalf("plugin = %+v", res.Plugin)
	}
	if res.Plugin.SHA256 != digestOf(body) {
		t.Errorf("sha256 = %q, want %q", res.Plugin.SHA256, digestOf(body))
	}
	if res.PackRef != packRefFor("upload-", "varroa-mcp-tools", "1.0.0") {
		t.Errorf("packRef = %q", res.PackRef)
	}
	if res.DryRun {
		t.Error("dryRun must be false on a committed upload")
	}
	// The bytes are downloadable.
	dl := ts.get("/download/plugins/varroa-mcp-tools/1.0.0/varroa-mcp-tools.hpi")
	defer func() { _ = dl.Body.Close() }()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", dl.StatusCode)
	}
}

func TestUpload_DryRunWritesNothing(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0"})

	resp := ts.postUpload(t, body, "?dryRun=true", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	res := decodeResult(t, resp)
	if !res.DryRun {
		t.Error("dryRun must be true")
	}
	if res.PackRef != "" {
		t.Errorf("packRef = %q, want empty on a dry run", res.PackRef)
	}
	if d, _ := ts.server.findPluginDigests(context.Background(), "acme", "1.0"); len(d) != 0 {
		t.Fatalf("store holds %v after a dry run", d)
	}
}

func TestUpload_InvalidArtifact(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	resp := ts.postUpload(t, []byte("this is not a zip"), "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrInvalidArtifact {
		t.Fatalf("error = %q, want %q", got, ErrInvalidArtifact)
	}
}

func TestUpload_MissingManifestFields(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	// A valid ZIP with a manifest that has no Short-Name.
	body := buildHPI(t, hpiSpec{omitShort: true, version: "1.0"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrMissingManifestFields {
		t.Fatalf("error = %q, want %q", got, ErrMissingManifestFields)
	}
}

func TestUpload_Duplicate(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0"})
	seedAddonPack(t, store, "seeded", "acme", "1.0", body, nil)

	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrDuplicate {
		t.Fatalf("error = %q, want %q", got, ErrDuplicate)
	}
}

func TestUpload_VersionDigestConflict(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)
	stored := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", longName: "Original"})
	uploaded := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", longName: "Tampered"})
	seedAddonPack(t, store, "seeded", "acme", "1.0", stored, nil)

	resp := ts.postUpload(t, uploaded, "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrVersionDigestConflict {
		t.Fatalf("error = %q, want %q", got, ErrVersionDigestConflict)
	}
}

// TestUpload_TwoDigestsAlreadyStored: a store that already holds two digests for
// one name@version must be a conflict, not classified by backend ordering.
func TestUpload_TwoDigestsAlreadyStored(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)
	a := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", longName: "A"})
	b := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", longName: "B"})
	seedAddonPack(t, store, "pack-a", "acme", "1.0", a, nil)
	seedAddonPack(t, store, "pack-b", "acme", "1.0", b, nil)

	if dg, _ := ts.server.findPluginDigests(context.Background(), "acme", "1.0"); len(dg) != 2 {
		t.Fatalf("findPluginDigests found %d digests, want 2", len(dg))
	}
	// Even re-uploading one of the two exact byte sequences is a conflict.
	resp := ts.postUpload(t, a, "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrVersionDigestConflict {
		t.Fatalf("error = %q, want %q", got, ErrVersionDigestConflict)
	}
}

func TestUpload_TooLarge(t *testing.T) {
	opts := append(uploadOpts(t), WithMaxUploadBytes(1024))
	ts := newTestServer(t, nil, opts...)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", filler: 64 * 1024})

	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrTooLarge {
		t.Fatalf("error = %q, want %q", got, ErrTooLarge)
	}
}

func TestUpload_UnresolvedDependencies(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0",
		deps: "acme-internal:2.0,junit:1.0;resolution:=optional"})

	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	rej := decodeRejection(t, resp)
	if rej.Error != ErrUnresolvedDependencies {
		t.Fatalf("error = %q, want %q", rej.Error, ErrUnresolvedDependencies)
	}
	if len(rej.Unresolved) != 1 || rej.Unresolved[0].Name != "acme-internal" {
		t.Fatalf("unresolved = %+v, want only acme-internal (the optional dep is never resolved)", rej.Unresolved)
	}
	if rej.Unresolved[0].Reason != StatusNotInStore || rej.Unresolved[0].Remediation == "" {
		t.Fatalf("row = %+v, want not-in-store with a remediation", rej.Unresolved[0])
	}
}

// TestUpload_StoredPackWithNoDependencyAnnotationIsAuthoritative guards against
// a FALSE closure-unverifiable. The dev.varroa.plugin.dependencies annotation is
// omitted when a plugin has no dependencies, so its absence alone must not be
// read as "unknown" for a pack that carries a kind.
//
// The genuine closure-unverifiable path — a stored pack whose dependency list
// cannot be determined — is not reachable through this handler in the current
// codebase: oci.ReadPluginPack hard-rejects a pack config with no kind, and
// oci.LayoutStore.Push refuses a manifest with no config descriptor, so a
// pre-contract pack is invisible to listPackInfos rather than merely opaque. The
// planner keeps the branch and TestPlanClosure_UnverifiableLegacyPack covers it
// against the storeLookup contract, which does admit a non-authoritative answer.
func TestUpload_StoredPackWithNoDependencyAnnotationIsAuthoritative(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)
	depBody := buildHPI(t, hpiSpec{shortName: "leaf", version: "1.0"})
	seedAddonPack(t, store, "leaf-pack", "leaf", "1.0", depBody, nil)

	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "leaf:1.0"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	res := decodeResult(t, resp)
	if len(res.Closure) != 1 || res.Closure[0].Status != StatusSatisfiedStore {
		t.Fatalf("closure = %+v, want satisfied-store", res.Closure)
	}
}

func TestUpload_ClosureTooDeep(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)

	const chain = maxClosureDepth + 3
	for i := 0; i < chain; i++ {
		name := fmt.Sprintf("d%02d", i)
		var deps []hpi.Dependency
		if i+1 < chain {
			deps = []hpi.Dependency{{Name: fmt.Sprintf("d%02d", i+1), Min: "1.0"}}
		}
		seedAddonPack(t, store, "pack-"+name, name, "1.0", buildHPI(t, hpiSpec{shortName: name, version: "1.0"}), deps)
	}

	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "d00:1.0"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrClosureTooDeep {
		t.Fatalf("error = %q, want %q", got, ErrClosureTooDeep)
	}
}

func TestUpload_RequiresSingleWriter(t *testing.T) {
	ts := newTestServer(t, nil,
		WithImportToken(testUploadToken),
		WithDeclaredPluginsFile(declaredFile(t)),
	)
	resp := ts.postUpload(t, buildHPI(t, hpiSpec{shortName: "acme", version: "1.0"}), "", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrUploadsRequireSingleWri {
		t.Fatalf("error = %q, want %q", got, ErrUploadsRequireSingleWri)
	}
}

// TestUpload_DeclaredSetUnavailable also asserts the ORDER: the precondition is
// checked before planning, so an upload with an unresolvable dependency still
// reports the 503 rather than a 422.
func TestUpload_DeclaredSetUnavailable(t *testing.T) {
	ts := newTestServer(t, nil,
		WithImportToken(testUploadToken),
		WithSingleWriter(true),
		WithDeclaredPluginsFile(filepath.Join(t.TempDir(), "does-not-exist")),
	)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "nope:9.9"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrDeclaredSetUnavailable {
		t.Fatalf("error = %q, want %q", got, ErrDeclaredSetUnavailable)
	}
}

func TestUpload_MetadataUnavailable(t *testing.T) {
	// An upstream that always fails makes every source unhealthy.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)

	opts := append(uploadOpts(t), pullThroughWithResolver(dead.URL)...)
	ts := newTestServer(t, nil, opts...)

	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "some-lib:1.0"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := decodeRejection(t, resp).Error; got != ErrMetadataUnavailable {
		t.Fatalf("error = %q, want %q", got, ErrMetadataUnavailable)
	}
}

func TestUpload_FetchFailed(t *testing.T) {
	depBody := buildHPI(t, hpiSpec{shortName: "some-lib", version: "1.9"})
	// Metadata advertises a checksum the download will not match.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/update-center.actual.json") {
			wrong := sha256.Sum256([]byte("not the bytes you will get"))
			_ = json.NewEncoder(w).Encode(map[string]any{"plugins": map[string]any{
				"some-lib": map[string]any{"version": "1.9", "sha256": base64.StdEncoding.EncodeToString(wrong[:])},
			}})
			return
		}
		_, _ = w.Write(depBody)
	}))
	t.Cleanup(up.Close)

	store := newTestStore(t)
	opts := append(uploadOpts(t), pullThroughWithResolver(up.URL)...)
	ts := newTestServer(t, store, opts...)

	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "some-lib:1.2"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	if got := decodeRejection(t, resp).Error; got != ErrFetchFailed {
		t.Fatalf("error = %q, want %q", got, ErrFetchFailed)
	}
	// A COMMIT failure leaves the store without a new manifest.
	if d, _ := ts.server.findPluginDigests(context.Background(), "acme", "1.0"); len(d) != 0 {
		t.Fatalf("store holds the uploaded plugin (%v) after a failed commit", d)
	}
	if d, _ := ts.server.findPluginDigests(context.Background(), "some-lib", "1.9"); len(d) != 0 {
		t.Fatalf("store holds a partially-written dependency (%v)", d)
	}
}

func TestUpload_Unauthorized(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	req, _ := http.NewRequest(http.MethodPost, ts.baseURL+"/api/v1/plugins", strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 5.11 / 5.12 — commit behaviour, concurrency, pack output
// ---------------------------------------------------------------------------

func TestUpload_CommitFetchesDependencies(t *testing.T) {
	depBody := buildHPI(t, hpiSpec{shortName: "some-lib", version: "1.9"})
	up := upstreamServer(t,
		map[string][]byte{"some-lib": depBody},
		map[string]string{"some-lib": "1.9"},
		nil,
	)
	store := newTestStore(t)
	opts := append(uploadOpts(t), pullThroughWithResolver(up.URL)...)
	ts := newTestServer(t, store, opts...)

	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "some-lib:1.2"})
	resp := ts.postUpload(t, body, "", map[string]string{uploadedByHeader: "bob@example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	res := decodeResult(t, resp)
	if len(res.Closure) != 1 || res.Closure[0].Status != StatusPlannedFetch || !res.Closure[0].Fetched {
		t.Fatalf("closure = %+v", res.Closure)
	}
	if d, _ := ts.server.findPluginDigests(context.Background(), "some-lib", "1.9"); len(d) != 1 {
		t.Fatalf("dependency digests = %v, want exactly one", d)
	}
}

// TestUpload_PackOutput verifies §5.12: kind, provenance, and the full layer
// annotation set on a committed upload.
func TestUpload_PackOutput(t *testing.T) {
	store := newTestStore(t)
	ts := newTestServer(t, store, uploadOpts(t)...)
	body := buildHPI(t, hpiSpec{
		shortName: "varroa-mcp-tools", version: "1.0.0",
		longName: "Varroa MCP Tools", requiredCore: "2.492",
		deps: "workflow-api:1384.vdc05a_48f535f;resolution:=optional",
	})
	resp := ts.postUpload(t, body, "", map[string]string{uploadedByHeader: "alice@example.com"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	ref := decodeResult(t, resp).PackRef

	cfg, plugins, err := oci.ReadPluginPack(context.Background(), store, ref)
	if err != nil {
		t.Fatalf("ReadPluginPack: %v", err)
	}
	if cfg.Kind != oci.PackKindAddon {
		t.Errorf("kind = %q, want %q", cfg.Kind, oci.PackKindAddon)
	}
	if cfg.UploadedBy != "alice@example.com" {
		t.Errorf("uploadedBy = %q", cfg.UploadedBy)
	}
	if cfg.UploadedAt == "" {
		t.Error("uploadedAt is empty")
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins = %+v", plugins)
	}

	manifest, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	var ann map[string]string
	for _, l := range manifest.Layers {
		if l.MediaType == oci.MediaTypePluginHPI {
			ann = l.Annotations
		}
	}
	want := map[string]string{
		oci.AnnPluginName:         "varroa-mcp-tools",
		oci.AnnPluginVersion:      "1.0.0",
		oci.AnnPluginSHA256:       digestOf(body),
		oci.AnnPluginDisplayName:  "Varroa MCP Tools",
		oci.AnnPluginRequiredCore: "2.492",
	}
	for k, v := range want {
		if ann[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, ann[k], v)
		}
	}
	var deps []map[string]any
	if err := json.Unmarshal([]byte(ann[oci.AnnPluginDependencies]), &deps); err != nil {
		t.Fatalf("dependencies annotation %q: %v", ann[oci.AnnPluginDependencies], err)
	}
	if len(deps) != 1 || deps[0]["name"] != "workflow-api" || deps[0]["optional"] != true {
		t.Fatalf("dependencies annotation = %+v", deps)
	}
}

// TestUpload_ConcurrentSameVersion: two simultaneous uploads of one name@version
// must produce exactly one 201 and one 409.
func TestUpload_ConcurrentSameVersion(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t)...)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0"})

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := ts.postUpload(t, body, "", nil)
			codes[i] = resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	created, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("statuses = %v, want exactly one 201 and one 409", codes)
	}
}

// TestUpload_DeclaredDependencyNeverFetched: a declared dependency is warned
// about, never fetched — a second writer choosing its own version would shadow
// the declaration.
func TestUpload_DeclaredDependencyNeverFetched(t *testing.T) {
	depBody := buildHPI(t, hpiSpec{shortName: "mailer", version: "472"})
	up := upstreamServer(t,
		map[string][]byte{"mailer": depBody},
		map[string]string{"mailer": "472"},
		nil,
	)
	store := newTestStore(t)
	opts := append(uploadOpts(t, "mailer@472"), pullThroughWithResolver(up.URL)...)
	ts := newTestServer(t, store, opts...)

	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "mailer:470"})
	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	res := decodeResult(t, resp)
	if len(res.Closure) != 1 || res.Closure[0].Status != StatusDeclaredNotYetStored {
		t.Fatalf("closure = %+v, want declared-not-yet-stored", res.Closure)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != StatusDeclaredNotYetStored {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	if d, _ := ts.server.findPluginDigests(context.Background(), "mailer", "472"); len(d) != 0 {
		t.Fatalf("mailer was fetched (%v) — a declared plugin must never be fetched by an upload", d)
	}
}

// TestUpload_LockTooOldIsAWarning: D6 — never a rejection.
func TestUpload_LockTooOldIsAWarning(t *testing.T) {
	ts := newTestServer(t, nil, uploadOpts(t, "credentials@1389.vd4")...)
	body := buildHPI(t, hpiSpec{shortName: "acme", version: "1.0", deps: "credentials:1400.v0"})

	resp := ts.postUpload(t, body, "", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — lock-too-old is a warning (%+v)", resp.StatusCode, decodeRejection(t, resp))
	}
	res := decodeResult(t, resp)
	if len(res.Warnings) != 1 || res.Warnings[0].Code != StatusLockTooOld {
		t.Fatalf("warnings = %+v, want lock-too-old", res.Warnings)
	}
}
