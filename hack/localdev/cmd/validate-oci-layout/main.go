// Command validate-oci-layout validates the checked-in localdev plugin-pack
// fixture at hack/localdev/pluginpack-fixture/. The PR Checks job runs it.
//
// The layout must hold exactly one manifest. Its ref name must be the path
// that localdev.sh imports, because "varroactl import --from dir://<path>"
// resolves the ref <path>. Every blob must resolve, oci.ReadPluginPack must
// read the pack, and the pack must name the profile of the lock baseline.
//
// Usage:
//
//	go run ./hack/localdev/cmd/validate-oci-layout [--dir <path>] [--ref <ref>]
//
// Exit code 0 = valid, or no index.json in the directory.
// Exit code 1 = invalid.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// fixtureRef is the path that localdev.sh passes to varroactl import --from dir://.
const fixtureRef = "hack/localdev/pluginpack-fixture"

// annPackProfile is the manifest annotation that oci.BuildPluginPack writes
// from PackConfig.Profile.
const annPackProfile = "dev.varroa.pack.profile"

func main() {
	dir := flag.String("dir", fixtureRef, "path to OCI-layout directory")
	ref := flag.String("ref", fixtureRef, "manifest ref name that the localdev import resolves")
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: resolving path %q: %v\n", *dir, err)
		os.Exit(1)
	}

	// Opening a layout writes index.json when it is missing, so check first.
	indexPath := filepath.Join(absDir, ocispec.ImageIndexFile)
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		fmt.Printf("SKIP: %s not found — fixture not generated yet\n", indexPath)
		os.Exit(0)
	}

	want := pluginlock.ProfileName(pluginlock.Baseline())
	if err := validateLayout(context.Background(), absDir, *ref, want); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("VALID: one plugin pack, ref %q, profile %q\n", *ref, want)
}

// validateLayout returns an error unless dir holds exactly one plugin-pack
// manifest tagged wantRef, every blob of that manifest resolves,
// oci.ReadPluginPack reads it, and the pack names wantProfile.
func validateLayout(ctx context.Context, dir, wantRef, wantProfile string) error {
	store, err := oci.NewLayoutStore(dir)
	if err != nil {
		return fmt.Errorf("open OCI layout at %q: %w", dir, err)
	}
	descs, err := store.ListManifests(ctx)
	if err != nil {
		return fmt.Errorf("list manifests (malformed index.json?): %w", err)
	}
	// An export into an existing layout keeps the old manifest in index.json.
	if len(descs) != 1 {
		return fmt.Errorf("index.json holds %d manifests, want 1: run hack/localdev/pluginpack-fixture/generate.sh", len(descs))
	}
	desc := descs[0]
	ref := desc.Annotations[ocispec.AnnotationRefName]
	if ref != wantRef {
		return fmt.Errorf("manifest %s has ref name %q, want %q", desc.Digest, ref, wantRef)
	}
	manifest, err := store.Pull(ctx, ref)
	if err != nil {
		return fmt.Errorf("pull manifest %q: %w", ref, err)
	}
	if manifest.ArtifactType != oci.ArtifactTypePluginPack {
		return fmt.Errorf("manifest %q has artifact type %q, want %q", ref, manifest.ArtifactType, oci.ArtifactTypePluginPack)
	}
	for _, b := range append([]oci.Descriptor{manifest.Config}, manifest.Layers...) {
		rc, err := store.FetchBlob(ctx, b.Digest)
		if err != nil {
			return fmt.Errorf("fetch blob %s of manifest %q: %w", b.Digest, ref, err)
		}
		_ = rc.Close()
	}
	cfg, _, err := oci.ReadPluginPack(ctx, store, ref)
	if err != nil {
		return fmt.Errorf("read plugin pack: %w", err)
	}
	if cfg.Profile != wantProfile {
		return fmt.Errorf("pack config names profile %q, want the baseline profile %q: run hack/localdev/pluginpack-fixture/generate.sh", cfg.Profile, wantProfile)
	}
	if got := manifest.Annotations[annPackProfile]; got != wantProfile {
		return fmt.Errorf("manifest annotation %s is %q, want %q: run hack/localdev/pluginpack-fixture/generate.sh", annPackProfile, got, wantProfile)
	}
	return nil
}
