package updatecenter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// errChecksumMismatch is returned by doPullThrough when the downloaded HPI bytes
// do not match the upstream-declared sha256. pullThroughPlugin maps this to 502.
var errChecksumMismatch = errors.New("upstream checksum mismatch")

// handleDownload serves GET /download/plugins/<name>/<version>/<name>.hpi.
// Path format: /download/plugins/<name>/<version>/<file>
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Parse path: /download/plugins/<name>/<version>/<name>.hpi
	path := strings.TrimPrefix(r.URL.Path, "/download/plugins/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 3 {
		writeError(w, http.StatusBadRequest, "invalid path: expected /download/plugins/<name>/<version>/<name>.hpi")
		return
	}
	name := parts[0]
	version := parts[1]

	// Try to find the plugin in the store and serve it.
	digest := s.findPluginDigest(r.Context(), name, version)
	if digest != "" {
		s.servePluginBlob(w, r, digest, name, version)
		return
	}

	// Store miss.
	if !s.pullThroughEnabled || s.resolver == nil {
		writeError(w, http.StatusNotFound, "plugin not found")
		return
	}

	// Pull-through: fetch from upstream.
	s.pullThroughPlugin(w, r, name, version)
}

// findPluginDigest scans all pack manifests for a plugin matching name and version
// and returns its blob digest (sha256:<hex>).
func (s *Server) findPluginDigest(ctx context.Context, name, version string) string {
	// Skips are deliberately discarded — see buildMetadataPayload. A download
	// for a plugin whose own manifest is unreadable falls through to a miss.
	packs, _, err := s.listPackInfos(ctx)
	if err != nil {
		s.logger.Warn("failed to list pack infos for download", "error", err)
		return ""
	}
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name == name && p.Version == version {
				return p.LayerDigest
			}
		}
	}
	return ""
}

// servePluginBlob streams a plugin blob from the store to the client.
func (s *Server) servePluginBlob(w http.ResponseWriter, r *http.Request, digest, name, version string) {
	rc, err := s.store.FetchBlob(r.Context(), digest)
	if err != nil {
		s.logger.Warn("failed to fetch plugin blob", "name", name, "version", version, "digest", digest, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fetch plugin blob")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.hpi"`, name))
	// Send the digest (strip sha256: prefix for the header, matching Jenkins expectations).
	sha256Hex := strings.TrimPrefix(digest, "sha256:")
	w.Header().Set("sha256", sha256Hex)
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, rc); err != nil {
		s.logger.Warn("error streaming plugin blob", "name", name, "version", version, "error", err)
	}
}

// pullThroughPlugin fetches a plugin from the upstream, verifies checksum,
// stores it, and serves it. Uses singleflight to avoid concurrent duplicate fetches.
func (s *Server) pullThroughPlugin(w http.ResponseWriter, r *http.Request, name, version string) {
	key := name + "@" + version

	result, err, _ := s.dlGroup.Do(key, func() (interface{}, error) {
		return s.doPullThrough(r.Context(), name, version)
	})
	if err != nil {
		s.logger.Warn("pull-through failed", "name", name, "version", version, "error", err)
		if errors.Is(err, errChecksumMismatch) {
			writeError(w, http.StatusBadGateway, "upstream checksum mismatch")
		} else {
			writeError(w, http.StatusNotFound, "plugin not found upstream")
		}
		return
	}

	pr := result.(pullThroughResult)
	if pr.err != nil {
		s.logger.Warn("pull-through failed", "name", name, "version", version, "error", pr.err)
		if errors.Is(pr.err, errChecksumMismatch) {
			writeError(w, http.StatusBadGateway, "upstream checksum mismatch")
		} else {
			writeError(w, http.StatusNotFound, "plugin not found upstream")
		}
		return
	}

	// Re-check in store: the singleflight may have stored it.
	digest := s.findPluginDigest(r.Context(), name, version)
	if digest == "" {
		digest = pr.digest
	}
	s.servePluginBlob(w, r, digest, name, version)
}

// pullThroughResult is the result of a pull-through operation.
type pullThroughResult struct {
	digest string
	err    error
}

// doPullThrough fetches plugin metadata from upstream, downloads the HPI,
// verifies the checksum, stores it, and returns the digest.
func (s *Server) doPullThrough(ctx context.Context, name, version string) (pullThroughResult, error) {
	// 1. Resolve the official sha256 across the configured metadata sources (weekly +
	//    any LTS-line dynamic-stable sources). ErrVersionUnavailable is a non-checksum
	//    error, so pullThroughPlugin maps it to a 404 (not found upstream).
	upstreamSHA256, err := s.resolver.ResolveSHA256(ctx, name, version)
	if err != nil {
		return pullThroughResult{}, fmt.Errorf("upstream metadata: %w", err)
	}

	// 2. Download the HPI from the upstream download URL.
	downloadURL := fmt.Sprintf("%s/plugins/%s/%s/%s.hpi",
		strings.TrimRight(s.pullThroughDownloadURL, "/"), name, version, name)

	s.logger.Info("pull-through download", "url", downloadURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return pullThroughResult{}, fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pullThroughResult{}, fmt.Errorf("download HPI: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return pullThroughResult{}, fmt.Errorf("download HPI: HTTP %d", resp.StatusCode)
	}

	// 3. Read the body and verify the sha256.
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return pullThroughResult{}, fmt.Errorf("read HPI body: %w", err)
	}

	actualDigest, _, err := oci.Sha256Digest(strings.NewReader(string(bodyBytes)))
	if err != nil {
		return pullThroughResult{}, fmt.Errorf("compute sha256: %w", err)
	}

	// upstreamSHA256 is the base64-encoded raw 32-byte SHA-256 from the upstream
	// update-center JSON. Decode to raw bytes, hex-encode, and compare.
	rawBytes, err := base64.StdEncoding.DecodeString(upstreamSHA256)
	if err != nil {
		s.logger.Warn("pull-through upstream sha256 is not valid base64", "name", name, "version", version, "error", err)
		return pullThroughResult{}, fmt.Errorf("upstream sha256 not valid base64: %w", err)
	}
	expectedDigest := "sha256:" + hex.EncodeToString(rawBytes)
	if actualDigest != expectedDigest {
		s.logger.Warn("pull-through checksum mismatch", "name", name, "version", version,
			"expected", expectedDigest, "actual", actualDigest)
		return pullThroughResult{}, errChecksumMismatch
	}

	// 4. Store as a discoverable plugin pack using BuildPluginPack so that
	//    findPluginDigest, /api/v1/inventory, and /update-center*.json all
	//    surface it. Use a TAG-SAFE, stable, collision-resistant ref:
	//    "pullthrough-<hex(sha256(name@version))[:12]>" — valid OCI tag charset,
	//    no '/' or ':', stable per name@version so repeated pulls are idempotent.
	hash := sha256.Sum256([]byte(name + "@" + version))
	packRef := "pullthrough-" + hex.EncodeToString(hash[:])[:12]
	now := time.Now().UTC().Format(time.RFC3339)
	plugins := []oci.ResolvedPlugin{{
		Name:    name,
		Version: version,
		SHA256:  actualDigest,
		Content: bytes.NewReader(bodyBytes),
		UpstreamURL: strings.TrimRight(s.pullThroughDownloadURL, "/") +
			fmt.Sprintf("/plugins/%s/%s/%s.hpi", name, version, name),
	}}
	// Derived metadata (displayName, requiredCore, dependencies) comes from the
	// plugin's own manifest. A parse failure must never fail a download in
	// flight: the bytes are already verified against the upstream checksum and
	// a client is waiting on them, so we log and pack without the annotations.
	if err := oci.ApplyHPIMetadata(&plugins[0], bodyBytes); err != nil {
		s.logger.Warn("pull-through HPI manifest unreadable — caching without derived metadata",
			"name", name, "version", version, "error", err)
	}
	// Pull-through writes a single-plugin pack, so it is an addon. `profile` is
	// empty per the addon contract; the provenance the old "pullthrough" string
	// carried is recorded more precisely per layer in upstreamUrl above.
	packCfg := oci.PackConfig{
		Kind:        oci.PackKindAddon,
		Profile:     "",
		PluginCount: 1,
		LockHash:    oci.LockHash(plugins),
		CreatedAt:   now,
	}
	if err := oci.BuildPluginPack(ctx, s.store, packRef, packCfg, plugins); err != nil {
		s.logger.Error("failed to build pull-through plugin pack — plugin is not cached in store",
			"name", name, "version", version, "ref", packRef, "error", err)
		// Serve the already-fetched, already-verified bytes for this request.
		// A store-write failure must not fail the client download, but it is
		// visible via the ERROR-level log.
	}

	return pullThroughResult{digest: actualDigest}, nil
}
