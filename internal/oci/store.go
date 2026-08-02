package oci

import (
	"context"
	"io"
)

// Descriptor describes an OCI content descriptor.
type Descriptor struct {
	MediaType   string
	Digest      string // "sha256:<hex>"
	Size        int64
	Annotations map[string]string
}

// Manifest represents an OCI manifest.
type Manifest struct {
	ArtifactType string
	Config       Descriptor
	Layers       []Descriptor
	Annotations  map[string]string
}

// BlobStore is the read/write surface every OCI-backed store in Varroa implements.
// C2's update-center service depends on THIS INTERFACE only — it must never redefine it
// or depend on LayoutStore/RegistryStore directly.
type BlobStore interface {
	Push(ctx context.Context, ref string, manifest Manifest) error
	PushBlob(ctx context.Context, mediaType string, content io.Reader) (digest string, size int64, err error)
	Pull(ctx context.Context, ref string) (Manifest, error)
	Resolve(ctx context.Context, ref string) (Descriptor, error)
	FetchBlob(ctx context.Context, digest string) (io.ReadCloser, error)
	ListManifests(ctx context.Context) ([]Descriptor, error)
}
