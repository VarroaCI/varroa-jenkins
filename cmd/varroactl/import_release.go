package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		importCmd := findSubCommand(root, "import")
		if importCmd == nil {
			importCmd = newImportCmd()
			root.AddCommand(importCmd)
		}
		importCmd.AddCommand(newImportReleaseCmd())
	})
}

func newImportReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release --from <tar|dir> --to oci://<registry>/<prefix> --manifest-digest sha256:<hex> [flags]",
		Short: "Verify and push a release transfer bundle to an internal registry",
		Long: `Import release reads a bundle that export release wrote. It checks the bundle
before it pushes anything. The manifest file must match --manifest-digest.
Every blob must match its recorded digest, and every artifact tag must resolve
to its recorded digest. A bundle with an extra or a missing blob is refused.

The push writes each chart, image, and plugin pack under the --to prefix:
  <prefix>/charts/<chart>:<version>
  <prefix>/<source repository path>:<source tag>
  <prefix>/plugin-pack:<profile>

It then prints the chart values that point the release at the registry. With
--uc, each plugin pack also goes to the update center. That needs
VARROACTL_UC_TOKEN.`,
		RunE: runImportRelease,
	}

	cmd.Flags().String("from", "", "bundle: tar://<path>.tar.gz or dir://<path> (required)")
	_ = cmd.MarkFlagRequired("from")
	cmd.Flags().String("to", "", "registry prefix: oci://<registry>/<prefix> (required)")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().String("manifest-digest", "", "SHA-256 of release-manifest.json, as sha256:<hex> (required)")
	_ = cmd.MarkFlagRequired("manifest-digest")
	cmd.Flags().String("uc", "", "update center to upload the plugin packs to: uc://<host>[:<port>]")
	cmd.Flags().String("work-dir", "", "directory for the extracted bundle (default: the system temp directory)")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")

	return cmd
}

// releaseImportPlan is the verified bundle with its destination.
type releaseImportPlan struct {
	dir      string
	layout   *oci.LayoutStore
	manifest releaseManifest
	base     string // registry/prefix, no scheme and no tag
}

func runImportRelease(cmd *cobra.Command, args []string) error {
	if cmd.Flags().Changed("output") {
		o, _ := cmd.Flags().GetString("output")
		if o != "yaml" && o != "json" {
			return usagef("import release supports -o yaml or -o json; got %q", o)
		}
	}
	format := "yaml"
	if cmd.Flags().Changed("output") {
		format, _ = cmd.Flags().GetString("output")
	}

	fromDest, _ := cmd.Flags().GetString("from")
	toDest, _ := cmd.Flags().GetString("to")
	wantDigest, _ := cmd.Flags().GetString("manifest-digest")
	ucFlag, _ := cmd.Flags().GetString("uc")
	workDirFlag, _ := cmd.Flags().GetString("work-dir")
	regOpts := oci.RegistryOptions{}
	regOpts.CredentialConfigPath, _ = cmd.Flags().GetString("registry-config")
	regOpts.Insecure, _ = cmd.Flags().GetBool("insecure")

	srcScheme, srcTarget, err := ParseOCIDest(fromDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}
	if srcScheme != "tar" && srcScheme != "dir" {
		return usagef("import release reads tar:// or dir://; got %s://", srcScheme)
	}

	dstScheme, dstTarget, err := ParseOCIDest(toDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}
	if dstScheme != "oci" {
		return usagef("import release pushes to oci://<registry>/<prefix>; got %s://", dstScheme)
	}
	if hasExplicitTag(dstTarget) {
		return usagef("the --to prefix takes no tag: %q", dstTarget)
	}
	if err := oci.ParseDigest(wantDigest); err != nil {
		return usagef("--manifest-digest: %v", err)
	}

	var ucTarget string
	if ucFlag != "" {
		if !strings.HasPrefix(ucFlag, "uc://") || len(ucFlag) == len("uc://") {
			return usagef("--uc takes uc://<host>[:<port>]; got %q", ucFlag)
		}
		ucTarget = strings.TrimPrefix(ucFlag, "uc://")
		if os.Getenv("VARROACTL_UC_TOKEN") == "" {
			return fmt.Errorf("VARROACTL_UC_TOKEN not set: --uc needs the import token before any push")
		}
	}

	// Stage the bundle directory. A tar is extracted into a fresh temp dir under
	// --work-dir. The extraction admits only the release layout.
	dir := srcTarget
	if srcScheme == "tar" {
		tmp, err := os.MkdirTemp(workDirFlag, "varroactl-import-*")
		if err != nil {
			return fmt.Errorf("create work directory: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		if err := extractReleaseTar(srcTarget, tmp, wantDigest); err != nil {
			return fmt.Errorf("extract bundle: %w", err)
		}
		dir = tmp
	}

	// Verification: no network call, and nothing is pushed on a failure.
	plan, err := verifyReleaseBundle(dir, wantDigest, dstTarget)
	if err != nil {
		return err
	}

	// Push. Each artifact lands under its destination name and is then read
	// back and compared with the manifest digest.
	pushed, err := pushReleaseBundle(cmd.Context(), plan, regOpts)
	if err != nil {
		return err
	}

	if ucTarget != "" {
		token := os.Getenv("VARROACTL_UC_TOKEN")
		for _, p := range plan.manifest.PluginPacks {
			if err := importPackToUC(cmd.Context(), plan.layout, p.LayoutTag, ucTarget, token); err != nil {
				return fmt.Errorf("upload pack %s to uc://%s: %w", p.Profile, ucTarget, err)
			}
		}
	}

	overrides, unattributed := releaseOverrides(plan)
	if format == "json" {
		return printJSON(os.Stdout, map[string]any{
			"manifestDigest": wantDigest,
			"pushed":         pushed,
			"overrides":      overrides,
			"unattributed":   unattributed,
			"unseen":         plan.manifest.Unseen,
		})
	}
	return printReleaseOverrides(os.Stdout, plan, overrides, unattributed)
}

// verifyReleaseBundle checks the bundle against its manifest. It reads files
// only. An error means the bundle must not be pushed.
func verifyReleaseBundle(dir, wantDigest, base string) (releaseImportPlan, error) {
	raw, m, err := readReleaseManifest(dir)
	if err != nil {
		return releaseImportPlan{}, err
	}
	gotDigest, _, err := oci.Sha256Digest(bytes.NewReader(raw))
	if err != nil {
		return releaseImportPlan{}, err
	}
	if gotDigest != wantDigest {
		return releaseImportPlan{}, fmt.Errorf("release manifest digest is %s, want %s: the bundle is not the one the release published", gotDigest, wantDigest)
	}

	// Blob set: every file under blobs/sha256 must be in the manifest with its
	// size, and every manifest blob must be present.
	blobs, err := oci.VerifyLayoutBlobs(dir)
	if err != nil {
		return releaseImportPlan{}, fmt.Errorf("verify bundle blobs: %w", err)
	}
	want := make(map[string]int64, len(m.Blobs))
	for _, b := range m.Blobs {
		want[b.Digest] = b.Size
	}
	got := make(map[string]int64, len(blobs))
	for _, b := range blobs {
		got[b.Digest] = b.Size
		size, ok := want[b.Digest]
		if !ok {
			return releaseImportPlan{}, fmt.Errorf("bundle holds blob %s that the manifest does not list", b.Digest)
		}
		if size != b.Size {
			return releaseImportPlan{}, fmt.Errorf("blob %s is %d bytes, the manifest records %d", b.Digest, b.Size, size)
		}
	}
	for d := range want {
		if _, ok := got[d]; !ok {
			return releaseImportPlan{}, fmt.Errorf("bundle is missing blob %s that the manifest lists", d)
		}
	}

	layout, err := oci.NewLayoutStore(dir)
	if err != nil {
		return releaseImportPlan{}, fmt.Errorf("open bundle layout: %w", err)
	}

	if err := checkDestinationNames(m); err != nil {
		return releaseImportPlan{}, err
	}

	// Tags: each artifact tag must resolve to its recorded digest.
	type tagRecord struct{ tag, digest string }
	tags := []tagRecord{{m.Chart.LayoutTag, m.Chart.Digest}}
	for _, img := range m.Images {
		tags = append(tags, tagRecord{img.LayoutTag, img.Digest})
	}
	for _, p := range m.PluginPacks {
		tags = append(tags, tagRecord{p.LayoutTag, p.Digest})
	}
	for _, t := range tags {
		if !oci.IsLayoutTagSafe(t.tag) {
			return releaseImportPlan{}, fmt.Errorf("manifest names an unsafe layout tag %q", t.tag)
		}
		desc, err := layout.Resolve(context.Background(), t.tag)
		if err != nil {
			return releaseImportPlan{}, fmt.Errorf("bundle layout has no artifact %s: %w", t.tag, err)
		}
		if desc.Digest != t.digest {
			return releaseImportPlan{}, fmt.Errorf("artifact %s resolves to %s, the manifest records %s", t.tag, desc.Digest, t.digest)
		}
	}

	// Two sources with one repository path and tag, from different hosts, land
	// on one destination. Such a pair is refused unless the digests match.
	destPrefix := strings.TrimSuffix(base, "/")
	seen := map[string]string{}
	for _, img := range m.Images {
		ref, err := parseImageRef(img.Source)
		if err != nil {
			return releaseImportPlan{}, err
		}
		dest := destPrefix + "/" + ref.Path + ":" + ref.destTag()
		if prev, ok := seen[dest]; ok && prev != img.Digest {
			return releaseImportPlan{}, fmt.Errorf("two images map to destination %s with different digests: rename one source", dest)
		}
		seen[dest] = img.Digest
	}

	return releaseImportPlan{dir: dir, layout: layout, manifest: m, base: destPrefix}, nil
}

// pushedArtifact records one artifact that an import pushed.
type pushedArtifact struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// pushReleaseBundle pushes the chart, images, and packs. It returns the
// destination refs with their digests.
func pushReleaseBundle(ctx context.Context, plan releaseImportPlan, regOpts oci.RegistryOptions) ([]pushedArtifact, error) {
	total := 1 + len(plan.manifest.Images) + len(plan.manifest.PluginPacks)
	pushed, err := pushReleaseArtifacts(ctx, plan, regOpts)
	if err != nil {
		return pushed, fmt.Errorf("push stopped after %d of %d artifacts; the %d artifacts already pushed remain in the registry, and a rerun of this bundle is safe: %w", len(pushed), total, len(pushed), err)
	}
	return pushed, nil
}

// pushReleaseArtifacts pushes each artifact in turn. It returns the artifacts
// that completed, so a caller can report a partial push.
func pushReleaseArtifacts(ctx context.Context, plan releaseImportPlan, regOpts oci.RegistryOptions) ([]pushedArtifact, error) {
	m := plan.manifest
	var pushed []pushedArtifact

	push := func(layoutTag, repo, destTag, wantDigest string) error {
		store, err := oci.NewRegistryStore(repo, regOpts)
		if err != nil {
			return fmt.Errorf("open registry for %s: %w", repo, err)
		}
		if err := oci.Copy(ctx, plan.layout, layoutTag, store, destTag); err != nil {
			return fmt.Errorf("push %s:%s: %w", repo, destTag, err)
		}
		desc, err := store.Resolve(ctx, destTag)
		if err != nil {
			return fmt.Errorf("resolve pushed %s:%s: %w", repo, destTag, err)
		}
		if desc.Digest != wantDigest {
			return fmt.Errorf("pushed %s:%s resolves to %s, the manifest records %s", repo, destTag, desc.Digest, wantDigest)
		}
		pushed = append(pushed, pushedArtifact{Ref: repo + ":" + destTag, Digest: desc.Digest})
		return nil
	}

	chartRepo := plan.base + "/charts/" + m.Chart.Name
	if err := push(m.Chart.LayoutTag, chartRepo, sanitizeTag(m.Chart.Version), m.Chart.Digest); err != nil {
		return pushed, err
	}
	for _, img := range m.Images {
		ref, err := parseImageRef(img.Source)
		if err != nil {
			return pushed, err
		}
		if err := push(img.LayoutTag, plan.base+"/"+ref.Path, ref.destTag(), img.Digest); err != nil {
			return pushed, err
		}
	}
	for _, p := range m.PluginPacks {
		if err := push(p.LayoutTag, plan.base+"/plugin-pack", p.Profile, p.Digest); err != nil {
			return pushed, err
		}
	}
	return pushed, nil
}

// releaseOverrides builds the values that point the chart at the pushed
// artifacts. An image that several values keys name gets one override per key.
// It also returns the images that no chart value names.
func releaseOverrides(plan releaseImportPlan) (map[string]any, []string) {
	out := map[string]any{}
	var unattributed []string
	var operatorRef string
	miteExplicit := false
	for _, img := range plan.manifest.Images {
		ref, err := parseImageRef(img.Source)
		if err != nil || len(img.ValuesKeys) == 0 {
			unattributed = append(unattributed, img.Source)
			continue
		}
		repo := plan.base + "/" + ref.Path
		for _, key := range img.ValuesKeys {
			spec := specForKey(key)
			if spec == nil {
				unattributed = append(unattributed, img.Source)
				continue
			}
			switch spec.Kind {
			case imageValueMap:
				setValue(out, append(append([]string{}, spec.Path...), "repository"), repo)
				setValue(out, append(append([]string{}, spec.Path...), "tag"), ref.destTag())
				if spec.Registry {
					// The destination path already names the registry. A registry
					// key from the source values would prefix the image again.
					setValue(out, append(append([]string{}, spec.Path...), "registry"), "")
				}
				if key == "operator.image" {
					operatorRef = repo + ":" + ref.destTag()
				}
			case imageValueString:
				setValue(out, spec.Path, repo+":"+ref.destTag())
				if key == "operator.miteImage" {
					miteExplicit = true
				}
			case imageValueRepo:
				setValue(out, spec.Path, repo)
			}
		}
	}
	// The operator passes its mite image to every controller pod. When the
	// bundle names no separate mite image, that image is the operator image.
	if operatorRef != "" && !miteExplicit {
		setValue(out, []string{"operator", "miteImage"}, operatorRef)
	}
	refs := make([]string, 0, len(plan.manifest.PluginPacks))
	for _, p := range plan.manifest.PluginPacks {
		refs = append(refs, plan.base+"/plugin-pack:"+p.Profile)
	}
	if len(refs) > 0 {
		setValue(out, []string{"updateCenter", "seed", "refs"}, refs)
	}
	sort.Strings(unattributed)
	return out, unattributed
}

// setValue sets a nested key, creating the maps along path.
func setValue(m map[string]any, path []string, v any) {
	cur := m
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

// printReleaseOverrides writes the values file and the notes an operator must
// read before the first install.
func printReleaseOverrides(w io.Writer, plan releaseImportPlan, overrides map[string]any, unattributed []string) error {
	body, err := yaml.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("encode overrides: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Values for release %s. Install the chart from the registry:\n", plan.manifest.VarroaVersion)
	fmt.Fprintf(&b, "#   helm install varroa oci://%s/charts/%s --version %s -f <this file>\n",
		plan.base, plan.manifest.Chart.Name, sanitizeTag(plan.manifest.Chart.Version))
	b.Write(body)
	b.WriteString("# Set global.image.pullSecretNames when the internal registry needs credentials.\n")
	b.WriteString("# An air-gapped install turns off upstream reads: set jenkins.upstreamURL to \"\".\n")
	if len(unattributed) > 0 {
		b.WriteString("# These bundle images are not in a chart value. Set them in the JCasC bundle:\n")
		for _, u := range unattributed {
			fmt.Fprintf(&b, "#   %s\n", u)
		}
	}
	b.WriteString("# This bundle cannot list these image sources. Mirror them by hand:\n")
	for _, u := range plan.manifest.Unseen {
		fmt.Fprintf(&b, "#   %s\n", u)
	}
	_, err = io.WriteString(w, b.String())
	return err
}
