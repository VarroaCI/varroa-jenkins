package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	godigest "github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

// LayoutStore implements BlobStore backed by an OCI-layout directory on disk.
type LayoutStore struct {
	store *oci.Store
	root  string
}

// NewLayoutStore opens (or creates) an OCI-layout store at the given directory path.
func NewLayoutStore(path string) (*LayoutStore, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("create layout dir: %w", err)
	}
	store, err := oci.New(path)
	if err != nil {
		return nil, fmt.Errorf("open oci store: %w", err)
	}
	return &LayoutStore{store: store, root: path}, nil
}

// target returns the underlying oras.Target so copy.go can hand it to oras.Copy.
func (l *LayoutStore) target() oras.Target {
	return l.store
}

// Push pushes a manifest with the given reference.
func (l *LayoutStore) Push(ctx context.Context, ref string, manifest Manifest) error {
	ociManifest := toOCIManifest(manifest)
	manifestBytes, err := json.Marshal(ociManifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBytes)
	if err := l.store.Push(ctx, desc, bytes.NewReader(manifestBytes)); err != nil {
		if errors.Is(err, errdef.ErrAlreadyExists) {
			// Manifest content already exists — still tag it
			return l.store.Tag(ctx, desc, ref)
		}
		return fmt.Errorf("push manifest content: %w", err)
	}
	return l.store.Tag(ctx, desc, ref)
}

// PushBlob pushes a blob and returns its digest and size.
func (l *LayoutStore) PushBlob(ctx context.Context, mediaType string, r io.Reader) (digest string, size int64, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	desc := content.NewDescriptorFromBytes(mediaType, data)
	if err := l.store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		if errors.Is(err, errdef.ErrAlreadyExists) {
			// Content already exists — this is harmless for idempotent pushes
			return desc.Digest.String(), desc.Size, nil
		}
		return "", 0, err
	}
	return desc.Digest.String(), desc.Size, nil
}

// Pull retrieves a manifest by reference.
func (l *LayoutStore) Pull(ctx context.Context, ref string) (Manifest, error) {
	src, err := l.store.Resolve(ctx, ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve %q: %w", ref, err)
	}
	rc, err := l.store.Fetch(ctx, src)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch manifest %q: %w", ref, err)
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
func (l *LayoutStore) Resolve(ctx context.Context, ref string) (Descriptor, error) {
	desc, err := l.store.Resolve(ctx, ref)
	if err != nil {
		return Descriptor{}, fmt.Errorf("resolve %q: %w", ref, err)
	}
	return descriptorFromOCI(desc), nil
}

// FetchBlob fetches a blob by digest.
func (l *LayoutStore) FetchBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	d, err := godigest.Parse(digest)
	if err != nil {
		return nil, fmt.Errorf("parse digest %q: %w", digest, err)
	}
	desc := ocispec.Descriptor{Digest: d}
	rc, err := l.store.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch blob %q: %w", digest, err)
	}
	return rc, nil
}

// ListManifests lists all manifests in the store by reading index.json.
func (l *LayoutStore) ListManifests(ctx context.Context) ([]Descriptor, error) {
	indexPath := filepath.Join(l.root, ocispec.ImageIndexFile)
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []Descriptor{}, nil
		}
		return nil, fmt.Errorf("read index: %w", err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("unmarshal index: %w", err)
	}
	descs := make([]Descriptor, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		descs = append(descs, descriptorFromOCI(m))
	}
	return descs, nil
}

// -- conversion helpers --

func toOCIManifest(m Manifest) ocispec.Manifest {
	layers := make([]ocispec.Descriptor, len(m.Layers))
	for i, l := range m.Layers {
		layers[i] = descriptorToOCI(l)
	}
	return ocispec.Manifest{
		Versioned:    imagespec.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: m.ArtifactType,
		Config:       descriptorToOCI(m.Config),
		Layers:       layers,
		Annotations:  m.Annotations,
	}
}

func fromOCIManifest(m ocispec.Manifest) Manifest {
	layers := make([]Descriptor, len(m.Layers))
	for i, l := range m.Layers {
		layers[i] = descriptorFromOCI(l)
	}
	return Manifest{
		ArtifactType: m.ArtifactType,
		Config:       descriptorFromOCI(m.Config),
		Layers:       layers,
		Annotations:  m.Annotations,
	}
}

func descriptorFromOCI(d ocispec.Descriptor) Descriptor {
	return Descriptor{
		MediaType:   d.MediaType,
		Digest:      d.Digest.String(),
		Size:        d.Size,
		Annotations: d.Annotations,
	}
}

func descriptorToOCI(d Descriptor) ocispec.Descriptor {
	parsed, err := godigest.Parse(d.Digest)
	if err != nil {
		panic(fmt.Sprintf("internal/oci: invalid digest %q: %v", d.Digest, err))
	}
	return ocispec.Descriptor{
		MediaType:   d.MediaType,
		Digest:      parsed,
		Size:        d.Size,
		Annotations: d.Annotations,
	}
}
