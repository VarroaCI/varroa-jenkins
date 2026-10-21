package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
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

	// --- Steps 1-3: resolve, download, verify (no store writes) ---
	pack, err := preparePack(cmd, profile, pluginsFile, downloadURLBase)
	if err != nil {
		return err
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

	// The dual-tag strategy applies only to an oci:// destination with no
	// explicit tag: the pack is pushed under the profile name (the floating
	// tag) and then also tagged with its lockHash (the immutable one).
	//
	// Every other case pushes to `target` verbatim — an explicit :tag is used as
	// given, and dir:///tar:// have no tags at all. floatingRef stays empty in
	// those cases and is never read; only the dualTag branch uses it.
	var floatingRef string
	dualTag := scheme == "oci" && !hasExplicitTag(target)
	if dualTag {
		floatingRef = profile
	}

	buildRef := target
	if dualTag {
		buildRef = profile
	}

	if err := pack.write(cmd.Context(), store, buildRef); err != nil {
		_ = finalize()
		return fmt.Errorf("build plugin pack: %w", err)
	}

	// --- Step 4: Dual-tag strategy (2.4a) ---
	if dualTag {
		if err := applyDualTag(cmd.Context(), store, profile, pack.cfg.LockHash); err != nil {
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
			"pluginCount": len(pack.resolved),
			"ref":         resolveRef,
		}
		if len(pack.unreadable) > 0 {
			// Named, not silent: these plugins are packed but carry no derived
			// metadata because their HPI manifest would not parse.
			out["unreadableManifests"] = pack.unreadable
		}
		return printJSON(os.Stdout, out)
	}

	fmt.Printf("Exported %d plugins with digest %s\n", len(pack.resolved), desc.Digest)
	if len(pack.unreadable) > 0 {
		fmt.Printf("Warning: %d plugin(s) packed without derived metadata (unreadable HPI manifest): %s\n",
			len(pack.unreadable), strings.Join(pack.unreadable, ", "))
	}
	return nil
}

// preparedPack is a plugin pack whose plugins are downloaded and verified
// but not yet written to a store. write is the only store operation, so a
// caller can open its destination after the network work succeeds.
type preparedPack struct {
	resolved       []oci.ResolvedPlugin
	unreadable     []string
	jenkinsVersion string // the profile's Jenkins version, "" in plugins-file mode
	coreVersion    string // the core the profile deploys: resolveVersion, else jenkinsVersion
	cfg            oci.PackConfig
}

// preparePack resolves a profile's closure, downloads and verifies each plugin,
// and returns the pack configuration. It writes nothing.
func preparePack(cmd *cobra.Command, profile, pluginsFile, downloadURLBase string) (preparedPack, error) {
	downloadURLBase = strings.TrimRight(downloadURLBase, "/")

	entries, jenkinsVersion, err := resolvePluginClosure(cmd, profile, pluginsFile, downloadURLBase)
	if err != nil {
		return preparedPack{}, err
	}
	coreVersion := jenkinsVersion
	if len(entries) > 0 && entries[0].ResolveVersion != "" {
		coreVersion = entries[0].ResolveVersion
	}

	resolvedPlugins, unreadable, err := downloadAndVerify(cmd.Context(), downloadURLBase, jenkinsVersion, entries)
	if err != nil {
		return preparedPack{}, err
	}

	return preparedPack{
		resolved:       resolvedPlugins,
		unreadable:     unreadable,
		jenkinsVersion: jenkinsVersion,
		coreVersion:    coreVersion,
		cfg: oci.PackConfig{
			Kind:           oci.PackKindProfile,
			JenkinsVersion: jenkinsVersion,
			Profile:        profile,
			LockHash:       oci.LockHash(resolvedPlugins),
			PluginCount:    len(resolvedPlugins),
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		},
	}, nil
}

// write builds the pack under ref in store. It is the only store write.
func (p preparedPack) write(ctx context.Context, store oci.BlobStore, ref string) error {
	return oci.BuildPluginPack(ctx, store, ref, p.cfg, p.resolved)
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
			// resolution: a pack with no plugins is never a valid artifact. The
			// message must let an operator tell a mistyped --profile from a
			// broken plugin set.
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
func downloadAndVerify(ctx context.Context, downloadURLBase, jenkinsVersion string, entries []resolvedEntry) ([]oci.ResolvedPlugin, []string, error) {
	// One resolver per run: its sources are computed once and its per-source
	// cache makes each metadata document a single fetch, not one per plugin.
	resolver := newExportResolver(downloadURLBase, entries)
	entries, err := resolveLatestPins(ctx, resolver, jenkinsVersion, entries)
	if err != nil {
		return nil, nil, err
	}

	resolved := make([]oci.ResolvedPlugin, 0, len(entries))
	var unreadable []string
	for _, e := range entries {
		dp, err := downloadOnePlugin(ctx, downloadURLBase, resolver, e)
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

// resolveLatestPins replaces every latest entry with a concrete version before
// any download. A pack never carries the word latest: its lock hash and its
// checksum both need one release. The core that filters the choice is the
// entry's own ResolveVersion, else the profile's Jenkins version. Offline
// mode passes no core, so the newest listed version wins.
func resolveLatestPins(ctx context.Context, resolver *ucmeta.Resolver, jenkinsVersion string, entries []resolvedEntry) ([]resolvedEntry, error) {
	out := make([]resolvedEntry, len(entries))
	for i, e := range entries {
		out[i] = e
		if !ucmeta.IsLatest(e.Version) {
			continue
		}
		core := e.ResolveVersion
		if core == "" {
			core = jenkinsVersion
		}
		a := resolver.ResolveLatestForCore(ctx, e.Name, core)
		if a.Outcome != ucmeta.Resolved || a.Version == "" {
			return nil, latestPinError(e.Name, core, a.Outcome)
		}
		fmt.Fprintf(os.Stderr, "resolved %s@latest to %s\n", e.Name, a.Version)
		out[i].Version = a.Version
	}
	return out, nil
}

// latestPinError names the reason a latest pin could not resolve.
func latestPinError(name, core string, outcome ucmeta.Outcome) error {
	switch {
	case outcome == ucmeta.SourcesDegraded:
		return fmt.Errorf("resolve %s@latest: an update-center metadata source could not be read; retry or pin an exact version", name)
	case core != "":
		return fmt.Errorf("resolve %s@latest: no update-center metadata source lists a version that runs on Jenkins %s; pin an exact version", name, core)
	default:
		return fmt.Errorf("resolve %s@latest: no update-center metadata source lists the plugin; pin an exact version", name)
	}
}

// userAgentRoundTripper stamps a User-Agent on every request it forwards.
//
// ucmeta sets none of its own, and an export has always identified itself as
// varroactl/<version>. Wrapping the client the resolver is built with covers
// both its metadata fetches and its archive-sidecar fetches.
type userAgentRoundTripper struct {
	rt http.RoundTripper
	ua string
}

func (u userAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating — RoundTrip must not modify the caller's request.
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", u.ua)
	return u.rt.RoundTrip(r)
}

// newExportResolver builds the checksum resolver for one export run.
//
// Sources are the weekly metadata first, then one dynamic-stable document per
// distinct non-empty ResolveVersion in first-seen order. They are computed once
// here and returned by a closure that always yields the same slice: ucmeta
// re-evaluates sources() on every ResolveSHA256 call, so building the list
// inside the closure would redo the work for every plugin.
//
// Every source derives from downloadURLBase, so offline mirrors and test
// servers keep working. The archive base is left at ucmeta's default; only
// tests move it.
func newExportResolver(downloadURLBase string, entries []resolvedEntry) *ucmeta.Resolver {
	base := strings.TrimRight(downloadURLBase, "/")
	srcs := []ucmeta.Source{{URL: base + "/update-center.actual.json"}}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.ResolveVersion == "" || seen[e.ResolveVersion] {
			continue
		}
		seen[e.ResolveVersion] = true
		srcs = append(srcs, ucmeta.Source{
			URL: fmt.Sprintf("%s/dynamic-stable-%s/update-center.actual.json", base, e.ResolveVersion),
		})
	}
	client := &http.Client{Transport: userAgentRoundTripper{rt: http.DefaultTransport, ua: "varroactl/" + version}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// The TTL is only ever compared against this run's own fetches, so it just
	// has to outlast the run: a large export downloads blobs serially and can
	// take a while.
	resolver := ucmeta.NewResolver(func() []ucmeta.Source { return srcs }, 24*time.Hour, client, logger)
	// An export is one run. A failed source is not fetched again in the run, so a dead source costs one fetch.
	resolver.SetFailureRetryLimit(0)
	return resolver
}

// sha256FromBase64 converts upstream's base64 digest to the "sha256:<hex>" form
// oci.Sha256Digest produces.
//
// The length check matters as much as the decode: base64 that is well-formed but
// carries the wrong number of bytes would otherwise produce a short or long hex
// string that simply fails the equality test downstream, reporting bad metadata
// as a sha256 MISMATCH — a supply-chain alarm — instead of as bad metadata.
func sha256FromBase64(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(raw) != sha256.Size {
		return "", fmt.Errorf("expected a %d-byte digest, got %d bytes", sha256.Size, len(raw))
	}
	return fmt.Sprintf("sha256:%x", raw), nil
}

// downloadOnePlugin downloads a single .hpi and verifies its sha256.
// The blob download URL always uses the original downloadURLBase root; the
// metadata source a checksum came from never influences it.
func downloadOnePlugin(ctx context.Context, downloadURLBase string, resolver *ucmeta.Resolver, entry resolvedEntry) (downloadedPlugin, error) {
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

	// Resolve the declared sha256: metadata sources first, then the artifact
	// archive's coordinate-addressed sidecar for versions no metadata lists.
	//
	// ErrVersionUnavailable is one sentinel covering a genuine absence, sources
	// that were skipped as unreachable, and an archive lookup that ran and
	// failed — so the message must not claim which of those happened.
	b64, err := resolver.ResolveSHA256(ctx, entry.Name, entry.Version)
	if err != nil {
		return downloadedPlugin{}, fmt.Errorf(
			"resolve sha256 for %s@%s: could not resolve from update-center metadata or the archive fallback: %w",
			entry.Name, entry.Version, err)
	}
	declaredSHA256, err := sha256FromBase64(b64)
	if err != nil {
		return downloadedPlugin{}, fmt.Errorf("decode sha256 for %s@%s: %w", entry.Name, entry.Version, err)
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

// hasExplicitTag reports whether an OCI target carries an explicit :tag.
//
// A bare strings.Contains(target, ":") cannot be used: a registry host may carry
// a port ("localhost:5099/varroa/plugin-pack"), which is a colon that is not a
// tag. Since a tag may not contain "/", a colon is a tag separator only when it
// appears after the last "/". Getting this wrong makes every ported registry
// skip the dual-tag path and then build a malformed reference.
func hasExplicitTag(target string) bool {
	colon := strings.LastIndex(target, ":")
	if colon < 0 {
		return false
	}
	return colon > strings.LastIndex(target, "/")
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
