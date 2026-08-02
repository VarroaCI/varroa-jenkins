// Package updatecenter implements the Varroa Update Center HTTP service.
// It serves Jenkins plugin metadata and HPI bytes backed by an oci.BlobStore.
package updatecenter

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// Server is the HTTP server for the update center.
type Server struct {
	store  oci.BlobStore
	logger *slog.Logger

	importToken string

	pullThroughEnabled     bool
	pullThroughUpstreamURL string
	pullThroughDownloadURL string

	// resolver resolves a plugin (name, version) -> upstream sha256 across the weekly
	// metadata source plus any operator-supplied LTS-line sources. Required when
	// pullThroughEnabled is true.
	resolver *ucmeta.Resolver

	storeReady bool
	mu         sync.Mutex

	// singleflight for concurrent download dedup.
	dlGroup singleflight.Group

	// Import resource limits (hardening against decompression-bomb DoS).
	// Zero means use the package-level defaults.
	maxImportEntries    int
	maxImportEntryBytes int64
	maxImportTotalBytes int64

	// declaredPluginsFile is the operator-written declared-plugins file, re-read
	// on every upload so a ConfigMap update lands without a restart. Empty means
	// the service was never told where it is, which is NOT the same as "nothing
	// is declared" — see ReadDeclaredPlugins.
	declaredPluginsFile string

	// maxUploadBytes caps an uploaded .hpi.
	maxUploadBytes int64

	// singleWriter gates the upload endpoint. Uploads are a read-modify-write
	// against a store with no conditional-push primitive, so a second writer
	// could not be excluded by any in-process lock.
	singleWriter bool

	// uploadLocks serializes the D4 admission check through the pack commit,
	// keyed by "name@version".
	uploadLocks   map[string]*sync.Mutex
	uploadLocksMu sync.Mutex
}

// NewServer creates a new update center Server.
func NewServer(store oci.BlobStore, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{
		store:               store,
		logger:              logger,
		maxImportEntries:    maxImportEntries,
		maxImportEntryBytes: maxImportEntryBytes,
		maxImportTotalBytes: maxImportTotalBytes,
		maxUploadBytes:      defaultMaxUploadBytes,
		uploadLocks:         make(map[string]*sync.Mutex),
	}
	for _, o := range opts {
		o(s)
	}
	if s.pullThroughEnabled && s.resolver == nil {
		logger.Error("update-center: pull-through enabled but no metadata resolver configured; " +
			"pull-through misses will 404 until WithMetadataResolver is supplied")
	}
	return s
}

// Option configures the Server.
type Option func(*Server)

// WithImportToken sets the shared secret for the import endpoint.
func WithImportToken(token string) Option {
	return func(s *Server) { s.importToken = token }
}

// WithPullThrough enables and configures pull-through caching.
func WithPullThrough(enabled bool, upstreamURL, downloadURL string) Option {
	return func(s *Server) {
		s.pullThroughEnabled = enabled
		s.pullThroughUpstreamURL = upstreamURL
		s.pullThroughDownloadURL = downloadURL
	}
}

// WithMetadataResolver sets the multi-source sha256 resolver used by pull-through.
// It MUST be set (non-nil) whenever pull-through is enabled.
func WithMetadataResolver(r *ucmeta.Resolver) Option {
	return func(s *Server) {
		s.resolver = r
	}
}

// WithImportLimits overrides the default import resource caps (for testing).
// Zero values are ignored (the server default is kept).
func WithImportLimits(maxEntries int, maxEntryBytes, maxTotalBytes int64) Option {
	return func(s *Server) {
		if maxEntries > 0 {
			s.maxImportEntries = maxEntries
		}
		if maxEntryBytes > 0 {
			s.maxImportEntryBytes = maxEntryBytes
		}
		if maxTotalBytes > 0 {
			s.maxImportTotalBytes = maxTotalBytes
		}
	}
}

// WithDeclaredPluginsFile sets the path of the operator-written declared-plugins
// file. It is re-read per upload; it is NOT gated on pull-through, because the
// air-gapped configuration is exactly where the declared set matters most.
func WithDeclaredPluginsFile(path string) Option {
	return func(s *Server) { s.declaredPluginsFile = path }
}

// WithMaxUploadBytes overrides the uploaded-artifact byte cap. Zero is ignored.
func WithMaxUploadBytes(n int64) Option {
	return func(s *Server) {
		if n > 0 {
			s.maxUploadBytes = n
		}
	}
}

// WithSingleWriter declares that this process is the update center's only writer.
// The upload endpoint refuses to run without it.
func WithSingleWriter(enabled bool) Option {
	return func(s *Server) { s.singleWriter = enabled }
}

// MarkReady notes that the store has answered at least one successful call.
func (s *Server) MarkReady() { s.mu.Lock(); s.storeReady = true; s.mu.Unlock() }

// isReady reports whether the store has proven reachable.
func (s *Server) isReady() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.storeReady }

// RegisterRoutes wires all handlers onto the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/update-center.actual.json", s.handleMetadataPlain)
	mux.HandleFunc("/update-center.json", s.handleMetadataJSONP)
	mux.HandleFunc("/download/plugins/", s.handleDownload)
	mux.HandleFunc("/api/v1/import", s.handleImport)
	mux.HandleFunc("/api/v1/plugins", s.handleUpload)
	mux.HandleFunc("/api/v1/inventory", s.handleInventory)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
}

// verifyToken compares the given token against the server's import token
// using constant-time comparison. Returns true if the token is valid.
// If no import token is configured, always returns false.
func (s *Server) verifyToken(token string) bool {
	if s.importToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.importToken), []byte(token)) == 1
}

// ---------------------------------------------------------------------------
// Shared helpers for scanning the store
// ---------------------------------------------------------------------------

// packInfo holds metadata extracted from a plugin pack manifest.
type packInfo struct {
	Ref     string
	Config  oci.PackConfig
	Plugins []pluginLayerInfo
}

// skippedPackInfo names one plugin-pack manifest that could not be read and
// why, so a caller can disclose specifics instead of a bare count.
type skippedPackInfo struct {
	Ref   string
	Error string
}

// pluginLayerInfo holds metadata about a single plugin layer from a manifest.
type pluginLayerInfo struct {
	Name        string
	Version     string
	SHA256      string
	SizeBytes   int64
	UpstreamURL string
	LayerDigest string
	// DisplayName, Description, Tags and RequiredCore are the MANIFEST.MF-derived
	// metadata T1.1's pack format records. Every one is optional: packs written
	// before those annotations existed carry only name/version/sha256/upstreamUrl.
	DisplayName  string
	Description  string
	Tags         []string
	RequiredCore string
	// Dependencies is the decoded dev.varroa.plugin.dependencies annotation. An
	// absent annotation yields nil, which is ambiguous on its own — see
	// packStoreLookup.Dependencies for the discriminator.
	Dependencies []hpi.Dependency
}

// listPackInfos enumerates all plugin-pack manifests in the store and returns
// their parsed metadata (config + plugin layer info).
//
// The second result names every plugin-pack manifest that could NOT be read —
// a manifest that would not pull, or a pack config that would not parse — with
// its ref and the read error, rather than a bare count. It is a return value
// rather than handler-local state because the callers disagree about what to
// do with it, and that disagreement should be visible at every call site: only
// /api/v1/inventory treats it specially, because only the inventory is used to
// decide what to prune, where a partial answer is indistinguishable from
// plugins having been deleted — and even there, the response now discloses the
// skip and serves what it can, rather than failing the whole request closed.
// The metadata and download routes deliberately keep serving what they can
// read — degraded service there beats failing a whole download because one
// unrelated manifest is unreachable.
//
// A manifest skipped for a non-matching ArtifactType is not a failure and is
// not recorded here: it is simply not a plugin pack.
func (s *Server) listPackInfos(ctx context.Context) ([]packInfo, []skippedPackInfo, error) {
	descs, err := s.store.ListManifests(ctx)
	if err != nil {
		return nil, nil, err
	}

	var packs []packInfo
	var skipped []skippedPackInfo
	for _, d := range descs {
		ref := d.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			ref = d.Digest
		}
		manifest, err := s.store.Pull(ctx, ref)
		if err != nil {
			s.logger.Warn("failed to pull manifest", "ref", ref, "error", err)
			skipped = append(skipped, skippedPackInfo{Ref: ref, Error: err.Error()})
			continue
		}
		if manifest.ArtifactType != oci.ArtifactTypePluginPack {
			continue
		}

		pi := packInfo{Ref: ref}
		pi.Plugins = pluginLayersFromManifest(manifest, s.logger)

		// Read pack config if available.
		if manifest.Config.Digest != "" {
			cfg, plugins, err := oci.ReadPluginPack(ctx, s.store, ref)
			if err != nil {
				s.logger.Warn("failed to read plugin pack config", "ref", ref, "error", err)
				skipped = append(skipped, skippedPackInfo{Ref: ref, Error: err.Error()})
				continue
			}
			pi.Config = cfg
			// Merge plugin info: ReadPluginPack returns name/version/sha256 without size,
			// so prefer layer descriptor info if available.
			_ = plugins
		}

		packs = append(packs, pi)
	}
	return packs, skipped, nil
}

// pluginLayersFromManifest extracts plugin metadata from a manifest's layer
// descriptors. A structured annotation that will not decode is logged and
// treated as absent — the store must keep serving the plugin either way.
func pluginLayersFromManifest(m oci.Manifest, logger *slog.Logger) []pluginLayerInfo {
	var plugins []pluginLayerInfo
	for _, l := range m.Layers {
		if l.MediaType != oci.MediaTypePluginHPI {
			continue
		}
		info := pluginLayerInfo{
			Name:         l.Annotations[oci.AnnPluginName],
			Version:      l.Annotations[oci.AnnPluginVersion],
			SHA256:       l.Annotations[oci.AnnPluginSHA256],
			SizeBytes:    l.Size,
			UpstreamURL:  l.Annotations[oci.AnnPluginUpstreamURL],
			LayerDigest:  l.Digest,
			DisplayName:  l.Annotations[oci.AnnPluginDisplayName],
			Description:  l.Annotations[oci.AnnPluginDescription],
			RequiredCore: l.Annotations[oci.AnnPluginRequiredCore],
		}
		if raw := l.Annotations[oci.AnnPluginTags]; raw != "" {
			var tags []string
			if err := json.Unmarshal([]byte(raw), &tags); err != nil {
				if logger != nil {
					logger.Warn("malformed plugin tags annotation; treating as absent",
						"plugin", info.Name, "version", info.Version, "error", err)
				}
			} else {
				info.Tags = tags
			}
		}
		if raw := l.Annotations[oci.AnnPluginDependencies]; raw != "" {
			var deps []struct {
				Name     string `json:"name"`
				Min      string `json:"min"`
				Optional bool   `json:"optional"`
			}
			if err := json.Unmarshal([]byte(raw), &deps); err != nil {
				if logger != nil {
					logger.Warn("malformed plugin dependencies annotation; treating as absent",
						"plugin", info.Name, "version", info.Version, "error", err)
				}
			} else {
				for _, d := range deps {
					info.Dependencies = append(info.Dependencies, hpi.Dependency{
						Name: d.Name, Min: d.Min, Optional: d.Optional,
					})
				}
			}
		}
		plugins = append(plugins, info)
	}
	return plugins
}

// writeJSON writes v as JSON to the response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
