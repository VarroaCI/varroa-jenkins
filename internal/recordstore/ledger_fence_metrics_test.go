package recordstore

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// metricValueByReason sums every recorder_fence_veto_total data point
// whose "reason" attribute equals reason.
func metricValueByReason(t *testing.T, reader *sdkmetric.ManualReader, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "recorder_fence_veto_total" {
				continue
			}
			data, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("reason")); ok && v.AsString() == reason {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// TestFenceVetoMetricCountsByReason asserts recorder_fence_veto_total
// counts one FENCED veto and one CROSS_TENANT veto under their own
// reason label, reading the counter after each claim.
func TestFenceVetoMetricCountsByReason(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)

	mp, reader := newTestMeterProvider(t)

	metrics, err := NewFenceMetrics(mp)
	if err != nil {
		t.Fatalf("new fence metrics: %v", err)
	}
	ledger := NewLedger(pool, NewPlacementFence(pool, metrics))

	seedTenantPlacement(t, db, testTenant, testNamespace, "cell-a", 5, "pod-a", "pod-a", nil)

	// A claim from a cell the tenant holds no member on vetoes FENCED.
	// A stale generation alone answers the retryable NOT_READY, because
	// it comes from a current member whose resolver cache is behind.
	staleReq := newTestClaimRequest("manual:"+newTestUUIDv7String(), 5)
	staleReq.Cell = "cell-old"
	resp := ledger.Claim(context.Background(), staleReq)
	if resp.ReasonCode != ReasonFenced {
		t.Fatalf("want FENCED, got %+v", resp)
	}
	if got := metricValueByReason(t, reader, ReasonFenced); got != 1 {
		t.Fatalf("FENCED veto count = %d, want 1", got)
	}

	// A svc-<other> actor against the same, now-matching, generation
	// vetoes CROSS_TENANT.
	crossReq := newTestClaimRequest("manual:"+newTestUUIDv7String(), 5)
	crossReq.Actor = "svc-other"
	resp = ledger.Claim(context.Background(), crossReq)
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("want CROSS_TENANT, got %+v", resp)
	}
	if got := metricValueByReason(t, reader, ReasonCrossTenant); got != 1 {
		t.Fatalf("CROSS_TENANT veto count = %d, want 1", got)
	}

	// The FENCED count from the first claim is unchanged.
	if got := metricValueByReason(t, reader, ReasonFenced); got != 1 {
		t.Fatalf("FENCED veto count after the second claim = %d, want 1", got)
	}
}

// TestFenceMetricsNilIsSafe asserts a PlacementFence built with no
// metrics answers a veto normally and panics nowhere.
func TestFenceMetricsNilIsSafe(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonNotReady {
		t.Fatalf("want vetoed/NOT_READY, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}
