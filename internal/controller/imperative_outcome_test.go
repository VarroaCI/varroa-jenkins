package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

func outcomeTestController() *v1alpha1.Controller {
	return &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell-1", Namespace: "cells"}}
}

// TestImperativeFailureEvent asserts a failed cancel builds one activity
// row that names the actor, the kind, the job path, the run, the HTTP
// status, and the reason.
func TestImperativeFailureEvent(t *testing.T) {
	res := &mitev1.CommandResult{
		CommandId:   "cancel-build-run-1",
		Success:     false,
		Error:       "cancel build acme/app: HTTP 404: build not found",
		HttpStatus:  404,
		CommandType: mitev1.CommandTypeCancelBuild,
		JobPath:     "acme/app",
		RunId:       "run-1",
		Actor:       "alice",
	}
	event, ok := imperativeOutcomeEvent(outcomeTestController(), res, time.Unix(0, 0))
	if !ok {
		t.Fatal("want an activity row for a failed command")
	}
	if event.Type != imperativeFailedEventType || event.Source != "operator" || event.Reason != "cancel_build" || event.Severity != "warning" {
		t.Fatalf("event = %+v, want type %s, source operator, reason cancel_build, severity warning", event, imperativeFailedEventType)
	}
	if event.Actor != "alice" {
		t.Fatalf("actor = %q, want alice", event.Actor)
	}
	if event.Controller != "cell-1" || event.Namespace != "cells" || event.ItemPath != "acme/app" {
		t.Fatalf("event address = %s/%s %s, want cells/cell-1 acme/app", event.Namespace, event.Controller, event.ItemPath)
	}
	for _, want := range []string{"cancel_build", "acme/app", "run-1", "HTTP 404", "build not found"} {
		if !strings.Contains(event.Message, want) {
			t.Fatalf("message %q does not name %q", event.Message, want)
		}
	}
}

// TestImperativeFailureEventKeepsRecorderActor asserts a refused recorder
// cancel keeps its row and names the recorder as the actor.
func TestImperativeFailureEventKeepsRecorderActor(t *testing.T) {
	res := &mitev1.CommandResult{Error: "not found", CommandType: mitev1.CommandTypeCancelBuild, Actor: recordstore.LedgerActionRecorder}
	event, ok := imperativeOutcomeEvent(outcomeTestController(), res, time.Now())
	if !ok || event.Type != imperativeFailedEventType || event.Actor != recordstore.LedgerActionRecorder {
		t.Fatalf("event = %+v ok=%v, want a command.failed row with the recorder actor", event, ok)
	}
}

// TestImperativeSuccessEventForPersonSentRunCommand asserts a person's
// successful cancel, input answer, or replay builds one command.succeeded
// row that names the actor and the HTTP status.
func TestImperativeSuccessEventForPersonSentRunCommand(t *testing.T) {
	for _, kind := range []mitev1.CommandType{
		mitev1.CommandTypeCancelBuild, mitev1.CommandTypeProceedInput,
		mitev1.CommandTypeAbortInput, mitev1.CommandTypeReplayBuild,
	} {
		res := &mitev1.CommandResult{Success: true, HttpStatus: 200, CommandType: kind, JobPath: "acme/app", RunId: "run-1", Actor: "alice"}
		event, ok := imperativeOutcomeEvent(outcomeTestController(), res, time.Now())
		if !ok {
			t.Fatalf("%s: want a command.succeeded row", kind.Label())
		}
		if event.Type != imperativeSucceededEventType || event.Severity != "info" || event.Actor != "alice" || event.Reason != kind.Label() {
			t.Fatalf("%s: event = %+v, want a command.succeeded info row by alice", kind.Label(), event)
		}
		for _, want := range []string{kind.Label(), "acme/app", "run-1", "succeeded", "HTTP 200"} {
			if !strings.Contains(event.Message, want) {
				t.Fatalf("%s: message %q does not name %q", kind.Label(), event.Message, want)
			}
		}
	}
}

// TestImperativeOutcomeEventSkipsOtherSuccessesAndDeferral asserts the
// activity stream gets no row for a recorder success, a trigger success,
// a success with no actor, a non-run command success, or a deferral.
func TestImperativeOutcomeEventSkipsOtherSuccessesAndDeferral(t *testing.T) {
	for _, res := range []*mitev1.CommandResult{
		{Success: true, CommandType: mitev1.CommandTypeCancelBuild},
		{Success: true, CommandType: mitev1.CommandTypeCancelBuild, Actor: recordstore.LedgerActionRecorder},
		{Success: true, CommandType: mitev1.CommandTypeTriggerBuild, Actor: "alice"},
		{Success: true, CommandType: mitev1.CommandTypeSafeRestart, Actor: "alice"},
		{Deferred: true, DeferReason: "builds running", CommandType: mitev1.CommandTypeSafeRestart},
	} {
		if _, ok := imperativeOutcomeEvent(outcomeTestController(), res, time.Now()); ok {
			t.Fatalf("want no activity row for %+v", res)
		}
	}
}

// TestRecordImperativeOutcomeCountsBothOutcomes asserts the counter
// records a success and a failure under their kind and outcome labels.
func TestRecordImperativeOutcomeCountsBothOutcomes(t *testing.T) {
	t.Parallel()
	mp, reader := newTestMeterProvider(t)
	rec := newTestReconcilerWithMeterProvider(mp, newTestClient())
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
