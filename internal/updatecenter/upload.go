package updatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// defaultMaxUploadBytes caps an uploaded .hpi. Overridable with
// VARROA_UC_MAX_UPLOAD_BYTES.
const defaultMaxUploadBytes int64 = 256 << 20 // 256 MiB

// uploadedByHeader carries the BFF-authenticated subject. The UC does not
// authenticate users — it trusts the import token — so this is provenance, not
// authorization.
const uploadedByHeader = "X-Varroa-Uploaded-By"

// UploadedPlugin identifies the artifact that was uploaded.
type UploadedPlugin struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	SHA256       string `json:"sha256"`
	DisplayName  string `json:"displayName,omitempty"`
	RequiredCore string `json:"requiredCore,omitempty"`
}

// UploadResult is the success envelope.
type UploadResult struct {
	Plugin               UploadedPlugin       `json:"plugin"`
	DryRun               bool                 `json:"dryRun"`
	PackRef              string               `json:"packRef,omitempty"`
	Closure              []ClosureEntry       `json:"closure"`
	OptionalDependencies []OptionalDependency `json:"optionalDependencies"`
	Warnings             []UploadWarning      `json:"warnings"`
}

// UploadRejection is the rejection envelope. `error` reuses the existing
// writeError shape so it stays consistent with every other UC endpoint;
// `message` and `unresolved` are additive.
type UploadRejection struct {
	Error      string                 `json:"error"`
	Message    string                 `json:"message,omitempty"`
	Unresolved []UnresolvedDependency `json:"unresolved,omitempty"`
}

// parsedUpload is the PARSE phase's output.
type parsedUpload struct {
	manifest hpi.PluginManifest
	digest   string // "sha256:<hex>"
	path     string // temp file holding the verified bytes
	size     int64
}

// handleUpload handles POST /api/v1/plugins — a multipart HPI upload, sibling to
// handleImport, sharing its Bearer-token gate and its temp-then-commit
// discipline.
//
// Phases: PARSE → declared-set precondition → D4 admission → PLAN → VALIDATE →
// COMMIT. Nothing is written until every mandatory dependency has resolved.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") || !s.verifyToken(strings.TrimPrefix(authHeader, "Bearer ")) {
		s.logger.Warn("upload request with missing or invalid token")
		writeError(w, http.StatusUnauthorized, "unauthorized: missing or invalid Bearer token")
		return
	}

	// Single-writer gate. An upload is a read-modify-write against a store with
	// no conditional-push primitive: PackConfig carries CreatedAt and UploadedBy,
	// so two writers producing byte-identical uploads still produce different
	// manifests on the same deterministic tag, and no in-process lock can close
	// that. This is refused rather than documented away.
	if !s.singleWriter {
		s.rejectUpload(w, http.StatusNotImplemented, ErrUploadsRequireSingleWri,
			"the update center is not configured as a single writer; uploads require a one-replica "+
				"deployment with a non-overlapping rollout strategy (set updateCenter.uploads.enabled)")
		return
	}

	dryRun := r.URL.Query().Get("dryRun") == "true"
	uploadedBy := r.Header.Get(uploadedByHeader)

	// --- PARSE ---
	parsed, code, msg := s.parseUpload(r)
	if code != "" {
		s.rejectUpload(w, statusForCode(code), code, msg)
		return
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(parsed.path)) }()

	// --- Declared-set precondition ---
	//
	// Checked BEFORE planning. An unreadable declared set is not "nothing is
	// declared": treating it that way would send every dependency to the upstream
	// tier and fetch unpinned versions for plugins that are in fact pinned.
	// Suppressing the precedence rule after the fact would not have made planning
	// lock-safe, which is why this is a precondition rather than a degradation
	// flag.
	declared, ok := ReadDeclaredPlugins(s.declaredPluginsFile)
	if !ok {
		s.rejectUpload(w, http.StatusServiceUnavailable, ErrDeclaredSetUnavailable,
			"the operator-written declared-plugins file is absent or unreadable; this is retryable and "+
				"resolves as soon as the operator's next reconcile writes it")
		return
	}

	// The D4 check and the COMMIT write are a read-modify-write, so they are
	// serialized per name@version.
	unlock := s.lockUpload(parsed.manifest.ShortName + "@" + parsed.manifest.Version)
	defer unlock()

	// --- D4 admission check ---
	if code, msg := s.admit(r.Context(), parsed); code != "" {
		s.rejectUpload(w, statusForCode(code), code, msg)
		return
	}

	// --- PLAN ---
	planner := &closurePlanner{
		store:       &packStoreLookup{srv: s},
		declared:    declared,
		resolver:    s.uploadResolver(),
		pullThrough: s.pullThroughEnabled && s.resolver != nil,
	}
	plan := planner.planClosure(r.Context(), parsed.manifest)

	// --- VALIDATE ---
	if code, status, msg := plan.envelope(); code != "" {
		writeJSON(w, status, UploadRejection{Error: code, Message: msg, Unresolved: plan.Unresolved})
		return
	}

	result := UploadResult{
		Plugin: UploadedPlugin{
			Name:         parsed.manifest.ShortName,
			Version:      parsed.manifest.Version,
			SHA256:       parsed.digest,
			DisplayName:  parsed.manifest.LongName,
			RequiredCore: parsed.manifest.RequiredCore,
		},
		DryRun:               dryRun,
		Closure:              nonNilClosure(plan.Closure),
		OptionalDependencies: nonNilOptional(plan.Optional),
		Warnings:             nonNilWarnings(plan.Warnings),
	}

	if dryRun {
		writeJSON(w, http.StatusOK, result)
		return
	}

	// --- COMMIT ---
	ref, err := s.commitUpload(r.Context(), parsed, plan, uploadedBy)
	if err != nil {
		s.logger.Error("upload commit failed", "plugin", parsed.manifest.ShortName,
			"version", parsed.manifest.Version, "error", err)
		s.rejectUpload(w, http.StatusBadGateway, ErrFetchFailed, err.Error())
		return
	}
	result.PackRef = ref
	s.logger.Info("plugin uploaded", "plugin", parsed.manifest.ShortName, "version", parsed.manifest.Version,
		"ref", ref, "fetched", len(plan.Fetches), "uploadedBy", uploadedBy)
	writeJSON(w, http.StatusCreated, result)
}

// ---------------------------------------------------------------------------
// PARSE
// ---------------------------------------------------------------------------

// parseUpload streams the `file` part to a temp file, computing sha256 in flight,
// then reads the manifest. On any rejection it removes the temp directory itself
// and returns a code; on success the caller owns the temp directory.
func (s *Server) parseUpload(r *http.Request) (parsedUpload, string, string) {
	part, err := firstFilePart(r)
	if err != nil {
		return parsedUpload{}, ErrInvalidArtifact, fmt.Sprintf("could not read the upload: %v", err)
	}
	defer func() { _ = part.Close() }()

	tmpDir, err := os.MkdirTemp("", "varroa-uc-upload-")
	if err != nil {
		s.logger.Error("failed to create temp dir for upload", "error", err)
		return parsedUpload{}, ErrInvalidArtifact, "failed to create a temp directory"
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	path := filepath.Join(tmpDir, "upload.hpi")
	f, err := os.Create(path) // #nosec G304 -- path is inside a freshly created temp dir
	if err != nil {
		cleanup()
		s.logger.Error("failed to create temp file for upload", "error", err)
		return parsedUpload{}, ErrInvalidArtifact, "failed to create a temp file"
	}

	// Read one byte past the cap so an exactly-at-cap upload is accepted and
	// anything larger is detected without buffering it.
	hasher := sha256.New()
	limited := io.LimitReader(part, s.maxUploadBytes+1)
	size, copyErr := io.Copy(io.MultiWriter(f, hasher), limited)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		cleanup()
		return parsedUpload{}, ErrInvalidArtifact, "failed to read the uploaded bytes"
	}
	if size > s.maxUploadBytes {
		cleanup()
		return parsedUpload{}, ErrTooLarge,
			fmt.Sprintf("the upload exceeds the %d-byte limit", s.maxUploadBytes)
	}

	mf, err := hpi.ParseHPIFile(path)
	if err != nil {
		cleanup()
		if errors.Is(err, hpi.ErrManifestNotFound) {
			return parsedUpload{}, ErrInvalidArtifact, "the archive contains no META-INF/MANIFEST.MF"
		}
		// hpi.ParseManifest rejects a manifest with no Short-Name or no
		// Plugin-Version; everything else is a malformed artifact.
		if strings.Contains(err.Error(), "has no Short-Name") || strings.Contains(err.Error(), "has no Plugin-Version") {
			return parsedUpload{}, ErrMissingManifestFields, err.Error()
		}
		return parsedUpload{}, ErrInvalidArtifact, err.Error()
	}

	return parsedUpload{
		manifest: mf,
		digest:   "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
		path:     path,
		size:     size,
	}, "", ""
}

// firstFilePart returns the first multipart part named "file".
func firstFilePart(r *http.Request) (*multipart.Part, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("body is not multipart/form-data: %w", err)
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no `file` part in the multipart body")
		}
		if err != nil {
			return nil, err
		}
		if part.FormName() == "file" {
			return part, nil
		}
		_ = part.Close()
	}
}

// ---------------------------------------------------------------------------
// D4 admission
// ---------------------------------------------------------------------------

// admit applies the D4 rule: a fixed version's bytes must never change.
//
// It collects EVERY digest stored for the uploaded name@version across ALL packs
// — findPluginDigest is not sufficient, because it returns the first matching
// layer in ListManifests order, so a store already holding two digests for one
// name@version would be classified by backend ordering. This applies regardless
// of the colliding pack's origin: upload, import, seed, or pull-through.
func (s *Server) admit(ctx context.Context, parsed parsedUpload) (string, string) {
	digests, err := s.findPluginDigests(ctx, parsed.manifest.ShortName, parsed.manifest.Version)
	if err != nil {
		// Fail CLOSED. This check enforces "a fixed version's bytes must never
		// change"; a store we cannot read is not evidence that no conflicting
		// digest exists. Admitting here would commit conflicting bytes for an
		// already-stored name@version and permanently break that invariant,
		// where rejecting only asks the caller to retry.
		return ErrMetadataUnavailable, fmt.Sprintf(
			"cannot verify whether %s@%s is already stored (plugin store unreadable); retry once the store is reachable",
			parsed.manifest.ShortName, parsed.manifest.Version)
	}
	switch {
	case len(digests) == 0:
		return "", ""
	case len(digests) == 1 && digests[0] == parsed.digest:
		return ErrDuplicate, fmt.Sprintf("%s@%s is already stored at this exact digest",
			parsed.manifest.ShortName, parsed.manifest.Version)
	default:
		return ErrVersionDigestConflict, fmt.Sprintf(
			"%s@%s is already stored at a different digest (%s); a fixed version's bytes must never change",
			parsed.manifest.ShortName, parsed.manifest.Version, strings.Join(digests, ", "))
	}
}

// findPluginDigests returns every distinct blob digest stored for name@version,
// across every pack.
// Both a listing error and a skipped manifest are returned as errors, so the
// caller fails the admission closed. Under-reporting is precisely the failure
// mode that matters here: this check exists to catch an already-stored
// name@version, and a manifest we could not read may be the one holding it.
// Admitting on a partial scan would commit conflicting bytes for a fixed
// version — unrecoverable — where rejecting only asks the caller to retry.
func (s *Server) findPluginDigests(ctx context.Context, name, version string) ([]string, error) {
	packs, skipped, err := s.listPackInfos(ctx)
	if err != nil {
		s.logger.Warn("failed to list pack infos for the admission check", "error", err)
		return nil, err
	}
	if len(skipped) > 0 {
		refs := make([]string, len(skipped))
		for i, sp := range skipped {
			refs[i] = sp.Ref
		}
		s.logger.Warn("plugin store scan skipped manifests; failing admission closed",
			"skipped", len(skipped), "refs", refs, "plugin", name, "version", version)
		return nil, fmt.Errorf("plugin store scan skipped %d manifest(s): %s", len(skipped), strings.Join(refs, ", "))
	}
	var out []string
	seen := map[string]struct{}{}
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name != name || p.Version != version || p.LayerDigest == "" {
				continue
			}
			if _, dup := seen[p.LayerDigest]; dup {
				continue
			}
			seen[p.LayerDigest] = struct{}{}
			out = append(out, p.LayerDigest)
		}
	}
	return out, nil
}

// lockUpload serializes the admission-check-through-commit window per
// name@version.
func (s *Server) lockUpload(key string) func() {
	s.uploadLocksMu.Lock()
	mu, ok := s.uploadLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		s.uploadLocks[key] = mu
	}
	s.uploadLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// ---------------------------------------------------------------------------
// COMMIT
// ---------------------------------------------------------------------------

// commitUpload downloads every planned-fetch dependency with sha256
// verification, then writes the packs. Every byte is fetched and verified before
// anything is written, so a fetch failure leaves the store without a new
// manifest.
//
// The uploaded plugin's pack is written LAST, so a partial write never leaves
// the plugin visible without its closure.
//
// DEVIATION from design.md §5.5: this writes one addon pack per plugin rather
// than a single multi-plugin pack. T1.1's frozen contract makes `addon` mean
// exactly one plugin with an empty profile, and task 5.8 requires the upload
// pack to carry the addon kind; a multi-plugin pack could not do both. Writing
// each fetched dependency as a pull-through-shaped addon pack also makes those
// bytes identical to what an on-demand pull-through would have produced, so a
// later pull-through of the same version is a no-op.
func (s *Server) commitUpload(ctx context.Context, parsed parsedUpload, plan Plan, uploadedBy string) (string, error) {
	type fetched struct {
		meta    plannedFetch
		bytes   []byte
		digest  string
		fromURL string
	}

	downloads := make([]fetched, 0, len(plan.Fetches))
	for _, f := range plan.Fetches {
		body, digest, url, err := s.fetchDependency(ctx, f)
		if err != nil {
			return "", fmt.Errorf("fetching dependency %s@%s: %w", f.Name, f.Version, err)
		}
		downloads = append(downloads, fetched{meta: f, bytes: body, digest: digest, fromURL: url})
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for _, d := range downloads {
		plugin := oci.ResolvedPlugin{
			Name:        d.meta.Name,
			Version:     d.meta.Version,
			SHA256:      d.digest,
			UpstreamURL: d.fromURL,
			Content:     strings.NewReader(string(d.bytes)),
		}
		if err := oci.ApplyHPIMetadata(&plugin, d.bytes); err != nil {
			s.logger.Warn("fetched dependency manifest unreadable — packing without derived metadata",
				"plugin", d.meta.Name, "version", d.meta.Version, "error", err)
		}
		ref := packRefFor("pullthrough-", d.meta.Name, d.meta.Version)
		cfg := oci.PackConfig{
			Kind:           oci.PackKindAddon,
			JenkinsVersion: plugin.RequiredCore,
			PluginCount:    1,
			LockHash:       oci.LockHash([]oci.ResolvedPlugin{plugin}),
			CreatedAt:      now,
			UploadedBy:     uploadedBy,
			UploadedAt:     now,
		}
		if err := oci.BuildPluginPack(ctx, s.store, ref, cfg, []oci.ResolvedPlugin{plugin}); err != nil {
			return "", fmt.Errorf("storing dependency %s@%s: %w", d.meta.Name, d.meta.Version, err)
		}
	}

	body, err := os.ReadFile(parsed.path) // #nosec G304 -- our own temp file
	if err != nil {
		return "", fmt.Errorf("re-reading the uploaded artifact: %w", err)
	}
	plugin := oci.ResolvedPlugin{
		Name:         parsed.manifest.ShortName,
		Version:      parsed.manifest.Version,
		SHA256:       parsed.digest,
		DisplayName:  parsed.manifest.LongName,
		RequiredCore: parsed.manifest.RequiredCore,
		Dependencies: parsed.manifest.Dependencies,
		Content:      strings.NewReader(string(body)),
	}
	ref := packRefFor("upload-", plugin.Name, plugin.Version)
	cfg := oci.PackConfig{
		Kind: oci.PackKindAddon,
		// Uploads are not profile-scoped; jenkinsVersion carries the plugin's own
		// Jenkins-Version, which is what the addon contract asks for.
		JenkinsVersion: plugin.RequiredCore,
		PluginCount:    1,
		LockHash:       oci.LockHash([]oci.ResolvedPlugin{plugin}),
		CreatedAt:      now,
		UploadedBy:     uploadedBy,
		UploadedAt:     now,
	}
	if err := oci.BuildPluginPack(ctx, s.store, ref, cfg, []oci.ResolvedPlugin{plugin}); err != nil {
		return "", fmt.Errorf("storing %s@%s: %w", plugin.Name, plugin.Version, err)
	}
	return ref, nil
}

// fetchDependency downloads one planned-fetch dependency and verifies its
// sha256, reusing doPullThrough's shape (upstream base64 → raw → hex → compare).
func (s *Server) fetchDependency(ctx context.Context, f plannedFetch) ([]byte, string, string, error) {
	if f.SHA256 == "" {
		return nil, "", "", errors.New("no upstream checksum is available for this version")
	}
	url := fmt.Sprintf("%s/plugins/%s/%s/%s.hpi",
		strings.TrimRight(s.pullThroughDownloadURL, "/"), f.Name, f.Version, f.Name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxUploadBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(body)) > s.maxUploadBytes {
		return nil, "", "", errors.New("the dependency exceeds the upload byte cap")
	}

	raw, err := base64.StdEncoding.DecodeString(f.SHA256)
	if err != nil {
		return nil, "", "", fmt.Errorf("upstream sha256 is not valid base64: %w", err)
	}
	expected := "sha256:" + hex.EncodeToString(raw)
	sum := sha256.Sum256(body)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != expected {
		return nil, "", "", fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	return body, actual, url, nil
}

// packRefFor builds a tag-safe, stable, collision-resistant pack ref.
func packRefFor(prefix, name, version string) string {
	sum := sha256.Sum256([]byte(name + "@" + version))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// packStoreLookup adapts the Server's pack listing to the planner's narrow
// store interface.
type packStoreLookup struct{ srv *Server }

func (l *packStoreLookup) Versions(ctx context.Context, name string) []string {
	packs, _, err := l.srv.listPackInfos(ctx)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name != name || p.Version == "" {
				continue
			}
			if _, dup := seen[p.Version]; dup {
				continue
			}
			seen[p.Version] = struct{}{}
			out = append(out, p.Version)
		}
	}
	return out
}

func (l *packStoreLookup) Dependencies(ctx context.Context, name, version string) ([]hpi.Dependency, bool) {
	packs, _, err := l.srv.listPackInfos(ctx)
	if err != nil {
		return nil, false
	}
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name != name || p.Version != version {
				continue
			}
			// A pack config with no kind predates the annotation contract, so its
			// silence about dependencies proves nothing.
			if pack.Config.Kind == "" {
				continue
			}
			return p.Dependencies, true
		}
	}
	return nil, false
}

// uploadResolver returns the metadata resolver, or nil when pull-through is off.
func (s *Server) uploadResolver() metaResolver {
	if s.resolver == nil {
		return nilResolver{}
	}
	return s.resolver
}

// nilResolver stands in when pull-through is disabled. The decision tree never
// reaches it in that configuration, but a nil interface would be a panic waiting
// for a future edit.
type nilResolver struct{}

func (nilResolver) ResolveExact(context.Context, string, string) ucmeta.Resolution {
	return ucmeta.Resolution{Outcome: ucmeta.NotListed}
}
func (nilResolver) ResolveSatisfying(context.Context, string, string) ucmeta.Resolution {
	return ucmeta.Resolution{Outcome: ucmeta.NotListed}
}

// rejectUpload writes a rejection envelope with no per-dependency diff.
func (s *Server) rejectUpload(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, UploadRejection{Error: code, Message: msg})
}

// statusForCode maps a PARSE/admission code onto its HTTP status.
func statusForCode(code string) int {
	switch code {
	case ErrInvalidArtifact, ErrMissingManifestFields:
		return http.StatusBadRequest
	case ErrDuplicate, ErrVersionDigestConflict:
		return http.StatusConflict
	case ErrTooLarge:
		return http.StatusRequestEntityTooLarge
	case ErrUploadsRequireSingleWri:
		return http.StatusNotImplemented
	case ErrDeclaredSetUnavailable, ErrMetadataUnavailable:
		return http.StatusServiceUnavailable
	case ErrFetchFailed:
		return http.StatusBadGateway
	default:
		return http.StatusUnprocessableEntity
	}
}

func nonNilClosure(v []ClosureEntry) []ClosureEntry {
	if v == nil {
		return []ClosureEntry{}
	}
	return v
}

func nonNilOptional(v []OptionalDependency) []OptionalDependency {
	if v == nil {
		return []OptionalDependency{}
	}
	return v
}

func nonNilWarnings(v []UploadWarning) []UploadWarning {
	if v == nil {
		return []UploadWarning{}
	}
	return v
}
