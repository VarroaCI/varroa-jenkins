package updatecenter

import (
	"context"
	"slices"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// latestCoreResolveTimeout bounds one upstream latest lookup made for a metadata request.
const latestCoreResolveTimeout = 10 * time.Second

// preferCompatible returns the candidates whose requiredCore runs on core. When no
// candidate qualifies, it returns candidates unchanged, so the tool sees the
// requiredCore error for the version it asked for.
func preferCompatible(candidates []pluginLayerInfo, core string) []pluginLayerInfo {
	var compatible []pluginLayerInfo
	for _, c := range candidates {
		if ucmeta.CoreCompatible(c.RequiredCore, core) {
			compatible = append(compatible, c)
		}
	}
	if len(compatible) == 0 {
		return candidates
	}
	return compatible
}

// pinnedInStore reports whether the declared set pins name at a version that one
// of the stored candidates holds. A core filter must not hide such a pin, because the
// declared set is the desired state.
func pinnedInStore(d DeclaredSet, name string, candidates []pluginLayerInfo) bool {
	for _, c := range candidates {
		if slices.Contains(d[name], c.Version) {
			return true
		}
	}
	return false
}

// declaredLatestNames returns the sorted names that the declared set lists at latest.
func declaredLatestNames(d DeclaredSet) []string {
	var names []string
	for name, versions := range d {
		for _, v := range versions {
			if ucmeta.IsLatest(v) {
				names = append(names, name)
				break
			}
		}
	}
	slices.Sort(names)
	return names
}

// upstreamLatestEntry asks the metadata sources for the highest version of name whose
// requiredCore runs on core. It returns false when pull-through is off, when there is
// no core to match, or when no healthy source lists a compatible version.
func (s *Server) upstreamLatestEntry(ctx context.Context, name, core string) (updateCenterPlugin, bool) {
	if core == "" || !s.pullThroughEnabled || s.resolver == nil {
		return updateCenterPlugin{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, latestCoreResolveTimeout)
	defer cancel()
	a := s.resolver.ResolveLatestForCore(ctx, name, core)
	if a.Outcome != ucmeta.Resolved || a.Meta == nil {
		return updateCenterPlugin{}, false
	}
	deps := make([]updateCenterDependency, 0, len(a.Meta.Dependencies))
	for _, d := range a.Meta.Dependencies {
		deps = append(deps, updateCenterDependency{Name: d.Name, Optional: d.Optional, Version: d.Version})
	}
	return updateCenterPlugin{
		Name:         name,
		Version:      a.Version,
		URL:          name + "/" + a.Version + "/" + name + ".hpi",
		SHA256:       a.SHA256,
		RequiredCore: a.Meta.RequiredCore,
		Dependencies: deps,
	}, true
}
