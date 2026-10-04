package recordstore

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
)

// TestSweepMarksARunWhosePodClockRunsFast asserts the silent test reads
// the recorder's receive stamp. The run's own pod reports a heartbeat one
// minute old, and the recorder received nothing for 17 minutes. A rule
// that read the pod clock would hold this dead build RUNNING forever.
func TestSweepMarksARunWhosePodClockRunsFast(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	now := time.Now()
	fresh := now.Add(-time.Minute)
	stale := now.Add(-17 * time.Minute)
	if _, err := db.Exec(`
		UPDATE varroa.runs SET last_heartbeat_at = $1, heartbeat_received_at = $2 WHERE run_id = $3
	`, fresh, stale, runID); err != nil {
		t.Fatalf("stamp the two heartbeat columns: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", false, &stale, &stale, nil)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	metrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil,
		DefaultSweepMaxPending, now, testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	var status, reason string
	if err := db.QueryRow(`
		SELECT status, payload->>'interrupted_reason' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&status, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusCompleted || reason != ReasonNoHeartbeat {
		t.Fatalf("status=%q reason=%q, want COMPLETED/%q", status, reason, ReasonNoHeartbeat)
	}
}

// TestSweepLeavesARunWhosePodClockRunsSlow asserts the mirror case. The
// pod reports a heartbeat 17 minutes old, and the recorder received one a
// minute ago. A rule that read the pod clock would interrupt this live
// build and cancel it.
func TestSweepLeavesARunWhosePodClockRunsSlow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	runID := seedRunningRun(t, pool, newTestClaimRequest("manual:"+newTestUUIDv7String(), 0))

	now := time.Now()
	fresh := now.Add(-time.Minute)
	stale := now.Add(-17 * time.Minute)
	if _, err := db.Exec(`
		UPDATE varroa.runs SET last_heartbeat_at = $1, heartbeat_received_at = $2 WHERE run_id = $3
	`, stale, fresh, runID); err != nil {
		t.Fatalf("stamp the two heartbeat columns: %v", err)
	}
	seedTenantPlacementLiveness(t, db, "pod-a", false, &stale, &stale, nil)

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)

	metrics, err := NewInterruptedMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("new interrupted metrics: %v", err)
	}

	marked, err := SweepInterruptedRuns(context.Background(), pool, conn, metrics, nil, nil,
		DefaultSweepMaxPending, now, testGrace, nil)
	if err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
	if marked != 0 {
		t.Fatalf("marked = %d, want 0", marked)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("status = %q, want %q", status, StatusRunning)
	}
}
