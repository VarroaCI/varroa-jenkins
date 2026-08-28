package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func coreEntry(name, version, requiredCore string, deps ...pluginDep) inventoryEntry { //nolint:unparam // symmetry with entry(); one root version in the tables is not a reason to hardcode it
	e := entry(name, version, deps...)
	e.RequiredCore = requiredCore
	return e
}

func verdictFor(t *testing.T, compat []v1alpha1.CatalogItemCompat, profile string) v1alpha1.CatalogItemCompat {
	t.Helper()
	for _, c := range compat {
		if c.Profile == profile {
			return c
		}
	}
	t.Fatalf("no verdict for profile %q in %+v", profile, compat)
	return v1alpha1.CatalogItemCompat{}
}

// ---------------------------------------------------------------------------
// 5.3 — classification
// ---------------------------------------------------------------------------

func TestCompat_CoreTooOld(t *testing.T) {
	entries := []inventoryEntry{coreEntry("root", "1.0", "2.555.1")}
	inv := newUCInventory(entries)
	profiles := []ucProfile{{Name: "old", Eligible: true, EffectiveCore: "2.479.3"}}
	cl, _ := resolveClosure(entries[0], inv, profiles)

	compat := evaluateCompat(cl, inv, profiles)
	if got := verdictFor(t, compat, "old").Verdict; got != verdictCoreTooOld {
		t.Fatalf("verdict = %q, want core-too-old", got)
	}
	cond := compatWarning(compat)
	if cond.Status != metav1.ConditionTrue || cond.Reason != verdictCoreTooOld {
		t.Errorf("CompatWarning = %+v", cond)
	}
}

// TestCompat_TransitiveCoreRequirementCounts pins that verdicts are computed
// over the closure PLUS the root, not the root alone.
func TestCompat_TransitiveCoreRequirementCounts(t *testing.T) {
	entries := []inventoryEntry{
		coreEntry("root", "1.0", "2.479.1", dep("greedy", "")),
		coreEntry("greedy", "1.0", "2.555.1"),
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{{Name: "old", Eligible: true, EffectiveCore: "2.479.3"}}
	cl, _ := resolveClosure(entries[0], inv, profiles)

	c := verdictFor(t, evaluateCompat(cl, inv, profiles), "old")
	if c.Verdict != verdictCoreTooOld {
		t.Fatalf("verdict = %q, want core-too-old", c.Verdict)
	}
	if !strings.Contains(c.Message, "greedy") {
		t.Errorf("message should name the offending closure member: %q", c.Message)
	}
}

func TestCompat_DepBelowMinimumIsStoreSideAndProfileIndependent(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("mailer", "9.0")),
		entry("mailer", "1.0"),
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{
		{Name: "a", Eligible: true, EffectiveCore: "2.479.3"},
		{Name: "b", Eligible: true, EffectiveCore: "2.555.3"},
	}
	cl, _ := resolveClosure(entries[0], inv, profiles)
	compat := evaluateCompat(cl, inv, profiles)

	for _, p := range []string{"a", "b"} {
		if got := verdictFor(t, compat, p).Verdict; got != verdictDepBelowMinimum {
			t.Errorf("profile %s verdict = %q, want dep-below-minimum on every profile", p, got)
		}
	}
}

// TestCompat_LockTooOldFiresWhenTheStoreSuppliedTheVersion is the D6 case a
// resolver-branch-driven implementation would silently skip: the store DID
// satisfy the minimum, but this profile's lock pins the plugin below it.
func TestCompat_LockTooOldFiresWhenTheStoreSuppliedTheVersion(t *testing.T) {
	entries := []inventoryEntry{
		entry("root", "1.0", dep("mailer", "2.0")),
		entry("mailer", "2.0"),
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{
		{Name: "stale", Eligible: true, EffectiveCore: "2.555.3", Lock: map[string]string{"mailer": "1.0"}},
		{Name: "fresh", Eligible: true, EffectiveCore: "2.555.3", Lock: map[string]string{"mailer": "2.0"}},
	}
	cl, _ := resolveClosure(entries[0], inv, profiles)
	if cl.Selected["mailer"].Provenance != provenanceStore {
		t.Fatalf("precondition: the store must have supplied the version, got %+v", cl.Selected["mailer"])
	}
	compat := evaluateCompat(cl, inv, profiles)

	if got := verdictFor(t, compat, "stale").Verdict; got != verdictLockTooOld {
		t.Errorf("stale profile verdict = %q, want lock-too-old", got)
	}
	if got := verdictFor(t, compat, "fresh").Verdict; got == verdictLockTooOld {
		t.Errorf("fresh profile must not report lock-too-old")
	}
}

// TestCompat_LockProvenanceShortfallIsLockTooOldNotDepBelowMinimum guards the
// precedence trap: dep-below-minimum outranks lock-too-old, so classifying by a
// bare "selected < minimum" test would HIDE the verdict D6 requires.
func TestCompat_LockProvenanceShortfallIsLockTooOldNotDepBelowMinimum(t *testing.T) {
	entries := []inventoryEntry{entry("root", "1.0", dep("absent", "5.0"))}
	inv := newUCInventory(entries)
	profiles := []ucProfile{
		{Name: "supplier", Eligible: true, EffectiveCore: "2.555.3", Lock: map[string]string{"absent": "1.0"}},
	}
	cl, err := resolveClosure(entries[0], inv, profiles)
	if err != nil {
		t.Fatalf("a lock-sourced shortfall must not be a failure: %v", err)
	}
	sel := cl.Selected["absent"]
	if sel.Provenance != provenanceLock || !sel.Shortfall {
		t.Fatalf("precondition: expected a lock-provenance shortfall, got %+v", sel)
	}

	c := verdictFor(t, evaluateCompat(cl, inv, profiles), "supplier")
	if c.Verdict != verdictLockTooOld {
		t.Fatalf("verdict = %q, want lock-too-old (dep-below-minimum is reserved for store-side shortfalls)", c.Verdict)
	}
}

func TestCompat_PrecedenceOrder(t *testing.T) {
	want := []string{verdictCoreTooOld, verdictDepBelowMinimum, verdictLockTooOld, verdictUnknown, verdictCompatible}
	for i := 1; i < len(want); i++ {
		if verdictRank(want[i-1]) >= verdictRank(want[i]) {
			t.Fatalf("%s must outrank %s", want[i-1], want[i])
		}
	}

	// A closure with BOTH a core problem and a store shortfall reports the
	// higher-precedence verdict.
	entries := []inventoryEntry{
		coreEntry("root", "1.0", "2.555.1", dep("mailer", "9.0")),
		entry("mailer", "1.0"),
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{{Name: "old", Eligible: true, EffectiveCore: "2.479.3"}}
	cl, _ := resolveClosure(entries[0], inv, profiles)
	if got := verdictFor(t, evaluateCompat(cl, inv, profiles), "old").Verdict; got != verdictCoreTooOld {
		t.Fatalf("verdict = %q, want the higher-precedence core-too-old", got)
	}
}

// ---------------------------------------------------------------------------
// 5.3a — core and eligibility
// ---------------------------------------------------------------------------

// TestCompat_LTSProfileComparedAgainstResolvedPatch is §5.1a: an LTS-line
// profile deploys the resolved patch, so comparing requiredCore against the bare
// line would report core-too-old for a plugin that in fact runs.
func TestCompat_LTSProfileComparedAgainstResolvedPatch(t *testing.T) {
	entries := []inventoryEntry{coreEntry("root", "1.0", "2.555.1")}
	inv := newUCInventory(entries)
	// EffectiveCore is the resolveVersion, not the bare line "2.555".
	profiles := []ucProfile{{Name: "lts-2.555", Eligible: true, EffectiveCore: "2.555.3"}}
	cl, _ := resolveClosure(entries[0], inv, profiles)

	if got := verdictFor(t, evaluateCompat(cl, inv, profiles), "lts-2.555").Verdict; got == verdictCoreTooOld {
		t.Fatal("an LTS profile resolving to 2.555.3 must not be core-too-old for requiredCore 2.555.1")
	}

	// The bare line WOULD have tripped it, which is the whole point of §5.1a.
	bare := []ucProfile{{Name: "lts-2.555", Eligible: true, EffectiveCore: "2.555"}}
	clBare, _ := resolveClosure(entries[0], inv, bare)
	if got := verdictFor(t, evaluateCompat(clBare, inv, bare), "lts-2.555").Verdict; got != verdictCoreTooOld {
		t.Fatalf("sanity: comparing against the bare line should trip core-too-old, got %q", got)
	}
}

func TestCompat_UnknownWhenCoreCannotBeJudged(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requiredCore  string
		effectiveCore string
	}{
		{"no requiredCore", "", "2.555.3"},
		{"empty effective core", "2.555.1", ""},
		{"unparseable effective core", "2.555.1", "not-a-version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := []inventoryEntry{coreEntry("root", "1.0", tc.requiredCore)}
			inv := newUCInventory(entries)
			profiles := []ucProfile{{Name: "p", Eligible: true, EffectiveCore: tc.effectiveCore}}
			cl, _ := resolveClosure(entries[0], inv, profiles)

			compat := evaluateCompat(cl, inv, profiles)
			if got := verdictFor(t, compat, "p").Verdict; got != verdictUnknown {
				t.Fatalf("verdict = %q, want unknown", got)
			}
			if cond := compatWarning(compat); cond.Status == metav1.ConditionTrue {
				t.Errorf("unknown must not raise CompatWarning: %+v", cond)
			}
		})
	}
}

// TestCompat_IneligibleProfileShortCircuits is the ordering guard: an
// ineligible profile must yield unknown even where core-too-old would otherwise
// fire, because a profile that cannot be judged must not produce a concrete
// verdict from untrusted data.
func TestCompat_IneligibleProfileShortCircuits(t *testing.T) {
	entries := []inventoryEntry{coreEntry("root", "1.0", "2.555.1")}
	inv := newUCInventory(entries)
	profiles := []ucProfile{{Name: "notready", Eligible: false, EffectiveCore: "2.479.3"}}
	cl, _ := resolveClosure(entries[0], inv, profiles)

	c := verdictFor(t, evaluateCompat(cl, inv, profiles), "notready")
	if c.Verdict != verdictUnknown {
		t.Fatalf("verdict = %q, want unknown (core-too-old would otherwise fire)", c.Verdict)
	}
}

func TestCompat_ZeroProfiles(t *testing.T) {
	entries := []inventoryEntry{coreEntry("root", "1.0", "2.555.1")}
	inv := newUCInventory(entries)
	cl, _ := resolveClosure(entries[0], inv, nil)

	compat := evaluateCompat(cl, inv, nil)
	if len(compat) != 0 {
		t.Fatalf("expected empty compat, got %+v", compat)
	}
	cond := compatWarning(compat)
	if cond.Status != metav1.ConditionUnknown || cond.Reason != "NoProfiles" {
		t.Fatalf("CompatWarning = %+v, want Unknown/NoProfiles", cond)
	}
}

func TestCompat_WarningCapsOffendersAtThree(t *testing.T) {
	compat := []v1alpha1.CatalogItemCompat{
		{Profile: "p1", Verdict: verdictCoreTooOld},
		{Profile: "p2", Verdict: verdictLockTooOld},
		{Profile: "p3", Verdict: verdictLockTooOld},
		{Profile: "p4", Verdict: verdictLockTooOld},
		{Profile: "p5", Verdict: verdictCompatible},
	}
	cond := compatWarning(compat)
	if cond.Status != metav1.ConditionTrue || cond.Reason != verdictCoreTooOld {
		t.Fatalf("cond = %+v", cond)
	}
	if !strings.Contains(cond.Message, "p1, p2, p3") || !strings.Contains(cond.Message, "(+1 more)") {
		t.Errorf("message should list the first three and count the rest: %q", cond.Message)
	}
	if strings.Contains(cond.Message, "p4") {
		t.Errorf("message should not list beyond three: %q", cond.Message)
	}
}

// TestCompat_VerdictsNeverInvalidateAnItem is the visual and structural
// expression of D6: derivability blocks, compatibility advises.
func TestCompat_VerdictsNeverInvalidateAnItem(t *testing.T) {
	entries := []inventoryEntry{
		coreEntry("root", "1.0", "2.999.9", dep("mailer", "9.0")),
		entry("mailer", "1.0"),
	}
	inv := newUCInventory(entries)
	profiles := []ucProfile{{Name: "old", Eligible: true, EffectiveCore: "2.479.3", Lock: map[string]string{"mailer": "0.1"}}}
	cl, err := resolveClosure(entries[0], inv, profiles)
	if err != nil {
		t.Fatalf("a fully-warning closure must still resolve: %v", err)
	}
	compat := evaluateCompat(cl, inv, profiles)
	if verdictFor(t, compat, "old").Verdict == verdictCompatible {
		t.Fatal("precondition: expected a warning verdict")
	}
	// Content is still produced, which is what keeps the item selectable.
	if content := closureContent(cl); !strings.Contains(content, "artifactId: root") {
		t.Fatalf("a warning verdict must not suppress content: %q", content)
	}
}
