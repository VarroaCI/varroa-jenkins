package pluginver

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// ---------------------------------------------------------------------------
// Golden table (design.md §2)
// ---------------------------------------------------------------------------

func TestCompare_Golden(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		why  string
	}{
		{"1413.v2ff1a_5e720fa_", "1384.vdc05a_48f535f", 1, "the leading numeric decides"},
		{"2.555.3", "2.555", 1, ""},
		{"1.0", "1.0.0", 0, "trailing zeros normalize away"},
		{"1.0-beta-1", "1.0", -1, "a qualifier sorts below a release"},
		{"1.0-rc1", "1.0-beta2", 1, ""},
		{"1.0-sp1", "1.0", 1, "sp sorts above a release"},
		{"867.vd09254229f9b_", "878.v3d9b_2f0b_2c2f", -1, ""},
		{"10", "9", 1, "numeric, not lexical"},
		{"1.0-zzz", "1.0-sp1", 1, "an unknown qualifier outranks the table"},

		// Contrary cases: a naive split-and-compare gets every one of these wrong.
		{"1-1", "1.1", -1, "a '-' between two digits opens a sub-list"},
		{"1.0-ea", "1.0", -1, "ea aliases to rc"},
		{"1.0-snapshot", "1.0-alpha", -1, "snapshot ranks below alpha"},
		{"1.0-ga", "1.0", 0, "ga aliases to the release qualifier"},
		{"1.0-final", "1.0", 0, "final aliases to the release qualifier"},
		{"1.0-cr1", "1.0-rc1", 0, "cr aliases to rc"},
		{"1.0a1", "1.0-alpha-1", 1, "the digit/letter transition and the hyphen build different trees"},
	}

	for _, tc := range cases {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			if got := Compare(tc.a, tc.b); got != tc.want {
				t.Errorf("Compare(%q, %q) = %d, want %d (%s)", tc.a, tc.b, got, tc.want, tc.why)
			}
			if got := Compare(tc.b, tc.a); got != -tc.want {
				t.Errorf("Compare(%q, %q) = %d, want %d (antisymmetry)", tc.b, tc.a, got, -tc.want)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		// The case the whole package exists for: a Plugin-Dependencies entry is a
		// minimum, and an incrementals release satisfies an older incrementals one.
		{"1413.v2ff1a_5e720fa_", "1384.vdc05a_48f535f", true},
		{"1384.vdc05a_48f535f", "1413.v2ff1a_5e720fa_", false},
		{"1.0", "1.0", true},
		{"1.0.0", "1.0", true},
		{"1.0", "1.0.1", false},
		{"2.555.3", "2.555", true},
		{"1.0-beta-1", "1.0", false},
	}
	for _, tc := range cases {
		if got := AtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// Upstream corpus
// ---------------------------------------------------------------------------

// versionsQualifier is upstream's VersionNumberTest / ComparableVersionTest
// qualifier sequence in strictly ascending order, with ONE adjustment:
// hudson.util.VersionNumber moves "snapshot" from Maven's position (between
// "rc" and the release qualifier) to the bottom of the table, so "1-SNAPSHOT"
// leads the sequence instead of sitting just before "1". Everything else is
// upstream's list verbatim.
var versionsQualifier = []string{
	"1-SNAPSHOT",
	"1-alpha2snapshot", "1-alpha2", "1-alpha-123",
	"1-beta-2", "1-beta123",
	"1-m2", "1-m11",
	"1-rc", "1-cr2", "1-rc123",
	"1",
	"1-sp", "1-sp2", "1-sp123",
	"1-abc", "1-def", "1-pom-1",
	"1-1-snapshot", "1-1", "1-2", "1-123",
}

// versionsNumber is upstream's numeric-ordering sequence, verbatim.
var versionsNumber = []string{
	"2.0", "2-1", "2.0.a", "2.0.0.a", "2.0.2", "2.0.123", "2.1.0", "2.1-a", "2.1b", "2.1-c",
	"2.1-1", "2.1.0.1", "2.2", "2.123", "11.a2", "11.a11", "11.b2", "11.b11", "11.m2", "11.m11",
	"11", "11.a", "11b", "11c", "11m",
}

func TestCompare_UpstreamCorpusOrdering(t *testing.T) {
	for _, seq := range [][]string{versionsQualifier, versionsNumber} {
		for i := 0; i < len(seq); i++ {
			for j := i + 1; j < len(seq); j++ {
				if got := Compare(seq[i], seq[j]); got != -1 {
					t.Errorf("Compare(%q, %q) = %d, want -1 (corpus order)", seq[i], seq[j], got)
				}
				if got := Compare(seq[j], seq[i]); got != 1 {
					t.Errorf("Compare(%q, %q) = %d, want 1 (corpus order)", seq[j], seq[i], got)
				}
			}
		}
	}
}

// corpus is the union of everything the tests exercise, used for the algebraic
// properties below.
func corpus() []string {
	out := append([]string{}, versionsQualifier...)
	out = append(out, versionsNumber...)
	out = append(out,
		"", "0", "1", "1.0", "1.0.0", "1.0-ga", "1.0-final", "1.0-ea", "1.0-cr1", "1.0-rc1",
		"1-1", "1.1", "1.0a1", "1.0-alpha-1", "1.0-zzz", "1.0-sp1",
		"1413.v2ff1a_5e720fa_", "1384.vdc05a_48f535f", "867.vd09254229f9b_", "878.v3d9b_2f0b_2c2f",
		"4.5.14", "4.5.14-269.vfa_2321039a_83", "2.479.3", "1.24", "1.9", "1.10",
		"-", ".", "..", "-1", "a", "_", "1_2", "999999999999999999999999.1",
	)
	return out
}

func TestCompare_Antisymmetry(t *testing.T) {
	c := corpus()
	for _, a := range c {
		for _, b := range c {
			if got, want := Compare(a, b), -Compare(b, a); got != want {
				t.Errorf("Compare(%q,%q)=%d but -Compare(%q,%q)=%d", a, b, got, b, a, want)
			}
		}
	}
}

func TestCompare_Reflexivity(t *testing.T) {
	for _, a := range corpus() {
		if got := Compare(a, a); got != 0 {
			t.Errorf("Compare(%q,%q) = %d, want 0", a, a, got)
		}
	}
}

// realisticCorpus is the shape of version string the closure planner actually
// compares: released plugin versions, lock pins, and incrementals builds. The
// full corpus() deliberately includes degenerate qualifier forms that upstream
// itself does not order transitively (see
// TestCompare_UpstreamNonTransitivityIsReproduced).
func realisticCorpus() []string {
	return []string{
		"1.0", "1.0.0", "1.1", "1.9", "1.10", "1.24", "2.0", "2.555", "2.555.3", "10.0",
		"1384.vdc05a_48f535f", "1413.v2ff1a_5e720fa_", "867.vd09254229f9b_", "878.v3d9b_2f0b_2c2f",
		"470.vb_9a_e8b_5b_58b_2", "472.vf7c289a_4b_420",
		"4.5.14", "4.5.14-269.vfa_2321039a_83", "1400.v0", "1389.vd4",
	}
}

func TestCompare_Transitivity(t *testing.T) {
	c := realisticCorpus()
	for _, a := range c {
		for _, b := range c {
			if Compare(a, b) > 0 {
				continue
			}
			for _, d := range c {
				if Compare(b, d) <= 0 && Compare(a, d) > 0 {
					t.Fatalf("transitivity violated: %q <= %q <= %q but %q > %q", a, b, d, a, d)
				}
			}
		}
	}
}

// TestCompare_UpstreamNonTransitivityIsReproduced pins a known quirk of
// hudson.util.VersionNumber / Maven's ComparableVersion: the ordering is
// antisymmetric but NOT transitive across mixed qualifier and numeric-zero
// forms. "1" < "1-sp" and "1-sp" < "1.0a1", yet "1" > "1.0a1", because a
// qualifier compares below an integer item while a MISSING item compares below
// a qualifier.
//
// This is asserted rather than fixed. The package is a port, and a "corrected"
// ordering would disagree with what Jenkins itself resolves — which is the only
// thing that matters for a Plugin-Dependencies minimum. No real plugin version
// takes these forms.
func TestCompare_UpstreamNonTransitivityIsReproduced(t *testing.T) {
	if Compare("1", "1-sp") != -1 {
		t.Error(`expected "1" < "1-sp"`)
	}
	if Compare("1-sp", "1.0a1") != -1 {
		t.Error(`expected "1-sp" < "1.0a1"`)
	}
	if Compare("1", "1.0a1") != 1 {
		t.Error(`expected "1" > "1.0a1" — the upstream quirk`)
	}
}

// TestCompare_Total asserts R12: every string parses. There is no error return,
// so the property under test is that no input panics and every pair is ordered.
func TestCompare_Total(t *testing.T) {
	weird := []string{
		"", " ", "-", "--", ".", "...", "-.-", "_", "___", "1_2_3",
		"\x00", "café", "1.2.3.4.5.6.7.8.9.10", "1-", "-1", "1..2", "1.-.2",
		"999999999999999999999999999999999999999999", "0000001",
	}
	for _, a := range weird {
		for _, b := range weird {
			got := Compare(a, b)
			if got < -1 || got > 1 {
				t.Errorf("Compare(%q,%q) = %d, out of range", a, b, got)
			}
		}
	}
	if Compare("0000001", "1") != 0 {
		t.Error("leading zeros must not change the integer value")
	}
}

// ---------------------------------------------------------------------------
// §9.4 — pluginver and jenkinsver are not interchangeable
// ---------------------------------------------------------------------------

// TestNotInterchangeableWithJenkinsver is the regression that keeps the two
// packages from being merged. An incrementals plugin version and its bare
// numeric prefix are DIFFERENT versions under pluginver; jenkinsver, which
// exists to read core image tags, deliberately collapses them.
func TestNotInterchangeableWithJenkinsver(t *testing.T) {
	const incrementals = "4.5.14-269.vfa_2321039a_83"
	const bare = "4.5.14"

	if Compare(incrementals, bare) == 0 {
		t.Errorf("pluginver must distinguish %q from %q — the incrementals suffix is the version", incrementals, bare)
	}
	// A non-empty trailing sub-list is a continuation of the release, so the
	// incrementals build is NEWER than the bare version — matching how Jenkins
	// itself orders an incrementals release against its base.
	if got := Compare(incrementals, bare); got != 1 {
		t.Errorf("Compare(%q, %q) = %d, want 1", incrementals, bare, got)
	}

	// jenkinsver truncates at the first '-', which is right for a core image
	// tag and wrong for a plugin version.
	ci, ok := jenkinsver.Core(incrementals)
	if !ok {
		t.Fatalf("jenkinsver.Core(%q) failed", incrementals)
	}
	cb, ok := jenkinsver.Core(bare)
	if !ok {
		t.Fatalf("jenkinsver.Core(%q) failed", bare)
	}
	if jenkinsver.Compare(ci, cb) != 0 {
		t.Fatalf("expected jenkinsver to collapse %q and %q", incrementals, bare)
	}

	// The core image tag stays jenkinsver's concern: it parses there and its
	// JDK suffix is meant to be discarded.
	const coreTag = "2.479.3-jdk17"
	core, ok := jenkinsver.Core(coreTag)
	if !ok || len(core) != 3 || core[0] != 2 || core[1] != 479 || core[2] != 3 {
		t.Fatalf("jenkinsver.Core(%q) = %v, %v; want [2 479 3], true", coreTag, core, ok)
	}
	// pluginver does not truncate it, which is exactly why it must not be used
	// for core tags.
	if Compare(coreTag, "2.479.3") == 0 {
		t.Errorf("pluginver must not collapse %q onto its core prefix", coreTag)
	}
}

// TestCompare_TwoSegmentPins guards the closure planner's real inputs: lock
// pins are often two-segment and must order numerically, not lexically.
func TestCompare_TwoSegmentPins(t *testing.T) {
	cases := [][3]interface{}{
		{"1.24", "1.9", 1},
		{"1.9", "1.10", -1},
		{"1.24", "1.24", 0},
	}
	for _, c := range cases {
		a, b, want := c[0].(string), c[1].(string), c[2].(int)
		if got := Compare(a, b); got != want {
			t.Errorf("Compare(%q,%q) = %d, want %d", a, b, got, want)
		}
	}
}
