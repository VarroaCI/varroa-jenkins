package main

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// newTestLedgerActionMetrics returns ledger-action metrics on a meter
// provider of the test, and the manual reader of that provider.
func newTestLedgerActionMetrics(t *testing.T) (*ledgerActionMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	mp, reader := newTestMeterProvider(t)
	m, err := newLedgerActionMetrics(mp)
	if err != nil {
		t.Fatalf("create ledger action metrics: %v", err)
	}
	return m, reader
}

// noopLedgerActionMetrics returns ledger-action metrics on a no-op meter
// provider, for a test that reads no metric.
func noopLedgerActionMetrics(t *testing.T) *ledgerActionMetrics {
	t.Helper()
	m, err := newLedgerActionMetrics(noop.NewMeterProvider())
	if err != nil {
		t.Fatalf("create ledger action metrics: %v", err)
	}
	return m
}

// ledgerActionRejectedSum reads the current varroa_ledger_actions_rejected_total
// sum for reason, or 0 when no data point for that reason exists yet.
func ledgerActionRejectedSum(t *testing.T, reader *sdkmetric.ManualReader, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "varroa-operator" {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "varroa_ledger_actions_rejected_total" {
				continue
			}
			sd, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("expected Int64 Sum data, got %T", m.Data)
			}
			for _, dp := range sd.DataPoints {
				v, _ := dp.Attributes.Value("reason")
				if v.AsString() == reason {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// TestHandleLedgerAction_UnknownKind_CountsRejection covers task 9.1a's
// "count the rejection" rule (design D15): an unknown kind increments
// varroa_ledger_actions_rejected_total{reason="unknown_kind"} by one,
// through the real handleLedgerAction call site.
func TestHandleLedgerAction_UnknownKind_CountsRejection(t *testing.T) {
	t.Parallel()
	metrics, reader := newTestLedgerActionMetrics(t)
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{Kind: "delete_everything", Cluster: "core", Namespace: "cells-ns", Controller: "cell-a"}
	data, _ := json.Marshal(action)

	handleLedgerAction(context.Background(), testLogger(), metrics, "varroa.ledger.actions.core", "core", rec, data)

	if got := ledgerActionRejectedSum(t, reader, "unknown_kind"); got != 1 {
		t.Fatalf("expected varroa_ledger_actions_rejected_total{reason=unknown_kind}=1, got %d", got)
	}
}

// TestHandleLedgerAction_OtherCluster_CountsForeignCluster covers the
// second guard behind the cluster-scoped subscription: an action that
// names another cluster takes no action, increments
// varroa_ledger_actions_rejected_total{reason="foreign_cluster"} by one,
// and leaves the unknown_kind count untouched.
func TestHandleLedgerAction_OtherCluster_CountsForeignCluster(t *testing.T) {
	t.Parallel()
	metrics, reader := newTestLedgerActionMetrics(t)
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionWakeCell, Cluster: "hive-b",
		Namespace: "cells-ns", Controller: "cell-a",
	}
	data, _ := json.Marshal(action)

	handleLedgerAction(context.Background(), testLogger(), metrics, "varroa.ledger.actions.core", "core", rec, data)

	if rec.wokeCalls != 0 || len(rec.sent) != 0 {
		t.Fatalf("expected no action for another cluster, got wokeCalls=%d sent=%d", rec.wokeCalls, len(rec.sent))
	}
	if got := ledgerActionRejectedSum(t, reader, "foreign_cluster"); got != 1 {
		t.Fatalf("expected varroa_ledger_actions_rejected_total{reason=foreign_cluster}=1, got %d", got)
	}
	if got := ledgerActionRejectedSum(t, reader, "unknown_kind"); got != 0 {
		t.Fatalf("expected varroa_ledger_actions_rejected_total{reason=unknown_kind}=0, got %d", got)
	}
}
