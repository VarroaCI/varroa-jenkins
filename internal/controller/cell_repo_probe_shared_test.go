package controller

import (
	"testing"
	"time"
)

// Two folders of one tenant that name the same repository share a verdict.
// One folder that recovers must not drop the verdict of the other.
func TestRepoVerdictSharedByFoldersOfOneRepository(t *testing.T) {
	const cell = "ns/cell"
	const url = "https://git.example.org/o/gone"
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prober := &countingProber{goneURLs: map[string]bool{url: true}}
	r := inlineProbeReconciler(prober)
	matched := map[string]bool{"t": true}
	tenants := []string{"t"}

	both := successResult(failedScan("t/a", url), failedScan("t/b", url))
	r.noteRepoScans(cell, both, matched, tenants, now)
	if got := r.repoGoneFolders(cell); len(got) != 2 {
		t.Fatalf("repoGoneFolders = %v, want both folders", got)
	}

	// Folder t/a recovers; t/b still fails on the same repository.
	r.noteRepoScans(cell, successResult(failedScan("t/b", url)), matched, tenants, now.Add(time.Minute))
	if got := r.repoGoneFolders(cell); len(got) != 1 || got[0] != "t/b" {
		t.Fatalf("repoGoneFolders = %v, want [t/b]", got)
	}
	if prober.count() != 1 {
		t.Fatalf("probes = %d, want 1: the shared verdict must not be probed again", prober.count())
	}
}
