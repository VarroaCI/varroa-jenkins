package controller

import (
	"fmt"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// Provenance values recorded on a closure entry.
const (
	provenanceStore = "store"
	provenanceLock  = "lock"
)

// ucProfile is one JenkinsVersionProfile as the derivation loop sees it: its
// effective deployed core and its materialized lock, plus whether it is
// eligible to be consulted at all.
//
// Eligibility is deliberately a field rather than something recomputed at each
// use. A profile whose plugin set is not ready may still carry a stale
// contentRef, and a stale lock voting in a unanimity test manufactures
// agreement that does not exist. Both the resolver's lock fallback and the
// verdict evaluator must apply the same rule, so it is decided once.
type ucProfile struct {
	Name string
	// EffectiveCore is spec.resolveVersion when set, else spec.version. An
	// LTS-line profile deploys the resolved patch, so comparing a plugin's
	// requiredCore against the bare line reports core-too-old for plugins that
	// in fact run. This value is READ from the profile and never derived,
	// inferred, or written back.
	EffectiveCore string
	Eligible      bool
	// Lock maps artifactId to the version this profile's plugin set pins.
	Lock map[string]string
}

// ucInventory is the store inventory indexed for resolution.
type ucInventory struct {
	// entries maps "name@version" to every canonical entry the store reported
	// for it. More than one means the store cannot resolve that pair to bytes.
	entries map[string][]inventoryEntry
	// candidates maps a plugin name to its non-poisoned versions, sorted
	// ascending by the pluginver total order.
	candidates map[string][]string
	// names is every distinct plugin name in the inventory, sorted.
	names []string
	// mandatoryEdges is the total count of non-optional dependency edges across
	// the whole inventory; it feeds the round cap.
	mandatoryEdges int
}

func invKey(name, version string) string { return name + "@" + version }

// newUCInventory indexes the fetched inventory and identifies poisoned
// (name, version) pairs — those the store reports more than one distinct
// canonical entry for. A poisoned pair cannot be resolved to bytes, so it is
// excluded from every candidate set before any ordering happens. The poison is
// per VERSION, not per plugin: a poisoned X@1 never blocks selecting a clean
// X@2.
func newUCInventory(entries []inventoryEntry) *ucInventory {
	inv := &ucInventory{
		entries:    make(map[string][]inventoryEntry, len(entries)),
		candidates: make(map[string][]string),
	}
	for _, e := range entries {
		if e.Name == "" || e.Version == "" {
			continue
		}
		inv.entries[invKey(e.Name, e.Version)] = append(inv.entries[invKey(e.Name, e.Version)], e)
	}

	nameSet := make(map[string]map[string]struct{})
	for _, group := range inv.entries {
		e := group[0]
		if _, ok := nameSet[e.Name]; !ok {
			nameSet[e.Name] = make(map[string]struct{})
		}
		if len(group) > 1 {
			// Poisoned: known to the store, never a candidate.
			continue
		}
		nameSet[e.Name][e.Version] = struct{}{}
		for _, d := range e.Dependencies {
			if !d.Optional && d.Name != "" {
				inv.mandatoryEdges++
			}
		}
	}

	inv.names = make([]string, 0, len(nameSet))
	for name, versions := range nameSet {
		inv.names = append(inv.names, name)
		vs := make([]string, 0, len(versions))
		for v := range versions {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool { return pluginver.Compare(vs[i], vs[j]) < 0 })
		if len(vs) > 0 {
			inv.candidates[name] = vs
		}
	}
	sort.Strings(inv.names)
	return inv
}

// poisoned reports whether the store holds conflicting entries for the pair.
func (inv *ucInventory) poisoned(name, version string) bool {
	return len(inv.entries[invKey(name, version)]) > 1
}

// entry returns the single canonical entry for a non-poisoned pair.
func (inv *ucInventory) entry(name, version string) (inventoryEntry, bool) {
	group := inv.entries[invKey(name, version)]
	if len(group) != 1 {
		return inventoryEntry{}, false
	}
	return group[0], true
}

// mandatoryDeps returns the non-optional dependencies a version declares, in
// canonical (name-sorted) order. Optional dependencies are excluded from every
// closure — the property this change, the upload planner, and the drift closure
// must all agree on.
func (inv *ucInventory) mandatoryDeps(name, version string) []pluginDep {
	e, ok := inv.entry(name, version)
	if !ok {
		return nil
	}
	out := make([]pluginDep, 0, len(e.Dependencies))
	for _, d := range e.Dependencies {
		if d.Optional || d.Name == "" {
			continue
		}
		out = append(out, d)
	}
	return out
}

// maxVersion returns the Compare-greater of two minimums, with "" losing to
// anything. It is total and cannot fail.
func maxVersion(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if pluginver.Compare(b, a) > 0 {
		return b
	}
	return a
}

// mergeConstraint adds a name to the accumulator or raises its minimum, and
// reports whether anything changed.
//
// Adding a NAME with an empty minimum is itself a change. Membership is tracked
// separately from the minimum, because a bare maxVersion("", "") comparison
// reports no change and would silently drop a dependency declared without a
// minimum — the common shape for a plugin that simply requires another.
func mergeConstraint(constraints map[string]string, name, minVer string) bool {
	cur, ok := constraints[name]
	if !ok {
		constraints[name] = minVer
		return true
	}
	m := maxVersion(cur, minVer)
	if m == cur {
		return false
	}
	constraints[name] = m
	return true
}

// ucSelection is one resolved closure member.
type ucSelection struct {
	Version    string
	Provenance string
	// Shortfall reports that the selected version is below the effective
	// minimum. It is never a failure: the version is still selected and the
	// shortfall is classified per profile as a verdict.
	Shortfall bool
	// LockSources names the eligible profiles whose locks supplied the pin,
	// set only when Provenance is "lock". It is what keeps a lock-sourced
	// shortfall a per-profile fact rather than a store-wide one.
	LockSources []string
}

// ucClosure is the solver's output.
type ucClosure struct {
	// Root is the (name, version) the item is derived for.
	RootName    string
	RootVersion string
	// RootMinimum is the accumulated constraint on the root's own name, when
	// the closure declared one; RootShortfall reports the root falling below it.
	RootMinimum   string
	RootShortfall bool
	// Selected maps every closure member (never the root) to its selection.
	Selected map[string]ucSelection
	// Minimums is the effective minimum in force for each member, "" when none
	// was declared.
	Minimums map[string]string
	// Direct marks members the root itself declared, as opposed to members
	// pulled in transitively.
	Direct map[string]bool
}

// resolveClosure computes the full mandatory dependency closure of root,
// pinning every member to an exact version.
//
// It is a fixpoint over a MONOTONE constraint accumulator: constraints are
// added and never retracted, and a name once in the accumulator stays in the
// closure. A single traversal is not enough for two reasons. A visited-set keyed
// on plugin name lets whichever path arrives first pin a version that satisfies
// its own minimum and violates another path's. And dependency edges are PER
// VERSION, so selecting a version reveals new constraints — recomputing minima
// from only the currently-selected versions cycles forever.
//
// The whole function is a pure function of its inputs: both passes iterate a
// sorted key slice rather than ranging a map, so identical inputs give a
// byte-identical fragment and status.contentHash does not flap between ticks.
func resolveClosure(root inventoryEntry, inv *ucInventory, profiles []ucProfile) (*ucClosure, error) {
	cl := &ucClosure{
		RootName:    root.Name,
		RootVersion: root.Version,
		Selected:    make(map[string]ucSelection),
		Minimums:    make(map[string]string),
		Direct:      make(map[string]bool),
	}

	constraints := make(map[string]string)
	for _, d := range inv.mandatoryDeps(root.Name, root.Version) {
		mergeConstraint(constraints, d.Name, d.Min)
		cl.Direct[d.Name] = true
	}

	// A bound counting only plugin names is wrong: one name's constraint can
	// rise once per stored version (root needs X>=1, X@1 needs X>=2, ...).
	// Every productive round either adds a name or raises a constraint to some
	// declared edge's Min, so names + edges bounds both. Computed once, at
	// entry — a bound derived from len(constraints) mid-loop would be a moving
	// target.
	maxRounds := len(inv.names) + inv.mandatoryEdges + 2

	for round := 0; round < maxRounds; round++ {
		changed := false

		// Pass 1 — selection.
		for _, name := range sortedKeys(constraints) {
			if name == root.Name {
				// The root's version is what the item IS. Re-selecting it would
				// let a dependency cycle change the item's own identity and
				// could emit the root twice at two versions. The constraint is
				// still recorded and still evaluated below.
				continue
			}
			sel, err := resolveOne(name, constraints[name], inv, profiles)
			if err != nil {
				return nil, err
			}
			if prev, ok := cl.Selected[name]; !ok || prev.Version != sel.Version {
				changed = true
			}
			cl.Selected[name] = sel
		}

		// Pass 2 — constraint accumulation.
		for _, name := range sortedKeys(cl.Selected) {
			for _, d := range inv.mandatoryDeps(name, cl.Selected[name].Version) {
				if mergeConstraint(constraints, d.Name, d.Min) {
					changed = true
				}
			}
		}

		if !changed {
			cl.Minimums = make(map[string]string, len(constraints))
			for name, m := range constraints {
				if name == root.Name {
					cl.RootMinimum = m
					cl.RootShortfall = m != "" && !pluginver.AtLeast(root.Version, m)
					continue
				}
				cl.Minimums[name] = m
			}
			// Re-stamp shortfalls against the FINAL minimums: a selection made
			// in an early round can be undercut by a constraint raised later.
			for name, sel := range cl.Selected {
				m := cl.Minimums[name]
				sel.Shortfall = m != "" && !pluginver.AtLeast(sel.Version, m)
				cl.Selected[name] = sel
			}
			return cl, nil
		}
	}

	// The bound is conservative: with finite candidate sets and monotone
	// constraints EVERY annotation graph converges within it, contradictory
	// ones included. Exhausting it therefore does not mean the plugin metadata
	// is bad — it means this solver or its arithmetic has a defect.
	return nil, fmt.Errorf("dependency resolution did not converge after %d rounds; "+
		"an internal solver invariant was violated", maxRounds)
}

// resolveOne selects a version for name given the effective minimum m.
//
// Selection never fails on a shortfall. It fails only when no version can be
// NAMED. The rows are ordered and first-match-wins, which makes them total and
// mutually exclusive by construction. m == "" means "no minimum" and is
// satisfied by every candidate, so row 3 covers it.
func resolveOne(name, m string, inv *ucInventory, profiles []ucProfile) (ucSelection, error) {
	candidates := inv.candidates[name]

	if len(candidates) == 0 {
		// Row 1 — no store candidate, but every eligible profile lock that
		// mentions the plugin agrees on one non-poisoned version.
		var pin string
		var sources []string
		for _, p := range profiles {
			if !p.Eligible {
				continue
			}
			v, ok := p.Lock[name]
			if !ok || v == "" {
				continue
			}
			if pin == "" {
				pin = v
			} else if pin != v {
				return ucSelection{}, fmt.Errorf(
					"dependency %q is absent from the update-center store and profile locks disagree on its version (%s vs %s)",
					name, pin, v)
			}
			sources = append(sources, p.Name)
		}
		if pin != "" && !inv.poisoned(name, pin) {
			return ucSelection{
				Version:     pin,
				Provenance:  provenanceLock,
				Shortfall:   m != "" && !pluginver.AtLeast(pin, m),
				LockSources: sources,
			}, nil
		}
		// Row 2 — the only failure row. A unanimous lock naming a poisoned
		// version lands here rather than re-admitting the poison.
		if pin != "" {
			return ucSelection{}, fmt.Errorf(
				"dependency %q is absent from the update-center store and its unanimous profile-lock pin %s "+
					"has conflicting store entries", name, pin)
		}
		if m != "" {
			return ucSelection{}, fmt.Errorf(
				"dependency %q (minimum %s) is absent from the update-center store and no profile lock pins it",
				name, m)
		}
		return ucSelection{}, fmt.Errorf(
			"dependency %q is absent from the update-center store and no profile lock pins it", name)
	}

	// Row 3 — the LEAST candidate satisfying the minimum. Picking the least
	// keeps the closure as close to the declared floor as the store allows.
	for _, v := range candidates {
		if m == "" || pluginver.AtLeast(v, m) {
			return ucSelection{Version: v, Provenance: provenanceStore}, nil
		}
	}

	// Row 4 — nothing satisfies the minimum: take the greatest candidate and
	// record a shortfall. This is not a rejection; it becomes a verdict.
	return ucSelection{
		Version:    candidates[len(candidates)-1],
		Provenance: provenanceStore,
		Shortfall:  true,
	}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// closureContent renders the plugins.yaml fragment: the root exactly once, plus
// every closure member, sorted by artifactId with every version exact. It
// satisfies both pin checks — validatePluginContent rejects an empty version and
// ValidateContent errors on empty and warns on "latest" — because a derived
// fragment never emits either.
func closureContent(cl *ucClosure) string {
	type pin struct{ id, version string }
	pins := make([]pin, 0, len(cl.Selected)+1)
	pins = append(pins, pin{cl.RootName, cl.RootVersion})
	for name, sel := range cl.Selected {
		pins = append(pins, pin{name, sel.Version})
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].id < pins[j].id })

	var b strings.Builder
	b.WriteString("plugins:\n")
	for _, p := range pins {
		fmt.Fprintf(&b, "  - artifactId: %s\n    version: %q\n", p.id, p.version)
	}
	return b.String()
}

// closureStatus projects the solver's record onto the CRD. status.content is a
// FLAT fragment: once serialized it can express neither direct-vs-transitive nor
// provenance nor the minimum in force, and the frontend can re-derive none of
// them — it has neither the store annotations nor the solver.
func closureStatus(cl *ucClosure) []v1alpha1.CatalogItemClosureEntry {
	out := make([]v1alpha1.CatalogItemClosureEntry, 0, len(cl.Selected)+1)
	out = append(out, v1alpha1.CatalogItemClosureEntry{
		ArtifactID: cl.RootName,
		Version:    cl.RootVersion,
		Direct:     true,
		Provenance: provenanceStore,
		Minimum:    cl.RootMinimum,
	})
	for _, name := range sortedKeys(cl.Selected) {
		sel := cl.Selected[name]
		out = append(out, v1alpha1.CatalogItemClosureEntry{
			ArtifactID: name,
			Version:    sel.Version,
			Direct:     cl.Direct[name],
			Provenance: sel.Provenance,
			Minimum:    cl.Minimums[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArtifactID < out[j].ArtifactID })
	return out
}
