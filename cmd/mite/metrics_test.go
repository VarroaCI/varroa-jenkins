package main

import (
	"context"
	"sort"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// noopMiteMetrics returns mite instruments on a no-op provider. A test
// that builds an Agent by hand sets its metrics field to this.
func noopMiteMetrics() miteMetrics {
	m, err := newMiteMetrics(noop.NewMeterProvider())
	if err != nil {
		panic(err)
	}
	return m
}

// mustNewAgent calls NewAgent with a no-op provider and fails the test on
// an error.
func mustNewAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	a, err := NewAgent(noop.NewMeterProvider(), cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return a
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

// collectMiteSums returns the sum of every int64 sum instrument on the
// "varroa-mite" scope of reader, by instrument name.
func collectMiteSums(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	sums := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != miteMeterName {
			t.Errorf("instrument scope = %q, want %q", sm.Scope.Name, miteMeterName)
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

func TestNewMiteMetricsCreatesEveryInstrumentOnTheMiteMeter(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	m, err := newMiteMetrics(mp)
	if err != nil {
		t.Fatalf("newMiteMetrics: %v", err)
	}

	ctx := context.Background()
	m.activityEventsDropped.Add(ctx, 1)
	m.activityEventsForwarded.Add(ctx, 1)
	m.claimsGranted.Add(ctx, 1)
	m.claimsDuplicate.Add(ctx, 1)
	m.claimsVetoed.Add(ctx, 1)
	m.claimsFailed.Add(ctx, 1)
	m.buildEventsForwarded.Add(ctx, 1)
	m.buildEventsAcked.Add(ctx, 1)
	m.buildEventsRejected.Add(ctx, 1)
	m.buildEventsRetainedBytes.Add(ctx, 1)
	m.logChunksForwarded.Add(ctx, 1)
	m.logChunksAcked.Add(ctx, 1)
	m.logChunksRejected.Add(ctx, 1)
	m.logChunksRetainedBytes.Add(ctx, 1)

	sums := collectMiteSums(t, reader)
	got := make([]string, 0, len(sums))
	for name := range sums {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"varroa.mite.activity.events.dropped",
		"varroa.mite.activity.events.forwarded",
		"varroa.mite.build_events.acked",
		"varroa.mite.build_events.forwarded",
		"varroa.mite.build_events.rejected",
		"varroa.mite.build_events.retained_bytes",
		"varroa.mite.claims.duplicate",
		"varroa.mite.claims.failed",
		"varroa.mite.claims.granted",
		"varroa.mite.claims.vetoed",
		"varroa.mite.log_chunks.acked",
		"varroa.mite.log_chunks.forwarded",
		"varroa.mite.log_chunks.rejected",
		"varroa.mite.log_chunks.retained_bytes",
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

func TestClaimDecisionCountsOnTheAgentProvider(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	a, err := NewAgent(mp, Config{})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	ctx := context.Background()
	for _, decision := range []string{"granted", "granted", "duplicate", "vetoed", ""} {
		a.countClaimDecision(ctx, &mitev1.ClaimResponse{Decision: decision})
	}

	sums := collectMiteSums(t, reader)
	want := map[string]int64{
		"varroa.mite.claims.granted":   2,
		"varroa.mite.claims.duplicate": 1,
		"varroa.mite.claims.vetoed":    1,
		"varroa.mite.claims.failed":    1,
	}
	for name, n := range want {
		if got := sums[name]; got != n {
			t.Errorf("%s = %d, want %d", name, got, n)
		}
	}
}
