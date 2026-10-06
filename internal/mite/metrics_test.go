package mite

import (
	"context"
	"sort"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// noopGatewayMetrics returns gateway instruments on a no-op provider. A
// test that builds a BusHandler by hand sets its metrics field to this.
func noopGatewayMetrics() gatewayMetrics {
	m, err := newGatewayMetrics(noop.NewMeterProvider())
	if err != nil {
		panic(err)
	}
	return m
}

// mustNewBusHandler calls NewBusHandler for the default cluster with a
// no-op provider and no activity publisher. It fails the test on an error.
func mustNewBusHandler(t *testing.T, conn *bus.Conn, snapshotKV, presenceKV *bus.KV) *BusHandler {
	t.Helper()
	h, err := NewBusHandler(noop.NewMeterProvider(), bus.DefaultCluster, conn, snapshotKV, presenceKV, nil)
	if err != nil {
		t.Fatalf("NewBusHandler: %v", err)
	}
	return h
}

// newTestMeterProvider returns a meter provider of the test with a manual
// reader. Pass the provider to the constructor that creates the
// instruments, and collect the values from the reader.
func newTestMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down test meter provider: %v", err)
		}
	})
	return mp, reader
}

// collectGatewaySums returns the sum of every int64 sum instrument on the
// "varroa-gateway" scope of reader, by instrument name.
func collectGatewaySums(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	sums := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != gatewayMeterName {
			t.Errorf("instrument scope = %q, want %q", sm.Scope.Name, gatewayMeterName)
			continue
		}
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Errorf("%s data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
				continue
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			sums[m.Name] = total
		}
	}
	return sums
}

// waitForGatewaySum waits until the instrument name on reader holds want.
func waitForGatewaySum(t *testing.T, reader *sdkmetric.ManualReader, name string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, ok := collectGatewaySums(t, reader)[name]
		if ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %d (recorded %v), want %d", name, got, ok, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewGatewayMetricsCreatesEveryInstrumentOnTheGatewayMeter(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	m, err := newGatewayMetrics(mp)
	if err != nil {
		t.Fatalf("newGatewayMetrics: %v", err)
	}

	ctx := context.Background()
	m.claimsRejected.Add(ctx, 1)
	m.logChunksRejected.Add(ctx, 1)
	m.logChunksBusy.Add(ctx, 1)
	m.logAppendRetries.Add(ctx, 1)
	m.runStateBytes.Add(ctx, 1)
	m.runStateRejected.Add(ctx, 1)
	m.miteStreamConnections.Add(ctx, 1)
	m.miteStreamDisconnects.Add(ctx, 1)
	m.miteWatchFailures.Add(ctx, 1)
	m.buildEventsDropped.Add(ctx, 1)
	m.triggerFailedTotal.Add(ctx, 1)
	m.triggerAcceptedTotal.Add(ctx, 1)
	m.resumeFailedTotal.Add(ctx, 1)
	m.commandRefusedTotal.Add(ctx, 1)

	sums := collectGatewaySums(t, reader)
	got := make([]string, 0, len(sums))
	for name := range sums {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"varroa.gateway.build_events.dropped",
		"varroa.gateway.claims.rejected",
		"varroa.gateway.command_refused",
		"varroa.gateway.log.append.retries",
		"varroa.gateway.log.chunk.busy",
		"varroa.gateway.log.chunk.rejected",
		"varroa.gateway.resume_failed",
		"varroa.gateway.run_state.bytes",
		"varroa.gateway.run_state.rejected",
		"varroa.gateway.trigger_accepted",
		"varroa.gateway.trigger_failed",
		"varroa.mite.stream.connections",
		"varroa.mite.stream.disconnects",
		"varroa.mite.watch.failures",
	}
	if len(got) != len(want) {
		t.Fatalf("instruments = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("instruments = %v, want %v", got, want)
		}
	}
}

func TestBusHandlerCountsStreamConnectionsOnItsProvider(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	h := newTestBusHandler()
	m, err := newGatewayMetrics(mp)
	if err != nil {
		t.Fatalf("newGatewayMetrics: %v", err)
	}
	h.metrics = m

	send := func(*mitev1.OperatorMessage) error { return nil }
	token := h.OnConnect("ctl", "varroa", "v1", time.Time{}, send, nil)
	waitForGatewaySum(t, reader, "varroa.mite.stream.connections", 1)

	h.OnDisconnect("ctl", "varroa", token)
	sums := collectGatewaySums(t, reader)
	if got := sums["varroa.mite.stream.connections"]; got != 0 {
		t.Errorf("varroa.mite.stream.connections = %d, want 0", got)
	}
	if got := sums["varroa.mite.stream.disconnects"]; got != 1 {
		t.Errorf("varroa.mite.stream.disconnects = %d, want 1", got)
	}
}

func TestClaimBridgeCountsAnUnhostedTenantRefusal(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	h := newClaimTestHandler(&fakeLedger{reply: &ledgerClaimResponse{Decision: "granted"}})
	m, err := newGatewayMetrics(mp)
	if err != nil {
		t.Fatalf("newGatewayMetrics: %v", err)
	}
	h.metrics = m
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{RequestId: "req-1", RunId: "run-1", JobPath: "search/build"}
	h.OnClaimRequest("cell1", "ns1", 1, req, sender.send)

	waitForGatewaySum(t, reader, "varroa.gateway.claims.rejected", 1)
}

func TestBuildEventForAnUnhostedTenantCountsADrop(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	m, err := newGatewayMetrics(mp)
	if err != nil {
		t.Fatalf("newGatewayMetrics: %v", err)
	}
	h.metrics = m
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1, JobPath: "search/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	waitForGatewaySum(t, reader, "varroa.gateway.build_events.dropped", 1)
	if pub.publishedCount() != 0 {
		t.Fatalf("published %d envelopes for an unhosted tenant, want 0", pub.publishedCount())
	}
}
