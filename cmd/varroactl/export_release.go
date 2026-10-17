package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		exportCmd := findSubCommand(root, "export")
		if exportCmd == nil {
			exportCmd = newExportCmd()
			root.AddCommand(exportCmd)
		}
		exportCmd.AddCommand(newExportReleaseCmd())
	})
}

// defaultReleaseChart is the chart the public release publishes. The export
// rewrites its owner on the public side, like every other ghcr.io reference.
const defaultReleaseChart = "oci://ghcr.io/varroaci/charts/varroa"

// releaseDownloadURLBase is where the export reads plugin metadata and archives.
// Tests replace it with a local server.
var releaseDownloadURLBase = "https://updates.jenkins.io"

// releaseExportOpts holds the flags of export release.
type releaseExportOpts struct {
	Version        string
	Values         []string
	Profiles       []string
	Chart          string
	BundleDirs     []string
	Images         []string
	Namespace      string
	Helm           string
	To             string
	RegistryConfig string
	Insecure       bool
}

func newExportReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release --version <v> --profile <name> --to <dest> [flags]",
		Short: "Export one air-gap transfer bundle for a Varroa release",
		Long: `Export release renders the Varroa chart with your values files and collects
every image the rendered chart names. It also exports each named version
profile as a plugin pack. It writes one artifact: the chart, the images, the
packs, and a release-manifest.json that records a SHA-256 for every blob.

The export needs a connected Varroa of the same version, because the profile
closures come from its API. It needs the helm binary to render the chart.

Each image must carry an exact tag or a digest. The tag latest is refused.
Every image must come from a chart value, a profile, a --bundle-dir, or an
--image flag. The export cannot read images in pipeline scripts or in pod
templates that users create through the API.

The --to destination uses the following schemes:
  tar://<path>.tar.gz   write one gzipped archive
  dir://<path>          write an OCI layout directory (must be empty or absent)
  oci://<registry>/<repo>  push every artifact to a repository, one tag each`,
		RunE: runExportRelease,
	}

	cmd.Flags().String("version", "", "Varroa release version (required)")
	cmd.Flags().StringArray("values", nil, "values file the release is deployed with (repeatable)")
	cmd.Flags().StringArray("profile", nil, "version profile to export as a plugin pack (repeatable, required)")
	cmd.Flags().String("chart", defaultReleaseChart, "chart: an oci:// reference (tag defaults to --version), a .tgz, or a chart directory")
	cmd.Flags().StringArray("bundle-dir", nil, "bundle directory whose JCasC pod templates name images (repeatable)")
	cmd.Flags().StringArray("image", nil, "extra image reference to include (repeatable)")
	cmd.Flags().String("namespace", "varroa-system", "release namespace used to render the chart")
	cmd.Flags().String("helm", "helm", "helm binary used to render the chart")
	cmd.Flags().String("to", "", "destination: tar://, dir://, or oci:// (required)")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")
	addClusterFlag(cmd)

	return cmd
}

func readReleaseExportOpts(cmd *cobra.Command) releaseExportOpts {
	var o releaseExportOpts
	o.Version, _ = cmd.Flags().GetString("version")
	o.Values, _ = cmd.Flags().GetStringArray("values")
	o.Profiles, _ = cmd.Flags().GetStringArray("profile")
	o.Chart, _ = cmd.Flags().GetString("chart")
	o.BundleDirs, _ = cmd.Flags().GetStringArray("bundle-dir")
	o.Images, _ = cmd.Flags().GetStringArray("image")
	o.Namespace, _ = cmd.Flags().GetString("namespace")
	o.Helm, _ = cmd.Flags().GetString("helm")
	o.To, _ = cmd.Flags().GetString("to")
	o.RegistryConfig, _ = cmd.Flags().GetString("registry-config")
	o.Insecure, _ = cmd.Flags().GetBool("insecure")
	return o
}

func runExportRelease(cmd *cobra.Command, args []string) error {
	if cmd.Flags().Changed("output") {
		o, _ := cmd.Flags().GetString("output")
		if o != "json" {
			return usagef("export release only supports -o json; got %q", o)
		}
	}

	opts := readReleaseExportOpts(cmd)
	if opts.Version == "" {
		return usagef("--version is required")
	}
	if len(opts.Profiles) == 0 {
		return usagef("at least one --profile is required")
	}
	if err := checkProfileNames(opts.Profiles); err != nil {
		return usagef("%v", err)
	}

	scheme, target, err := ParseOCIDest(opts.To)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}
	switch scheme {
	case "uc":
		return usagef("export release does not write to uc://: export to tar:// or dir://, then run import release --uc")
	case "oci":
		if hasExplicitTag(target) {
			return usagef("the oci destination takes a repository without a tag: %q", target)
		}
	}

	// Checks that need no network. Nothing is written until they pass.
	switch scheme {
	case "dir":
		if err := checkEmptyDirTarget(target); err != nil {
			return err
		}
	case "tar":
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("tar destination %s already exists: choose a new name", target)
		}
	}

	ctx := cmd.Context()
	tmp, err := os.MkdirTemp("", "varroactl-release-*")
	if err != nil {
		return fmt.Errorf("create work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// The bundle is built in the work directory. A destination receives the
	// finished bundle only after every check has passed.
	layoutDir := filepath.Join(tmp, "bundle")
	if err := os.MkdirAll(layoutDir, 0755); err != nil {
		return fmt.Errorf("create bundle directory: %w", err)
	}
	layout, err := oci.NewLayoutStore(layoutDir)
	if err != nil {
		return fmt.Errorf("open bundle layout: %w", err)
	}

	helm := helmRunner{bin: opts.Helm}
	regOpts := oci.RegistryOptions{CredentialConfigPath: opts.RegistryConfig, Insecure: opts.Insecure}

	// 1. Chart: stored in the layout, and a local .tgz for rendering.
	chart, chartTgz, err := stageChart(ctx, helm, layout, opts, tmp, regOpts)
	if err != nil {
		return err
	}

	// 2. Values: the defaults merged with each file, in order. Only the file
	// names and digests reach the manifest.
	defaults, err := loadChartDefaults(ctx, helm, chartTgz, tmp)
	if err != nil {
		return err
	}
	userValues := map[string]any{}
	var valuesRecords []releaseValues
	for _, path := range opts.Values {
		vals, digest, err := loadValuesFile(path)
		if err != nil {
			return err
		}
		userValues = mergeValues(userValues, vals)
		valuesRecords = append(valuesRecords, releaseValues{Name: filepath.Base(path), SHA256: digest})
	}
	merged := mergeValues(defaults, userValues)
	helm.secrets = valueSecrets(userValues)

	// 3. Render the chart the release deploys, and read its images.
	rendered, err := helm.template(ctx, chartTgz, opts.Namespace, opts.Values)
	if err != nil {
		return err
	}
	renderedSources, err := collectRenderedImages(rendered)
	if err != nil {
		return err
	}

	// 4. Profile packs. Each pack is built after its download and check, and
	// names the core it deploys, which sets its Jenkins image.
	jenkinsRepo, _ := lookupValue(merged, []string{"jenkins", "image", "repository"})
	jenkinsRepoStr, _ := jenkinsRepo.(string)
	if jenkinsRepoStr == "" {
		return fmt.Errorf("the chart sets no jenkins.image.repository: the profile Jenkins images cannot be named")
	}
	var packs []releasePack
	var profileSources []imageSource
	for _, profile := range opts.Profiles {
		pk, err := preparePack(cmd, profile, "", releaseDownloadURLBase)
		if err != nil {
			return err
		}
		if err := pk.write(ctx, layout, profile); err != nil {
			return fmt.Errorf("build pack for profile %s: %w", profile, err)
		}
		desc, err := layout.Resolve(ctx, profile)
		if err != nil {
			return fmt.Errorf("resolve pack for profile %s: %w", profile, err)
		}
		packs = append(packs, releasePack{Profile: profile, LayoutTag: profile, Digest: desc.Digest, PluginCount: len(pk.resolved)})
		if pk.coreVersion == "" {
			return fmt.Errorf("profile %s names no Jenkins core version: its Jenkins image cannot be named", profile)
		}
		profileSources = append(profileSources, imageSource{
			Raw:       jenkinsRepoStr + ":" + pk.coreVersion,
			Origin:    "profile:" + profile,
			Where:     "profile " + profile,
			ValuesKey: "jenkins.image.repository",
		})
	}

	// 5. Bundle and flag images.
	valueRefs := valueImageRefs(merged)
	bundleSources, err := bundleDirImages(opts.BundleDirs, valueRefs["jenkins.agentImage"])
	if err != nil {
		return err
	}
	var flagSources []imageSource
	for _, img := range opts.Images {
		flagSources = append(flagSources, imageSource{Raw: img, Origin: "flag", Where: "--image"})
	}

	// 6. Attribute every image. The order keeps chart sources first, so a
	// chart value wins when two sources name one image.
	all := make([]imageSource, 0, len(renderedSources)+len(profileSources)+len(bundleSources)+len(flagSources))
	all = append(all, renderedSources...)
	all = append(all, profileSources...)
	all = append(all, bundleSources...)
	all = append(all, flagSources...)
	planned, err := planImages(all, valueRefs)
	if err != nil {
		return err
	}

	// 7. Copy each image into the layout, with its full multi-arch graph.
	images := make([]releaseImage, 0, len(planned))
	for _, p := range planned {
		tag := layoutTagForImage(p.Ref)
		src, err := oci.NewRegistryStore(p.Ref.pullRepo(), regOpts)
		if err != nil {
			return fmt.Errorf("open registry for %s: %w", p.Ref.Raw, err)
		}
		if err := oci.Copy(ctx, src, p.Ref.sourceRef(), layout, tag); err != nil {
			return fmt.Errorf("copy image %s: %w", p.Ref.Raw, err)
		}
		desc, err := layout.Resolve(ctx, tag)
		if err != nil {
			return fmt.Errorf("resolve copied image %s: %w", p.Ref.Raw, err)
		}
		images = append(images, releaseImage{
			Source:     p.Ref.Raw,
			LayoutTag:  tag,
			Digest:     desc.Digest,
			MediaType:  desc.MediaType,
			ValuesKeys: p.ValuesKeys,
			Origin:     p.Origin,
		})
	}

	// The layout store leaves an empty ingest directory behind. Remove it, so the
	// bundle holds only what the manifest lists. Remove fails on a non-empty
	// directory, which is the case to keep.
	_ = os.Remove(filepath.Join(layoutDir, "ingest"))

	// 8. Manifest: every blob in the layout is listed with its size.
	blobs, err := oci.VerifyLayoutBlobs(layoutDir)
	if err != nil {
		return fmt.Errorf("hash bundle blobs: %w", err)
	}
	relBlobs := make([]releaseBlob, 0, len(blobs))
	for _, b := range blobs {
		relBlobs = append(relBlobs, releaseBlob{Digest: b.Digest, Size: b.Size})
	}
	m := releaseManifest{
		SchemaVersion: releaseManifestSchema,
		Kind:          releaseManifestKind,
		VarroaVersion: opts.Version,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Chart:         chart,
		Images:        images,
		PluginPacks:   packs,
		ValuesFiles:   valuesRecords,
		Blobs:         relBlobs,
		Unseen:        releaseUnseen,
	}
	if err := checkUniqueLayoutTags(m); err != nil {
		return err
	}
	manifestDigest, err := writeReleaseManifest(layoutDir, m)
	if err != nil {
		return err
	}

	// 9. Finish the destination.
	out := map[string]any{
		"manifestDigest": manifestDigest,
		"varroaVersion":  opts.Version,
		"chart":          chart.Version,
		"images":         len(images),
		"pluginPacks":    len(packs),
		"to":             opts.To,
	}
	switch scheme {
	case "dir":
		if err := writeDirAtomic(layoutDir, target); err != nil {
			return fmt.Errorf("write bundle to %s: %w", target, err)
		}
	case "tar":
		archive := filepath.Join(tmp, "bundle.tar.gz")
		if err := createReleaseTarGz(archive, layoutDir); err != nil {
			return fmt.Errorf("write archive: %w", err)
		}
		if err := writeArchiveAtomic(archive, target); err != nil {
			return fmt.Errorf("write archive %s: %w", target, err)
		}
		archiveDigest, err := fileSHA256(target)
		if err != nil {
			return fmt.Errorf("hash archive: %w", err)
		}
		out["archiveDigest"] = "sha256:" + archiveDigest
	case "oci":
		if err := pushReleaseToRegistry(ctx, layout, layoutDir, m, target, regOpts); err != nil {
			return err
		}
	}

	if cmd.Flags().Changed("output") {
		return printJSON(os.Stdout, out)
	}
	fmt.Printf("Exported release bundle v%s to %s\n", opts.Version, opts.To)
	fmt.Printf("  manifest digest: %s\n", manifestDigest)
	if d, ok := out["archiveDigest"]; ok {
		fmt.Printf("  archive digest:  %s\n", d)
	}
	fmt.Printf("  chart %s, %d images, %d plugin packs\n", chart.Version, len(images), len(packs))
	return nil
}

// stageChart puts the chart into the layout. It returns the chart record and a
// local .tgz path that helm can render. An oci:// chart is copied, so its
// manifest digest stays the one the registry publishes.
func stageChart(ctx context.Context, helm helmRunner, layout *oci.LayoutStore, opts releaseExportOpts, tmp string, regOpts oci.RegistryOptions) (releaseChart, string, error) {
	var tgz string
	var meta chartMeta
	var configJSON []byte
	var desc oci.Descriptor
	var finalTag string

	if strings.HasPrefix(opts.Chart, "oci://") {
		repo, tag := splitOCITag(strings.TrimPrefix(opts.Chart, "oci://"), opts.Version)
		store, err := oci.NewRegistryStore(repo, regOpts)
		if err != nil {
			return releaseChart{}, "", fmt.Errorf("open chart registry %s: %w", repo, err)
		}
		man, err := store.Pull(ctx, tag)
		if err != nil {
			return releaseChart{}, "", fmt.Errorf("pull chart manifest %s:%s: %w", repo, tag, err)
		}
		var layer *oci.Descriptor
		for i := range man.Layers {
			if man.Layers[i].MediaType == mediaTypeHelmChartLayer {
				layer = &man.Layers[i]
				break
			}
		}
		if layer == nil {
			return releaseChart{}, "", fmt.Errorf("chart %s:%s has no helm chart layer", repo, tag)
		}
		rc, err := store.FetchBlob(ctx, layer.Digest)
		if err != nil {
			return releaseChart{}, "", fmt.Errorf("fetch chart archive: %w", err)
		}
		tgz = filepath.Join(tmp, "chart-source.tgz")
		if err := writeReaderToFile(tgz, rc); err != nil {
			return releaseChart{}, "", err
		}
		meta, _, err = helm.showChart(ctx, tgz)
		if err != nil {
			return releaseChart{}, "", err
		}
		finalTag = "chart-" + meta.Name + "-" + sanitizeTag(meta.Version)
		if err := oci.Copy(ctx, store, tag, layout, finalTag); err != nil {
			return releaseChart{}, "", fmt.Errorf("copy chart: %w", err)
		}
		desc, err = layout.Resolve(ctx, finalTag)
		if err != nil {
			return releaseChart{}, "", fmt.Errorf("resolve chart: %w", err)
		}
	} else {
		info, err := os.Stat(opts.Chart)
		if err != nil {
			return releaseChart{}, "", fmt.Errorf("chart %s: %w", opts.Chart, err)
		}
		if info.IsDir() {
			tgz, err = helm.packageDir(ctx, opts.Chart, tmp)
			if err != nil {
				return releaseChart{}, "", err
			}
		} else {
			tgz = opts.Chart
		}
		meta, configJSON, err = helm.showChart(ctx, tgz)
		if err != nil {
			return releaseChart{}, "", err
		}
		finalTag = "chart-" + meta.Name + "-" + sanitizeTag(meta.Version)
		desc, err = pushChartArtifact(ctx, layout, finalTag, tgz, configJSON)
		if err != nil {
			return releaseChart{}, "", err
		}
	}

	if !oci.IsValidRepoPath(meta.Name) {
		return releaseChart{}, "", fmt.Errorf("chart name %q is not a valid OCI repository name", meta.Name)
	}
	if !oci.IsLayoutTagSafe(sanitizeTag(meta.Version)) {
		return releaseChart{}, "", fmt.Errorf("chart version %q is not a valid OCI tag", meta.Version)
	}
	return releaseChart{
		Name:      meta.Name,
		Version:   meta.Version,
		LayoutTag: finalTag,
		Digest:    desc.Digest,
	}, tgz, nil
}

// splitOCITag splits host/repo[:tag]. The fallback tag applies when none is
// written.
func splitOCITag(ref, fallback string) (string, string) {
	if hasExplicitTag(ref) {
		i := strings.LastIndex(ref, ":")
		return ref[:i], ref[i+1:]
	}
	return ref, fallback
}

// writeReaderToFile copies r into a new file at path and closes r.
func writeReaderToFile(path string, r io.ReadCloser) error {
	defer func() { _ = r.Close() }()
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// pushReleaseToRegistry pushes every layout artifact to repo under its layout
// tag, then pushes the manifest as a tagged artifact.
func pushReleaseToRegistry(ctx context.Context, layout *oci.LayoutStore, layoutDir string, m releaseManifest, repo string, regOpts oci.RegistryOptions) error {
	dst, err := oci.NewRegistryStore(repo, regOpts)
	if err != nil {
		return fmt.Errorf("open registry %s: %w", repo, err)
	}
	tags := []string{m.Chart.LayoutTag}
	for _, img := range m.Images {
		tags = append(tags, img.LayoutTag)
	}
	for _, p := range m.PluginPacks {
		tags = append(tags, p.LayoutTag)
	}
	for _, tag := range tags {
		if err := oci.Copy(ctx, layout, tag, dst, tag); err != nil {
			return fmt.Errorf("push %s to %s: %w", tag, repo, err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(layoutDir, releaseManifestFile))
	if err != nil {
		return fmt.Errorf("read manifest for push: %w", err)
	}
	cfgDigest, cfgSize, err := dst.PushBlob(ctx, mediaTypeEmptyConfig, bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("push manifest config: %w", err)
	}
	layerDigest, layerSize, err := dst.PushBlob(ctx, mediaTypeReleaseManifest, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("push manifest: %w", err)
	}
	return dst.Push(ctx, releaseManifestTag, oci.Manifest{
		Config: oci.Descriptor{MediaType: mediaTypeEmptyConfig, Digest: cfgDigest, Size: cfgSize},
		Layers: []oci.Descriptor{{MediaType: mediaTypeReleaseManifest, Digest: layerDigest, Size: layerSize}},
	})
}
