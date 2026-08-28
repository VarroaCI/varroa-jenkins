package controller

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// dep is a terse mandatory dependency literal for the tables below.
func dep(name, minVer string) pluginDep { return pluginDep{Name: name, Min: minVer} }

// optDep is a terse optional dependency literal.
func optDep(name, minVer string) pluginDep { return pluginDep{Name: name, Min: minVer, Optional: true} }

func entry(name, version string, deps ...pluginDep) inventoryEntry {
	return inventoryEntry{Name: name, Version: version, SHA256: "sha256:" + name + version, Dependencies: deps}
}

func mustResolve(t *testing.T, root inventoryEntry, entries []inventoryEntry, profiles []ucProfile) *ucClosure {
	t.Helper()
	inv := newUCInventory(entries)
	cl, err := resolveClosure(root, inv, profiles)
	if err != nil {
		t.Fatalf("resolveClosure: %v", err)
	}
	return cl
}

func selectedVersions(cl *ucClosure) map[string]string {
	out := make(map[string]string, len(cl.Selected))
	for name, sel := range cl.Selected {
		out[name] = sel.Version
	}
	return out
}

// ---------------------------------------------------------------------------
// 0.1a — the jenkinsver / pluginver boundary
// ---------------------------------------------------------------------------

// TestPluginVersionQualifierNotTruncated is the guard R1 exists for. Applying
// jenkinsver to a plugin version truncates at the first '-', so two distinct
// releases compare EQUAL — which would silently corrupt lock-too-old, candidate
// ordering, and the browser's default-version rule.
func TestPluginVersionQualifierNotTruncated(t *testing.T) {
	const truncated = "4.5.14"
	const full = "4.5.14-269.vfa_2321039a_83"

	if pluginver.Compare(full, truncated) == 0 {
		t.Errorf("pluginver must not treat %q as equal to its truncation %q", full, truncated)
	}
	// And the incrementals suffix orders against another incrementals build.
	if pluginver.Compare("4.5.14-269.vfa_2321039a_83", "4.5.14-270.va_1234567890a_") >= 0 {
		t.Error("pluginver must order incrementals suffixes, not ignore them")
	}
	// Demonstrate the failure mode being guarded against: jenkinsver DOES
	// truncate, which is exactly why it may only touch core versions.
	if core, ok := jenkinsver.Core(full); !ok || len(core) != 3 {
		t.Fatalf("jenkinsver.Core(%q) = %v, %v", full, core, ok)
	}
	atLeast, ok := jenkinsver.AtLeast(full, truncated)
	if !ok || !atLeast {
		t.Fatalf("jenkinsver.AtLeast(%q,%q) = %v,%v", full, truncated, atLeast, ok)
	}
	if a, _ := jenkinsver.AtLeast(truncated, full); !a {
		t.Error("jenkinsver truncation makes these mutually >= — the reason it is confined to core versions")
	}
}

// ---------------------------------------------------------------------------
// 3.1a — no unparseable-version branch exists
// ---------------------------------------------------------------------------

// TestNoUnparseableVersionBranch asserts the ratified R12 property holds where
// it matters: an arbitrary, structureless version string still orders and still
// resolves, so no code path in this change can branch on "cannot parse".
func TestNoUnparseableVersionBranch(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("weird", "not-a-version-at-all")),
		entry("weird", "%%%"),
		entry("weird", "zzz-9"),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	if _, ok := cl.Selected["weird"]; !ok {
		t.Fatalf("an arbitrary minimum must still resolve, got %+v", cl.Selected)
	}
	// maxVersion is total: it never errors and "" always loses.
	if got := maxVersion("", "%%%"); got != "%%%" {
		t.Errorf(`maxVersion("", "%%%%%%") = %q`, got)
	}
	if got := maxVersion("%%%", ""); got != "%%%" {
		t.Errorf(`maxVersion("%%%%%%", "") = %q`, got)
	}
}

// ---------------------------------------------------------------------------
// 4.5 — fixpoint behavior
// ---------------------------------------------------------------------------

// TestResolveClosure_FixpointCounterexample is design §4.1's counterexample. A
// naive single pass that recomputes minima from only the currently-selected
// versions cycles C@1 -> D@2 -> C@2 forever; the monotone accumulator stabilizes
// at {C@2, D@2}.
func TestResolveClosure_FixpointCounterexample(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("C", "1")),
		entry("C", "1", dep("D", "2")),
		entry("C", "2"),
		entry("D", "2", dep("C", "2")),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	got := selectedVersions(cl)
	if got["C"] != "2" || got["D"] != "2" {
		t.Fatalf("expected {C@2, D@2}, got %v", got)
	}
	// D stays even though the finally-selected C@2 no longer requires it. The
	// closure errs toward inclusion: over-pinning a plugin the store already
	// holds is safe, under-pinning is not.
	if len(got) != 2 {
		t.Fatalf("expected exactly C and D in the closure, got %v", got)
	}
}

func TestResolveClosure_MultiPathMinimaTakeTheMaximum(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("A", ""), dep("B", "")),
		entry("A", "1.0", dep("C", "1.0")),
		entry("B", "1.0", dep("C", "3.0")),
		entry("C", "1.0"),
		entry("C", "3.0"),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	if got := selectedVersions(cl)["C"]; got != "3.0" {
		t.Fatalf("C should take the maximum minimum, got %q", got)
	}
	if cl.Minimums["C"] != "3.0" {
		t.Errorf("effective minimum for C = %q, want 3.0", cl.Minimums["C"])
	}
}

func TestResolveClosure_OptionalDependenciesExcluded(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("needed", "1.0"), optDep("chatty", "1.0")),
		entry("needed", "1.0"),
		entry("chatty", "1.0"),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	if _, ok := cl.Selected["chatty"]; ok {
		t.Fatalf("optional dependency must be excluded, got %v", selectedVersions(cl))
	}
	if !strings.Contains(closureContent(cl), "needed") || strings.Contains(closureContent(cl), "chatty") {
		t.Errorf("content = %q", closureContent(cl))
	}
}

// TestResolveClosure_CycleThroughRoot pins §4.2.1: the root is not a solver
// variable. A constraint on the root is recorded and evaluated, but the root is
// never re-selected and appears exactly once in the fragment.
func TestResolveClosure_CycleThroughRoot(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("A", "1.0")),
		entry("root", "2.0"),
		entry("A", "1.0", dep("root", "2.0")),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	if _, ok := cl.Selected["root"]; ok {
		t.Fatal("the root must never enter selected")
	}
	if cl.RootMinimum != "2.0" || !cl.RootShortfall {
		t.Errorf("constraint on the root should be recorded and evaluated: min=%q shortfall=%v", cl.RootMinimum, cl.RootShortfall)
	}
	content := closureContent(cl)
	if n := strings.Count(content, "artifactId: root"); n != 1 {
		t.Fatalf("root must appear exactly once, appeared %d times:\n%s", n, content)
	}
	if !strings.Contains(content, `version: "1.0"`) {
		t.Errorf("root must be pinned at its OWN version:\n%s", content)
	}
}

func TestResolveClosure_DependencyWithNoMinimumEntersClosure(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("bare", "")),
		entry("bare", "7.7"),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	if got := selectedVersions(cl)["bare"]; got != "7.7" {
		t.Fatalf("a dependency declared with no minimum must still enter the closure, got %v", selectedVersions(cl))
	}
	if cl.Minimums["bare"] != "" {
		t.Errorf("minimum should be empty, got %q", cl.Minimums["bare"])
	}
}

// TestResolveClosure_RoundCapExhaustion drives the loop past its bound with a
// hand-built inventory whose round cap is artificially small.
func TestResolveClosure_RoundCapExhaustion(t *testing.T) {
	// A chain X@1 -> X>=2 -> X@2 -> X>=3 ... raises one name's constraint once
	// per stored version. With the correct bound this converges; the test
	// asserts the guard is a bound and not a panic by shrinking it.
	entries := []inventoryEntry{
		entry("root", "1.0", dep("X", "1")),
		entry("X", "1", dep("X", "2")),
		entry("X", "2", dep("X", "3")),
		entry("X", "3"),
	}
	inv := newUCInventory(entries)
	if _, err := resolveClosure(entries[0], inv, nil); err != nil {
		t.Fatalf("the real bound must converge: %v", err)
	}

	// Now force exhaustion by lying about the inventory size.
	inv.names = nil
	inv.mandatoryEdges = 0 // maxRounds becomes 2
	_, err := resolveClosure(entries[0], inv, nil)
	if err == nil {
		t.Fatal("expected non-convergence to be reported")
	}
	if !strings.Contains(err.Error(), "internal solver invariant") {
		t.Errorf("exhaustion must blame the solver, not the plugin metadata: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4.5a — resolve rows
// ---------------------------------------------------------------------------

func TestResolveOne_Rows(t *testing.T) {
	entries := []inventoryEntry{
		entry("store-only", "1.0"),
		entry("store-only", "2.0"),
		entry("store-only", "3.0"),
		entry("too-old", "1.0"),
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{
		{Name: "lts", Eligible: true, Lock: map[string]string{"lock-only": "9.9", "disagree": "1.0"}},
		{Name: "weekly", Eligible: true, Lock: map[string]string{"lock-only": "9.9", "disagree": "2.0"}},
	}

	t.Run("row1 lock fallback with provenance", func(t *testing.T) {
		sel, err := resolveOne("lock-only", "1.0", inv, profiles)
		if err != nil {
			t.Fatalf("row 1 must not fail: %v", err)
		}
		if sel.Version != "9.9" || sel.Provenance != provenanceLock {
			t.Fatalf("got %+v", sel)
		}
		if len(sel.LockSources) != 2 {
			t.Errorf("provenance must carry the supplying profiles, got %v", sel.LockSources)
		}
	})

	t.Run("row2 unresolvable when nothing names a version", func(t *testing.T) {
		if _, err := resolveOne("nowhere", "1.0", inv, profiles); err == nil {
			t.Fatal("expected the only failure row to fire")
		}
	})

	t.Run("row2 unresolvable when locks disagree", func(t *testing.T) {
		if _, err := resolveOne("disagree", "", inv, profiles); err == nil {
			t.Fatal("disagreeing locks must not establish unanimity")
		}
	})

	t.Run("row3 least satisfying candidate", func(t *testing.T) {
		sel, err := resolveOne("store-only", "2.0", inv, profiles)
		if err != nil {
			t.Fatalf("row 3: %v", err)
		}
		if sel.Version != "2.0" || sel.Provenance != provenanceStore || sel.Shortfall {
			t.Fatalf("got %+v, want the LEAST satisfying candidate from the store", sel)
		}
	})

	t.Run("row3 empty minimum satisfied by every candidate", func(t *testing.T) {
		sel, _ := resolveOne("store-only", "", inv, profiles)
		if sel.Version != "1.0" {
			t.Fatalf("an empty minimum must be satisfied by the least candidate, got %+v", sel)
		}
	})

	t.Run("row4 greatest candidate plus a shortfall", func(t *testing.T) {
		sel, err := resolveOne("too-old", "5.0", inv, profiles)
		if err != nil {
			t.Fatalf("a shortfall must never be a failure: %v", err)
		}
		if sel.Version != "1.0" || sel.Provenance != provenanceStore || !sel.Shortfall {
			t.Fatalf("got %+v", sel)
		}
	})
}

// ---------------------------------------------------------------------------
// 4.5b — poisoning and eligibility
// ---------------------------------------------------------------------------

func TestResolve_PoisonedVersionExcludedPerVersion(t *testing.T) {
	// Two conflicting entries for X@1.0, one clean entry for X@2.0.
	entries := []inventoryEntry{
		entry("root", "1.0", dep("X", "")),
		{Name: "X", Version: "1.0", SHA256: "sha256:aaa"},
		{Name: "X", Version: "1.0", SHA256: "sha256:bbb"},
		entry("X", "2.0"),
	}
	inv := newUCInventory(entries)
	if !inv.poisoned("X", "1.0") {
		t.Fatal("X@1.0 should be poisoned")
	}
	if inv.poisoned("X", "2.0") {
		t.Fatal("poison is per version, not per plugin")
	}
	cl, err := resolveClosure(entries[0], inv, nil)
	if err != nil {
		t.Fatalf("a poisoned version must not block a clean one: %v", err)
	}
	if got := selectedVersions(cl)["X"]; got != "2.0" {
		t.Fatalf("X = %q, want 2.0", got)
	}
}

func TestResolve_PoisonedLockPinIsNotReadmitted(t *testing.T) {
	// The plugin is absent from the store as a candidate (its only version is
	// poisoned), and the unanimous lock names exactly that poisoned version.
	entries := []inventoryEntry{
		entry("root", "1.0", dep("X", "")),
		{Name: "X", Version: "1.0", SHA256: "sha256:aaa"},
		{Name: "X", Version: "1.0", SHA256: "sha256:bbb"},
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{{Name: "lts", Eligible: true, Lock: map[string]string{"X": "1.0"}}}
	_, err := resolveClosure(entries[0], inv, profiles)
	if err == nil {
		t.Fatal("a unanimous lock naming a poisoned version must be unresolvable, not a back door")
	}
	if !strings.Contains(err.Error(), "conflicting store entries") {
		t.Errorf("error should name the conflict: %v", err)
	}
}

func TestResolve_IneligibleProfileDoesNotVote(t *testing.T) {
	entries := []inventoryEntry{entry("root", "1.0", dep("X", ""))}
	inv := newUCInventory(entries)

	for _, tc := range []struct {
		name    string
		profile ucProfile
	}{
		{"plugin set not ready", ucProfile{Name: "p", Eligible: false, Lock: map[string]string{"X": "1.0"}}},
		{"no materialized lock", ucProfile{Name: "p", Eligible: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveClosure(entries[0], inv, []ucProfile{tc.profile}); err == nil {
				t.Fatal("an ineligible profile must not count toward unanimity")
			}
		})
	}

	// The same profile, eligible, does resolve it.
	ok := ucProfile{Name: "p", Eligible: true, Lock: map[string]string{"X": "1.0"}}
	if _, err := resolveClosure(entries[0], inv, []ucProfile{ok}); err != nil {
		t.Fatalf("an eligible profile should supply the pin: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4.6 — determinism
// ---------------------------------------------------------------------------

// TestResolveClosure_Deterministic is what catches a stray range over a Go map.
func TestResolveClosure_Deterministic(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("A", "1.0"), dep("B", ""), dep("C", "2.0")),
		entry("A", "1.0", dep("D", "1.0"), dep("E", "")),
		entry("A", "2.0", dep("D", "3.0")),
		entry("B", "1.0", dep("C", "1.0")),
		entry("C", "1.0"),
		entry("C", "2.0", dep("E", "2.0")),
		entry("D", "1.0"),
		entry("D", "3.0"),
		entry("E", "1.0"),
		entry("E", "2.0"),
	}
	inv := newUCInventory(entries)

	var want string
	for i := 0; i < 50; i++ {
		cl, err := resolveClosure(entries[0], inv, nil)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := sha256Hex([]byte(closureContent(cl)))
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("run %d produced contentHash %s, want %s\n%s", i, got, want, closureContent(cl))
		}
	}
}

func TestClosureContent_PinsEveryVersionExactly(t *testing.T) {
	entries := []inventoryEntry{
		entry("acme-widget", "1.2.0", dep("mailer", "1.0")),
		entry("mailer", "1.5"),
	}
	cl := mustResolve(t, entries[0], entries, nil)
	got := closureContent(cl)
	want := "plugins:\n" +
		"  - artifactId: acme-widget\n    version: \"1.2.0\"\n" +
		"  - artifactId: mailer\n    version: \"1.5\"\n"
	if got != want {
		t.Fatalf("content =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "latest") || strings.Contains(got, `version: ""`) {
		t.Error("a derived fragment must never emit an empty or 'latest' version")
	}
}

func TestClosureStatus_RecordsProvenanceAndMinimum(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("direct", "1.0")),
		entry("direct", "1.0", dep("transitive", "2.0")),
		entry("transitive", "2.0"),
	}
	profiles := []ucProfile{{Name: "lts", Eligible: true, Lock: map[string]string{}}}
	cl := mustResolve(t, entries[0], entries, profiles)
	status := closureStatus(cl)

	byID := map[string]int{}
	for i, e := range status {
		byID[e.ArtifactID] = i
	}
	if len(status) != 3 {
		t.Fatalf("expected root + 2 members, got %d: %+v", len(status), status)
	}
	if e := status[byID["direct"]]; !e.Direct || e.Provenance != provenanceStore || e.Minimum != "1.0" {
		t.Errorf("direct entry = %+v", e)
	}
	if e := status[byID["transitive"]]; e.Direct {
		t.Errorf("transitive entry should not be marked direct: %+v", e)
	}
	if e := status[byID["root"]]; !e.Direct || e.Version != "1.0" {
		t.Errorf("root entry = %+v", e)
	}
}
