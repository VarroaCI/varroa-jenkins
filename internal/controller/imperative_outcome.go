package controller

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// Imperative command outcome labels. A deferred result is a postponed
// SAFE_RESTART drain, not a failure, so it takes its own label.
const (
	imperativeOutcomeSuccess  = "success"
	imperativeOutcomeFailure  = "failure"
	imperativeOutcomeDeferred = "deferred"
)

// imperativeFailedEventType is the activity type of one failed imperative
// command result.
const imperativeFailedEventType = "command.failed"

// imperativeSucceededEventType is the activity type of one successful run
// command that a person sent.
const imperativeSucceededEventType = "command.succeeded"

// imperativeResultsTotal counts every imperative command result the
// operator drains, labelled by kind and outcome.
var imperativeResultsTotal metric.Int64Counter

func init() { bindImperativeOutcomeMetrics() }

// bindImperativeOutcomeMetrics (re)binds the counter against the current
// global MeterProvider. A test re-invokes it after it swaps in a test
// MeterProvider.
func bindImperativeOutcomeMetrics() {
	m := otel.Meter("varroa-operator")
	imperativeResultsTotal, _ = m.Int64Counter("varroa.imperative_command.results",
		metric.WithDescription("Imperative command results from the mite, labelled by kind and outcome"),
	)
}

// imperativeOutcome answers the outcome label of one result.
func imperativeOutcome(res *mitev1.CommandResult) string {
	switch {
	case res.Success:
		return imperativeOutcomeSuccess
	case res.Deferred:
		return imperativeOutcomeDeferred
	default:
		return imperativeOutcomeFailure
	}
}

// personSentRunCommand reports whether a result answers a run command
// that a person sent: a cancel, an input answer, or a replay whose actor
// is not empty and is not the recorder. A trigger is not in the set,
// because the recorder already writes a record for each trigger outcome.
func personSentRunCommand(res *mitev1.CommandResult) bool {
	switch res.CommandType {
	case mitev1.CommandTypeCancelBuild, mitev1.CommandTypeProceedInput,
		mitev1.CommandTypeAbortInput, mitev1.CommandTypeReplayBuild:
	default:
		return false
	}
	return res.Actor != "" && res.Actor != recordstore.LedgerActionRecorder
}

// imperativeOutcomeEvent builds the activity row of one imperative result.
// A failure always gets a command.failed row. A success gets a
// command.succeeded row only for a person-sent run command. Every other
// success and every deferral answers false: the activity stream holds
// audit-worthy events only.
func imperativeOutcomeEvent(cr *v1alpha1.Controller, res *mitev1.CommandResult, now time.Time) (activity.Event, bool) {
	outcome := imperativeOutcome(res)
	var eventType, severity, verb string
	switch {
	case outcome == imperativeOutcomeFailure:
		eventType, severity, verb = imperativeFailedEventType, "warning", "failed"
	case outcome == imperativeOutcomeSuccess && personSentRunCommand(res):
		eventType, severity, verb = imperativeSucceededEventType, "info", "succeeded"
	default:
		return activity.Event{}, false
	}
	kind := res.CommandType.Label()
	subject := kind
	if res.JobPath != "" {
		subject = fmt.Sprintf("%s of %s", kind, res.JobPath)
	}
	if res.RunId != "" {
		subject = fmt.Sprintf("%s (run %s)", subject, res.RunId)
	}
	message := fmt.Sprintf("%s %s", subject, verb)
	if res.HttpStatus != 0 {
		message = fmt.Sprintf("%s (HTTP %d)", message, res.HttpStatus)
	}
	if outcome == imperativeOutcomeFailure {
		message = fmt.Sprintf("%s: %s", message, res.Error)
	}
	return activity.Event{
		Timestamp:  now,
		Type:       eventType,
		Source:     "operator",
		Severity:   severity,
		Actor:      res.Actor,
		Controller: cr.Name,
		Namespace:  cr.Namespace,
		Reason:     kind,
		ItemPath:   res.JobPath,
		Message:    message,
	}, true
}

// recordImperativeOutcome counts one imperative result and publishes at
// most one activity row for it (imperativeOutcomeEvent). The mite and the
// gateway keep no other operator-visible record of a refused command.
func (r *Reconciler) recordImperativeOutcome(ctx context.Context, cr *v1alpha1.Controller, res *mitev1.CommandResult) {
	imperativeResultsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", res.CommandType.Label()),
		attribute.String("outcome", imperativeOutcome(res)),
	))
	if r.activityPublisher == nil {
		return
	}
	if event, ok := imperativeOutcomeEvent(cr, res, time.Now().UTC()); ok {
		r.activityPublisher.Publish(event)
	}
}
