package oci

import (
	"context"
	"fmt"

	"oras.land/oras-go/v2"
)

// hasTarget is implemented by LayoutStore and RegistryStore to expose the
// underlying oras.Target for use with oras.Copy.
type hasTarget interface {
	target() oras.Target
}

// Copy copies content (manifest + blobs) from src to dst using oras-go's copy mechanism.
// srcRef and dstRef are the source and destination references (tags or digests).
func Copy(ctx context.Context, src BlobStore, srcRef string, dst BlobStore, dstRef string) error {
	srcTarget, ok := src.(hasTarget)
	if !ok {
		return fmt.Errorf("source BlobStore does not support oras.Copy (type %T)", src)
	}
	dstTarget, ok := dst.(hasTarget)
	if !ok {
		return fmt.Errorf("destination BlobStore does not support oras.Copy (type %T)", dst)
	}

	_, err := oras.Copy(ctx, srcTarget.target(), srcRef, dstTarget.target(), dstRef, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("oras copy from %q to %q: %w", srcRef, dstRef, err)
	}
	return nil
}
