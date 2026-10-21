package controller

import (
	"testing"
	"time"
)

// A probe still running when its folder stops failing must not leave a gone
// verdict behind for a later failure of the same repository.
func TestRepoProbeInFlightDropDoesNotCacheStaleVerdict(t *testing.T) {
	const cell = "ns/cell"
	const url = "https://git.example.org/o/gone"
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prober := &countingProber{goneURLs: map[string]bool{url: true}}
	r := &Reconciler{}
	r.repoProbe.prober = prober
	var pending []func()
	r.repoProbe.goFn = func(f func()) { pending = append(pending, f) }
	matched := map[string]bool{"t": true}
	tenants := []string{"t"}

	// The first failed scan starts a probe that stays in flight.
	r.noteRepoScans(cell, successResult(failedScan("t/a", url)), matched, tenants, now)
	if len(pending) != 1 {
		t.Fatalf("started probes = %d, want 1", len(pending))
	}
	// The folder stops failing while the probe runs.
	r.noteRepoScans(cell, successResult(), matched, tenants, now.Add(time.Minute))
	// The probe finishes and answers gone.
	pending[0]()
	// A later failure of the same repository must probe again, not release.
	r.noteRepoScans(cell, successResult(failedScan("t/a", url)), matched, tenants, now.Add(2*time.Minute))
	if got := r.repoGoneFolders(cell); got != nil {
		t.Fatalf("repoGoneFolders = %v, want nil before a fresh probe", got)
	}
	if len(pending) != 2 {
		t.Fatalf("started probes = %d, want 2", len(pending))
	}
}
