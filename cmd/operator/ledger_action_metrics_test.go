package main

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// withTestLedgerActionMeter swaps in a test MeterProvider backed by a
// ManualReader and rebinds the package-level ledgerActionsRejected
// instrument against it, so a test observes the real production counter
// (design D15, task 9.1a) rather than a throwaway local one. It restores
// the original provider and binding on cleanup.
func withTestLedgerActionMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	bindLedgerActionMetrics()
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		bindLedgerActionMetrics()
	})
	return reader
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
	reader := withTestLedgerActionMeter(t)
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{Kind: "delete_everything", Cluster: "core", Namespace: "cells-ns", Controller: "cell-a"}
	data, _ := json.Marshal(action)

	handleLedgerAction(context.Background(), testLogger(), "core", rec, data)

	if got := ledgerActionRejectedSum(t, reader, "unknown_kind"); got != 1 {
		t.Fatalf("expected varroa_ledger_actions_rejected_total{reason=unknown_kind}=1, got %d", got)
	}
}

// TestHandleLedgerAction_OtherCluster_CountsNoRejection covers task
// 9.1a's distinction between "ignore" (an action naming another
// cluster) and "reject and count" (an unknown kind): the task text
// states these as two separate rules, so an action for another cluster
// must leave the counter untouched.
func TestHandleLedgerAction_OtherCluster_CountsNoRejection(t *testing.T) {
	reader := withTestLedgerActionMeter(t)
	rec := &fakeLedgerActionRunner{}
	action := recordstore.LedgerAction{
		Kind: recordstore.LedgerActionWakeCell, Cluster: "hive-b",
		Namespace: "cells-ns", Controller: "cell-a",
	}
	data, _ := json.Marshal(action)

	handleLedgerAction(context.Background(), testLogger(), "core", rec, data)

	if got := ledgerActionRejectedSum(t, reader, "unknown_kind"); got != 0 {
		t.Fatalf("expected no rejection counted for another cluster's action, got %d", got)
	}
}
