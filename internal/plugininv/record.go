// Package plugininv implements controller plugin inventory collection,
// canonical hashing, and closure-aware provenance classification. It is pure
// Go with no imports from internal/mite, internal/controller, or internal/api
// — an import cycle through pluginlock is the failure mode being prevented.
package plugininv

import (
	"fmt"
	"sort"
	"time"
)

// Tri is a three-valued logic type where the integer order IS the proposal's
// null < false < true ordering. This is the one documented exception to R19:
// inside plugininv the integer order is meaningful for sorting.
type Tri uint8

const (
	// TriUnknown is the zero value, representing null/unobservable.
	TriUnknown Tri = 0
	// TriFalse represents an observed false value.
	TriFalse Tri = 1
	// TriTrue represents an observed true value.
	TriTrue Tri = 2
)

// Dep is one declared dependency of a plugin: name, minimum version, and
// whether it is optional.
type Dep struct {
	Name     string
	Min      string
	Optional bool
}

// Record is a single installed plugin as observed from a collection source.
type Record struct {
	Name     string
	Version  string
	Enabled  Tri
	Detached Tri
	Bundled  Tri
	Deps     []Dep
}

// Inventory is the full set of plugins collected from a controller, plus
// collection metadata.
type Inventory struct {
	Records          []Record
	CollectedAt      time.Time
	Source           string // "jenkins-api" | "filesystem"
	CollectionFailed bool
	CollectionError  string
	Warnings         []string
}

// Source constants for Inventory.Source.
const (
	SourceJenkinsAPI = "jenkins-api"
	SourceFilesystem = "filesystem"
)

// depLess reports whether a sorts before b by the tuple (name, min, optional),
// with false before true for optional.
func depLess(a, b Dep) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Min != b.Min {
		return a.Min < b.Min
	}
	// false before true
	return !a.Optional && b.Optional
}

// recordLess is the full total order over Record: name, version, enabled,
// detached, bundled, then the byte-wise comparison of the canonicalized deps
// rendering. Deferred to after deps are sorted.
func recordLess(a, b Record, aDeps, bDeps string) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Version != b.Version {
		return a.Version < b.Version
	}
	if a.Enabled != b.Enabled {
		return a.Enabled < b.Enabled
	}
	if a.Detached != b.Detached {
		return a.Detached < b.Detached
	}
	if a.Bundled != b.Bundled {
		return a.Bundled < b.Bundled
	}
	return aDeps < bDeps
}

// Normalize performs in order: dep dedupe/sort, record sort by the full total
// order, record dedupe by name (keeping the first, appending a warning per
// drop). It is idempotent.
func (inv *Inventory) Normalize() {
	// Step 1: dedupe and sort deps within each record.
	for i := range inv.Records {
		inv.Records[i].Deps = normalizeDeps(inv.Records[i].Deps)
	}

	// Pre-compute the canonical deps rendering for each record so recordLess
	// can compare them without re-rendering.
	type indexed struct {
		r    Record
		deps string
	}
	items := make([]indexed, len(inv.Records))
	for i, r := range inv.Records {
		items[i] = indexed{r: r, deps: depsCanonical(r.Deps)}
	}

	// Step 2: sort by the full total order.
	sort.Slice(items, func(i, j int) bool {
		return recordLess(items[i].r, items[j].r, items[i].deps, items[j].deps)
	})

	// Step 3: dedupe by name, keeping the first, warning on drops.
	seen := make(map[string]bool, len(items))
	deduped := make([]Record, 0, len(items))
	for _, it := range items {
		if seen[it.r.Name] {
			inv.Warnings = append(inv.Warnings,
				fmt.Sprintf("plugininv: duplicate record for plugin %q dropped", it.r.Name))
			continue
		}
		seen[it.r.Name] = true
		deduped = append(deduped, it.r)
	}
	inv.Records = deduped
}

// normalizeDeps deduplicates and sorts a dependency list. Identical (name, min,
// optional) triples collapse; same-name entries differing in min or optional
// are all retained.
func normalizeDeps(deps []Dep) []Dep {
	if len(deps) <= 1 {
		return deps
	}

	// Sort first by the tuple order.
	sort.Slice(deps, func(i, j int) bool {
		return depLess(deps[i], deps[j])
	})

	// Deduplicate identical triples.
	out := make([]Dep, 0, len(deps))
	for i, d := range deps {
		if i > 0 && d.Name == out[len(out)-1].Name &&
			d.Min == out[len(out)-1].Min &&
			d.Optional == out[len(out)-1].Optional {
			continue
		}
		out = append(out, d)
	}
	return out
}

// depsCanonical renders deps to their canonical form used for record ordering.
// This must match the rendering in hash.go's writeDeps.
func depsCanonical(deps []Dep) string {
	if len(deps) == 0 {
		return `[]`
	}
	var buf []byte
	buf = append(buf, '[')
	for i, d := range deps {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"name":"`...)
		buf = append(buf, d.Name...)
		buf = append(buf, `","min":"`...)
		buf = append(buf, d.Min...)
		buf = append(buf, `","optional":`...)
		if d.Optional {
			buf = append(buf, "true}"...)
		} else {
			buf = append(buf, "false}"...)
		}
	}
	buf = append(buf, ']')
	return string(buf)
}
