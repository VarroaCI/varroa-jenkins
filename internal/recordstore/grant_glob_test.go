package recordstore

import "testing"

// TestMatchGlob covers the tenant-export-grant capability's three
// metacharacters and both anchors.
func TestMatchGlob(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		input   string
		want    bool
	}{
		{"a literal pattern matches itself", "deploy-prod", "deploy-prod", true},
		{"a literal pattern does not match a different string", "deploy-prod", "deploy-staging", false},
		{"question mark matches one character", "deploy-?", "deploy-1", true},
		{"question mark does not match two characters", "deploy-?", "deploy-12", false},
		{"star matches within one path segment", "release/*", "release/1.0", true},
		{"star does not cross a slash", "release/*", "release/1.0/rc1", false},
		{"double star crosses a slash", "release/**", "release/1.0/rc1", true},
		{"the left anchor rejects a longer prefix", "prod", "not-prod", false},
		{"the right anchor rejects a longer suffix", "prod", "prod-2", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchGlob(tc.pattern, tc.input); got != tc.want {
				t.Fatalf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
			}
		})
	}
}

// TestMatchExportEntryStripsTenantPrefix asserts a job resource is made
// relative to the target tenant's own folder before it is matched.
func TestMatchExportEntryStripsTenantPrefix(t *testing.T) {
	exports := []exportEntry{{Job: "release/*", Callers: []string{"payments"}}}

	entries, ok := matchExportEntries(exports, GrantResourceJob, "beta/release/1.0", "beta")
	if !ok {
		t.Fatal("want a match after stripping the tenant prefix, got none")
	}
	if len(entries) != 1 || len(entries[0].Callers) != 1 || entries[0].Callers[0] != "payments" {
		t.Fatalf("matched entries = %v, want one entry with callers [payments]", entries)
	}
}

// TestMatchExportEntriesUnionOfOverlappingEntries asserts every export
// entry whose glob matches resource is returned, not only the first
// one: two entries with overlapping job globs both count toward the
// grant orders' union.
func TestMatchExportEntriesUnionOfOverlappingEntries(t *testing.T) {
	exports := []exportEntry{
		{Job: "release/*", Callers: []string{"search"}},
		{Job: "release/prod", Callers: []string{"payments"}},
	}

	entries, ok := matchExportEntries(exports, GrantResourceJob, "beta/release/prod", "beta")
	if !ok {
		t.Fatal("want a match, got none")
	}
	if len(entries) != 2 {
		t.Fatalf("matched entries = %d, want 2 (both overlapping globs match)", len(entries))
	}
}

// TestMatchExportEntryJobOutsidePrefixDoesNotMatch asserts a job path
// that does not carry the target tenant's own folder prefix matches
// nothing, rather than matching against the unstripped path.
func TestMatchExportEntryJobOutsidePrefixDoesNotMatch(t *testing.T) {
	exports := []exportEntry{{Job: "release/*", Callers: []string{"payments"}}}

	if _, ok := matchExportEntries(exports, GrantResourceJob, "gamma/release/1.0", "beta"); ok {
		t.Fatal("want no match for a job path outside the target tenant's own folder")
	}
}

// TestMatchExportEntryLockIsAlreadyRelative asserts a lock resource is
// matched as given, with no prefix stripped by the evaluator.
func TestMatchExportEntryLockIsAlreadyRelative(t *testing.T) {
	exports := []exportEntry{{Locks: []string{"deploy-*"}, Callers: []string{"alpha"}}}

	if _, ok := matchExportEntries(exports, GrantResourceLock, "deploy-prod", "beta"); !ok {
		t.Fatal("want a lock match, got none")
	}
}

// TestMatchExportEntryKindMismatch asserts a job export grants no lock,
// and a lock export grants no job.
func TestMatchExportEntryKindMismatch(t *testing.T) {
	jobOnly := []exportEntry{{Job: "api", Callers: []string{"alpha"}}}
	if _, ok := matchExportEntries(jobOnly, GrantResourceLock, "deploy-prod", "beta"); ok {
		t.Fatal("want no match: a job entry does not grant a lock")
	}

	lockOnly := []exportEntry{{Locks: []string{"deploy-*"}, Callers: []string{"alpha"}}}
	if _, ok := matchExportEntries(lockOnly, GrantResourceJob, "api", "beta"); ok {
		t.Fatal("want no match: a lock entry does not grant a job")
	}
}
