package controller

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ApplyRemovalPaths returns the spec-relative, dot-separated paths of every
// JSON-null key in a spec patch, sorted. These are exactly the paths
// ApplyControllerSpecSSA translates to removals before applying (translateNulls
// in clientset_client.go); the seam derives them itself and reports which
// removals did not take effect, so this helper is shared by the test fakes
// that simulate the seam and by unit tests. A null reachable through a list is
// rejected with ErrNullInList, matching apply — the same patch is never
// accepted for apply but mis-classified here. spec is not mutated.
func ApplyRemovalPaths(spec map[string]any) ([]string, error) {
	_, removals, err := translateNulls(spec)
	if err != nil {
		return nil, err
	}
	return removalPathsFromSet(removals), nil
}

// removalPathsFromSet converts a removal set (as produced by translateNulls)
// into sorted spec-relative paths. Sorting keeps the reported order
// deterministic across callers.
func removalPathsFromSet(removals map[string]bool) []string {
	paths := make([]string, 0, len(removals))
	for p := range removals {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// UnappliedRemovalsFromSpec reports which of the requested spec-relative
// removal paths are still present in a JSON-shaped spec map — the "spec" value
// of the applied object's UNSTRUCTURED form, exactly as the apiserver returned
// it. This is the production presence check ApplyControllerSpecSSA uses: an
// unstructured map distinguishes "absent" from "present and zero-valued", so a
// field another manager retained at its zero value (e.g. hibernation.enabled:
// false) is correctly reported as unapplied. It reports only the paths that
// still resolve to a value; it never reports which manager owns a field
// (ownership does not survive stripManagedFields on the Brood path, so
// reporting it would diverge between routes).
func UnappliedRemovalsFromSpec(spec map[string]any, removals []string) []bus.UnappliedRemoval {
	if len(removals) == 0 {
		return nil
	}
	var out []bus.UnappliedRemoval
	for _, p := range removals {
		if pathPresent(spec, p) {
			out = append(out, bus.UnappliedRemoval{Field: "spec." + p})
		}
	}
	return out
}

// UnappliedRemovals is the TYPED convenience wrapper over
// UnappliedRemovalsFromSpec, for callers (and test fakes) that only hold a
// *v1alpha1.Controller: it marshals the typed object to JSON and checks
// presence on the resulting spec map. It deliberately inherits the typed
// object's omitempty — a retained zero value (e.g. hibernation.enabled=false)
// reads as ABSENT here — which is exactly why production must use the
// unstructured path via ApplyControllerSpecSSA and never this wrapper.
func UnappliedRemovals(applied *v1alpha1.Controller, removals []string) []bus.UnappliedRemoval {
	if len(removals) == 0 {
		return nil
	}
	raw, err := json.Marshal(applied)
	if err != nil {
		// A controller that cannot be marshalled cannot be verified; report the
		// removals so a real block is never silently swallowed.
		return allUnapplied(removals)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return allUnapplied(removals)
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return nil
	}
	return UnappliedRemovalsFromSpec(spec, removals)
}

// allUnapplied wraps every requested removal as unapplied (used when the
// applied object cannot be marshalled to verify presence).
func allUnapplied(removals []string) []bus.UnappliedRemoval {
	out := make([]bus.UnappliedRemoval, 0, len(removals))
	for _, p := range removals {
		out = append(out, bus.UnappliedRemoval{Field: "spec." + p})
	}
	return out
}

// pathPresent reports whether the dot-separated path resolves to a present
// value in the map. At each node the ENTIRE remaining path is tried as a
// literal key first: free-form map keys (podOverrides.podAnnotations,
// ingressSpec.annotations) legitimately contain dots (e.g. "example.com/keep"),
// and a removal path reaching such a leaf must not be split into nested
// segments. Only when the literal lookup misses is one dot-segment consumed
// and the walk descends. A missing node, or a node that is not a map, reads as
// absent.
func pathPresent(m map[string]any, path string) bool {
	cur := any(m)
	remaining := path
	for {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := mm[remaining]; ok {
			return true
		}
		idx := strings.IndexByte(remaining, '.')
		if idx < 0 {
			return false
		}
		next, has := mm[remaining[:idx]]
		if !has {
			return false
		}
		cur = next
		remaining = remaining[idx+1:]
	}
}
