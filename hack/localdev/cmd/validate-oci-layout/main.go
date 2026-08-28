// Command validate-oci-layout validates that hack/localdev/pluginpack-fixture/
// is a structurally valid OCI image-layout. It is invoked by CI (the
// localdev-validate job) to check the checked-in fixture after generation.
//
// Usage:
//
//	go run ./hack/localdev/cmd/validate-oci-layout [--dir <path>]
//
// Exit code 0 = valid or empty (fixture not yet generated).
// Exit code 1 = structurally invalid (missing index, malformed JSON, dangling blob).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func main() {
	dir := flag.String("dir", "hack/localdev/pluginpack-fixture", "path to OCI-layout directory")
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: resolving path %q: %v\n", *dir, err)
		os.Exit(1)
	}

	// Check if the directory exists and has an index.json.
	indexPath := filepath.Join(absDir, ocispec.ImageIndexFile)
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		// Fixture not yet generated — skip gracefully.
		fmt.Printf("SKIP: %s not found — fixture not generated yet\n", indexPath)
		os.Exit(0)
	}

	store, err := oci.NewLayoutStore(absDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: opening OCI layout at %q: %v\n", absDir, err)
		os.Exit(1)
	}

	ctx := context.Background()
	descs, err := store.ListManifests(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: listing manifests (malformed index.json?): %v\n", err)
		os.Exit(1)
	}

	if len(descs) == 0 {
		fmt.Fprintf(os.Stderr, "ERROR: index.json exists but contains no manifests\n")
		os.Exit(1)
	}

	fmt.Printf("OK: %d manifest(s) in index.json\n", len(descs))

	// Resolve every manifest descriptor and verify its blob + all layer/config blobs.
	for _, desc := range descs {
		fmt.Printf("  checking manifest %s (%d bytes, media type %s)\n",
			desc.Digest, desc.Size, desc.MediaType)

		manifest, err := store.Pull(ctx, desc.Digest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: pulling manifest %s: %v\n", desc.Digest, err)
			os.Exit(1)
		}

		// Check config blob.
		if manifest.Config.Digest != "" {
			rc, err := store.FetchBlob(ctx, manifest.Config.Digest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: fetching config blob %s (referenced by manifest %s): %v\n",
					manifest.Config.Digest, desc.Digest, err)
				os.Exit(1)
			}
			_ = rc.Close()
			fmt.Printf("    config blob %s: OK (%d bytes)\n", manifest.Config.Digest, manifest.Config.Size)
		}

		// Check each layer blob.
		for _, layer := range manifest.Layers {
			rc, err := store.FetchBlob(ctx, layer.Digest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: fetching layer blob %s (referenced by manifest %s): %v\n",
					layer.Digest, desc.Digest, err)
				os.Exit(1)
			}
			_ = rc.Close()
			fmt.Printf("    layer blob %s: OK (%d bytes)\n", layer.Digest, layer.Size)
		}
	}

	fmt.Println("VALID: OCI layout structure is intact")
}
