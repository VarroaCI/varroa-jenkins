package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		root.AddCommand(newExportCmd())
	})
}

// pluginsFileFormat is the local struct for parsing --plugins-file YAML.
// D6 boundary: cmd/varroactl does NOT import internal/controller/*.
type pluginsFileFormat struct {
	Core    []string                 `json:"core" yaml:"core"`
	Plugins []pluginsFilePluginEntry `json:"plugins" yaml:"plugins"`
}

type pluginsFilePluginEntry struct {
	ArtifactID string `json:"artifactId" yaml:"artifactId"`
	Version    string `json:"version" yaml:"version"`
}

func newExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export plugins --profile <name> --to <dest> [flags]",
		Short: "Export a plugin pack to an OCI destination",
		Long: `Export resolves a version profile's plugin closure, downloads and verifies each plugin,
and builds an OCI plugin pack at the destination.

The --to destination uses the following schemes:
  oci://<registry>/<repo>[:<tag>]   push to an OCI registry
  dir://<path>                      write to an OCI layout directory
  tar://<path>.tar.gz                write to a gzipped tar archive`,
		RunE: runExport,
	}

	cmd.Flags().String("profile", "", "version profile name (required)")
	_ = cmd.MarkFlagRequired("profile")
	cmd.Flags().String("plugins-file", "", "offline plugin lock file (hack/gen-plugin-lock.sh format)")
	cmd.Flags().String("to", "", "destination (oci://, dir://, tar://) (required)")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().String("download-url-base", "https://updates.jenkins.io", "base URL for plugin downloads")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")
	addClusterFlag(cmd)

	return cmd
}

func runExport(cmd *cobra.Command, args []string) error {
	// Validate -o flag
	if cmd.Flags().Changed("output") {
		o, _ := cmd.Flags().GetString("output")
		if o != "json" {
			return usagef("export only supports -o json; got %q", o)
		}
	}

	profile, _ := cmd.Flags().GetString("profile")
	pluginsFile, _ := cmd.Flags().GetString("plugins-file")
	toDest, _ := cmd.Flags().GetString("to")
	downloadURLBase, _ := cmd.Flags().GetString("download-url-base")
	registryConfig, _ := cmd.Flags().GetString("registry-config")
	insecure, _ := cmd.Flags().GetBool("insecure")

	downloadURLBase = strings.TrimRight(downloadURLBase, "/")

	// --- Step 1: Resolve closure ---
	resolved, jenkinsVersion, err := resolvePluginClosure(cmd, profile, pluginsFile, downloadURLBase)
	if err != nil {
		return err
	}

	// --- Step 2: Phase A — download + verify (no store writes) ---
	resolvedPlugins, unreadableManifests, err := downloadAndVerify(cmd.Context(), downloadURLBase, resolved)
	if err != nil {
		return err
	}

	// --- Step 3: Phase B — build pack (single store write) ---
	lockHash := oci.LockHash(resolvedPlugins)
	cfg := oci.PackConfig{
		Kind:           oci.PackKindProfile,
		JenkinsVersion: jenkinsVersion,
		Profile:        profile,
		LockHash:       lockHash,
		PluginCount:    len(resolvedPlugins),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	// Determine ref for BuildPluginPack
	scheme, target, err := ParseOCIDest(toDest)
	if err != nil {
		// Check if it's our unrecognized-scheme error
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}

	store, finalize, err := openOCIDest(scheme, target, registryConfig, insecure)
	if err != nil {
		return err
	}

	// Determine ref and whether dual-tag strategy applies
	// Dual-tag: only when scheme=="oci" AND no explicit :tag
	var floatingRef string
	dualTag := scheme == "oci" && !strings.Contains(target, ":")
	if dualTag {
		floatingRef = profile
	} else if scheme == "oci" {
		// Explicit tag: ref is everything after the last colon in the target
		// target is like "registry/repo:tag" — we use the profile as the tag
		// Actually, the spec says: explicit :tag → use that tag directly
		// So ref = target as-is (since it already contains :tag)
		floatingRef = "" // not used
	}

	buildRef := target
	if dualTag {
		buildRef = profile
	}

	if err := oci.BuildPluginPack(cmd.Context(), store, buildRef, cfg, resolvedPlugins); err != nil {
		_ = finalize()
		return fmt.Errorf("build plugin pack: %w", err)
	}

	// --- Step 4: Dual-tag strategy (2.4a) ---
	if dualTag {
		if err := applyDualTag(cmd.Context(), store, profile, lockHash); err != nil {
			_ = finalize()
			return err
		}
	}

	// --- Step 5: Finalize (for tar://) ---
	if err := finalize(); err != nil {
		return fmt.Errorf("finalize destination: %w", err)
	}

	// --- Step 6: Output ---
	resolveRef := buildRef
	if dualTag {
		resolveRef = floatingRef
	}
	desc, err := store.Resolve(cmd.Context(), resolveRef)
	if err != nil {
		return fmt.Errorf("resolve manifest: %w", err)
	}

	outputJSON, _ := cmd.Flags().GetString("output")
	if outputJSON == "json" {
		out := map[string]any{
			"digest":      desc.Digest,
			"pluginCount": len(resolvedPlugins),
			"ref":         resolveRef,
		}
		if len(unreadableManifests) > 0 {
			// Named, not silent: these plugins are packed but carry no derived
			// metadata because their HPI manifest would not parse.
			out["unreadableManifests"] = unreadableManifests
		}
		return printJSON(os.Stdout, out)
	}

	fmt.Printf("Exported %d plugins with digest %s\n", len(resolvedPlugins), desc.Digest)
	if len(unreadableManifests) > 0 {
		fmt.Printf("Warning: %d plugin(s) packed without derived metadata (unreadable HPI manifest): %s\n",
			len(unreadableManifests), strings.Join(unreadableManifests, ", "))
	}
	return nil
}

// resolvePluginClosure resolves the plugin closure either from the BFF API or
// from a local --plugins-file.
func resolvePluginClosure(cmd *cobra.Command, profile, pluginsFile, downloadURLBase string) ([]resolvedEntry, string, error) {
	if pluginsFile != "" {
		return resolveFromPluginsFile(pluginsFile, profile)
	}
	return resolveFromBFF(cmd, profile, downloadURLBase)
}

// resolvedEntry is an intermediate representation: a single name@version line.
type resolvedEntry struct {
	Name           string
	Version        string
	ResolveVersion string // LTS-line exact patch, empty for weekly/exact-pin profiles
}

// resolveFromBFF fetches the version profile from the BFF API.
func resolveFromBFF(cmd *cobra.Command, profile string, _ string) ([]resolvedEntry, string, error) {
	cluster := resolveCrdCluster(cmd)
	path := "/clusters/" + cluster + "/version-profiles"

	httpResp, err := rawRequest(cmd, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("fetch version profiles: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return nil, "", errFromResponse(b, httpResp.StatusCode)
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read version profiles response: %w", err)
	}

	var envelope struct {
		Items []struct {
			Name           string   `json:"name"`
			Version        string   `json:"version"`
			ResolveVersion string   `json:"resolveVersion"`
			Plugins        []string `json:"plugins"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, "", fmt.Errorf("decode version profiles: %w", err)
	}

	for _, item := range envelope.Items {
		if item.Name == profile {
			entries := make([]resolvedEntry, 0, len(item.Plugins))
			for _, line := range item.Plugins {
				parts := strings.SplitN(line, "@", 2)
				if len(parts) != 2 || parts[1] == "" {
					return nil, "", fmt.Errorf("plugin line %q in profile %q lacks @version", line, profile)
				}
				entries = append(entries, resolvedEntry{Name: parts[0], Version: parts[1], ResolveVersion: item.ResolveVersion})
			}
			// Fail closed, regardless of the status that accompanied the empty
			// resolution: a pack with no plugins is never a valid artifact, and
			// publishing one silently is the #416 failure mode. The message must
			// let an operator tell a mistyped --profile from a broken plugin set.
			if len(entries) == 0 {
				return nil, "", fmt.Errorf(
					"version profile %q resolved to no plugins: refusing to build an empty pack "+
						"(the profile exists but its plugin set is empty or unreadable)", profile)
			}
			return entries, item.Version, nil
		}
	}

	return nil, "", fmt.Errorf("version profile %q not found", profile)
}

// resolveFromPluginsFile parses a --plugins-file YAML and flattens core/plugins.
func resolveFromPluginsFile(path string, _ string) ([]resolvedEntry, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read plugins file: %w", err)
	}

	var pf pluginsFileFormat
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, "", fmt.Errorf("parse plugins file: %w", err)
	}

	// Build a lookup from artifactId -> version
	pluginMap := make(map[string]string, len(pf.Plugins))
	for _, p := range pf.Plugins {
		pluginMap[p.ArtifactID] = p.Version
	}

	// Determine the export set. An explicit `core:` list selects exactly those
	// plugins (each must resolve to a version in `plugins:`); when `core:` is
	// omitted, every plugin in `plugins:` is exported. Preserve file order for
	// deterministic packs.
	entries := make([]resolvedEntry, 0, len(pf.Plugins))
	if len(pf.Core) > 0 {
		for _, name := range pf.Core {
			version, ok := pluginMap[name]
			if !ok {
				return nil, "", fmt.Errorf("core plugin %q not found in plugins list", name)
			}
			entries = append(entries, resolvedEntry{Name: name, Version: version})
		}
	} else {
		for _, p := range pf.Plugins {
			entries = append(entries, resolvedEntry{Name: p.ArtifactID, Version: p.Version})
		}
	}

	// An empty export is never intended: it produces a valid-but-useless pack
	// that fails air-gapped installs mysteriously (Jenkins finds no plugins).
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("plugins file %q lists no plugins to export", path)
	}

	// JenkinsVersion is "" in offline mode (D2 boundary, not an error).
	return entries, "", nil
}

// downloadAndVerify performs Phase A: downloads each .hpi and verifies its sha256
// against the update-center.actual.json. Returns ResolvedPlugin slices with
// verified content, plus the names of plugins whose HPI manifest would not
// parse.
//
// A manifest that will not parse is NOT fatal here: identity comes from the
// resolved lock entry, so the pack is still correct and installable. An
// 84-plugin profile export must not fail wholesale because one upstream
// artifact has an odd manifest — the derived annotations are omitted, a warning
// is logged, and the plugin is named in the command's output.
func downloadAndVerify(ctx context.Context, downloadURLBase string, entries []resolvedEntry) ([]oci.ResolvedPlugin, []string, error) {
	// Fetch update-center.actual.json once per run
	ucURL := downloadURLBase + "/update-center.actual.json"
	ucData, err := fetchUpdateCenterJSON(ctx, ucURL)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch update-center metadata: %w", err)
	}

	resolved := make([]oci.ResolvedPlugin, 0, len(entries))
	var unreadable []string
	for _, e := range entries {
		dp, err := downloadOnePlugin(ctx, downloadURLBase, ucData, e)
		if err != nil {
			return nil, nil, err
		}
		if dp.metadataErr != nil {
			fmt.Fprintf(os.Stderr, "warning: %v; packing %s@%s without derived metadata\n", dp.metadataErr, e.Name, e.Version)
			unreadable = append(unreadable, e.Name)
		}
		resolved = append(resolved, dp.plugin)
	}
	return resolved, unreadable, nil
}

// fetchUpdateCenterJSON downloads and parses the update-center JSON.
func fetchUpdateCenterJSON(ctx context.Context, ucURL string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ucURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "varroactl/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("update-center returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read update-center JSON: %w", err)
	}

	var uc map[string]any
	if err := json.Unmarshal(body, &uc); err != nil {
		return nil, fmt.Errorf("parse update-center JSON: %w", err)
	}
	return uc, nil
}

// downloadOnePlugin downloads a single .hpi and verifies its sha256.
// The blob download URL always uses the original downloadURLBase root.
// On version mismatch, if the entry has a ResolveVersion, lookupPluginSHA256
// falls back to the dynamic-stable-<resolveVersion> metadata endpoint.
func downloadOnePlugin(ctx context.Context, downloadURLBase string, ucData map[string]any, entry resolvedEntry) (downloadedPlugin, error) {
	pluginURL := fmt.Sprintf("%s/download/plugins/%s/%s/%s.hpi",
		downloadURLBase, entry.Name, entry.Version, entry.Name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pluginURL, nil)
	if err != nil {
		return downloadedPlugin{}, err
	}
	req.Header.Set("User-Agent", "varroactl/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return downloadedPlugin{}, fmt.Errorf("download %s@%s: %w", entry.Name, entry.Version, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return downloadedPlugin{}, fmt.Errorf("download %s@%s: HTTP %d", entry.Name, entry.Version, resp.StatusCode)
	}

	// Read into memory, then compute sha256
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return downloadedPlugin{}, fmt.Errorf("read %s@%s: %w", entry.Name, entry.Version, err)
	}

	computedDigest, _, err := oci.Sha256Digest(bytes.NewReader(data))
	if err != nil {
		return downloadedPlugin{}, fmt.Errorf("digest %s@%s: %w", entry.Name, entry.Version, err)
	}

	// Look up declared sha256 from update-center JSON
	declaredSHA256, err := lookupPluginSHA256(ctx, ucData, entry.Name, entry.Version, entry.ResolveVersion, downloadURLBase)
	if err != nil {
		return downloadedPlugin{}, fmt.Errorf("lookup %s@%s in update-center: %w", entry.Name, entry.Version, err)
	}

	if computedDigest != declaredSHA256 {
		return downloadedPlugin{}, fmt.Errorf(
			"sha256 mismatch for %s@%s: downloaded %s, declared %s",
			entry.Name, entry.Version, computedDigest, declaredSHA256)
	}

	rp := oci.ResolvedPlugin{
		Name:        entry.Name,
		Version:     entry.Version,
		SHA256:      computedDigest,
		UpstreamURL: pluginURL,
		Content:     bytes.NewReader(data),
	}
	// Derived metadata (displayName, requiredCore, dependencies) comes from the
	// plugin's own manifest. A parse failure is non-fatal for a bulk export.
	metaErr := oci.ApplyHPIMetadata(&rp, data)
	return downloadedPlugin{plugin: rp, metadataErr: metaErr}, nil
}

// downloadedPlugin pairs a verified plugin with the non-fatal outcome of
// reading its HPI manifest. A nil metadataErr means the derived annotations
// are populated; a non-nil one means they were omitted and the plugin must be
// named in the command's output.
type downloadedPlugin struct {
	plugin      oci.ResolvedPlugin
	metadataErr error
}

// ucPluginEntry is the shape of each entry in update-center.actual.json's
// "plugins" map.  The real schema is a flat object (not nested by version),
// and sha256 is base64-encoded.
type ucPluginEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"` // base64-encoded sha256 digest
}

// lookupPluginSHA256 finds the declared sha256 for a plugin in the update-center JSON.
// The update-center stores plugins[name] as a flat object with name, version, and
// sha256 (base64-encoded). Only the current version is listed, so a version
// mismatch gets a clear error.
//
// If resolveVersion is set and the current lookup fails on version mismatch,
// the function retries once against the dynamic-stable-<resolveVersion> metadata
// endpoint before erroring. The blob download URL always uses the original root
// base (downloadURLBase) — this fallback changes only the metadata lookup URL.
func lookupPluginSHA256(ctx context.Context, ucData map[string]any, name, version, resolveVersion, downloadURLBase string) (string, error) {
	sha256, err := lookupPluginSHA256FromData(ucData, name, version)
	if err == nil {
		return sha256, nil
	}

	// On version mismatch, retry once against dynamic-stable-<resolveVersion>.
	if resolveVersion != "" && isVersionMismatch(err) {
		stableURL := fmt.Sprintf("%s/dynamic-stable-%s/update-center.actual.json",
			strings.TrimRight(downloadURLBase, "/"), resolveVersion)
		stableUC, fetchErr := fetchUpdateCenterJSON(ctx, stableURL)
		if fetchErr == nil {
			sha256, retryErr := lookupPluginSHA256FromData(stableUC, name, version)
			if retryErr == nil {
				return sha256, nil
			}
		}
	}

	return "", err
}

// isVersionMismatch reports whether an error from lookupPluginSHA256FromData
// is a version-mismatch error (current version differs from requested).
func isVersionMismatch(err error) bool {
	return strings.Contains(err.Error(), "not available in update-center (current:")
}

// lookupPluginSHA256FromData performs the actual sha256 lookup in a single
// update-center JSON blob without any dynamic-stable fallback.
func lookupPluginSHA256FromData(ucData map[string]any, name, version string) (string, error) {
	plugins, ok := ucData["plugins"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("update-center JSON missing 'plugins' key")
	}

	pData, ok := plugins[name].(map[string]any)
	if !ok {
		return "", fmt.Errorf("plugin %q not found in update-center", name)
	}

	// Marshal back to JSON so we can unmarshal into our typed struct
	pJSON, err := json.Marshal(pData)
	if err != nil {
		return "", fmt.Errorf("marshal plugin %q entry: %w", name, err)
	}
	var entry ucPluginEntry
	if err := json.Unmarshal(pJSON, &entry); err != nil {
		return "", fmt.Errorf("unmarshal plugin %q entry: %w", name, err)
	}

	if entry.Version != version {
		return "", fmt.Errorf(
			"version %q of plugin %q not available in update-center (current: %q); "+
				"export pins must match the current version",
			version, name, entry.Version)
	}

	if entry.SHA256 == "" {
		return "", fmt.Errorf("no sha256 for %s@%s in update-center", name, version)
	}

	// Decode base64 → raw bytes → hex-encode → prefix with "sha256:"
	raw, err := base64.StdEncoding.DecodeString(entry.SHA256)
	if err != nil {
		return "", fmt.Errorf("decode base64 sha256 for %s@%s: %w", name, version, err)
	}

	return fmt.Sprintf("sha256:%x", raw), nil
}

// applyDualTag implements the 2.4a dual-tag strategy.
// floatingRef is the already-pushed floating tag (== profile name).
func applyDualTag(ctx context.Context, store oci.BlobStore, profile, lockHash string) error {
	manifest, err := store.Pull(ctx, profile)
	if err != nil {
		return fmt.Errorf("pull floating tag %q: %w", profile, err)
	}

	// Get the lockHash from the manifest annotations
	storedLockHash := manifest.Annotations["dev.varroa.pack.lockHash"]
	if storedLockHash == "" {
		return fmt.Errorf("floating tag %q missing lockHash annotation", profile)
	}

	lockHash12 := storedLockHash[:12]
	if len(storedLockHash) < 12 {
		return fmt.Errorf("lockHash too short: %q", storedLockHash)
	}

	immutableRef := profile + "-" + lockHash12

	// Resolve immutable tag
	_, err = store.Resolve(ctx, immutableRef)
	if err != nil {
		// Not found → retag (Push with same manifest)
		return store.Push(ctx, immutableRef, manifest)
	}

	// Found — check full lockHash
	existingManifest, err := store.Pull(ctx, immutableRef)
	if err != nil {
		return fmt.Errorf("pull immutable tag %q: %w", immutableRef, err)
	}

	existingFullLockHash := existingManifest.Annotations["dev.varroa.pack.lockHash"]
	if existingFullLockHash == lockHash {
		// Identical — no-op
		return nil
	}

	// lockHash12 collision
	return fmt.Errorf(
		"lockHash12 collision on immutable tag %q: existing full lockHash=%s, new full lockHash=%s",
		immutableRef, existingFullLockHash, lockHash)
}
