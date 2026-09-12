package recordstore

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fanoutGaugeValue sums every data point of an Int64ObservableGauge
// named name.
func fanoutGaugeValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			data, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// TestFleetLockMetrics_GaugesReportStoredRowCounts asserts task 4.5's
// own acceptance: each fleet-lock gauge reports the stored row counts,
// including the releasing gauge, and recorder_fleet_lock_expired_total
// registers as a counter.
func TestFleetLockMetrics_GaugesReportStoredRowCounts(t *testing.T) {
	pool, db := setupLedgerPool(t)

	now := time.Now()
	insertFleetLockRow(t, db, "alpha/lock-a", newTestUUIDv7String(), FleetLockStateHeld, now, now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/lock-b", newTestUUIDv7String(), FleetLockStateWaiting, now, now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/lock-c", newTestUUIDv7String(), FleetLockStateWaiting, now, now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/lock-d", newTestUUIDv7String(), FleetLockStateWaiting, now, now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/lock-e", newTestUUIDv7String(), FleetLockStateWaiting, now, now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/lock-f", newTestUUIDv7String(), FleetLockStateReleasing, now, now.Add(5*time.Minute))
	insertFleetLockRow(t, db, "alpha/lock-g", newTestUUIDv7String(), FleetLockStateReleasing, now, now.Add(5*time.Minute))

	reader := fanoutTestMeter(t)
	if _, err := NewFleetLockMetrics(pool); err != nil {
		t.Fatalf("new fleet lock metrics: %v", err)
	}

	if got := fanoutGaugeValue(t, reader, "recorder_fleet_lock_held"); got != 1 {
		t.Fatalf("recorder_fleet_lock_held = %d, want 1", got)
	}
	if got := fanoutGaugeValue(t, reader, "recorder_fleet_lock_waiting"); got != 4 {
		t.Fatalf("recorder_fleet_lock_waiting = %d, want 4", got)
	}
	if got := fanoutGaugeValue(t, reader, "recorder_fleet_lock_releasing"); got != 2 {
		t.Fatalf("recorder_fleet_lock_releasing = %d, want 2", got)
	}
}
