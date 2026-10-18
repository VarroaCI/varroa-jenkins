package bundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// CatalogLayerMediaType is the media type of the layer that holds a catalog
// source tree in an OCI artifact.
const CatalogLayerMediaType = "application/vnd.varroa.catalog.v1.tar+gzip"

// CatalogArtifactType is the artifact type of an OCI artifact that holds a
// catalog source tree.
const CatalogArtifactType = "application/vnd.varroa.catalog.v1"

// CatalogFetch describes where a CatalogSource tree comes from.
type CatalogFetch struct {
	RepoURL  string
	Revision string
	OCIRef   string

	GitAuth *GitAuth
	OCIAuth *OCIAuth

	// Cloner clones RepoURL when CloneCache is nil. It is required for git sources.
	Cloner *GitCloner
	// CloneCache, when non-nil, serves RepoURL instead of Cloner.
	CloneCache *CloneCache
}

// FetchCatalog writes the source tree of a CatalogSource into dir and returns
// the resolved revision. The tree is the repository root for a git source and
// the root of the catalog layer for an OCI source. dir must exist and be empty.
// The caller applies spec.path with CatalogRoot.
func FetchCatalog(ctx context.Context, f CatalogFetch, dir string) (string, error) {
	switch {
	case f.RepoURL != "":
		if f.CloneCache != nil {
			sha, _, err := f.CloneCache.Checkout(ctx, f.RepoURL, f.Revision, dir, f.GitAuth)
			if err != nil {
				return "", fmt.Errorf("clone repo: %w", err)
			}
			return sha, nil
		}
		if f.Cloner == nil {
			return "", fmt.Errorf("clone repo: no cloner configured")
		}
		sha, err := f.Cloner.Clone(ctx, f.RepoURL, f.Revision, dir, f.GitAuth)
		if err != nil {
			return "", fmt.Errorf("clone repo: %w", err)
		}
		return sha, nil
	case f.OCIRef != "":
		var configPath string
		if f.OCIAuth != nil {
			var err error
			configPath, err = WriteDockerConfigJSON(f.OCIAuth)
			if err != nil {
				return "", fmt.Errorf("prepare OCI auth: %w", err)
			}
			defer func() { _ = os.RemoveAll(filepath.Dir(configPath)) }()
		}
		store, err := oci.NewRegistryStore(f.OCIRef, oci.RegistryOptions{
			CredentialConfigPath: configPath,
			Insecure:             false,
		})
		if err != nil {
			return "", fmt.Errorf("create registry store for %q: %w", f.OCIRef, err)
		}
		return FetchCatalogLayer(ctx, store, f.OCIRef, dir)
	default:
		return "", fmt.Errorf("must set either repoURL or ociRef")
	}
}

// FetchCatalogLayer pulls the OCI artifact at ref, extracts its catalog layer
// into dir, and returns the manifest digest of ref. The first layer whose media
// type is CatalogLayerMediaType is used.
func FetchCatalogLayer(ctx context.Context, store oci.BlobStore, ref, dir string) (string, error) {
	desc, err := store.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve OCI ref %q: %w", ref, err)
	}

	manifest, err := store.Pull(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("pull OCI artifact %q: %w", ref, err)
	}

	var layer *oci.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == CatalogLayerMediaType {
			layer = &manifest.Layers[i]
			break
		}
	}
	if layer == nil {
		return "", fmt.Errorf("OCI artifact %q: no layer with media type %s", ref, CatalogLayerMediaType)
	}

	rc, err := store.FetchBlob(ctx, layer.Digest)
	if err != nil {
		return "", fmt.Errorf("fetch catalog layer %q: %w", layer.Digest, err)
	}
	defer func() { _ = rc.Close() }()

	if err := untarGzip(rc, dir); err != nil {
		return "", fmt.Errorf("extract catalog bundle: %w", err)
	}
	return desc.Digest, nil
}

// CatalogRoot returns the directory that holds the catalog index. An empty
// path selects dir. Otherwise the path is resolved inside dir.
func CatalogRoot(dir, path string) (string, error) {
	if path == "" {
		return dir, nil
	}
	root, err := ResolveContainedPath(dir, path)
	if err != nil {
		return "", fmt.Errorf("catalog path: %w", err)
	}
	return root, nil
}
