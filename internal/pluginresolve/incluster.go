package pluginresolve

import (
	"context"
	"encoding/base64"
	"encoding/hex"

	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// InClusterSource resolves against the in-cluster plugin-pack store: it walks
// every manifest the store holds and picks the newest version satisfying
// minVersion, exactly as UpstreamSource's caller-visible contract does.
type InClusterSource struct {
	Store oci.BlobStore
}

// Resolve implements MetadataSource. A manifest this store cannot read (a
// failed Pull, or a plugin pack that will not parse) does not fail the
// lookup outright: the store serves what it can. Only when every manifest is
// unreadable AND none of the readable ones list the plugin does the lookup
// report SourcesDegraded rather than the provable NotListed.
func (s InClusterSource) Resolve(ctx context.Context, name, minVersion string) ucmeta.Resolution {
	descs, err := s.Store.ListManifests(ctx)
	anyDegraded := err != nil

	var best *oci.ResolvedPlugin
	var bestDigest string

	for _, d := range descs {
		ref := d.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			ref = d.Digest
		}
		manifest, err := s.Store.Pull(ctx, ref)
		if err != nil {
			anyDegraded = true
			continue
		}
		if manifest.ArtifactType != oci.ArtifactTypePluginPack {
			continue
		}
		_, plugins, err := oci.ReadPluginPack(ctx, s.Store, ref)
		if err != nil {
			anyDegraded = true
			continue
		}

		for i := range plugins {
			p := plugins[i]
			if p.Name != name || !pluginver.AtLeast(p.Version, minVersion) {
				continue
			}
			if best == nil || preferCandidate(p, d.Digest, *best, bestDigest) {
				best, bestDigest = &p, d.Digest
			}
		}
	}

	if best == nil {
		if anyDegraded {
			return ucmeta.Resolution{Outcome: ucmeta.SourcesDegraded}
		}
		return ucmeta.Resolution{Outcome: ucmeta.NotListed}
	}
	return ucmeta.Resolution{Outcome: ucmeta.Resolved, Meta: toPluginMeta(*best)}
}

// preferCandidate reports whether candidate should replace incumbent as the
// resolved answer. A higher version always wins; a tie on version is broken
// first by completeness (a pack whose HPI metadata could not be derived at
// build time loses to one that has it) and then by the lexicographically
// smaller manifest digest, so two packs offering identical plugin content
// resolve to the same winner regardless of ListManifests order.
func preferCandidate(candidate oci.ResolvedPlugin, candidateDigest string, incumbent oci.ResolvedPlugin, incumbentDigest string) bool {
	if cmp := pluginver.Compare(candidate.Version, incumbent.Version); cmp != 0 {
		return cmp > 0
	}
	if cc, ic := completeness(candidate), completeness(incumbent); cc != ic {
		return cc > ic
	}
	return candidateDigest < incumbentDigest
}

// completeness scores how much HPI-derived metadata a resolved plugin carries.
// ApplyHPIMetadata may fail non-fatally at pack-build time, so two packs can
// list the same plugin version with different amounts of derived data.
func completeness(p oci.ResolvedPlugin) int {
	score := 0
	if p.RequiredCore != "" {
		score++
	}
	if len(p.Dependencies) > 0 {
		score++
	}
	return score
}

// toPluginMeta converts a resolved plugin-pack entry into the ucmeta.PluginMeta
// shape a MetadataSource answer carries.
func toPluginMeta(p oci.ResolvedPlugin) ucmeta.PluginMeta {
	deps := make([]ucmeta.Dep, 0, len(p.Dependencies))
	for _, d := range p.Dependencies {
		deps = append(deps, ucmeta.Dep{Name: d.Name, Version: d.Min, Optional: d.Optional})
	}
	return ucmeta.PluginMeta{
		Name:         p.Name,
		Version:      p.Version,
		SHA256:       toBase64SHA256(p.SHA256),
		RequiredCore: p.RequiredCore,
		Dependencies: deps,
	}
}

// toBase64SHA256 converts a "sha256:<hex>" digest to the base64 encoding
// ucmeta.PluginMeta.SHA256 carries. This restates
// internal/updatecenter/metadata.go's conversion rather than importing it: the
// two packages read the digest from different stores and must not couple on a
// shared helper for three standard-library calls.
func toBase64SHA256(digest string) string {
	const prefix = "sha256:"
	hexDigest := digest
	if len(digest) > len(prefix) && digest[:len(prefix)] == prefix {
		hexDigest = digest[len(prefix):]
	}
	raw, err := hex.DecodeString(hexDigest)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}
