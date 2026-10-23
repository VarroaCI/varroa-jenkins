package sse

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// runNotifyJSON builds a decodable notification body for one key.
func runNotifyJSON(kind, cluster, tenant, jobPath string) []byte {
	return []byte(`{"kind":"` + kind + `","cluster":"` + cluster + `","tenant":"` +
		tenant + `","job_path":"` + jobPath + `","run_id":"run-1"}`)
}

// waitRecord reads one record, or fails the test after a second.
func waitRecord(t *testing.T, ch <-chan Record) Record {
	t.Helper()
	select {
	case rec, open := <-ch:
		if !open {
			t.Fatal("channel closed before a record arrived")
		}
		return rec
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a record")
		return Record{}
	}
}

// TestRunFanout_SubscriberReceivesItsOwnKeysOnly proves a subscriber
// registered under two keys receives a notification for each one, and
// nothing for a third key.
func TestRunFanout_SubscriberReceivesItsOwnKeysOnly(t *testing.T) {
	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	ch := rf.Subscribe([]string{"core/team-a", "core/team-b"}, nil)
	defer rf.Unsubscribe([]string{"core/team-a", "core/team-b"}, ch)

	rf.deliverPayload(runNotifyJSON("run", "core", "team-a", "team-a/app"))
	rf.deliverPayload(runNotifyJSON("stage", "core", "team-b", "team-b/app"))
	rf.deliverPayload(runNotifyJSON("run", "core", "team-c", "team-c/app"))

	wantKinds := []string{"run", "stage"}
	for i, want := range wantKinds {
		rec := waitRecord(t, ch)
		if rec.Event != want {
			t.Errorf("record %d event = %q, want %q", i, rec.Event, want)
		}
	}

	select {
	case rec := <-ch:
		t.Errorf("team-c notification reached a team-a and team-b subscriber: %v", rec.Event)
	case <-time.After(50 * time.Millisecond):
		// Expected: the fan-out routes to the notification's own key only.
	}
}

// TestRunFanout_FullBufferDropsOneRecord proves a full buffer drops the
// overflow record and keeps the subscription alive.
func TestRunFanout_FullBufferDropsOneRecord(t *testing.T) {
	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	keys := []string{"core/team-a"}
	ch := rf.Subscribe(keys, nil)
	defer rf.Unsubscribe(keys, ch)

	const bufferSize = 64
	for i := 0; i < bufferSize+1; i++ {
		rf.deliverPayload(runNotifyJSON("run", "core", "team-a", "team-a/app"))
	}

	for i := 0; i < bufferSize; i++ {
		waitRecord(t, ch)
	}

	select {
	case <-ch:
		t.Error("the overflow record was not dropped")
	case <-time.After(50 * time.Millisecond):
		// Expected: the buffer holds 64 records and drops the 65th.
	}

	// The subscription survives the drop: an empty buffer takes a record.
	rf.deliverPayload(runNotifyJSON("stage", "core", "team-a", "team-a/app"))
	if rec := waitRecord(t, ch); rec.Event != "stage" {
		t.Errorf("after the drop, event = %q, want %q", rec.Event, "stage")
	}
}

// TestRunFanout_DropsUndecodableAndIncompleteKey proves the fan-out drops
// a body it cannot decode, a body with no cluster, and a body with no
// tenant, and that no subscriber sees any of them.
func TestRunFanout_DropsUndecodableAndIncompleteKey(t *testing.T) {
	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	keys := []string{"core/team-a", "/team-a", "core/"}
	ch := rf.Subscribe(keys, nil)
	defer rf.Unsubscribe(keys, ch)

	rf.deliverPayload([]byte("not json at all"))
	rf.deliverPayload(runNotifyJSON("run", "", "team-a", "team-a/app"))
	rf.deliverPayload(runNotifyJSON("run", "core", "", "team-a/app"))

	select {
	case rec := <-ch:
		t.Errorf("a dropped payload reached a subscriber: %v", rec.Event)
	case <-time.After(50 * time.Millisecond):
		// Expected: all three payloads were dropped.
	}
}

// TestRunFanout_UnmatchedRaisesCounter proves a notification whose key
// holds no subscriber raises bff.run_notify.unmatched.
func TestRunFanout_UnmatchedRaisesCounter(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	rf := mustNewRunFanout(t, nil, mp)
	keys := []string{"core/team-a"}
	ch := rf.Subscribe(keys, nil)
	defer rf.Unsubscribe(keys, ch)

	rf.deliverPayload(runNotifyJSON("run", "core", "team-b", "team-b/app"))

	select {
	case rec := <-ch:
		t.Errorf("a notification for another key reached the subscriber: %v", rec.Event)
	case <-time.After(50 * time.Millisecond):
		// Expected: team-b holds no subscriber, so the fan-out drops it.
	}

	if got := unmatchedCounterValue(t, reader); got != 1 {
		t.Errorf("bff.run_notify.unmatched = %d, want 1", got)
	}
}

// mustNewRunFanout calls NewRunFanout and fails the test on an error.
func mustNewRunFanout(t *testing.T, conn *bus.Conn, mp metric.MeterProvider) *RunFanout {
	t.Helper()
	rf, err := NewRunFanout(conn, mp)
	if err != nil {
		t.Fatalf("NewRunFanout: %v", err)
	}
	return rf
}

// unmatchedCounterValue reads the unmatched counter's total from a
// manual reader.
func unmatchedCounterValue(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != RunUnmatchedMetric {
				continue
			}
			if scope.Scope.Name != "varroa-bff" {
				t.Errorf("metric %s scope = %q, want varroa-bff", m.Name, scope.Scope.Name)
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s data = %T, want a sum", m.Name, m.Data)
			}
			if len(sum.DataPoints) == 0 {
				return 0
			}
			return sum.DataPoints[0].Value
		}
	}
	return 0
}

// TestRunFanout_WildcardKeyReceivesNoRunNotification proves a subscriber
// registered under the wildcard key receives no run notification, because
// the fan-out delivers to a notification's own key only.
func TestRunFanout_WildcardKeyReceivesNoRunNotification(t *testing.T) {
	bf := NewBusFanout(nil)
	wildCh := bf.SubscribeAll()
	defer bf.Unsubscribe("*", wildCh)

	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	keys := []string{"*"}
	wild := rf.Subscribe(keys, nil)
	defer rf.Unsubscribe(keys, wild)

	rf.deliverPayload(runNotifyJSON("run", "core", "team-a", "team-a/app"))

	for name, ch := range map[string]<-chan Record{"bus fanout": wildCh, "run fanout": wild} {
		select {
		case rec := <-ch:
			t.Errorf("%s wildcard subscriber received a run notification: %v", name, rec.Event)
		case <-time.After(50 * time.Millisecond):
			// Expected: the run fan-out has no wildcard delivery.
		}
	}
}

// TestRunFanout_UnsubscribeClosesOnce proves one channel shared by two
// keys closes once, and a second unsubscribe call is safe.
func TestRunFanout_UnsubscribeClosesOnce(t *testing.T) {
	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	keys := []string{"core/team-a", "core/team-b"}
	ch := rf.Subscribe(keys, nil)

	rf.Unsubscribe(keys, ch)
	rf.Unsubscribe(keys, ch)

	if _, open := <-ch; open {
		t.Error("channel is not closed after unsubscribe")
	}
}

// TestRunFanout_UnsubscribeWithOneKeyRemovesEveryKey proves a partial key
// list still leaves no registration, so a later delivery writes to no
// closed channel.
func TestRunFanout_UnsubscribeWithOneKeyRemovesEveryKey(t *testing.T) {
	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	ch := rf.Subscribe([]string{"core/team-a", "core/team-b"}, nil)

	// The caller names one key alone.
	rf.Unsubscribe([]string{"core/team-a"}, ch)

	if _, open := <-ch; open {
		t.Error("channel is not closed after unsubscribe")
	}

	// A delivery on the key the caller did not name must find no
	// subscriber: a write to the closed channel would panic here.
	rf.deliverPayload(runNotifyJSON("run", "core", "team-b", "team-b/app"))
	rf.deliverPayload(runNotifyJSON("run", "core", "team-a", "team-a/app"))

	if _, open := <-ch; open {
		t.Error("a delivery reached the subscriber after unsubscribe")
	}
}

// TestRunFanout_NotificationForUnmatchedKeyKeepsRunning proves the
// unmatched path leaves the fan-out's other keys working.
func TestRunFanout_NotificationForUnmatchedKeyKeepsRunning(t *testing.T) {
	rf := mustNewRunFanout(t, nil, noop.NewMeterProvider())
	keys := []string{"core/team-a"}
	ch := rf.Subscribe(keys, nil)
	defer rf.Unsubscribe(keys, ch)

	rf.deliverPayload(runNotifyJSON("run", "core", "tenant-z", "tenant-z/app"))
	rf.deliverPayload(runNotifyJSON("run", "core", "team-a", "team-a/app"))

	rec := waitRecord(t, ch)
	n, ok := rec.Data.(recordstore.RunNotification)
	if !ok {
		t.Fatalf("record data = %T, want a RunNotification", rec.Data)
	}
	if n.Tenant != "team-a" {
		t.Errorf("delivered tenant = %q, want %q", n.Tenant, "team-a")
	}
}
