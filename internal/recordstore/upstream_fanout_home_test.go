package recordstore

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// runFanoutForHome runs one fan-out round over fx and answers the metric
// reader and the actions the round published.
func runFanoutForHome(t *testing.T, fx *fanoutTestFixture) (*sdkmetric.ManualReader, chan LedgerAction) {
	t.Helper()
	reader := fanoutTestMeter(t)
	metrics, err := NewUpstreamFanoutMetrics()
	if err != nil {
		t.Fatalf("new fan-out metrics: %v", err)
	}
	received := subscribeCancelActions(t, fx.conn, 4)
	if err := RunUpstreamFanout(context.Background(), fx.deps(metrics), fx.producer, time.Now(), nil); err != nil {
		t.Fatalf("RunUpstreamFanout: %v", err)
	}
	return reader, received
}

// wantFanoutAddress waits for one trigger_run action and asserts it
// names namespace/cell.
func wantFanoutAddress(t *testing.T, received chan LedgerAction, namespace, cell string) {
	t.Helper()
	select {
	case action := <-received:
		if action.Kind != LedgerActionTriggerRun {
			t.Fatalf("kind = %q, want trigger_run", action.Kind)
		}
		if action.Namespace != namespace || action.Controller != cell {
			t.Fatalf("address = %s/%s, want %s/%s", action.Namespace, action.Controller, namespace, cell)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action")
	}
}

// TestRunUpstreamFanout_AddressesTheSubscriberHomeSlot asserts the
// action names the member of the subscriber job's home slot, not the
// cell the stored schedule row names.
func TestRunUpstreamFanout_AddressesTheSubscriberHomeSlot(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "cells", "cell-a", 1, "{}")
	setPlacementMembers(t, fx.db, "beta",
		[]member{activeMember("cell-a", 0, "i0"), activeMember("cell-b1", 1, "i1")},
		map[string]int32{"beta/" + fanoutSubscriberJob: 1})
	// The stored row names slot 0's cell, the state a scale leaves.
	fx.seedSubscriber(testCluster, "beta", false)

	_, received := runFanoutForHome(t, fx)
	wantFanoutAddress(t, received, "cells", "cell-b1")
}

// TestRunUpstreamFanout_TakeoverAddressesThePromotedCell asserts the
// action names the cell that now holds the home slot after a takeover,
// not the former holder the stored row names.
func TestRunUpstreamFanout_TakeoverAddressesThePromotedCell(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "cells", "cell-a", 1, "{}")
	setPlacementMembers(t, fx.db, "beta",
		[]member{activeMember("cell-a", 0, "i0"), activeMember("cell-b2", 1, "i2")},
		map[string]int32{"beta/" + fanoutSubscriberJob: 1})
	fx.seedSubscriber(testCluster, "beta", false)
	if _, err := fx.db.Exec(`
		UPDATE varroa.job_schedules SET cell = 'cell-b1' WHERE tenant = 'beta'
	`); err != nil {
		t.Fatalf("name the former holder: %v", err)
	}

	_, received := runFanoutForHome(t, fx)
	wantFanoutAddress(t, received, "cells", "cell-b2")
}

// TestRunUpstreamFanout_UnboundHomeSlotCountsUnplaced asserts a home slot
// that holds no member publishes nothing and counts reason "unplaced".
func TestRunUpstreamFanout_UnboundHomeSlotCountsUnplaced(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "cells", "cell-a", 1, "{}")
	setPlacementMembers(t, fx.db, "beta",
		[]member{activeMember("cell-a", 0, "i0")},
		map[string]int32{"beta/" + fanoutSubscriberJob: 1})
	fx.seedSubscriber(testCluster, "beta", false)

	reader, received := runFanoutForHome(t, fx)
	select {
	case action := <-received:
		t.Fatalf("an unbound home slot published %+v", action)
	case <-time.After(500 * time.Millisecond):
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_skipped_total",
		map[string]string{"reason": FanoutSkipUnplaced}); got != 1 {
		t.Fatalf("unplaced skips = %d, want 1", got)
	}
	if got := fanoutMetricSum(t, reader, "recorder_upstream_fanout_total", nil); got != 0 {
		t.Fatalf("published = %d, want 0", got)
	}
}

// TestRunUpstreamFanout_OneSlotTenantKeepsTheSlotZeroCell asserts a
// placement row with an empty member list answers its scalar cell, the
// synthetic slot-0 member, and not the stored schedule row's cell.
func TestRunUpstreamFanout_OneSlotTenantKeepsTheSlotZeroCell(t *testing.T) {
	fx := newFanoutFixture(t)
	fx.setExports([]string{"beta"})
	seedGrantPlacement(t, fx.db, testCluster, "beta", "jenkins", "cell-b", 1, "{}")
	fx.seedSubscriber(testCluster, "beta", false)

	_, received := runFanoutForHome(t, fx)
	wantFanoutAddress(t, received, "jenkins", "cell-b")
}

// TestRunUpstreamFanout_NativeEdgeReachesSlotOneHolder asserts a native
// upstream edge inside one tenant reaches the holder of the subscriber
// job's home slot when the stored row names slot 0's cell.
func TestRunUpstreamFanout_NativeEdgeReachesSlotOneHolder(t *testing.T) {
	fx := newFanoutFixture(t)
	setPlacementMembers(t, fx.db, fanoutProducerTenant,
		[]member{activeMember("cell-a", 0, "i0"), activeMember("cell-p1", 1, "i1")},
		map[string]int32{fanoutProducerTenant + "/deploy": 1})
	fx.seedNativeSubscriber("deploy", fanoutProducerTenant+"/build", "SUCCESS", false)

	_, received := runFanoutForHome(t, fx)
	wantFanoutAddress(t, received, "cells", "cell-p1")
}
