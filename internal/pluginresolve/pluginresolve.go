// Package pluginresolve computes a pinned, transitive, mandatory Jenkins
// plugin closure from a set of seed artifactIds and a target Jenkins core
// version, against a caller-supplied metadata source.
//
// It is a pure library: it performs no filesystem or network I/O of its own.
// All I/O is delegated to a MetadataSource implementation the caller supplies,
// or to plain byte slices passed as ordinary arguments. It has no caller in
// this repository yet — see the add-incluster-plugin-resolver change.
package pluginresolve

import (
	"context"
	"errors"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// PluginPin is one member of a resolved Closure: an exact plugin version,
// pinned by a MetadataSource lookup.
type PluginPin struct {
	ArtifactID   string
	Version      string
	SHA256       string // base64, matching ucmeta.PluginMeta's encoding
	RequiredCore string
}

// Closure is the pinned, transitive, mandatory dependency set Resolve
// produces. Plugins is sorted lexicographically by ArtifactID.
type Closure struct {
	Plugins []PluginPin
}

// MetadataSource answers "what version of this plugin, at or above this
// minimum, is available?". The two implementations in this package
// (UpstreamSource, InClusterSource) are both thin adapters over existing
// code; a caller may supply its own for testing.
type MetadataSource interface {
	Resolve(ctx context.Context, name, minVersion string) ucmeta.Resolution
}

// ErrInvalidVersion is returned when a version string this package must parse
// — the resolution target, or a RequiredCore value read off a plugin/HPI —
// does not parse. It is a caller/data error, not retryable.
var ErrInvalidVersion = errors.New("pluginresolve: invalid version")

// ErrCoreFloorExceeded is returned when a resolved plugin version's
// RequiredCore exceeds the resolution target. It is a caller/data error, not
// retryable: the seed set or the target must change.
var ErrCoreFloorExceeded = errors.New("pluginresolve: plugin requires a newer Jenkins core than the target")

// ErrUnresolved is returned when a MetadataSource answers NotListed for a
// required name: every configured source is healthy and none lists it. This
// is a provable negative, not retryable.
var ErrUnresolved = errors.New("pluginresolve: plugin version not found")

// ErrMetadataUnavailable is returned when a MetadataSource answers
// SourcesDegraded for a required name: at least one source failed and no
// healthy source answered. The negative is unprovable, so this is retryable.
var ErrMetadataUnavailable = errors.New("pluginresolve: plugin metadata unavailable")

// ErrTooDeep is returned when the transitive walk exceeds maxResolveDepth.
// Not retryable: the seed set forms a chain too long to be a real Jenkins
// plugin closure.
var ErrTooDeep = errors.New("pluginresolve: dependency closure exceeded the depth cap")

// ErrRootCoreFloorExceeded is returned by AssertCoreFloor when the root HPI's
// own RequiredCore exceeds the target. It is distinct from
// ErrCoreFloorExceeded because the root is not a resolved Closure member — it
// is the caller-supplied HPI being asserted against.
var ErrRootCoreFloorExceeded = errors.New("pluginresolve: root plugin requires a newer Jenkins core than the target")
