package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// retryHome reads jobPath's home the way the sweep's placement read
// answers it.
func retryHome(t *testing.T, pool livenessQuerier, jobPath string) JobHome {
	t.Helper()
	row, exists, err := readTenantPlacementLivenessRow(context.Background(), pool, testCluster, testTenant)
	if err != nil {
		t.Fatalf("read placement: %v", err)
	}
	return jobHomeOf(row.members, exists, jobPath)
}

// interruptedRetryRun answers a RUNNING candidate on cell-a for jobPath.
func interruptedRetryRun(jobPath string) runInterruptCandidate {
	return runInterruptCandidate{
		runID: uuid.Must(uuid.NewV7()), cluster: testCluster, tenant: testTenant,
		namespace: jsTestNS, cell: "cell-a", jobPath: jobPath, status: StatusRunning, actor: "alice",
	}
}

// TestAfterMarkRetry_AddressesTheHomeSlot asserts the automatic retry
// names the member of the job's home slot, not the cell of the marked
// run.
func TestAfterMarkRetry_AddressesTheHomeSlot(t *testing.T) {
	pool, db := setupLedgerPool(t)
	jobPath := testTenant + "/retry-home"
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	setPlacementMembers(t, db, testTenant,
		[]member{activeMember("cell-a", 0, "i0"), activeMember("cell-b1", 1, "i1")},
		map[string]int32{jobPath: 1})

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	afterMark(context.Background(), conn, retryHome(t, pool, jobPath), nil, nil,
		interruptedRetryRun(jobPath), ReasonCellLost, interruptedRetryAutomatic, nil)

	actions := wait(1)
	a := actions[0]
	if a.Kind != LedgerActionRetryRun {
		t.Fatalf("kind = %s, want retry_run", a.Kind)
	}
	if a.Namespace != "cells" || a.Controller != "cell-b1" {
		t.Fatalf("retry address = %s/%s, want cells/cell-b1", a.Namespace, a.Controller)
	}
}

// TestAfterMarkRetry_UnplacedControllerKeepsTheRunCell asserts a tenant
// with no placement row retries on the marked run's own cell.
func TestAfterMarkRetry_UnplacedControllerKeepsTheRunCell(t *testing.T) {
	pool, _ := setupLedgerPool(t)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	jobPath := testTenant + "/retry-unplaced"
	afterMark(context.Background(), conn, retryHome(t, pool, jobPath), nil, nil,
		interruptedRetryRun(jobPath), ReasonCellLost, interruptedRetryAutomatic, nil)

	a := wait(1)[0]
	if a.Namespace != jsTestNS || a.Controller != "cell-a" {
		t.Fatalf("retry address = %s/%s, want %s/cell-a", a.Namespace, a.Controller, jsTestNS)
	}
}

// TestAfterMarkRetry_UnboundHomeSlotPublishesNothing asserts a home slot
// that holds no member publishes no retry and counts one unplaced skip.
func TestAfterMarkRetry_UnboundHomeSlotPublishesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	jobPath := testTenant + "/retry-unbound"
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	setPlacementMembers(t, db, testTenant,
		[]member{activeMember("cell-a", 0, "i0")}, map[string]int32{jobPath: 1})

	reader, restore := swapMeterProvider(t)
	defer restore()
	metrics, err := NewInterruptedMetrics()
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	afterMark(context.Background(), conn, retryHome(t, pool, jobPath), metrics, nil,
		interruptedRetryRun(jobPath), ReasonCellLost, interruptedRetryAutomatic, nil)

	wait(0)
	if n := sumCounterByAttr(t, reader, "recorder_retry_skipped_total", "reason", "unplaced"); n != 1 {
		t.Fatalf("recorder_retry_skipped_total{reason=unplaced} = %d, want 1", n)
	}
}

// TestSweepInterruptedRuns_RetryAddressesTheHomeSlot asserts the sweep
// sends the automatic retry of a run whose cell-a instance is gone to the
// member that now holds the job's home slot.
func TestSweepInterruptedRuns_RetryAddressesTheHomeSlot(t *testing.T) {
	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	past := time.Now().Add(-17 * time.Minute)
	if _, err := db.Exec(`UPDATE varroa.runs SET heartbeat_received_at = $1 WHERE run_id = $2`, past, runID); err != nil {
		t.Fatalf("backdate heartbeat_received_at: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "i0", true, nil, nil,
		[]TenantSyncDeadInstance{{Instance: "pod-a", LastSeenAt: past}})
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements SET policy = '{"interruptedRetry":"automatic"}'::jsonb
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("set automatic retry policy: %v", err)
	}
	setPlacementMembers(t, db, testTenant,
		[]member{activeMember("cell-a2", 0, "i0"), activeMember("cell-b1", 1, "i1")},
		map[string]int32{testJobPath: 1})

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	wait := collectBackstopActions(t, conn)

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, nil,
		DefaultSweepMaxPending, time.Now(), testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	retry := wait(1)[0]
	if retry.Kind != LedgerActionRetryRun {
		t.Fatalf("kind = %s, want retry_run", retry.Kind)
	}
	if retry.Namespace != "cells" || retry.Controller != "cell-b1" {
		t.Fatalf("retry address = %s/%s, want cells/cell-b1", retry.Namespace, retry.Controller)
	}
}
