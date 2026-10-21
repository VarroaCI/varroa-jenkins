package controller

import (
	"testing"
	"time"
)

// Two spellings of one repository share one verdict and one probe.
func TestRepoVerdictNormalizesRepoURL(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	prober := &countingProber{}
	r := inlineProbeReconciler(prober)
	d := successResult(
		failedScan("t/a", "https://git.example.org/o/r.git"),
		failedScan("t/b", "https://git.example.org/o/r"),
	)
	r.noteRepoScans("ns/cell", d, map[string]bool{"t": true}, []string{"t"}, now)
	if got := prober.count(); got != 1 {
		t.Fatalf("probes = %d, want 1 for one repository", got)
	}
}
