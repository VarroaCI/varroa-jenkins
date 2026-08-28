package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// RegistryOptions configures the RegistryStore.
type RegistryOptions struct {
	// CredentialConfigPath is the path to a Docker config.json with credentials.
	// If empty, $DOCKER_CONFIG/config.json is tried as a fallback.
	// If neither yields credentials for the target host, anonymous access is used.
	CredentialConfigPath string

	// Insecure, when true, uses plain HTTP instead of HTTPS.
	Insecure bool
}

// RegistryStore implements BlobStore backed by a remote OCI registry.
type RegistryStore struct {
	repo   *remote.Repository
	refStr string
}

// NewRegistryStore creates a new RegistryStore connected to the given registry reference.
func NewRegistryStore(ref string, opts RegistryOptions) (*RegistryStore, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("parse reference %q: %w", ref, err)
	}

	repo.PlainHTTP = opts.Insecure

	// Set up credential resolution
	credFunc, err := buildCredentialFuncForHost(opts.CredentialConfigPath)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials: %w", err)
	}
	if credFunc != nil {
		repo.Client = &auth.Client{
			Credential: credFunc,
		}
	}

	return &RegistryStore{repo: repo, refStr: ref}, nil
}

// buildCredentialFuncForHost resolves credentials for the registry host in the given ref.
// If no credential config is available, it returns nil (anonymous access).
func buildCredentialFuncForHost(configPath string) (auth.CredentialFunc, error) {
	// Determine config path
	path, err := resolveConfigPath(configPath)
	if err != nil {
		return nil, err
	}
	if path == "" {
		// No config path configured — anonymous access
		return nil, nil
	}

	// If the config file doesn't exist, use anonymous access
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat config file: %w", err)
	}

	// Use oras-go's credential store for full Docker config.json resolution
	store, err := credentials.NewStore(path, credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("open credential store: %w", err)
	}
	return credentials.Credential(store), nil
}

// resolveConfigPath resolves the Docker config.json path from an explicit path,
// $DOCKER_CONFIG, or the default ~/.docker location.
func resolveConfigPath(cfgPath string) (string, error) {
	if cfgPath != "" {
		return cfgPath, nil
	}
	dockerCfg := os.Getenv("DOCKER_CONFIG")
	if dockerCfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dockerCfg = filepath.Join(home, ".docker")
	}
	return filepath.Join(dockerCfg, "config.json"), nil
}

// target returns the underlying oras.Target so copy.go can hand it to oras.Copy.
func (r *RegistryStore) target() oras.Target {
	return r.repo
}

// GetRepo exposes the underlying remote repository for test inspection.
func (r *RegistryStore) GetRepo() *remote.Repository {
	return r.repo
}

// Push pushes a manifest with the given reference.
func (r *RegistryStore) Push(ctx context.Context, ref string, manifest Manifest) error {
	ociManifest := toOCIManifest(manifest)
	manifestBytes, err := json.Marshal(ociManifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBytes)
	return r.repo.PushReference(ctx, desc, bytes.NewReader(manifestBytes), ref)
}

// PushBlob pushes a blob and returns its digest and size.
func (r *RegistryStore) PushBlob(ctx context.Context, mediaType string, contentReader io.Reader) (digest string, size int64, err error) {
	data, err := io.ReadAll(contentReader)
	if err != nil {
		return "", 0, err
	}
	desc := content.NewDescriptorFromBytes(mediaType, data)
	if err := r.repo.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		return "", 0, err
	}
	return desc.Digest.String(), desc.Size, nil
}

// Pull retrieves a manifest by reference.
func (r *RegistryStore) Pull(ctx context.Context, ref string) (Manifest, error) {
	_, rc, err := r.repo.FetchReference(ctx, ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch reference %q: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	ociManifestBytes, err := io.ReadAll(rc)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %q: %w", ref, err)
	}
	var ociManifest ocispec.Manifest
	if err := json.Unmarshal(ociManifestBytes, &ociManifest); err != nil {
		return Manifest{}, fmt.Errorf("unmarshal manifest %q: %w", ref, err)
	}
	return fromOCIManifest(ociManifest), nil
}

// Resolve resolves a reference to a descriptor.
func (r *RegistryStore) Resolve(ctx context.Context, ref string) (Descriptor, error) {
	desc, err := r.repo.Resolve(ctx, ref)
	if err != nil {
		return Descriptor{}, fmt.Errorf("resolve %q: %w", ref, err)
	}
	return descriptorFromOCI(desc), nil
}

// FetchBlob fetches a blob by digest.
func (r *RegistryStore) FetchBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	d, err := godigest.Parse(digest)
	if err != nil {
		return nil, fmt.Errorf("parse digest %q: %w", digest, err)
	}
	_, rc, err := r.repo.Blobs().FetchReference(ctx, d.String())
	if err != nil {
		return nil, fmt.Errorf("fetch blob %q: %w", digest, err)
	}
	return rc, nil
}

// ListManifests lists all manifests in the repository (via tag listing).
func (r *RegistryStore) ListManifests(ctx context.Context) ([]Descriptor, error) {
	var descs []Descriptor
	if err := r.repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			desc, err := r.repo.Resolve(ctx, tag)
			if err != nil {
				return fmt.Errorf("resolve tag %q: %w", tag, err)
			}
			descs = append(descs, descriptorFromOCI(desc))
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	return descs, nil
}
