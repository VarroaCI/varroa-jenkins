package pluginresolve

import (
	"context"
	"fmt"
	"sort"

	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// maxResolveDepth caps the transitive walk. It matches
// internal/updatecenter/closure.go's maxClosureDepth: a real Jenkins plugin
// closure never approaches this depth, so exceeding it means a cycle or a
// metadata source answering nonsense.
const maxResolveDepth = 32

// Resolve computes the pinned, transitive, mandatory dependency closure of
// seeds against source, gating every resolved member's RequiredCore against
// target. It re-enqueues a name whenever a newly discovered dependency raises
// its minimum above what was already resolved, so the final answer reflects
// the highest minimum any member of the closure declared.
func Resolve(ctx context.Context, target string, seeds []string, source MetadataSource) (Closure, error) {
	if _, ok := jenkinsver.Core(target); !ok {
		return Closure{}, fmt.Errorf("%w: target %q", ErrInvalidVersion, target)
	}

	w := &walker{
		requirements: map[string]string{},
		depths:       map[string]int{},
		resolved:     map[string]ucmeta.PluginMeta{},
	}
	for _, seed := range seeds {
		if err := w.raise(seed, "0", 1); err != nil {
			return Closure{}, err
		}
	}

	for len(w.queue) > 0 {
		name := w.queue[0]
		w.queue = w.queue[1:]
		minVersion := w.requirements[name]

		res := source.Resolve(ctx, name, minVersion)
		switch res.Outcome {
		case ucmeta.Resolved:
			meta := res.Meta
			if err := checkCoreFloor(target, meta.RequiredCore, meta.Name, meta.Version); err != nil {
				return Closure{}, err
			}
			w.resolved[name] = meta
			for _, dep := range meta.Dependencies {
				if dep.Optional {
					continue
				}
				if err := w.raise(dep.Name, dep.Version, w.depths[name]+1); err != nil {
					return Closure{}, err
				}
			}
		case ucmeta.NotListed:
			return Closure{}, fmt.Errorf("%w: %s@%s", ErrUnresolved, name, minVersion)
		case ucmeta.SourcesDegraded:
			return Closure{}, fmt.Errorf("%w: %s@%s", ErrMetadataUnavailable, name, minVersion)
		}
	}

	plugins := make([]PluginPin, 0, len(w.resolved))
	for name, meta := range w.resolved {
		plugins = append(plugins, PluginPin{
			ArtifactID:   name,
			Version:      meta.Version,
			SHA256:       meta.SHA256,
			RequiredCore: meta.RequiredCore,
		})
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].ArtifactID < plugins[j].ArtifactID })
	return Closure{Plugins: plugins}, nil
}

// walker holds the worklist state for one Resolve call: requirements tracks
// the highest minimum seen so far per name, depths its first-seen depth, and
// resolved the metadata each name settled on.
type walker struct {
	requirements map[string]string
	depths       map[string]int
	resolved     map[string]ucmeta.PluginMeta
	queue        []string
}

// raise records a requirement on name at depth, re-enqueuing it when the
// requirement rises above what was already recorded. The depth cap is
// checked unconditionally, before the standing-requirement check below, so a
// chain that only ever tightens an already-seen requirement still trips
// ErrTooDeep once it runs deep enough.
func (w *walker) raise(name, minVersion string, depth int) error {
	if depth > maxResolveDepth {
		return fmt.Errorf("%w: %s", ErrTooDeep, name)
	}
	cur, seen := w.requirements[name]
	if seen && pluginver.AtLeast(cur, minVersion) {
		return nil
	}
	if !seen {
		w.depths[name] = depth
	}
	w.requirements[name] = minVersion
	w.queue = append(w.queue, name)
	return nil
}

// checkCoreFloor gates a resolved plugin's RequiredCore against target. An
// empty RequiredCore has no floor to check.
func checkCoreFloor(target, requiredCore, name, version string) error {
	if requiredCore == "" {
		return nil
	}
	atLeast, ok := jenkinsver.AtLeast(target, requiredCore)
	if !ok {
		return fmt.Errorf("%w: %s@%s requires core %q", ErrInvalidVersion, name, version, requiredCore)
	}
	if !atLeast {
		return fmt.Errorf("%w: %s@%s requires core >= %q, target is %q", ErrCoreFloorExceeded, name, version, requiredCore, target)
	}
	return nil
}
