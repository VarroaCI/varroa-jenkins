package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestBranchScanFailurePublishesOnce checks that a failed scan writes one
// pipeline.branch_scan_failed row, that a repeat envelope with the same scan
// writes no second row, and that a new failed scan writes its own row.
func TestBranchScanFailurePublishesOnce(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	base := time.Now().UTC()

	failedScan := func(id string) *BranchScanInput {
		return &BranchScanInput{ScanID: id, State: "failed", Result: "FAILURE", EndedAt: base.Format(time.RFC3339)}
	}
	project := func(scan *BranchScanInput) JobBranchProjectInput {
		p := oneBranchProject("acme/scan")
		p.LastScan = scan
		return p
	}
	apply := func(at time.Duration, p JobBranchProjectInput) {
		t.Helper()
		if _, err := g.Apply(ctx, newJobBranchesEnvelope(t, jsTestCell, base.Add(at), JobBranchesPayload{
			Hash: "h", Total: 1, Projects: []JobBranchProjectInput{p},
		})); err != nil {
			t.Fatalf("apply job.branches: %v", err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM varroa.activity
			WHERE type = 'pipeline.branch_scan_failed' AND item_path = 'acme/scan'
		`).Scan(&n); err != nil {
			t.Fatalf("count branch scan rows: %v", err)
		}
		return n
	}

	apply(time.Second, project(failedScan("scan-1")))
	if n := count(); n != 1 {
		t.Fatalf("after the first failed scan: rows = %d, want 1", n)
	}
	apply(2*time.Second, project(failedScan("scan-1")))
	if n := count(); n != 1 {
		t.Fatalf("after a repeat of the same scan: rows = %d, want 1", n)
	}
	apply(3*time.Second, project(failedScan("scan-2")))
	if n := count(); n != 2 {
		t.Fatalf("after a new failed scan: rows = %d, want 2", n)
	}
}
