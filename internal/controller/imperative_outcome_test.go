package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func outcomeTestController() *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-1", Namespace: "cells"}}
}

// TestImperativeFailureEvent asserts a failed cancel builds one activity
// row that names the kind, the job path, the run, and the reason.
func TestImperativeFailureEvent(t *testing.T) {
	res := &mitev1.CommandResult{
		CommandId:   "cancel-build-run-1",
		Success:     false,
		Error:       "cancel build acme/app: HTTP 404: build not found",
		CommandType: mitev1.CommandTypeCancelBuild,
		JobPath:     "acme/app",
		RunId:       "run-1",
	}
	event, ok := imperativeFailureEvent(outcomeTestController(), res, time.Unix(0, 0))
	if !ok {
		t.Fatal("want an activity row for a failed command")
	}
	if event.Type != imperativeFailedEventType || event.Source != "operator" || event.Reason != "cancel_build" {
		t.Fatalf("event = %+v, want type %s, source operator, reason cancel_build", event, imperativeFailedEventType)
	}
	if event.Controller != "cell-1" || event.Namespace != "cells" || event.ItemPath != "acme/app" {
		t.Fatalf("event address = %s/%s %s, want cells/cell-1 acme/app", event.Namespace, event.Controller, event.ItemPath)
	}
	for _, want := range []string{"cancel_build", "acme/app", "run-1", "build not found"} {
		if !strings.Contains(event.Message, want) {
			t.Fatalf("message %q does not name %q", event.Message, want)
		}
	}
}

// TestImperativeFailureEventSkipsSuccessAndDeferral asserts the activity
// stream gets no row for a success or a deferred restart.
func TestImperativeFailureEventSkipsSuccessAndDeferral(t *testing.T) {
	for _, res := range []*mitev1.CommandResult{
		{Success: true, CommandType: mitev1.CommandTypeCancelBuild},
		{Deferred: true, DeferReason: "builds running", CommandType: mitev1.CommandTypeSafeRestart},
	} {
		if _, ok := imperativeFailureEvent(outcomeTestController(), res, time.Now()); ok {
			t.Fatalf("want no activity row for %+v", res)
		}
	}
}

// TestRecordImperativeOutcomeCountsBothOutcomes asserts the counter
// records a success and a failure under their kind and outcome labels.
func TestRecordImperativeOutcomeCountsBothOutcomes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	bindImperativeOutcomeMetrics()
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		bindImperativeOutcomeMetrics()
	})

	rec := newTestReconciler(newTestClient())
	ctx := context.Background()
	cr := outcomeTestController()
	rec.recordImperativeOutcome(ctx, cr, &mitev1.CommandResult{Success: true, CommandType: mitev1.CommandTypeCancelBuild})
	rec.recordImperativeOutcome(ctx, cr, &mitev1.CommandResult{Error: "not found", CommandType: mitev1.CommandTypeCancelBuild})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "varroa.imperative_command.results" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("data type = %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				kind, _ := dp.Attributes.Value(attribute.Key("kind"))
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				got[kind.AsString()+"/"+outcome.AsString()] += dp.Value
			}
		}
	}
	if got["cancel_build/success"] != 1 || got["cancel_build/failure"] != 1 {
		t.Fatalf("counts = %v, want one success and one failure for cancel_build", got)
	}
}
