package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// branchScanStateFailed is the scan state that the plugin writes when a scan
// ends in error.
const branchScanStateFailed = "failed"

// isBranchScanFailure reports whether a scan ended in failure. A scan fails
// when its state is failed, or when its result names a Jenkins outcome other
// than SUCCESS. A scan that has not finished has an empty result.
func isBranchScanFailure(scan *BranchScanInput) bool {
	if scan.State == branchScanStateFailed {
		return true
	}
	return scan.Result != "" && scan.Result != "SUCCESS"
}

// recordBranchScanFailure queues one pipeline.branch_scan_failed event for a
// failed scan the store has not recorded yet. The stored scan id names the
// last scan that the row holds, so a repeat envelope with the same scan adds
// no event.
func recordBranchScanFailure(ctx context.Context, tx pgx.Tx, env IngestEnvelope, project JobBranchProjectInput,
	out *activityOutbox) error {
	scan := project.LastScan
	var storedID, storedState *string
	err := tx.QueryRow(ctx, `
		SELECT last_scan->>'scan_id', last_scan->>'state' FROM varroa.job_branch_projects
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, env.Cluster, project.Tenant, project.JobPath).Scan(&storedID, &storedState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read stored branch scan: %w", err)
	}
	// The plugin exports one scan first as running, then as failed. Only a
	// stored failure of this scan id suppresses the event.
	if storedID != nil && *storedID == scan.ScanID && storedState != nil && *storedState == branchScanStateFailed {
		return nil
	}

	at := env.OccurredAt
	ended, err := parseOptionalRFC3339(scan.EndedAt)
	if err != nil {
		return fmt.Errorf("%w: decode branch scan ended_at: %v", ErrMalformedEnvelope, err)
	}
	if ended != nil {
		at = *ended
	}
	ev := activity.Event{
		Timestamp:  at,
		Type:       "pipeline.branch_scan_failed",
		Source:     "recorder",
		Severity:   "warning",
		Cluster:    env.Cluster,
		Namespace:  env.Namespace,
		Controller: env.Cell,
		ItemPath:   project.JobPath,
		Reason:     scan.ScanID,
		Result:     scan.Result,
		// The message names the scan, so two failures at one instant keep
		// distinct event keys.
		Message: fmt.Sprintf("branch scan %s of %s failed (%s)", scan.ScanID, project.JobPath, scan.Result),
	}
	if scan.Message != "" {
		ev.Message += ": " + scan.Message
	}
	return insertActivityForPublish(ctx, tx, ev, out)
}
