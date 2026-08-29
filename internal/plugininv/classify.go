package plugininv

import (
	"sort"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// Provenance class labels (R19: labels, never ordinals across boundaries).
const (
	// ClassBootstrap is the closure root reported by the embedded plugin lock,
	// plus any recorded non-root member retained by the cross-check.
	ClassBootstrap = "bootstrap"
	// ClassDeclared marks a member of the declared plugin set.
	ClassDeclared = "declared"
	// ClassJenkinsSupplied marks a plugin where detached or bundled is true
	// and that was not already classified as declared.
	ClassJenkinsSupplied = "jenkins-supplied"
	// ClassDependency marks a plugin reachable from a class 1, 2 or 3 plugin
	// through mandatory edges only.
	ClassDependency = "dependency"
	// ClassOptionalDependency marks a plugin reachable from an expected root
	// through a path containing at least one optional edge.
	ClassOptionalDependency = "optional-dependency"
	// ClassUnmanaged marks a plugin that is none of the above — hand-installed.
	ClassUnmanaged = "unmanaged"
	// ClassUnmanagedOrOptional is the degradation-ladder bucket when optional
	// edges are dropped.
	ClassUnmanagedOrOptional = "unmanagedOrOptional"
)

// DeclaredBy tiers.
const (
	DeclaredByCore           = "core-lock"
	DeclaredByControllerSpec = "controller-spec"
	DeclaredByBundle         = "bundle"
)

// Version verdicts.
const (
	VerdictMatch   = "match"
	VerdictAhead   = "ahead"
	VerdictBehind  = "behind"
	VerdictMissing = "missing"
)

// ---------------------------------------------------------------------------
// Input / output types
// ---------------------------------------------------------------------------

// DeclaredPlugin is one entry of the declared plugin set.
type DeclaredPlugin struct {
	Name         string
	Version      string
	Tier         string   // DeclaredByCore, DeclaredByControllerSpec, or DeclaredByBundle
	Contributors []string // bundle input labels; empty when tier != bundle or key absent
}

// Inputs is the assembled input for a classification run.
type Inputs struct {
	Inventory        Inventory
	Declared         []DeclaredPlugin
	BootstrapRoot    string   // Bootstrap(...)[0].ArtifactID; "" if none
	BootstrapMembers []string // recorded non-root members, cross-check only
	BootstrapMatched bool
}

// ClassifiedPlugin is the classification result for one installed plugin.
type ClassifiedPlugin struct {
	Name           string
	Version        string
	Class          string   // provenance class label
	DeclaredBy     string   // tier label for class-2, empty otherwise
	Contributors   []string // bundle inputs for class-2 (empty if not bundle tier or absent)
	ImpliedBy      []string // expected roots that reach this plugin (classes 4, 5)
	OptionalEdge   string   // the name of the optional-edge plugin on the path (class 5 only)
	VersionVerdict string   // ahead/behind/missing for class-2 only
	Enabled        Tri
	Detached       Tri
	Bundled        Tri
}

// Advisory is a non-classification finding.
type Advisory struct {
	Code       string // "dependencyMinimumUnsatisfied"
	Plugin     string // the dependent (who declares the requirement)
	Dependency string // the dependency that is below minimum
	Min        string // declared minimum
	Version    string // installed version
}

// Classification is the complete result of a Classify run.
type Classification struct {
	Plugins              []ClassifiedPlugin
	Advisories           []Advisory
	BootstrapApproximate bool
	Total                int
	Counts               map[string]int // class label → count
}

// ---------------------------------------------------------------------------
// Classify
// ---------------------------------------------------------------------------

// Classify assigns every installed plugin exactly one provenance class.
// It is a pure function: no context, no clock, no I/O.
func Classify(in Inputs) Classification {
	// Build the declared map: name → DeclaredPlugin.
	declared := make(map[string]DeclaredPlugin, len(in.Declared))
	for _, d := range in.Declared {
		// First writer wins: the core set wins collisions per design §8.
		if _, ok := declared[d.Name]; !ok {
			declared[d.Name] = d
		}
	}

	// Build the installed map and classify every plugin.
	type row struct {
		rec   Record
		class string
		cp    *ClassifiedPlugin // nil until assigned
	}
	installed := make(map[string]*row, len(in.Inventory.Records))
	names := make([]string, 0, len(in.Inventory.Records))
	for _, r := range in.Inventory.Records {
		names = append(names, r.Name)
		installed[r.Name] = &row{rec: r}
	}

	// The "expected" set: plugins in classes 1, 2, or 3 — these are the roots
	// from which reachability is computed.
	expected := make(map[string]bool, len(declared)+3)

	// Step 1: bootstrap root only → class 1.
	if in.BootstrapRoot != "" {
		if rw, ok := installed[in.BootstrapRoot]; ok {
			rw.class = ClassBootstrap
			rw.cp = &ClassifiedPlugin{
				Name:    rw.rec.Name,
				Version: rw.rec.Version,
				Class:   ClassBootstrap,
			}
			expected[in.BootstrapRoot] = true
		}
	}

	// Step 2: declared (before jenkins-supplied — load-bearing precedence).
	for _, d := range in.Declared {
		rw, ok := installed[d.Name]
		if !ok {
			continue
		}
		if rw.class != "" {
			continue // already classified (e.g. bootstrap root)
		}
		rw.class = ClassDeclared
		cp := &ClassifiedPlugin{
			Name:         rw.rec.Name,
			Version:      rw.rec.Version,
			Class:        ClassDeclared,
			DeclaredBy:   d.Tier,
			Contributors: d.Contributors,
			Enabled:      rw.rec.Enabled,
			Detached:     rw.rec.Detached,
			Bundled:      rw.rec.Bundled,
		}
		// Version drift for class-2 only.
		cp.VersionVerdict = versionVerdict(rw.rec.Version, d.Version)
		rw.cp = cp
		expected[d.Name] = true
	}

	// Step 3: jenkins-supplied (Detached || Bundled).
	for _, name := range names {
		rw := installed[name]
		if rw.class != "" {
			continue
		}
		if rw.rec.Detached == TriTrue || rw.rec.Bundled == TriTrue {
			rw.class = ClassJenkinsSupplied
			rw.cp = &ClassifiedPlugin{
				Name:     rw.rec.Name,
				Version:  rw.rec.Version,
				Class:    ClassJenkinsSupplied,
				Enabled:  rw.rec.Enabled,
				Detached: rw.rec.Detached,
				Bundled:  rw.rec.Bundled,
			}
			expected[name] = true
		}
	}

	// Build adjacency: name → []Dep from the installed graph.
	adj := make(map[string][]Dep, len(installed))
	for name, rw := range installed {
		adj[name] = rw.rec.Deps
	}
	installedHas := func(name string) bool {
		_, ok := installed[name]
		return ok
	}

	// Expected roots reaching each plugin, for the published impliedBy field.
	// mandatoryRoots is the mandatory-only closure (class 4); anyRoots also
	// follows optional edges (class 5, which reports its optional edge
	// separately).
	mandatoryRoots := reachableRoots(expected, adj, installedHas, false)
	anyRoots := reachableRoots(expected, adj, installedHas, true)

	// Step 4: Pass 1 BFS over mandatory edges from expected roots → class 4.
	// Edges followed by name unconditionally.
	bfs1Visited := make(map[string]bool, len(expected))
	for name := range expected {
		bfs1Visited[name] = true
	}
	queue := make([]string, 0, len(expected))
	for name := range expected {
		queue = append(queue, name)
	}
	// declaredParents tracks each plugin's IMMEDIATE predecessors — the plugins
	// that literally declare a dependency edge onto it. This is what the
	// unsatisfied-minimum advisory pass needs, since the version minimum being
	// violated is declared by the immediate parent, not by the root.
	//
	// It is deliberately NOT what the published impliedBy field carries: that
	// one names the expected roots whose closure reaches the plugin (see
	// mandatoryRoots/anyRoots below). The two must stay separate maps, or
	// impliedBy reports the parent instead of the root.
	declaredParents := make(map[string]map[string]bool)

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, dep := range adj[cur] {
			if dep.Optional {
				continue // mandatory edges only
			}
			rw, ok := installed[dep.Name]
			if !ok {
				continue // not installed
			}

			// Record the immediate parent that declares this edge.
			if declaredParents[dep.Name] == nil {
				declaredParents[dep.Name] = make(map[string]bool)
			}
			declaredParents[dep.Name][cur] = true

			if !bfs1Visited[dep.Name] {
				bfs1Visited[dep.Name] = true
				if rw.class == "" {
					rw.class = ClassDependency
				}
				queue = append(queue, dep.Name)
			}
		}
	}

	// Collect unsatisfied-minimum advisories from pass 1. Deduplicate by
	// (dependent, dependency).
	advSeen := make(map[string]bool)
	var advisories []Advisory
	for _, name := range names {
		rw := installed[name]
		if rw.class != ClassDependency {
			continue
		}
		// Check every plugin that directly declares this dependency: the
		// minimum being compared is the one on that declaring edge.
		for root := range declaredParents[name] {
			rootRw := installed[root]
			if rootRw == nil {
				continue
			}
			for _, dep := range adj[root] {
				if dep.Name != name || dep.Optional {
					continue
				}
				if dep.Min != "" && !pluginver.AtLeast(rw.rec.Version, dep.Min) {
					key := root + "\x00" + name
					if !advSeen[key] {
						advSeen[key] = true
						advisories = append(advisories, Advisory{
							Code:       "dependencyMinimumUnsatisfied",
							Plugin:     root,
							Dependency: name,
							Min:        dep.Min,
							Version:    rw.rec.Version,
						})
					}
				}
			}
		}
	}

	// Fill class-4 ClassifiedPlugins.
	for _, name := range names {
		rw := installed[name]
		if rw.class != ClassDependency {
			continue
		}
		cp := &ClassifiedPlugin{
			Name:     rw.rec.Name,
			Version:  rw.rec.Version,
			Class:    ClassDependency,
			Enabled:  rw.rec.Enabled,
			Detached: rw.rec.Detached,
			Bundled:  rw.rec.Bundled,
		}
		roots := make([]string, 0, len(mandatoryRoots[name]))
		for r := range mandatoryRoots[name] {
			roots = append(roots, r)
		}
		// Sorted: this is built from a map, and Go randomizes map iteration, so
		// an unsorted slice makes the published impliedBy order differ between
		// otherwise identical classifications — which shows up as spurious
		// change in any consumer that diffs this list.
		sort.Strings(roots)
		cp.ImpliedBy = roots
		rw.cp = cp
	}

	// Step 5: Pass 2 BFS with visited set keyed on (plugin, optionalSeenOnPath).
	// Nodes already assigned are skipped for assignment but still traversed through.
	type bfs2Key struct {
		plugin       string
		optionalSeen bool
	}
	bfs2Visited := make(map[bfs2Key]bool)
	queue2 := make([]bfs2Key, 0, len(expected))
	for name := range expected {
		key := bfs2Key{plugin: name, optionalSeen: false}
		bfs2Visited[key] = true
		queue2 = append(queue2, key)
	}

	for len(queue2) > 0 {
		cur := queue2[0]
		queue2 = queue2[1:]

		for _, dep := range adj[cur.plugin] {
			rw, ok := installed[dep.Name]
			if !ok {
				continue
			}

			newOptionalSeen := cur.optionalSeen || dep.Optional
			nextKey := bfs2Key{plugin: dep.Name, optionalSeen: newOptionalSeen}

			if !bfs2Visited[nextKey] {
				bfs2Visited[nextKey] = true

				// Assign if still unclassified and optional was seen.
				if rw.class == "" && newOptionalSeen {
					rw.class = ClassOptionalDependency
					cp := &ClassifiedPlugin{
						Name:     rw.rec.Name,
						Version:  rw.rec.Version,
						Class:    ClassOptionalDependency,
						Enabled:  rw.rec.Enabled,
						Detached: rw.rec.Detached,
						Bundled:  rw.rec.Bundled,
					}
					// Find the optional edge that got us here.
					// KNOWN LIMITATION: this records the immediate BFS
					// predecessor, which is the true optional-edge source only
					// when the optional edge is the LAST hop. For
					// root --optional--> A --mandatory--> B this sets B's
					// OptionalEdge to "A", implying A→B was optional when the
					// optional edge was actually root→A. Left as-is
					// deliberately: no consumer reads this field yet (transport
					// .ClassifiedPlugin and the CRD status both drop it), and
					// changing the traversal to carry the originating optional
					// edge is a closure-semantics change with nothing to
					// validate it against. Fix it together with the first
					// consumer.
					cp.OptionalEdge = cur.plugin
					// Record the immediate parent for advisory purposes, but
					// publish the expected roots reaching this plugin through
					// the full closure — the optional edge itself is reported
					// separately in OptionalEdge.
					if declaredParents[dep.Name] == nil {
						declaredParents[dep.Name] = make(map[string]bool)
					}
					declaredParents[dep.Name][cur.plugin] = true
					roots := make([]string, 0, len(anyRoots[dep.Name]))
					for r := range anyRoots[dep.Name] {
						roots = append(roots, r)
					}
					sort.Strings(roots) // deterministic order; see class-4 emit above
					cp.ImpliedBy = roots
					rw.cp = cp
				}

				// Always traverse through, even if already assigned.
				queue2 = append(queue2, nextKey)
			}
		}
	}

	// Step 6: everything left is class 6 (unmanaged).
	for _, name := range names {
		rw := installed[name]
		if rw.class != "" {
			continue
		}
		rw.class = ClassUnmanaged
		cp := &ClassifiedPlugin{
			Name:     rw.rec.Name,
			Version:  rw.rec.Version,
			Class:    ClassUnmanaged,
			Enabled:  rw.rec.Enabled,
			Detached: rw.rec.Detached,
			Bundled:  rw.rec.Bundled,
		}
		rw.cp = cp
	}

	// Step 7: cross-check — recorded non-root bootstrap members that landed in
	// class 6 move to class 1 and set BootstrapApproximate.
	bootstrapApproximate := !in.BootstrapMatched
	for _, member := range in.BootstrapMembers {
		if member == in.BootstrapRoot {
			continue // root was already handled
		}
		rw, ok := installed[member]
		if !ok {
			continue
		}
		if rw.class == ClassUnmanaged {
			rw.class = ClassBootstrap
			rw.cp.Class = ClassBootstrap
			bootstrapApproximate = true
		}
	}

	// Assemble the result.
	plugins := make([]ClassifiedPlugin, 0, len(names))
	counts := make(map[string]int)
	for _, name := range names {
		rw := installed[name]
		cp := *rw.cp
		plugins = append(plugins, cp)
		counts[cp.Class]++
	}

	return Classification{
		Plugins:              plugins,
		Advisories:           sortAdvisories(advisories),
		BootstrapApproximate: bootstrapApproximate,
		Total:                len(plugins),
		Counts:               counts,
	}
}

// versionVerdict compares installed vs declared version for a class-2 plugin.
func versionVerdict(installed, declared string) string {
	if installed == "" {
		return VerdictMissing
	}
	if declared == "" {
		// Declared without a version pin: treat as match (nothing to compare).
		return VerdictMatch
	}
	cmp := pluginver.Compare(installed, declared)
	switch {
	case cmp > 0:
		return VerdictAhead
	case cmp < 0:
		return VerdictBehind
	default:
		return VerdictMatch
	}
}

// reachableRoots returns, for each installed plugin, the set of expected roots
// whose dependency closure reaches it.
//
// It runs one traversal per root rather than a single visited-once sweep from
// all roots at once. A shared node must record EVERY root that reaches it, and
// a visited-once sweep records only whichever root got there first: for
// A → B → C and X → B, a single sweep marks C as reached from one of A/X and
// silently drops the other.
//
// followOptional selects the full closure (optional edges included) over the
// mandatory-only closure.
func reachableRoots(expected map[string]bool, adj map[string][]Dep, installed func(string) bool, followOptional bool) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for root := range expected {
		seen := map[string]bool{root: true}
		stack := []string{root}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, dep := range adj[cur] {
				if dep.Optional && !followOptional {
					continue
				}
				if !installed(dep.Name) || seen[dep.Name] {
					continue
				}
				seen[dep.Name] = true
				if out[dep.Name] == nil {
					out[dep.Name] = make(map[string]bool)
				}
				out[dep.Name][root] = true
				stack = append(stack, dep.Name)
			}
		}
	}
	return out
}

// sortAdvisories imposes a deterministic total order on the advisory list.
//
// Advisories are accumulated while ranging over declaredParents, which is a
// map, so a dependency with two or more declaring parents emits its advisories
// in randomized order. That order is observable: the operator's
// classificationUnchanged compares advisories positionally, so an unsorted list
// makes an otherwise-identical classification compare unequal on every reconcile
// and rewrite the invc/ read model forever. Same failure mode the ImpliedBy
// sort fixes, on the other list produced from a map.
func sortAdvisories(a []Advisory) []Advisory {
	sort.Slice(a, func(i, j int) bool {
		if a[i].Plugin != a[j].Plugin {
			return a[i].Plugin < a[j].Plugin
		}
		if a[i].Dependency != a[j].Dependency {
			return a[i].Dependency < a[j].Dependency
		}
		if a[i].Code != a[j].Code {
			return a[i].Code < a[j].Code
		}
		if a[i].Min != a[j].Min {
			return a[i].Min < a[j].Min
		}
		return a[i].Version < a[j].Version
	})
	return a
}
