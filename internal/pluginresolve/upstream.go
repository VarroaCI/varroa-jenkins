package pluginresolve

import (
	"context"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// UpstreamSource resolves against updates.jenkins.io metadata via an existing
// *ucmeta.Resolver. It adds no logic of its own.
type UpstreamSource struct {
	Resolver *ucmeta.Resolver
}

// Resolve implements MetadataSource. A nil Resolver reports SourcesDegraded
// rather than panicking: no source answered, and the negative is unprovable,
// so callers treat it as retryable metadata unavailability.
func (s UpstreamSource) Resolve(ctx context.Context, name, minVersion string) ucmeta.Resolution {
	if s.Resolver == nil {
		return ucmeta.Resolution{Outcome: ucmeta.SourcesDegraded}
	}
	return s.Resolver.ResolveSatisfying(ctx, name, minVersion)
}
