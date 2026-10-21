package controller

import (
	"testing"
	"time"
)

// Two cells that track one tenant and repository share one verdict.
// A folder that recovers in one cell must not drop the verdict of the other.
func TestRepoVerdictSharedAcrossCells(t *testing.T) {
	const url = "https://git.example.org/o/gone"
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prober := &countingProber{goneURLs: map[string]bool{url: true}}
	r := inlineProbeReconciler(prober)
	matched := map[string]bool{"t": true}
	tenants := []string{"t"}

	r.noteRepoScans("ns/cell-a", successResult(failedScan("t/a", url)), matched, tenants, now)
	r.noteRepoScans("ns/cell-b", successResult(failedScan("t/a", url)), matched, tenants, now)
	if prober.count() != 1 {
		t.Fatalf("probes = %d, want one for the tenant and repository", prober.count())
	}

	// Cell A recovers; cell B keeps its confirmed verdict.
	r.noteRepoScans("ns/cell-a", successResult(), matched, tenants, now.Add(time.Minute))
	if got := r.repoGoneFolders("ns/cell-b"); len(got) != 1 {
		t.Fatalf("cell B repoGoneFolders = %v, want its gone folder", got)
	}
}
